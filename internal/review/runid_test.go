package review

import (
	"strings"
	"testing"
)

// TestReviewRunIDFor_PRBare pins the canonical row RunID shape from
// ADR-0030 §Per-row RunID templates for a review that does NOT link
// an issue. The subject is `PR<pr>` and the row RunID is
// `<sid>-<ts>-PR<pr>`. This is the headline shape that the review
// daemon's prepareReviewRun must mint and persist in `run.json`.
//
// Issue #1551: review daemon must emit a canonical per-row RunID —
// not the legacy literal `RunID: "review"` alias — so the run
// folder, run.json, run.log, run.sock, and review-state.json all
// live under a folder whose name matches the RowID.
func TestReviewRunIDFor_PRBare(t *testing.T) {
	got := reviewRunIDFor(42, 0, "260625120000", "abcd")
	want := "260625120000-abcd-PR42"
	if got != want {
		t.Errorf("reviewRunIDFor(42, 0, ...) = %q, want %q", got, want)
	}
}

// TestReviewRunIDFor_PRWithLinkedIssue pins the with-linked-issue
// shape from ADR-0030 §Per-row RunID templates: the subject becomes
// `<linkedIssue>-PR<pr>` so the per-row RunID is
// `<sid>-<ts>-<linkedIssue>-PR<pr>`. This is the new shape the
// review daemon must mint when the PR body or its native
// closingIssuesReferences carries a linked issue number.
func TestReviewRunIDFor_PRWithLinkedIssue(t *testing.T) {
	got := reviewRunIDFor(42, 1551, "260625120000", "abcd")
	want := "260625120000-abcd-1551-PR42"
	if got != want {
		t.Errorf("reviewRunIDFor(42, 1551, ...) = %q, want %q", got, want)
	}
}

// TestReviewRunIDFor_NoLiteralReview guarantees the canonical row
// RunID is never the literal "review" alias that older review runs
// used as a folder name and as the `RunID` field on `run.json`.
// Acceptance criterion: "No code path writes `RunID: \"review\"` into
// `run.json`". The canonical mint must always include the
// `<sid>-<ts>` prefix and the `PR<pr>` suffix.
func TestReviewRunIDFor_NoLiteralReview(t *testing.T) {
	got := reviewRunIDFor(1, 0, "260625120000", "0001")
	if got == "review" {
		t.Fatalf("reviewRunIDFor must never return the literal %q, got %q", "review", got)
	}
	if !strings.HasPrefix(got, "260625120000-0001-") {
		t.Errorf("reviewRunIDFor must include <sid>-<ts>- prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "-PR1") {
		t.Errorf("reviewRunIDFor must end with -PR<pr>, got %q", got)
	}
}
