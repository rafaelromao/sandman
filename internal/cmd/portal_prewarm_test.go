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
