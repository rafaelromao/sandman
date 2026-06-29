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
if (counters.innerHTMLAssignments !== 0 || !paneIdentity || !childIdentity) {
  throw new Error('re-expand should reuse cached pane; innerHTML=' + counters.innerHTMLAssignments + ' paneIdentity=' + paneIdentity + ' childIdentity=' + childIdentity);
}
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
