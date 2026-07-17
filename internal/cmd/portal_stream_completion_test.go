package cmd

import "testing"

// TestPortalProto_StreamingRunCompletes_PaneUpdatesWithFinalLog pins the
// downstream half of the fix for issue #2262: when a streaming subject
// transitions active->completed and the final saved log is delivered to the
// diff module (via the refresh() forceFetch), the pane must update to show the
// final content, not stay frozen at the streamed snapshot.
//
// The upstream half (detecting the transition in refresh() and calling
// loadRunDetail with forceFetch: true) lives in portal.html and is not
// unit-exercisable here; this test pins the contract that the diff module
// honors the transition when it receives the final log.
//
// For the impl-as-subject, parent.key === subject.key so the streaming shield
// (portal_diff.js:1431) and the snap-back guard (portal_diff.js:1493) both
// behave as before; the test exercises the same code path with a grouped
// review subject to match the user's reproduction.
func TestPortalStream_PaneUpdatesAfterStreamingRunCompletes(t *testing.T) {
	js := `const body = makeMockBody();
const parent = {
  key: 'impl-1', runId: 'impl-1', kind: 'active', status: 'running',
  issueLabel: '#1', issueNumber: 1, socketPath: '/tmp/impl.sock', log: 'impl line',
};
const review = {
  key: 'PR42', runId: 'PR42', kind: 'active', status: 'reviewing',
  issueLabel: 'PR42', issueNumber: 1, prNumber: 42, review: true,
  socketPath: '/tmp/review.sock', log: 'streamed line 1\nstreamed line 2',
};
const streamingKeys = new Set(['PR42']);
const opts = {
  helpers, stopGroups: new Set(), streamingKeys,
  runs: [parent, review], visibleRuns: [parent],
  expandedKey: 'PR42', tabs: { PR42: 'log' },
};
SandmanPortalDiff.diffRuns(body, [parent], opts);
const parentDetail = body.querySelector('tr.detail-row[data-detail-for="impl-1"]');
const reviewPre = parentDetail.querySelector('pre[data-scroll-key="PR42"]');
const before = reviewPre.getAttribute('data-rendered-log') || '';
if (before !== 'streamed line 1\nstreamed line 2') throw new Error('initial pane wrong, got: ' + JSON.stringify(before));

// Simulate what refresh() delivers after the fix: the review is now completed
// and the detail endpoint (forceFetch) returned the final saved log. The new
// log is a superset of the streamed content (the daemon writes the same lines
// to the broadcaster and the file, in order).
const finalLog = 'streamed line 1\nstreamed line 2\nfinal line written after SSE closed';
const reviewCompleted = Object.assign({}, review, { kind: 'completed', status: 'changes_requested', log: finalLog });
const parentUnchanged = Object.assign({}, parent);
// The streaming key must be released (the run is no longer active) so the
// poll can reconcile the pane.
streamingKeys.delete('PR42');
SandmanPortalDiff.diffRuns(body, [parentUnchanged], Object.assign({}, opts, {
  streamingKeys, runs: [parentUnchanged, reviewCompleted],
}));
const reviewPreAfter = parentDetail.querySelector('pre[data-scroll-key="PR42"]');
const after = reviewPreAfter ? (reviewPreAfter.getAttribute('data-rendered-log') || '') : '<pane removed>';
if (after === before) throw new Error('FIX REGRESSION: pane stayed frozen at streamed content, final log did not replace it');
if (!/final line written after SSE closed/.test(after)) {
  throw new Error('expected final log to be visible in pane, got: ' + JSON.stringify(after));
}
console.log('PASS');
`
	runNodeScript(t, js)
}
