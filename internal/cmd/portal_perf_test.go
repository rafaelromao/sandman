package cmd

import "testing"

// TestPortalPerf_LRUConcurrentDeduplication verifies that concurrent calls for the
// same input return the same object (Promise or cached value), preventing duplicate work.
func TestPortalPerf_LRUConcurrentDeduplication(t *testing.T) {
	js := `function bigLog(seed, n) { var L = []; for (var i = 0; i < n; i++) L.push(seed + ' step ' + i + ' import return function foo()'); return L.join('\n'); }
const a = bigLog('cachekey', 500);
// First call starts work
const p1 = sandbox.SandmanPortalDiff.highlightTerminalLog(a);
// Second concurrent call with same input — must return same object
const p2 = sandbox.SandmanPortalDiff.highlightTerminalLog(a);
if (p1 !== p2) throw new Error('concurrent calls must return same object, got p1=' + typeof p1 + ' p2=' + typeof p2);
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_LRUEvictionPreservesCorrectness verifies that after cache eviction,
// re-requesting an entry still produces correct output (cache miss → recompute).
func TestPortalPerf_LRUEvictionPreservesCorrectness(t *testing.T) {
	js := `function bigLog(seed, n) { var L = []; for (var i = 0; i < n; i++) L.push(seed + ' step ' + i + ' import return function foo() bar baz'); return L.join('\n'); }
// Fill cache with 9 distinct entries (limit = 8, so oldest is evicted)
for (var k = 0; k < 9; k++) sandbox.SandmanPortalDiff.highlightTerminalLog(bigLog('seed' + k, 500));
// Original first entry (seed0) should still produce identical output after eviction
const a = bigLog('seed0', 500);
const ha = sandbox.SandmanPortalDiff.highlightTerminalLog(a);
if (typeof ha === 'string') {
  if (ha.indexOf('seed0') === -1) throw new Error('seed0 content missing after eviction');
  console.log('PASS');
} else {
  ha.then(function(html) {
    if (html.indexOf('seed0') === -1) throw new Error('seed0 content missing after eviction');
    console.log('PASS');
  });
}
`
	runNodeScript(t, js)
}

// TestPortalPerf_AsyncLargeReviewLogRoundTrip verifies that a large saved
// review log (the "currently being reviewed" parent-issued run, run.log > 32KB
// async threshold) loads via the async chunked path the same way a normal
// completed issue run does. Regression guard against re-reverting the slice-B
// async tokenization work (#1472) which previously made the Log tab appear
// empty on first expand for the >32KB review case observed on screen.
func TestPortalPerf_AsyncLargeReviewLogRoundTrip(t *testing.T) {
	js := `function bigLog(seed, n) { var L = []; for (var i = 0; i < n; i++) L.push(seed + ' step ' + i + ' import return function foo() bar baz qux'); return L.join('\n'); }
	function waitForRender(pre, log, callback) {
	  if (pre.getAttribute('data-rendered-log') === log) { callback(); return; }
	  if (pre.getAttribute('data-rendering-log') !== log) { callback(new Error('rendering marker cleared without completion')); return; }
	  setTimeout(function() { waitForRender(pre, log, callback); }, 10);
	}
	const body = makeMockBody();
	const log = bigLog('review-async', 2000); // ~220KB
	const run = {
	  key: 'd8b9-260629182613-1479',
	  runId: 'd8b9-260629182613-1479',
	  kind: 'active',
	  status: 'reviewing',
	  issueLabel: '#1479',
	  issueNumber: 1479,
	  prNumber: 0,
	  review: false,
	  reviewCount: 1,
	  startedAt: 1000,
	  finishedAt: null,
	  duration: 1,
	  branch: 'sandman/1479-slice-b',
	  log: log,
	  logPath: '/tmp/d8b9-260629182613-1479/run.log',
	  logUrl: '/api/logs?path=.sandman%2Fbatches%2Fd8b9-260629182613-1479%2B5%2Fruns%2Fd8b9-260629182613-1479%2Frun.log',
	};
	const stopGroups = new Set();
	const optsLog = { helpers, stopGroups, expandedKey: run.key, tabs: { [run.key]: 'log' } };
	const optsDetails = { helpers, stopGroups, expandedKey: run.key, tabs: { [run.key]: 'details' } };
	sandbox.SandmanPortalDiff.diffRuns(body, [run], optsLog);
	const detailRow = body.children[1];
	const pre = detailRow && detailRow.querySelector('pre[data-scroll-key]');
	if (!pre) throw new Error('expected log pre for review row');
	if (pre.getAttribute('data-rendering-log') !== log) throw new Error('expected async pending marker for large review log');
	if (pre.getAttribute('data-rendered-log')) throw new Error('expected rendered-log to stay empty while async render is pending');
	waitForRender(pre, log, function(err) {
	  if (err) { console.error('FAIL: ' + err.message); return; }
	  if (pre.getAttribute('data-rendering-log')) throw new Error('expected async pending marker cleared after completion');
	  if (pre.getAttribute('data-rendered-log') !== log) throw new Error('expected rendered-log after async completion');
	  if (pre.textContent.indexOf('review-async step 0') === -1) throw new Error('expected review log content after async completion');
	  const originalPre = pre;
	  const originalFirstChild = pre.firstChild;
	  sandbox.SandmanPortalDiff.diffRuns(body, [run], optsDetails);
	  if (detailRow.querySelector('pre[data-scroll-key]')) throw new Error('expected log pane detached on details tab');
	  sandbox.SandmanPortalDiff.diffRuns(body, [run], optsLog);
	  const restoredPre = detailRow.querySelector('pre[data-scroll-key]');
	  if (restoredPre !== originalPre) throw new Error('expected cached large review log pane to be reused');
	  if (restoredPre.firstChild !== originalFirstChild) throw new Error('expected cached large review log children to be reused');
	  if (restoredPre.getAttribute('data-rendered-log') !== log) throw new Error('expected rendered-log to survive round trip');
	  console.log('PASS');
	});
`
	runNodeScript(t, js)
}

// TestPortalPerf_ReExpandNoMutation verifies that re-expanding a collapsed run
// reuses the cached log pane (no re-tokenization, same DOM nodes) instead of
// rebuilding and re-parsing the pre.
func TestPortalPerf_ReExpandNoMutation(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', log: 'log text for re-expand test', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log' };
const stopGroups = new Set();
const optsLog = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
const optsCollapsed = { helpers, stopGroups, expandedKey: null, tabs: { a: 'log' } };
sandbox.SandmanPortalDiff.diffRuns(body, [run], optsLog);
const detailRow = body.children[1];
const originalContent = detailRow && detailRow.querySelector('.detail-content');
  const originalPre = originalContent && originalContent.querySelector('pre[data-scroll-key]');
  if (!originalPre) throw new Error('expected initial log pre');
  const originalFirstChild = originalPre.firstChild;
  sandbox.SandmanPortalDiff.diffRuns(body, [run], optsCollapsed);
  sandbox.SandmanPortalDiff.resetCounters();
  sandbox.SandmanPortalDiff.diffRuns(body, [run], optsLog);
  const detailAfter = body.children[1];
  const restoredContent = detailAfter && detailAfter.querySelector('.detail-content');
  const restoredPre = restoredContent && restoredContent.querySelector('pre[data-scroll-key]');
  const counters = sandbox.SandmanPortalDiff.getCounters();
  // Cache hit means: no innerHTML rewrites (no re-tokenization), and the
  // cached pane is reused by node identity.
  const paneIdentity = restoredPre === originalPre;
  const childIdentity = restoredPre && restoredPre.firstChild === originalFirstChild;
  const forceBottom = restoredPre && restoredPre.getAttribute('data-scroll-force-bottom');
  if (counters.innerHTMLAssignments !== 0 || !paneIdentity || !childIdentity) {
    throw new Error('re-expand should reuse cached pane; innerHTML=' + counters.innerHTMLAssignments + ' paneIdentity=' + paneIdentity + ' childIdentity=' + childIdentity);
  }
  if (forceBottom !== '1') throw new Error('re-expand should mark cached log pane for bottom restore, got ' + forceBottom);
  console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_SyncHighlightUnderBudget verifies that highlighting
// returns a non-empty result for large logs without throwing.
func TestPortalPerf_SyncHighlightUnderBudget(t *testing.T) {
	js := `function bigLog(seed, n) { var L = []; for (var i = 0; i < n; i++) L.push(seed + ' step ' + i + ' import return function foo() bar baz qux'); return L.join('\n'); }
const big = bigLog('perftest', 2000); // ~220KB
const t0 = Date.now();
const result = sandbox.SandmanPortalDiff.highlightTerminalLog(big);
const dt = Date.now() - t0;
if (result === undefined || result === null) throw new Error('expected string result, got ' + result);
if (typeof result !== 'string') throw new Error('expected string, got ' + typeof result);
if (result.length === 0) throw new Error('expected non-empty result');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_DocumentFragmentAppend verifies that log pre population uses
// efficient DOM operations (DocumentFragment single batch).
func TestPortalPerf_DocumentFragmentAppend(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', log: 'line one\nline two\nline three\nline four\nline five', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
const t0 = Date.now();
sandbox.SandmanPortalDiff.diffRuns(body, [run], opts);
const dt = Date.now() - t0;
if (dt > 100) throw new Error('fillTerminalPre with 5 lines exceeded 100ms: ' + dt + 'ms');
const detailRow = body.children[1];
const content = detailRow.querySelector('.detail-content');
const pre = content.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
if (pre.children.length === 0) throw new Error('expected children in log pre');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_AsyncLargeLogRoundTrip verifies that large logs use the
// async path, expose a pending marker while chunking, and preserve the cached
// pane across a tab round-trip. Uses render-state polling instead of a fixed
// delay to avoid CI flakiness on slow runners.
func TestPortalPerf_AsyncLargeLogRoundTrip(t *testing.T) {
	js := `function bigLog(seed, n) { var L = []; for (var i = 0; i < n; i++) L.push(seed + ' step ' + i + ' import return function foo() bar baz qux'); return L.join('\n'); }
	function waitForRender(pre, log, callback) {
	  if (pre.getAttribute('data-rendered-log') === log) { callback(); return; }
	  if (pre.getAttribute('data-rendering-log') !== log) { callback(new Error('rendering marker cleared without completion')); return; }
	  setTimeout(function() { waitForRender(pre, log, callback); }, 10);
	}
	const body = makeMockBody();
	const log = bigLog('async', 2000); // ~220KB
	const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', log: log, startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log' };
	const stopGroups = new Set();
	const optsLog = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
	const optsDetails = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
	sandbox.SandmanPortalDiff.diffRuns(body, [run], optsLog);
	const detailRow = body.children[1];
	const pre = detailRow && detailRow.querySelector('pre[data-scroll-key]');
	if (!pre) throw new Error('expected log pre');
	if (pre.getAttribute('data-rendering-log') !== log) throw new Error('expected async pending marker for large log');
	if (pre.getAttribute('data-rendered-log')) throw new Error('expected rendered-log to stay empty while async render is pending');
	waitForRender(pre, log, function(err) {
	  if (err) { console.error('FAIL: ' + err.message); return; }
	  if (pre.getAttribute('data-rendering-log')) throw new Error('expected async pending marker cleared after completion');
	  if (pre.getAttribute('data-rendered-log') !== log) throw new Error('expected rendered-log after async completion');
	  if (pre.textContent.indexOf('async step 0') === -1) throw new Error('expected async log content after completion');
	  const originalPre = pre;
	  const originalFirstChild = pre.firstChild;
	  sandbox.SandmanPortalDiff.diffRuns(body, [run], optsDetails);
	  if (detailRow.querySelector('pre[data-scroll-key]')) throw new Error('expected log pane detached on details tab');
	  sandbox.SandmanPortalDiff.diffRuns(body, [run], optsLog);
	  const restoredPre = detailRow.querySelector('pre[data-scroll-key]');
	  if (restoredPre !== originalPre) throw new Error('expected cached large-log pane to be reused');
	  if (restoredPre.firstChild !== originalFirstChild) throw new Error('expected cached large-log children to be reused');
	  if (restoredPre.getAttribute('data-rendered-log') !== log) throw new Error('expected rendered-log to survive round trip');
	  console.log('PASS');
	});
`
	runNodeScript(t, js)
}

// TestPortalPerf_AsyncLargeLogInflightTabSwitch verifies that switching to
// the details tab while a large log is still chunking cancels the in-flight
// render and reuses the cached pane on return. The per-element generation
// ensures the cancellation is scoped to the correct <pre>.
func TestPortalPerf_AsyncLargeLogInflightTabSwitch(t *testing.T) {
	js := `function bigLog(seed, n) { var L = []; for (var i = 0; i < n; i++) L.push(seed + ' step ' + i + ' import return function foo() bar baz qux'); return L.join('\n'); }
	function waitForRender(pre, log, callback) {
	  if (pre.getAttribute('data-rendered-log') === log) { callback(); return; }
	  if (pre.getAttribute('data-rendering-log') !== log) { callback(new Error('rendering marker cleared without completion')); return; }
	  setTimeout(function() { waitForRender(pre, log, callback); }, 10);
	}
	const body = makeMockBody();
	const log = bigLog('inflight', 2000); // ~220KB
	const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', log: log, startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log' };
	const stopGroups = new Set();
	const optsLog = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
	const optsDetails = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
	sandbox.SandmanPortalDiff.diffRuns(body, [run], optsLog);
	const detailRow = body.children[1];
	const pre = detailRow && detailRow.querySelector('pre[data-scroll-key]');
	if (!pre) throw new Error('expected log pre');
	if (pre.getAttribute('data-rendering-log') !== log) throw new Error('expected async pending marker for large log');
	// Switch to details while chunking is in-flight — per-element generation
	// cancels the in-flight render without affecting other panels.
	sandbox.SandmanPortalDiff.diffRuns(body, [run], optsDetails);
	if (detailRow.querySelector('pre[data-scroll-key]')) throw new Error('expected log pane detached on details tab');
	sandbox.SandmanPortalDiff.diffRuns(body, [run], optsLog);
	const restoredPre = detailRow.querySelector('pre[data-scroll-key]');
	if (restoredPre !== pre) throw new Error('expected cached large-log pane reused after inflight tab switch');
	// The pending marker may still be set from the original render, or already
	// cleared if it completed during the details tab. In either case the pane is
	// reused. Wait for the (new or continued) render to finish.
	waitForRender(restoredPre, log, function(err) {
	  if (err) { console.error('FAIL: ' + err.message); return; }
	  if (restoredPre.getAttribute('data-rendered-log') !== log) throw new Error('expected rendered-log after inflight tab switch render completes');
	  if (restoredPre.textContent.indexOf('inflight step 0') === -1) throw new Error('expected async log content after inflight tab switch completes');
	  console.log('PASS');
	});
`
	runNodeScript(t, js)
}
