package cmd

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/paths"
)

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

func TestRenderRunMeta_PendingPublicationRendersInProgress(t *testing.T) {
	js := `const run = {
  key: 'impl-2472', runId: 'impl-2472', batchKey: 'parent-batch',
  kind: 'completed', status: 'success', reviewCount: 1,
  reviewVerdict: 'Approved', reviewPendingPublication: true, reviewLive: false,
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('1 review - In Progress') < 0) throw new Error('expected pending publication to render "1 review - In Progress", got ' + JSON.stringify(meta));
if (meta.indexOf('Approved') >= 0) throw new Error('pending publication must suppress the local Approved verdict, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// orphanReviewGroupMeta projects an orphan linked review group (two terminal
// reviews of issue 2472, no implementation parent) through the Go issue-group
// owner, ships it as the summary payload and returns the rendered meta line of
// the table row. Each review is described as (runID, startedAt, finishedAt,
// publication status, decision).
type orphanReviewFixture struct {
	runID       string
	started     time.Time
	finished    time.Time
	publication string
	decision    string
}

func orphanReviewGroupMeta(t *testing.T, want, forbidden string, reviews ...orphanReviewFixture) {
	t.Helper()
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)
	runs := make([]portalRun, 0, len(reviews))
	for _, review := range reviews {
		runDir := filepath.Join(repoRoot, ".sandman", "batches", "b-"+review.runID, "runs", review.runID)
		writeReviewPublicationFixture(t, runDir, review.publication, review.finished, review.decision)
		finished := review.finished
		runs = append(runs, portalRun{
			Key: review.runID, RunID: review.runID, IssueNumber: 2472, PRNumber: 42, Review: true,
			Kind: "completed", Status: "success", StartedAt: review.started, FinishedAt: &finished,
			BatchKey: "b-" + review.runID, RunDir: runDir,
		})
	}
	projected := (&portalRunsView{}).projectIssueGroups(layout, runs)
	runsJSON, err := json.Marshal(portalSummaryRuns(projected))
	if err != nil {
		t.Fatal(err)
	}
	js := `const visible = visibleRunsForTable(JSON.parse('` + string(runsJSON) + `'));
if (visible.length !== 1) throw new Error('expected one orphan review row, got ' + JSON.stringify(visible));
const meta = helpers.renderRunMeta(visible[0]);
if (meta.indexOf('` + want + `') < 0) throw new Error('expected ` + want + `, got ' + JSON.stringify(meta));
if ('` + forbidden + `' && meta.indexOf('` + forbidden + `') >= 0) throw new Error('unexpected ` + forbidden + `, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestVisibleRunsForIssueGroup_PendingPublicationOverridesOlderVerdict(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	orphanReviewGroupMeta(t, "2 reviews - In Progress", "Approved",
		orphanReviewFixture{"review-old", base, base.Add(2 * time.Minute), "success", "## Decision\n**APPROVED**\n"},
		orphanReviewFixture{"review-new", base.Add(5 * time.Minute), base.Add(7 * time.Minute), "pending", ""},
	)
}

func TestVisibleRunsForIssueGroup_UnreadableNewestReviewDoesNotResurrectOlderVerdict(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	orphanReviewGroupMeta(t, "2 reviews - Unclear", "Approved",
		orphanReviewFixture{"review-old", base, base.Add(2 * time.Minute), "success", "## Decision\n**APPROVED**\n"},
		orphanReviewFixture{"review-new", base.Add(5 * time.Minute), base.Add(7 * time.Minute), "success", ""},
	)
}

func TestVisibleRunsForIssueGroup_EqualTimestampUsesDescendingRunID(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	orphanReviewGroupMeta(t, "2 reviews - In Progress", "Approved",
		orphanReviewFixture{"review-a", base, base.Add(2 * time.Minute), "success", "## Decision\n**APPROVED**\n"},
		orphanReviewFixture{"review-z", base, base.Add(2 * time.Minute), "pending", ""},
	)
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
