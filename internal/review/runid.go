package review

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/runid"
)

// reviewRunIDFor returns the canonical per-row RunID for a review
// run, per ADR-0030 §Per-row RunID templates:
//
//   - Review without a linked issue:  subject `PR<pr>`,
//     RowID `<sid>-<ts>-PR<pr>`.
//
//   - Review with a linked issue:     subject `<linkedIssue>-PR<pr>`,
//     RowID `<sid>-<ts>-<linkedIssue>-PR<pr>`.
//
// The `<sid>-<ts>` prefix is owned by `runid.NewBatch` and threaded
// through unchanged; this helper builds only the per-row subject
// portion. Issue #1551 replaces the legacy literal `"review"` alias
// that older review launches used as both the row RunID and the run
// folder name — every review run now lands under a folder whose name
// is exactly its RowID.
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
