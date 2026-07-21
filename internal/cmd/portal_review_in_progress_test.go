package cmd

import "testing"

// B-tag vocabulary for issue #2109 (review canonical row) and issue #2340:
//   B3 — live review renders "N review(s) - In Progress" in JS counter
//   B4 — live signal wins over a stale terminal verdict in JS counter
//   B5 — no-live-no-verdict fallback renders "N review(s) - Unclear"
//   B7 — issue #2340 user-facing repro: a parent with two reviews
//        (one terminal with verdict="Changes requested", one live)
//        renders "N reviews - In Progress"

// TestRenderRunMeta_LiveReviewRendersInProgress pins issue #2109 (B3):
// when the Go server stamps reviewLive=true on a parent implementation row
// (because at least one sibling review child has Status == "reviewing"),
// renderRunMeta must render "N review(s) - In Progress" instead of the
// misleading "N review(s) - Unclear" fallback that today is only correct for
// terminal-but-unparseable reviews. The literal "In Progress" string is the
// new front-end contract that resolves the AC1 user-facing bug.
func TestRenderRunMeta_LiveReviewRendersInProgress(t *testing.T) {
	js := `const run = {
  key: 'impl-9', runId: 'impl-9', batchKey: 'parent-batch',
  kind: 'active', status: 'reviewing', reviewCount: 1, reviewVerdict: '', reviewLive: true,
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('1 review - In Progress') < 0) throw new Error('expected "1 review - In Progress" in meta when reviewLive=true and reviewVerdict empty, got ' + JSON.stringify(meta));
if (meta.indexOf('Unclear') >= 0) throw new Error('expected no "Unclear" fallback while review is in flight, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_TwoLiveReviewsRenderInProgress pins issue #2109
// (B3 plural): a parent with 2 in-flight reviews (reviewCount=2,
// reviewLive=true) must render "2 reviews - In Progress". Plural form
// regression guard so the "review" vs "reviews" join is preserved alongside
// the new fallback.
func TestRenderRunMeta_TwoLiveReviewsRenderInProgress(t *testing.T) {
	js := `const run = {
  key: 'impl-10', runId: 'impl-10', batchKey: 'parent-batch',
  kind: 'active', status: 'reviewing', reviewCount: 2, reviewVerdict: '', reviewLive: true,
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('2 reviews - In Progress') < 0) throw new Error('expected "2 reviews - In Progress" in meta when reviewLive=true and reviewCount=2, got ' + JSON.stringify(meta));
if (meta.indexOf('Unclear') >= 0) throw new Error('expected no "Unclear" fallback while reviews are in flight, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_LiveReviewWinsOverTerminalVerdict pins issue #2340 (B4,
// flipped): when the server carries both reviewLive=true (a sibling review
// child is still in flight) and a populated reviewVerdict (a previous review
// round already finished and wrote a parseable ## Decision marker), the live
// signal wins and "In Progress" renders — NOT the stale verdict. This is
// the issue #2340 contract: a live review child means the current round has
// not landed, so "In Progress" is the only honest answer regardless of what
// a prior review wrote into its decision.md. Repro: #2321 row showed
// "2 reviews - Changes Requested" while a new review was still running.
func TestRenderRunMeta_LiveReviewWinsOverTerminalVerdict(t *testing.T) {
	js := `const runApproved = {
  key: 'impl-11', runId: 'impl-11', batchKey: 'parent-batch',
  kind: 'active', status: 'reviewing', reviewCount: 1, reviewVerdict: 'Approved', reviewLive: true,
};
const metaApproved = helpers.renderRunMeta(runApproved);
if (metaApproved.indexOf('1 review - In Progress') < 0) throw new Error('expected "1 review - In Progress" when reviewLive=true and reviewVerdict="Approved" (issue #2340: live beats stale verdict), got ' + JSON.stringify(metaApproved));
if (metaApproved.indexOf('1 review - Approved') >= 0) throw new Error('expected no "Approved" when reviewLive=true (live signal must override the stale verdict), got ' + JSON.stringify(metaApproved));

const runChanges = {
  key: 'impl-12', runId: 'impl-12', batchKey: 'parent-batch',
  kind: 'active', status: 'reviewing', reviewCount: 1, reviewVerdict: 'Changes requested', reviewLive: true,
};
const metaChanges = helpers.renderRunMeta(runChanges);
if (metaChanges.indexOf('1 review - In Progress') < 0) throw new Error('expected "1 review - In Progress" when reviewLive=true and reviewVerdict="Changes requested" (issue #2340: live beats stale verdict), got ' + JSON.stringify(metaChanges));
if (metaChanges.indexOf('1 review - Changes Requested') >= 0) throw new Error('expected no "Changes Requested" when reviewLive=true (live signal must override the stale verdict), got ' + JSON.stringify(metaChanges));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_TwoReviewsOneLiveStaleVerdictRendersInProgress pins
// the exact user-facing repro from issue #2340 (B7): a parent with two
// sibling reviews where one has already finished with verdict="Changes
// requested" and one is still in flight. The portal must render
// "2 reviews - In Progress", not "2 reviews - Changes Requested". Mirrors
// the screenshot on #2321 row (Batch 260721104202-24a8-2318+11,
// run 260721104202-24a8-2321) where the operator saw the stale
// "Changes Requested" label.
func TestRenderRunMeta_TwoReviewsOneLiveStaleVerdictRendersInProgress(t *testing.T) {
	js := `const run = {
  key: 'impl-2340', runId: 'impl-2340', batchKey: 'parent-batch',
  kind: 'active', status: 'reviewing',
  reviewCount: 2, reviewVerdict: 'Changes requested', reviewLive: true,
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('2 reviews - In Progress') < 0) throw new Error('expected "2 reviews - In Progress" when one review is live and the prior verdict is stale (issue #2340 repro), got ' + JSON.stringify(meta));
if (meta.indexOf('2 reviews - Changes Requested') >= 0) throw new Error('expected no "Changes Requested" when reviewLive=true (issue #2340: live beats stale verdict), got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_NoLiveNoVerdictStillRendersUnclear pins issue #2109
// (B5) + issue #2220: the existing fallback "N review(s) - Unclear"
// still fires when reviewVerdict is empty, reviewLive is false, AND the
// impl run did not succeed — i.e. the terminal-but-unparseable review
// path on a failed/aborted run (AC4). This is a regression guard that
// pins the new ordering: terminal verdict > live signal > success
// short-circuit > Unclear. Note the status='failure' (not 'success'):
// issue #2220 upgraded the success branch to render "Approved".
func TestRenderRunMeta_NoLiveNoVerdictStillRendersUnclear(t *testing.T) {
	js := `const run = {
  key: 'impl-13', runId: 'impl-13', batchKey: 'parent-batch',
  kind: 'completed', status: 'failure', reviewCount: 1, reviewVerdict: '', reviewLive: false,
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('1 review - Unclear') < 0) throw new Error('expected "1 review - Unclear" when reviewLive=false and reviewVerdict empty on a failed run (AC4: terminal-but-unparseable review), got ' + JSON.stringify(meta));
if (meta.indexOf('In Progress') >= 0) throw new Error('expected no "In Progress" when reviewLive=false, got ' + JSON.stringify(meta));
if (meta.indexOf('1 review - Approved') >= 0) throw new Error('expected no "Approved" short-circuit on status="failure", got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}
