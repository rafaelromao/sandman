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

// TestPortalPerf_LogPaneLRUEvictionPreservesTouchedEntry verifies that the
// log-pane cache drops only the least-recently-used pane on overflow and keeps
// the rest of the cached panes intact.
func TestPortalPerf_LogPaneLRUEvictionPreservesTouchedEntry(t *testing.T) {
	js := `const body = makeMockBody();
const stopGroups = new Set();
function run(n) { return { key: 'r' + n, kind: 'active', status: 'running', issueLabel: '#' + n, runId: 'r' + n, log: 'log ' + n }; }

for (var k = 1; k <= 8; k++) sandbox.SandmanPortalDiff.tokenizeForCache(run(k), helpers);
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 8) throw new Error('expected cache size 8 before eviction, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());

const touched = run(2);
const optsLog = { helpers, stopGroups, expandedKey: touched.key, tabs: { [touched.key]: 'log' } };
const optsDetails = { helpers, stopGroups, expandedKey: touched.key, tabs: { [touched.key]: 'details' } };
sandbox.SandmanPortalDiff.diffRuns(body, [touched], optsLog);
sandbox.SandmanPortalDiff.diffRuns(body, [touched], optsDetails);

sandbox.SandmanPortalDiff.tokenizeForCache(run(9), helpers);
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 8) throw new Error('expected cache size to stay capped at 8, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
if (sandbox.SandmanPortalDiff.hasLogPaneCached('r1')) throw new Error('expected oldest pane r1 to be evicted');
if (!sandbox.SandmanPortalDiff.hasLogPaneCached('r2')) throw new Error('expected touched pane r2 to survive eviction');
for (var k = 3; k <= 8; k++) {
  if (!sandbox.SandmanPortalDiff.hasLogPaneCached('r' + k)) throw new Error('expected pane r' + k + ' to survive eviction');
}
if (!sandbox.SandmanPortalDiff.hasLogPaneCached('r9')) throw new Error('expected new pane r9 to be cached');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_AsyncLargeReviewLogRoundTrip verifies that a large saved
// review log (the "currently being reviewed" parent-issued run, run.log well
// above the async chunk threshold) loads via the async chunked path the same
// way a normal completed issue run does. Regression guard against re-reverting
// the slice-B async tokenization work (#1472) which previously made the Log
// tab appear empty on first expand for the large review case observed on
// screen.
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
	  key: '260629182613-d8b9-1479',
	  runId: '260629182613-d8b9-1479',
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
	  branch: '1479-slice-b',
	  log: log,
	  logPath: '/tmp/260629182613-d8b9-1479/run.log',
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

// TestPortalPerf_AsyncMidBandAndSyncSmallAt16KBThreshold is the regression
// guard for the threshold lowered from 32 KB to 16 KB (#1561). It exercises
// both sides of the new threshold:
//  1. Mid-band (~20 KB) log is above the new 16 KB threshold and below the
//     old 32 KB threshold, so it takes the async chunked path on the new
//     threshold (and would have taken the sync path on the old one). The
//     pre must show data-rendering-log synchronously and data-rendered-log
//     only after chunking completes.
//  2. Small (~4 KB) log stays below the new 16 KB threshold and takes the
//     synchronous path: data-rendered-log set synchronously, no lingering
//     data-rendering-log.
func TestPortalPerf_AsyncMidBandAndSyncSmallAt16KBThreshold(t *testing.T) {
	js := `function bigLog(seed, n) { var L = []; for (var i = 0; i < n; i++) L.push(seed + ' step ' + i + ' import return function foo() bar baz qux'); return L.join('\n'); }
	function waitForRender(pre, log, callback) {
	  if (pre.getAttribute('data-rendered-log') === log) { callback(); return; }
	  if (pre.getAttribute('data-rendering-log') !== log) { callback(new Error('rendering marker cleared without completion')); return; }
	  setTimeout(function() { waitForRender(pre, log, callback); }, 10);
	}

	// --- Assertion 1: ~20 KB mid-band log takes the async chunked path. ---
	const midbandLog = bigLog('midband', 373); // 21523 bytes > 16 KB threshold
	const midbandRun = { key: 'midband', kind: 'completed', status: 'success', issueLabel: 'M', runId: 'r-mid', log: midbandLog, startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/mid.log' };
	const midBody = makeMockBody();
	const midOpts = { helpers, stopGroups: new Set(), expandedKey: 'midband', tabs: { midband: 'log' } };
	sandbox.SandmanPortalDiff.diffRuns(midBody, [midbandRun], midOpts);
	const midDetailRow = midBody.children[1];
	const midPre = midDetailRow && midDetailRow.querySelector('pre[data-scroll-key]');
	if (!midPre) throw new Error('expected log pre for mid-band run');
	if (midPre.getAttribute('data-rendering-log') !== midbandLog) throw new Error('expected async pending marker for mid-band log (above new 16KB threshold)');
	if (midPre.getAttribute('data-rendered-log')) throw new Error('expected rendered-log to stay empty while async render is pending');
	waitForRender(midPre, midbandLog, function(err) {
	  if (err) { console.error('FAIL: ' + err.message); return; }
	  if (midPre.getAttribute('data-rendering-log')) throw new Error('expected async pending marker cleared after completion');
	  if (midPre.getAttribute('data-rendered-log') !== midbandLog) throw new Error('expected rendered-log after async completion');
	  if (midPre.textContent.indexOf('midband step 0') === -1) throw new Error('expected mid-band log content after completion');

	  // --- Assertion 2: ~4 KB small log still takes the synchronous path. ---
	  const smallLog = bigLog('small', 73); // 4004 bytes < 16 KB threshold
	  const smallRun = { key: 'small', kind: 'completed', status: 'success', issueLabel: 'S', runId: 'r-small', log: smallLog, startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/small.log' };
	  const smallBody = makeMockBody();
	  const smallOpts = { helpers, stopGroups: new Set(), expandedKey: 'small', tabs: { small: 'log' } };
	  sandbox.SandmanPortalDiff.diffRuns(smallBody, [smallRun], smallOpts);
	  const smallDetailRow = smallBody.children[1];
	  const smallPre = smallDetailRow && smallDetailRow.querySelector('pre[data-scroll-key]');
	  if (!smallPre) throw new Error('expected log pre for small run');
	  if (smallPre.getAttribute('data-rendered-log') !== smallLog) throw new Error('expected sync rendered-log set immediately for small log');
	  if (smallPre.getAttribute('data-rendering-log')) throw new Error('expected sync path: no lingering data-rendering-log for small log');
	  if (smallPre.textContent.indexOf('small step 0') === -1) throw new Error('expected small log content after sync render');
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

// TestPortalPerf_StreamCoalescer_LeadingFlushBatchesOneLineIntoFragmentAppend
// SSE coalescer work for #1563: a single scheduled line
// should land as a single flush that calls highlightTerminalLog exactly
// once with the joined batch and appends the result to the run's <pre>
// using a single DocumentFragment.appendChild. appendStreamLine no longer
// touches the DOM in the SSE hot path.
func TestPortalPerf_StreamCoalescer_LeadingFlushBatchesOneLineIntoFragmentAppend(t *testing.T) {
	js := `
const rafQueue = [];
function fakeRaf(cb) { rafQueue.push(cb); }
const fakeDoc = {
  createDocumentFragment() { return { nodeType: 11, children: [], appendChild(child) { this.children.push(child); return child; } }; },
  createElement(tag) {
    const el = { nodeType: 1, tagName: String(tag || "DIV").toUpperCase(), children: [], _innerHTML: "",
      appendChild(child) { child.parentNode = el; this.children.push(child); return child; },
      get firstChild() { return this.children[0] || null; },
      set innerHTML(v) { this._innerHTML = v; this.children = []; },
      get innerHTML() { return this._innerHTML || ""; },
    };
    return el;
  },
};

const pre = {
  tagName: 'PRE', nodeType: 1, children: [],
  setAttribute(name, value) { this[name] = value; if (name === 'data-rendered-log') this.dataRendered = value; },
  getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
  removeAttribute(name) { delete this[name]; },
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; },
  scrollTop: 0, scrollHeight: 0,
  get firstChild() { return this.children[0] || null; },
};
pre.setAttribute('data-rendered-log', '');

const highlightCalls = [];
const highlight = function(text) { highlightCalls.push(text); return '<span>' + text + '</span>'; };
const preFor = function(runKey) { return pre; };
const portalScroll = { isSticky() { return true; } };

const coalescer = createStreamCoalescer({
  rAF: fakeRaf, highlight: highlight, preFor: preFor, portalScroll: portalScroll, doc: fakeDoc, cap: 16,
});
coalescer.seedKnownLines('a', []);

// 1 incoming SSE line in the same task that triggered us
coalescer.scheduleLine('a', 'line1');
if (rafQueue.length !== 1) throw new Error('expected one rAF scheduled, got ' + rafQueue.length);
if (coalescer.pendingSize('a') !== 1) throw new Error('expected buffer to hold 1 line, got ' + coalescer.pendingSize('a'));

// Drive the leading flush
rafQueue.shift()();

// 1 single flush means exactly 1 highlight call with the joined text
if (highlightCalls.length !== 1) throw new Error('expected 1 highlight call, got ' + highlightCalls.length);
if (highlightCalls[0] !== 'line1\n') throw new Error('expected highlight input "line1\\n", got ' + JSON.stringify(highlightCalls[0]));

// Exactly one appendChild to the pre (the DocumentFragment), so the
// per-line scratch-and-walk pattern no longer exists.
if (pre.children.length !== 1) throw new Error('expected 1 child in pre after flush, got ' + pre.children.length);
if (pre.dataRendered !== 'line1\n') throw new Error('expected data-rendered-log updated, got ' + JSON.stringify(pre.dataRendered));
if (coalescer.pendingSize('a') !== 0) throw new Error('expected buffer to drain to 0, got ' + coalescer.pendingSize('a'));
console.log('PASS');
`
	runStreamCoalescerScript(t, js)
}

// TestPortalPerf_StreamCoalescer_DedupAcrossBufferedBatch
// coalescer work: duplicate lines within the same
// pending batch must not produce duplicate DOM writes; lines already in
// the run's known-lines set must be dropped at schedule time.
func TestPortalPerf_StreamCoalescer_DedupAcrossBufferedBatch(t *testing.T) {
	js := `
const rafQueue = [];
function fakeRaf(cb) { rafQueue.push(cb); }
const fakeDoc = {
  createDocumentFragment() { return { nodeType: 11, children: [], appendChild(child) { this.children.push(child); return child; } }; },
  createElement(tag) {
    const el = { nodeType: 1, tagName: String(tag || "DIV").toUpperCase(), children: [], _innerHTML: "",
      appendChild(child) { child.parentNode = el; this.children.push(child); return child; },
      get firstChild() { return this.children[0] || null; },
      set innerHTML(v) { this._innerHTML = v; this.children = []; },
      get innerHTML() { return this._innerHTML || ""; },
    };
    return el;
  },
};

const pre = {
  tagName: 'PRE', nodeType: 1, children: [], dataRendered: '',
  setAttribute(name, value) { this[name] = value; if (name === 'data-rendered-log') this.dataRendered = value; },
  getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
  removeAttribute(name) { delete this[name]; },
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; },
  scrollTop: 0, scrollHeight: 0,
  get firstChild() { return this.children[0] || null; },
};
pre.setAttribute('data-rendered-log', '');

const highlightCalls = [];
const highlight = function(text) { highlightCalls.push(text); return '<span>' + text + '</span>'; };
const preFor = function(runKey) { return pre; };
const portalScroll = { isSticky() { return true; } };

const coalescer = createStreamCoalescer({
  rAF: fakeRaf, highlight: highlight, preFor: preFor, portalScroll: portalScroll, doc: fakeDoc, cap: 16,
});

// 'dup' is already in the run's known-lines (e.g. already in the cached
// snapshot at startRunStream time) — scheduleLine must drop it before
// touching the buffer.
coalescer.seedKnownLines('a', ['dup']);
if (coalescer.pendingSize('a') !== 0) throw new Error('expected empty buffer after seedKnownLines, got ' + coalescer.pendingSize('a'));

coalescer.scheduleLine('a', 'dup');
coalescer.scheduleLine('a', 'new');
coalescer.scheduleLine('a', 'dup'); // duplicate inside buffered batch

if (coalescer.pendingSize('a') !== 1) throw new Error('expected buffer to retain only 1 unique line, got ' + coalescer.pendingSize('a'));

rafQueue.shift()();
if (highlightCalls.length !== 1) throw new Error('expected 1 highlight call, got ' + highlightCalls.length);
if (highlightCalls[0] !== 'new\n') throw new Error('expected highlight input "new\\n", got ' + JSON.stringify(highlightCalls[0]));
console.log('PASS');
`
	runStreamCoalescerScript(t, js)
}

// TestPortalPerf_StreamCoalescer_BurstOf100MessagesProducesAtMost3Flushes
// SSE coalescer headline acceptance test from #1563: 100 SSE messages
// over a controlled rAF clock must coalesce into <=3 flushes, sticky-bottom
// must be preserved after each batch.
func TestPortalPerf_StreamCoalescer_BurstOf100MessagesProducesAtMost3Flushes(t *testing.T) {
	js := `
const rafQueue = [];
function fakeRaf(cb) { rafQueue.push(cb); }

const pre = {
  tagName: 'PRE', nodeType: 1, children: [], dataRendered: '',
  setAttribute(name, value) { this[name] = value; if (name === 'data-rendered-log') this.dataRendered = value; },
  getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
  removeAttribute(name) { delete this[name]; },
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; },
  scrollTop: 0, scrollHeight: 0,
  get firstChild() { return this.children[0] || null; },
};
pre.setAttribute('data-rendered-log', '');

const fakeDoc = {
  createDocumentFragment() { return { nodeType: 11, children: [], appendChild(child) { this.children.push(child); return child; } }; },
  createElement(tag) {
    const el = { nodeType: 1, tagName: String(tag || "DIV").toUpperCase(), children: [], _innerHTML: "",
      appendChild(child) { child.parentNode = el; this.children.push(child); return child; },
      get firstChild() { return this.children[0] || null; },
      set innerHTML(v) { this._innerHTML = v; this.children = []; },
      get innerHTML() { return this._innerHTML || ""; },
    };
    return el;
  },
};
const highlightCalls = [];
const highlight = function(text) { highlightCalls.push(text); return '<span>' + text + '</span>'; };
const preFor = function() { return pre; };
const portalScroll = { isSticky() { return true; } };

const coalescer = createStreamCoalescer({
  rAF: fakeRaf, highlight: highlight, preFor: preFor, portalScroll: portalScroll, doc: fakeDoc, cap: 16,
});
coalescer._debug.enableCounters();
coalescer.seedKnownLines('a', []);

// Simulate the worst-case click-burst scenario: 100 SSE messages land
// in a single event-loop tick so they all coalesce into one trailing
// rAF callback. The coalescer's tail-most-first contract keeps the
// most recent 16 lines (the cap) and drops the first 84 — the issue's
// "tail matters most" framing explicitly permits this. The leading
// flush + at-most-two-trailing-bursts pattern from the issue narrows
// to a single flush for a single-tick burst.
for (let i = 0; i < 100; i++) coalescer.scheduleLine('a', 'line' + i);
// Single tight rAF window: just fire the rAF.
while (rafQueue.length) rafQueue.shift()();

const flushCount = coalescer._debug.flushCount();
// The issue's contract is "<=3 flushes per 100 ms" — the leading-flush
// plus at-most-two-trailing-bursts pattern. We assert a hard upper
// bound of 3 here, and the lower bound of at least 1 to
// catch any regression that drops flushes entirely.
if (flushCount > 3) throw new Error('expected at most 3 flushes for 100-message burst, got ' + flushCount);
if (flushCount < 1) throw new Error('expected at least 1 flush, got 0');
// 100 unique messages must have triggered at least one tokenize + DOM write.
if (highlightCalls.length !== flushCount) throw new Error('expected one highlight call per flush, got ' + highlightCalls.length + ' highlights vs ' + flushCount + ' flushes');
// Each highlight input should contain a non-empty joined block ending in newline.
for (let i = 0; i < highlightCalls.length; i++) {
  if (highlightCalls[i].slice(-1) !== '\n') throw new Error('expected highlight input to end with newline, got ' + JSON.stringify(highlightCalls[i]));
}

// Sticky-bottom: after every flush the pre's scrollTop is at scrollHeight.
if (pre.scrollTop !== pre.scrollHeight) throw new Error('expected sticky-bottom pre.scrollTop === scrollHeight, got scrollTop=' + pre.scrollTop + ' scrollHeight=' + pre.scrollHeight);
console.log('PASS');
`
	runStreamCoalescerScript(t, js)
}

// TestPortalPerf_StreamCoalescer_OverflowAtCapTailMostFirst
// when the per-run buffer grows past cap, the trailing rAF
// truncates the buffer to the most recent `cap` lines before calling
// highlight. The oldest lines stay buffered only as long as the buffer
// itself does; they are flushed first. The headline burst test (≤3 flushes)
// still passes because no synchronous flush is scheduled on overflow.
func TestPortalPerf_StreamCoalescer_OverflowAtCapTailMostFirst(t *testing.T) {
	js := `
const rafQueue = [];
function fakeRaf(cb) { rafQueue.push(cb); }
const fakeDoc = {
  createDocumentFragment() { return { nodeType: 11, children: [], appendChild(c) { this.children.push(c); return c; } }; },
  createElement(tag) { return { nodeType: 1, children: [], _innerHTML: '', appendChild(c) { this.children.push(c); return c; }, get firstChild() { return this.children[0] || null; }, set innerHTML(v) { this._innerHTML = v; this.children = []; }, get innerHTML() { return this._innerHTML || ''; } }; },
};

const pre = {
  tagName: 'PRE', nodeType: 1, children: [], dataRendered: '',
  setAttribute(name, value) { this[name] = value; if (name === 'data-rendered-log') this.dataRendered = value; },
  getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
  removeAttribute(name) { delete this[name]; },
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; },
  scrollTop: 0, scrollHeight: 0,
  get firstChild() { return this.children[0] || null; },
};
pre.setAttribute('data-rendered-log', '');

const highlightCalls = [];
const highlight = function(text) { highlightCalls.push(text); return '<span>' + text + '</span>'; };
const preFor = function() { return pre; };
const portalScroll = { isSticky() { return false; } };

const coalescer = createStreamCoalescer({
  rAF: fakeRaf, highlight: highlight, preFor: preFor, portalScroll: portalScroll, doc: fakeDoc, cap: 4,
});
coalescer._debug.enableCounters();
coalescer.seedKnownLines('a', []);

// Schedule 6 distinct lines into a cap=4 coalescer. No sync flush on
// overflow — only the trailing rAF drains, and it must keep the most
// recent 4 lines (tail-most-first).
coalescer.scheduleLine('a', 'A1');
coalescer.scheduleLine('a', 'A2');
coalescer.scheduleLine('a', 'A3');
coalescer.scheduleLine('a', 'A4');
coalescer.scheduleLine('a', 'A5');
coalescer.scheduleLine('a', 'A6');
if (coalescer._debug.flushCount() !== 0) throw new Error('expected 0 flushes before rAF drain, got ' + coalescer._debug.flushCount());
if (coalescer.pendingSize('a') !== 6) throw new Error('expected buffer to hold all 6 lines pre-flush, got ' + coalescer.pendingSize('a'));

while (rafQueue.length) rafQueue.shift()();

if (coalescer._debug.flushCount() !== 1) throw new Error('expected exactly 1 flush after rAF drain, got ' + coalescer._debug.flushCount());
if (highlightCalls.length !== 1) throw new Error('expected 1 highlight call after flush, got ' + highlightCalls.length);
// Tail-most-first: the highlight input should contain the last 4 lines (A3-A6), not the first ones.
if (highlightCalls[0].indexOf('A4') < 0) throw new Error('expected tail-most-first highlight to include A4, got ' + JSON.stringify(highlightCalls[0]));
if (highlightCalls[0].indexOf('A3') < 0) throw new Error('expected tail-most-first highlight to include A3, got ' + JSON.stringify(highlightCalls[0]));
if (highlightCalls[0].indexOf('A6') < 0) throw new Error('expected tail-most-first highlight to include A6, got ' + JSON.stringify(highlightCalls[0]));
// data-rendered-log mirrors only the tail (oldest line dropped from a single batch).
if (pre.dataRendered !== 'A3\nA4\nA5\nA6\n') throw new Error('expected data-rendered-log to hold only tail 4 lines, got ' + JSON.stringify(pre.dataRendered));
console.log('PASS');
`
	runStreamCoalescerScript(t, js)
}

// TestPortalPerf_StreamCoalescer_StickyToggleReflectsRunScrolling
// sticky=false must leave scrollTop alone.
func TestPortalPerf_StreamCoalescer_StickyToggleReflectsRunScrolling(t *testing.T) {
	js := `
const rafQueue = [];
function fakeRaf(cb) { rafQueue.push(cb); }
const fakeDoc = {
  createDocumentFragment() { return { nodeType: 11, children: [], appendChild(child) { this.children.push(child); return child; } }; },
  createElement(tag) {
    const el = { nodeType: 1, tagName: String(tag || "DIV").toUpperCase(), children: [], _innerHTML: "",
      appendChild(child) { child.parentNode = el; this.children.push(child); return child; },
      get firstChild() { return this.children[0] || null; },
      set innerHTML(v) { this._innerHTML = v; this.children = []; },
      get innerHTML() { return this._innerHTML || ""; },
    };
    return el;
  },
};

const pre = {
  tagName: 'PRE', nodeType: 1, children: [], dataRendered: '',
  setAttribute(name, value) { this[name] = value; if (name === 'data-rendered-log') this.dataRendered = value; },
  getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
  removeAttribute(name) { delete this[name]; },
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; },
  scrollTop: 42, scrollHeight: 1000,
  get firstChild() { return this.children[0] || null; },
};
pre.setAttribute('data-rendered-log', '');

const highlight = function(text) { return '<span>' + text + '</span>'; };
const preFor = function() { return pre; };
const stickyStates = { a: false };
const portalScroll = { isSticky(runKey) { return stickyStates[runKey] === true; } };

const coalescer = createStreamCoalescer({
  rAF: fakeRaf, highlight: highlight, preFor: preFor, portalScroll: portalScroll, doc: fakeDoc, cap: 16,
});
coalescer.seedKnownLines('a', []);
coalescer.scheduleLine('a', 'one');
rafQueue.shift()();

if (pre.scrollTop !== 42) throw new Error('expected scrollTop preserved (sticky=false), got ' + pre.scrollTop);

stickyStates.a = true;
coalescer.scheduleLine('a', 'two');
rafQueue.shift()();
if (pre.scrollTop !== pre.scrollHeight) throw new Error('expected scrollTop=scrollHeight (sticky=true), got ' + pre.scrollTop);
console.log('PASS');
`
	runStreamCoalescerScript(t, js)
}

// TestPortalPerf_StreamCoalescer_SubjectSwitchClearsPendingBuffer
// is the subject-switch behavior from #1563 AC: switching the live run
// while lines are buffered must drop the previous run's pending buffer so
// no stale DOM writes happen.
func TestPortalPerf_StreamCoalescer_SubjectSwitchClearsPendingBuffer(t *testing.T) {
	js := `
const rafQueue = [];
function fakeRaf(cb) { rafQueue.push(cb); }
const fakeDoc = {
  createDocumentFragment() { return { nodeType: 11, children: [], appendChild(child) { this.children.push(child); return child; } }; },
  createElement(tag) {
    const el = { nodeType: 1, tagName: String(tag || "DIV").toUpperCase(), children: [], _innerHTML: "",
      appendChild(child) { child.parentNode = el; this.children.push(child); return child; },
      get firstChild() { return this.children[0] || null; },
      set innerHTML(v) { this._innerHTML = v; this.children = []; },
      get innerHTML() { return this._innerHTML || ""; },
    };
    return el;
  },
};

const pres = {};
function makePre(key) {
  return {
    tagName: 'PRE', nodeType: 1, children: [], dataRendered: '',
    setAttribute(name, value) { this[name] = value; if (name === 'data-rendered-log') this.dataRendered = value; },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; },
    appendChild(child) { child.parentNode = this; this.children.push(child); return child; },
    scrollTop: 0, scrollHeight: 0,
    get firstChild() { return this.children[0] || null; },
  };
}
pres.a = makePre('a'); pres.a.setAttribute('data-rendered-log', '');
pres.b = makePre('b'); pres.b.setAttribute('data-rendered-log', '');

const highlightCalls = [];
const highlight = function(text) { highlightCalls.push(text); return '<span>' + text + '</span>'; };
const preFor = function(key) { return pres[key]; };
const portalScroll = { isSticky() { return false; } };

const coalescer = createStreamCoalescer({
  rAF: fakeRaf, highlight: highlight, preFor: preFor, portalScroll: portalScroll, doc: fakeDoc, cap: 16,
});
coalescer.seedKnownLines('a', []);
coalescer.seedKnownLines('b', []);

// Buffer pending lines for run A
coalescer.scheduleLine('a', 'stale-A-1');
coalescer.scheduleLine('a', 'stale-A-2');
if (coalescer.pendingSize('a') !== 2) throw new Error('expected buffer size 2 on A, got ' + coalescer.pendingSize('a'));

// Subject switch: clear A's pending buffer without flushing, then buffer
// lines for the new run B
coalescer.clearBuffer('a');
if (coalescer.pendingSize('a') !== 0) throw new Error('expected A buffer cleared after subject switch, got ' + coalescer.pendingSize('a'));
coalescer.scheduleLine('b', 'fresh-B-1');

// Drain all queued rAFs. The A rAF is still scheduled from before
// clearBuffer; it will fire on an empty buffer (no-op). Only B's rAF
// should call highlight. The point of clearBuffer is that A's BUFFER
// is gone, so even if the rAF fires there is no DOM write to leak.
while (rafQueue.length) rafQueue.shift()();
if (highlightCalls.length !== 1) throw new Error('expected exactly 1 highlight call (B only), got ' + highlightCalls.length);
if (highlightCalls[0] !== 'fresh-B-1\n') throw new Error('expected highlight call to be B line, got ' + JSON.stringify(highlightCalls[0]));
if (pres.a.children.length !== 0) throw new Error('expected pre A untouched, got ' + pres.a.children.length + ' children');
if (pres.a.dataRendered !== '') throw new Error('expected pre A data-rendered-log unchanged, got ' + JSON.stringify(pres.a.dataRendered));
if (pres.b.dataRendered !== 'fresh-B-1\n') throw new Error('expected pre B data-rendered-log updated, got ' + JSON.stringify(pres.b.dataRendered));
console.log('PASS');
`
	runStreamCoalescerScript(t, js)
}
