package cmd

import "testing"

// TestPortalPerf_PrewarmLogPaneCache_PopulatesFromTokenizeForCache is the
// tracer bullet for issue #1564: tokenizeForCache writes a pane into the
// same logPaneCache Map the live pane-swap path reads from, so the next
// click on the pre-warmed run hits the cache instead of the cold path.
func TestPortalPerf_PrewarmLogPaneCache_PopulatesFromTokenizeForCache(t *testing.T) {
	js := `const run = { key: 'a', kind: 'active', status: 'running', issueLabel: '#1', runId: 'r1', log: 'line one\nline two\nline three' };
const pane = sandbox.SandmanPortalDiff.tokenizeForCache(run, helpers);
if (!pane) throw new Error('expected tokenizeForCache to return a pane, got null');
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 1) throw new Error('expected logPaneCache.size === 1 after tokenizeForCache, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
if (!sandbox.SandmanPortalDiff.hasLogPaneCached('r1')) throw new Error('expected logPaneCache to have subjectKey r1');
const pre = pane.querySelector ? pane.querySelector('pre[data-scroll-key]') : null;
if (!pre) throw new Error('expected pane to contain a pre[data-scroll-key]');
if (pre.getAttribute('data-rendered-log') !== 'line one\nline two\nline three') throw new Error('expected data-rendered-log to match input log, got ' + pre.getAttribute('data-rendered-log'));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_TokenizeNoOpWhenCached is the
// idempotence contract for tokenizeForCache: a second call with the same
// subject returns the same node (no rebuild) and does not start another
// fillTerminalPre cycle (no new data-rendering-log on the cached pane).
func TestPortalPerf_PrewarmLogPaneCache_TokenizeNoOpWhenCached(t *testing.T) {
	js := `const run = { key: 'a', kind: 'active', status: 'running', issueLabel: '#1', runId: 'r1', log: 'line one\nline two\nline three' };
const pane1 = sandbox.SandmanPortalDiff.tokenizeForCache(run, helpers);
const pre1 = pane1.querySelector('pre[data-scroll-key]');
const rendered1 = pre1.getAttribute('data-rendered-log');
const pane2 = sandbox.SandmanPortalDiff.tokenizeForCache(run, helpers);
if (pane2 !== pane1) throw new Error('expected second call to return same node, got new pane');
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 1) throw new Error('expected cache size to stay at 1, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
const pre2 = pane2.querySelector('pre[data-scroll-key]');
if (pre2 !== pre1) throw new Error('expected same pre node, got new pre');
if (pre2.getAttribute('data-rendering-log')) throw new Error('expected no new render cycle on the cached pre, found data-rendering-log=' + pre2.getAttribute('data-rendering-log'));
if (pre2.getAttribute('data-rendered-log') !== rendered1) throw new Error('expected data-rendered-log to be untouched, got ' + pre2.getAttribute('data-rendered-log'));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_TokenizeSkipsEmptyLog verifies that
// runs with empty or whitespace-only logs are not added to the cache.
func TestPortalPerf_PrewarmLogPaneCache_TokenizeSkipsEmptyLog(t *testing.T) {
	js := `const blankRun = { key: 'a', kind: 'active', status: 'running', issueLabel: '#1', runId: 'r1', log: '   \n\n  ' };
const pane = sandbox.SandmanPortalDiff.tokenizeForCache(blankRun, helpers);
if (pane) throw new Error('expected tokenizeForCache to return null for blank log, got pane');
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 0) throw new Error('expected cache size to stay at 0 for blank log, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
if (sandbox.SandmanPortalDiff.hasLogPaneCached('r1')) throw new Error('expected subjectKey r1 NOT to be cached for blank log');
const noLogRun = { key: 'b', kind: 'active', status: 'running', issueLabel: '#2', runId: 'r2' };
const pane2 = sandbox.SandmanPortalDiff.tokenizeForCache(noLogRun, helpers);
if (pane2) throw new Error('expected tokenizeForCache to return null for missing log, got pane');
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 0) throw new Error('expected cache size to stay at 0 for missing log, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_PicksTopNActiveFirst verifies the
// top-N selection (issue #1564 spec): active runs first, then by
// lastOutputAt desc, capped at topN. The fixture has 3 active and 2
// completed runs; with topN=3, the 3 active subject keys are cached
// and the completed ones are not.
func TestPortalPerf_PrewarmLogPaneCache_PicksTopNActiveFirst(t *testing.T) {
	js := `const runs = [
  { key: 'a1', runId: 'a1', kind: 'active', status: 'running', issueLabel: '#1', lastOutputAt: '2024-01-01T00:00:01Z', log: 'active one' },
  { key: 'a2', runId: 'a2', kind: 'active', status: 'running', issueLabel: '#2', lastOutputAt: '2024-01-01T00:00:03Z', log: 'active two' },
  { key: 'a3', runId: 'a3', kind: 'active', status: 'running', issueLabel: '#3', lastOutputAt: '2024-01-01T00:00:02Z', log: 'active three' },
  { key: 'c1', runId: 'c1', kind: 'completed', status: 'success', issueLabel: '#4', log: 'completed one' },
  { key: 'c2', runId: 'c2', kind: 'completed', status: 'success', issueLabel: '#5', log: 'completed two' },
];
const n = sandbox.SandmanPortalDiff.prewarmLogPaneCache(runs, helpers, { topN: 3 });
if (n !== 3) throw new Error('expected 3 newly cached, got ' + n);
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 3) throw new Error('expected cache size 3, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
for (const key of ['a1', 'a2', 'a3']) {
  if (!sandbox.SandmanPortalDiff.hasLogPaneCached(key)) throw new Error('expected active subject ' + key + ' to be cached');
}
for (const key of ['c1', 'c2']) {
  if (sandbox.SandmanPortalDiff.hasLogPaneCached(key)) throw new Error('expected completed subject ' + key + ' NOT to be cached');
}
console.log('PASS');
`
	runNodeScript(t, js)
}
