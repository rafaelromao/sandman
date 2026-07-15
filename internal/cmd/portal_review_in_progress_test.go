package cmd

import "testing"

// TestRenderRunMeta_LiveReviewRendersInProgress pins issue #2109 slice 3 (B3):
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

// TestRenderRunMeta_TwoLiveReviewsRenderInProgress pins issue #2109 slice 3
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

// TestRenderRunMeta_TerminalVerdictWinsOverLive pins issue #2109 slice 4
// (B4): when the server carries both reviewLive=true and a terminal
// reviewVerdict (defensive case: the parent projection ran before all live
// reviews had settled), the verdict wins and "Approved" / "Changes
// Requested" renders — NOT "In Progress". This guards the existing
// test-pinned behaviour for AC2 and AC3 even when the live flag rides
// alongside.
func TestRenderRunMeta_TerminalVerdictWinsOverLive(t *testing.T) {
	js := `const runApproved = {
  key: 'impl-11', runId: 'impl-11', batchKey: 'parent-batch',
  kind: 'active', status: 'reviewing', reviewCount: 1, reviewVerdict: 'Approved', reviewLive: true,
};
const metaApproved = helpers.renderRunMeta(runApproved);
if (metaApproved.indexOf('1 review - Approved') < 0) throw new Error('expected "1 review - Approved" when reviewVerdict="Approved", got ' + JSON.stringify(metaApproved));
if (metaApproved.indexOf('In Progress') >= 0) throw new Error('expected no "In Progress" when reviewVerdict is set, got ' + JSON.stringify(metaApproved));

const runChanges = {
  key: 'impl-12', runId: 'impl-12', batchKey: 'parent-batch',
  kind: 'active', status: 'reviewing', reviewCount: 1, reviewVerdict: 'Changes requested', reviewLive: true,
};
const metaChanges = helpers.renderRunMeta(runChanges);
if (metaChanges.indexOf('1 review - Changes Requested') < 0) throw new Error('expected "1 review - Changes Requested" when reviewVerdict="Changes requested", got ' + JSON.stringify(metaChanges));
if (metaChanges.indexOf('In Progress') >= 0) throw new Error('expected no "In Progress" when reviewVerdict is set, got ' + JSON.stringify(metaChanges));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_NoLiveNoVerdictStillRendersUnclear pins issue #2109
// slice 5 (B5) + issue #2220: the existing fallback "N review(s) - Unclear"
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
