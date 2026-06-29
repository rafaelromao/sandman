package cmd

import "testing"

// Regression tests for issue #1421: the detail panel must not synchronously
// rebuild the entire log on tab round-trips, and highlightTerminalLog must
// memoize large inputs. These run against the real portal_diff.js via the
// Node harness (see runNodeScript / sharedMockHelpers in portal_diff_test.go).

func TestPortalDiffLogPane_PreservedAcrossEventsRoundTrip(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line one\nline two\nline three', events: [{ type: 'start', timestamp: 1, payload: { ok: true } }] };
const stopGroups = new Set();
const optsLog = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
const optsEvents = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], optsLog);
const detailRow = body.children[1];
let content = detailRow.querySelector('.detail-content');
const originalPre = content.querySelector('pre[data-scroll-key]');
if (!originalPre) throw new Error('expected log pre after initial build');
if (originalPre.getAttribute('data-rendered-log') !== 'line one\nline two\nline three') throw new Error('expected rendered-log attr');
const originalFirstChild = originalPre.firstChild;
const originalChildCount = originalPre.children.length;

// Switch away to Events: the log pane is detached (cached), not discarded.
SandmanPortalDiff.diffRuns(body, [run], optsEvents);
content = detailRow.querySelector('.detail-content');
if (content.querySelector('pre[data-scroll-key]')) throw new Error('log pre must be detached while on events tab');
if (!content.querySelector('pre[data-rendered-json]')) throw new Error('expected events pane after switch');

// Return to Log: the SAME pane node (and its children) is re-attached — no
// re-tokenize / re-parse. Node identity, not mutation count, is the signal:
// a rebuild would create a brand-new <pre> with brand-new children.
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], optsLog);
content = detailRow.querySelector('.detail-content');
const restoredPre = content.querySelector('pre[data-scroll-key]');
if (!restoredPre) throw new Error('expected log pre after returning to log');
if (restoredPre !== originalPre) throw new Error('log pane must be reused (same node), not rebuilt');
if (restoredPre.firstChild !== originalFirstChild) throw new Error('log pane children must be reused, not re-parsed');
if (restoredPre.children.length !== originalChildCount) throw new Error('log pane child count must be stable');
if (restoredPre.getAttribute('data-rendered-log') !== 'line one\nline two\nline three') throw new Error('rendered-log attr must survive the round-trip');
if (restoredPre.textContent.indexOf('line two') === -1) throw new Error('log content must be intact after round-trip');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffLogPane_PreservedAcrossDetailsRoundTrip(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', log: 'completed log body', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log' };
const stopGroups = new Set();
const optsLog = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
const optsDetails = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run], optsLog);
const detailRow = body.children[1];
const originalPre = detailRow.querySelector('.detail-content').querySelector('pre[data-scroll-key]');
if (!originalPre) throw new Error('expected log pre after initial build');
const originalFirstChild = originalPre.firstChild;

SandmanPortalDiff.diffRuns(body, [run], optsDetails);
if (detailRow.querySelector('.detail-content').querySelector('pre[data-scroll-key]')) throw new Error('log pre must be detached while on details tab');

SandmanPortalDiff.diffRuns(body, [run], optsLog);
const restoredPre = detailRow.querySelector('.detail-content').querySelector('pre[data-scroll-key]');
if (restoredPre !== originalPre) throw new Error('log pane must be reused across details round-trip');
if (restoredPre.firstChild !== originalFirstChild) throw new Error('log pane children must be reused across details round-trip');
if (restoredPre.textContent.indexOf('completed log body') === -1) throw new Error('log content must be intact');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_MemoizesLargeInput(t *testing.T) {
	js := `function bigLog(seed, n) { var L = []; for (var i = 0; i < n; i++) L.push(seed + ' step ' + i + ' import return function foo()'); return L.join('\n'); }
const a = bigLog('alpha', 500);
const b = bigLog('beta', 500);
const ha1 = SandmanPortalDiff.highlightTerminalLog(a);
const ha2 = SandmanPortalDiff.highlightTerminalLog(a);
const hb = SandmanPortalDiff.highlightTerminalLog(b);
// Concurrent calls for the same input must return the same object (Promise
// or cached string); this is the de-duplication contract for the chunked
// renderer.
if (ha1 !== ha2) throw new Error('identical input must return identical output');
if (ha1 === hb) throw new Error('distinct inputs must not collide in the cache');
// Drain any pending Promises so we can compare HTML content.
const settle = (p) => (p && typeof p.then === 'function') ? p : Promise.resolve(p);
function checkContent(label, expected) {
  return Promise.all([settle(ha1), settle(ha2), settle(hb)]).then(function(arr) {
    if (arr[0].indexOf(expected) === -1) throw new Error('expected ' + expected + ' content in ' + label + ' (0)');
    if (arr[1].indexOf(expected) === -1) throw new Error('expected ' + expected + ' content in ' + label + ' (1)');
  });
}
checkContent('a', 'alpha').then(function() {
  if (typeof hb !== 'string' && typeof hb.then !== 'function') {
    throw new Error('hb has unexpected type: ' + typeof hb);
  }
  // After overflowing the cache with distinct large inputs, a re-query of the
  // original must still produce the correct output (eviction is safe).
  for (var k = 0; k < 20; k++) SandmanPortalDiff.highlightTerminalLog(bigLog('seed' + k, 500));
  const ha3 = SandmanPortalDiff.highlightTerminalLog(a);
  return settle(ha3).then(function(html) {
    if (html.indexOf('alpha') === -1) throw new Error('output must stay correct after cache eviction');
    console.log('PASS');
  });
}).catch(function(err) { throw err; });
`
	runNodeScript(t, js)
}
