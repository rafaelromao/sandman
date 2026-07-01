package review

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/runid"
)

// reviewRunIDFor returns the canonical per-row RunID for a review
// run, per ADR-0030 §Per-row RunID templates:
//
//   - Review without a linked issue:  subject `PR<pr>`,
//     RowID `<shortid>-<ts>-PR<pr>`.
//
//   - Review with a linked issue:     subject `<linkedIssue>-PR<pr>`,
//     RowID `<shortid>-<ts>-<linkedIssue>-PR<pr>`.
//
// Review runs are ordinary AgentRuns (NOT a special review-only kind):
// the per-row RunID is a real ADR-0030 RunID, and the run folder under
// `.sandman/batches/<batch-id>/runs/<runID>/` is named after it. The
// `<shortid>-<ts>` prefix is owned by `runid.NewBatch` and threaded
// through unchanged; this helper builds only the per-row subject
// portion. The legacy literal `"review"` alias and the `runs/review/`
// folder name are explicitly NOT used by any current code path; they
// survive only as a negative check in `TestReviewRunIDFor_NoLiteralReview`.
//
// linkedIssue is the PR's linked/closing issue number, obtained via
// `(*github.PR).LinkedIssueNumber()`. Pass 0 when the PR does not
// link an issue.
func reviewRunIDFor(prNumber int, linkedIssue int, ts string, shortid string) string {
	subject := fmt.Sprintf("PR%d", prNumber)
	if linkedIssue > 0 {
		subject = fmt.Sprintf("%d-PR%d", linkedIssue, prNumber)
	}
	return runid.NewRunID(runid.KindReview, subject, ts, shortid)
}
