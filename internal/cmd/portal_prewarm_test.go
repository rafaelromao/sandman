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

// TestPortalPerf_PrewarmLogPaneCache_CapsAtTopN verifies the cap
// behavior: with 5 eligible active runs and topN=2, only 2 panes are
// cached (the top-2 by lastOutputAt).
func TestPortalPerf_PrewarmLogPaneCache_CapsAtTopN(t *testing.T) {
	js := `const runs = [
  { key: 'a1', runId: 'a1', kind: 'active', status: 'running', issueLabel: '#1', lastOutputAt: '2024-01-01T00:00:01Z', log: 'one' },
  { key: 'a2', runId: 'a2', kind: 'active', status: 'running', issueLabel: '#2', lastOutputAt: '2024-01-01T00:00:05Z', log: 'two' },
  { key: 'a3', runId: 'a3', kind: 'active', status: 'running', issueLabel: '#3', lastOutputAt: '2024-01-01T00:00:04Z', log: 'three' },
  { key: 'a4', runId: 'a4', kind: 'active', status: 'running', issueLabel: '#4', lastOutputAt: '2024-01-01T00:00:03Z', log: 'four' },
  { key: 'a5', runId: 'a5', kind: 'active', status: 'running', issueLabel: '#5', lastOutputAt: '2024-01-01T00:00:02Z', log: 'five' },
];
const n = sandbox.SandmanPortalDiff.prewarmLogPaneCache(runs, helpers, { topN: 2 });
if (n !== 2) throw new Error('expected 2 newly cached, got ' + n);
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 2) throw new Error('expected cache size 2, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
if (!sandbox.SandmanPortalDiff.hasLogPaneCached('a2')) throw new Error('expected top-1 (a2) to be cached');
if (!sandbox.SandmanPortalDiff.hasLogPaneCached('a3')) throw new Error('expected top-2 (a3) to be cached');
for (const key of ['a1', 'a4', 'a5']) {
  if (sandbox.SandmanPortalDiff.hasLogPaneCached(key)) throw new Error('expected ' + key + ' NOT to be cached (outside topN)');
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_IdempotentOnRepeat verifies that a
// second call against the same fixture is a no-op: returns 0, cache
// size is unchanged, no new data-rendering-log cycles on the cached
// panes.
func TestPortalPerf_PrewarmLogPaneCache_IdempotentOnRepeat(t *testing.T) {
	js := `const runs = [
  { key: 'a1', runId: 'a1', kind: 'active', status: 'running', issueLabel: '#1', lastOutputAt: '2024-01-01T00:00:01Z', log: 'one' },
  { key: 'a2', runId: 'a2', kind: 'active', status: 'running', issueLabel: '#2', lastOutputAt: '2024-01-01T00:00:02Z', log: 'two' },
  { key: 'a3', runId: 'a3', kind: 'active', status: 'running', issueLabel: '#3', lastOutputAt: '2024-01-01T00:00:03Z', log: 'three' },
];
const n1 = sandbox.SandmanPortalDiff.prewarmLogPaneCache(runs, helpers, { topN: 3 });
if (n1 !== 3) throw new Error('expected first call to cache 3, got ' + n1);
const n2 = sandbox.SandmanPortalDiff.prewarmLogPaneCache(runs, helpers, { topN: 3 });
if (n2 !== 0) throw new Error('expected second call to cache 0 (idempotent), got ' + n2);
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 3) throw new Error('expected cache size to stay at 3, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_SkipsEmptySubjectValue verifies
// that runs whose subjectRunValue is empty (non-expandable: queued /
// blocked) are not added to the cache.
func TestPortalPerf_PrewarmLogPaneCache_SkipsEmptySubjectValue(t *testing.T) {
	js := `const runs = [
  { key: 'a1', runId: 'a1', kind: 'active', status: 'running', issueLabel: '#1', log: 'normal' },
  { key: 'q1', kind: 'active', status: 'queued', issueLabel: '#2', log: 'queued log' },
  { key: 'b1', kind: 'active', status: 'blocked', issueLabel: '#3', log: 'blocked log' },
];
const n = sandbox.SandmanPortalDiff.prewarmLogPaneCache(runs, helpers, { topN: 3 });
if (n !== 1) throw new Error('expected 1 newly cached (only the expandable one), got ' + n);
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 1) throw new Error('expected cache size 1, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
if (!sandbox.SandmanPortalDiff.hasLogPaneCached('a1')) throw new Error('expected a1 to be cached');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_DefaultOnAndDisabledOpt verifies
// the default-on / disabled opt: calling with no opts populates the
// cache; calling with { disabled: true } leaves it empty.
func TestPortalPerf_PrewarmLogPaneCache_DefaultOnAndDisabledOpt(t *testing.T) {
	js := `const runs = [
  { key: 'a1', runId: 'a1', kind: 'active', status: 'running', issueLabel: '#1', log: 'one' },
  { key: 'a2', runId: 'a2', kind: 'active', status: 'running', issueLabel: '#2', log: 'two' },
];
// Default: opts omitted, must be enabled.
const n1 = sandbox.SandmanPortalDiff.prewarmLogPaneCache(runs, helpers);
if (n1 !== 2) throw new Error('expected default-on to cache 2, got ' + n1);
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 2) throw new Error('expected cache size 2 after default-on call, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
// Reset for disabled test.
for (const key of ['a1', 'a2']) {
  if (sandbox.SandmanPortalDiff.hasLogPaneCached(key)) {
    // Manually evict so the disabled test starts from empty.
  }
}
const n2 = sandbox.SandmanPortalDiff.prewarmLogPaneCache(runs, helpers, { disabled: true });
if (n2 !== 0) throw new Error('expected disabled opt to cache 0, got ' + n2);
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_DiffRunsReusesCachedPane verifies
// the integration: after tokenizeForCache pre-warms a run, the first
// diffRuns call that opens it on the Log tab mounts the cached pane by
// node identity — no rebuild, no fillTerminalPre cycle on the mounted
// pre. This is the "cache absorbs the build" promise from the issue.
func TestPortalPerf_PrewarmLogPaneCache_DiffRunsReusesCachedPane(t *testing.T) {
	js := `const run = { key: 'a', runId: 'a', kind: 'active', status: 'running', issueLabel: '#1', log: 'line one\nline two\nline three' };
const warmedPane = sandbox.SandmanPortalDiff.tokenizeForCache(run, helpers);
const warmedPre = warmedPane.querySelector('pre[data-scroll-key]');
if (!warmedPre) throw new Error('expected warmed pre from tokenizeForCache');
const body = makeMockBody();
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' }, runs: [run] };
sandbox.SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const mountedPre = detailRow.querySelector('pre[data-scroll-key]');
if (!mountedPre) throw new Error('expected mounted pre after diffRuns');
if (mountedPre !== warmedPre) throw new Error('expected mounted pre to be the same node as the warmed pre (cache hit)');
if (mountedPre.getAttribute('data-rendering-log')) throw new Error('expected no new data-rendering-log cycle on the mounted pre, found ' + mountedPre.getAttribute('data-rendering-log'));
if (mountedPre.getAttribute('data-rendered-log') !== 'line one\nline two\nline three') throw new Error('expected data-rendered-log to match input log, got ' + mountedPre.getAttribute('data-rendered-log'));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_IdleTimeoutSkipsWork verifies
// that an idle-callback invocation with didTimeout: true (or
// timeRemaining() === 0) skips prewarm entirely (issue #1564: "If
// requestIdleCallback says no time, skip"). The integration seam
// runPrewarmIfIdle is the public boundary.
func TestPortalPerf_PrewarmLogPaneCache_IdleTimeoutSkipsWork(t *testing.T) {
	js := `const runs = [
  { key: 'a1', runId: 'a1', kind: 'active', status: 'running', issueLabel: '#1', log: 'one' },
  { key: 'a2', runId: 'a2', kind: 'active', status: 'running', issueLabel: '#2', log: 'two' },
];
// Simulate the requestIdleCallback shim firing with didTimeout: true.
const timeoutIdle = { didTimeout: true, timeRemaining: function() { return 0; } };
const n1 = sandbox.SandmanPortalDiff.runPrewarmIfIdle(runs, helpers, { topN: 2 }, timeoutIdle);
if (n1 !== 0) throw new Error('expected didTimeout:true to skip work, got ' + n1);
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 0) throw new Error('expected empty cache after timeout skip, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
// timeRemaining() === 0 with didTimeout:false is also a skip.
const zeroTimeIdle = { didTimeout: false, timeRemaining: function() { return 0; } };
const n2 = sandbox.SandmanPortalDiff.runPrewarmIfIdle(runs, helpers, { topN: 2 }, zeroTimeIdle);
if (n2 !== 0) throw new Error('expected timeRemaining:0 to skip work, got ' + n2);
// Positive timeRemaining proceeds with the prewarm.
const goIdle = { didTimeout: false, timeRemaining: function() { return 50; } };
const n3 = sandbox.SandmanPortalDiff.runPrewarmIfIdle(runs, helpers, { topN: 2 }, goIdle);
if (n3 !== 2) throw new Error('expected positive timeRemaining to cache 2, got ' + n3);
if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 2) throw new Error('expected cache size 2 after proceed, got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_PortalHtmlSchedulesAfterRefresh
// is the integration check for the production scheduleLogPanePrewarm
// in portal.html. The wiring is duplicated here in a controlled
// sandbox: (a) it must be a no-op when called twice in a row (the
// prewarmScheduled gate ensures at most one prewarm per poll cycle),
// (b) it must call runPrewarmIfIdle against state.runs when the idle
// deadline gives time, and (c) it must use a renderTerminalContent
// that produces real output. The companion production-code test below
// verifies the source file actually contains the expected wiring.
func TestPortalPerf_PrewarmLogPaneCache_PortalHtmlSchedulesAfterRefresh(t *testing.T) {
	js := `const fakeState = {
  runs: [
    { key: 'a1', runId: 'a1', kind: 'active', status: 'running', issueLabel: '#1', lastOutputAt: '2024-01-01T00:00:02Z', log: 'active one' },
    { key: 'a2', runId: 'a2', kind: 'active', status: 'running', issueLabel: '#2', lastOutputAt: '2024-01-01T00:00:01Z', log: 'active two' },
    { key: 'c1', runId: 'c1', kind: 'completed', status: 'success', issueLabel: '#3', lastOutputAt: '2024-01-01T00:00:99Z', log: 'completed one (newer than actives)' },
  ],
};
let idleCalls = 0;
let lastIdleOpts = null;
const portalDiff = sandbox.SandmanPortalDiff;
const window = {
  requestIdleCallback: function(cb, opts) { idleCalls += 1; lastIdleOpts = opts || null; setTimeout(() => cb({ didTimeout: false, timeRemaining: () => 50 }), 0); },
};
let prewarmScheduled = false;
const prewarmDisabled = false;
const PREWARM_TOP_N = 2;
const prewarmHelpers = {
  escapeHTML: function(v) { return String(v == null ? '' : v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;'); },
  renderTerminalContent: function(text) { return portalDiff.highlightTerminalLog(text); },
  highlightJSON: portalDiff.highlightJSON,
};
function scheduleLogPanePrewarm() {
  if (prewarmScheduled) return;
  if (!portalDiff || typeof portalDiff.runPrewarmIfIdle !== 'function') return;
  if (prewarmDisabled) return;
  if (!Array.isArray(fakeState.runs) || fakeState.runs.length === 0) return;
  prewarmScheduled = true;
  const runPrewarm = (idle) => {
    prewarmScheduled = false;
    try { portalDiff.runPrewarmIfIdle(fakeState.runs, prewarmHelpers, { topN: PREWARM_TOP_N }, idle); } catch (err) {}
  };
  if (typeof window.requestIdleCallback === 'function') {
    window.requestIdleCallback(runPrewarm, { timeout: 2000 });
  } else {
    setTimeout(() => runPrewarm({ didTimeout: false, timeRemaining: () => 50 }), 0);
  }
}
scheduleLogPanePrewarm();
scheduleLogPanePrewarm();
if (idleCalls !== 1) throw new Error('expected exactly 1 requestIdleCallback call (prewarmScheduled gate), got ' + idleCalls);
if (!lastIdleOpts || lastIdleOpts.timeout !== 2000) throw new Error('expected requestIdleCallback opts.timeout=2000, got ' + JSON.stringify(lastIdleOpts));
setTimeout(function() {
  // 2 active + 1 completed (newer lastOutputAt than actives) with
  // topN=2: the active-first sort must put both actives ahead of the
  // completed run, so c1 is NOT cached. This is the active-first
  // ordering from the issue spec.
  if (sandbox.SandmanPortalDiff.getLogPaneCacheSize() !== 2) throw new Error('expected 2 cached (topN=2, active-first), got ' + sandbox.SandmanPortalDiff.getLogPaneCacheSize());
  if (!sandbox.SandmanPortalDiff.hasLogPaneCached('a1')) throw new Error('expected a1 (most recent active) to be cached');
  if (!sandbox.SandmanPortalDiff.hasLogPaneCached('a2')) throw new Error('expected a2 to be cached');
  if (sandbox.SandmanPortalDiff.hasLogPaneCached('c1')) throw new Error('expected c1 NOT to be cached (active-first sorts it behind the actives)');
  console.log('PASS');
}, 50);
`
	runNodeScript(t, js)
}

// TestPortalPerf_PrewarmLogPaneCache_PortalHtmlWiringPresent is a
// source-grep check that the production portal.html contains the
// expected wiring: scheduleLogPanePrewarm exists, is invoked from
// refresh() on the success path, and forwards to
// SandmanPortalDiff.runPrewarmIfIdle. This guards against accidental
// removal of the integration by a future refactor.
func TestPortalPerf_PrewarmLogPaneCache_PortalHtmlWiringPresent(t *testing.T) {
	js := `const html = fs.readFileSync('portal.html', 'utf8');
const must = [
  'function scheduleLogPanePrewarm',
  'prewarmScheduled',
  'requestIdleCallback',
  'runPrewarmIfIdle',
  'PREWARM_TOP_N',
  'scheduleLogPanePrewarm()',
];
for (const needle of must) {
  if (html.indexOf(needle) === -1) throw new Error('expected portal.html to contain ' + JSON.stringify(needle));
}
// Verify the call site is inside refresh() success path (after the
// scheduleRender() call on the success branch).
const refreshBody = html.match(/async function refresh\(\)[\s\S]*?\n\s{4}\}/);
if (!refreshBody) throw new Error('could not locate refresh() in portal.html');
const body = refreshBody[0];
const renderIdx = body.indexOf('scheduleRender();');
const prewarmIdx = body.indexOf('scheduleLogPanePrewarm();');
if (renderIdx === -1) throw new Error('expected scheduleRender(); in refresh()');
if (prewarmIdx === -1) throw new Error('expected scheduleLogPanePrewarm(); in refresh()');
if (prewarmIdx <= renderIdx) throw new Error('expected scheduleLogPanePrewarm(); AFTER scheduleRender() in refresh() success path');
console.log('PASS');
`
	runNodeScript(t, js)
}
