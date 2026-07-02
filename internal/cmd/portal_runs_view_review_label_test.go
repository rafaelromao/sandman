package cmd

import (
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortalRunsView_RunFromState_ReviewWithoutIssueNumberUsesReviewOfPRLabel
// is the tracer bullet for issue #1667: the main label of an orphan
// review run (review=true, issueNumber=0) must render as
// "Review of #<prNumber>" instead of leaking the raw runID into the UI.
//
// The previous behaviour of `RunState.IssueLabel()` for review runs was
// to return the runID itself. The portal projection previously preserved
// that as the cell name. For orphan review rows that already recovered
// their issue number (visibleRunForIssueGroup, ADR-0029 §Review-only
// orphan label), the cell shows "Review of #N". This test asserts the
// same convention — with N taken from pr_number — for orphan rows that
// have no issue number.
func TestPortalRunsView_RunFromState_ReviewWithoutIssueNumberUsesReviewOfPRLabel(t *testing.T) {
	v := &portalRunsView{}
	runID := "a0c19-260622193226-PR1508"
	state := events.RunState{
		RunID: runID,
		Started: events.Event{
			Type:      "run.started",
			RunID:     runID,
			Timestamp: time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC),
			Payload: map[string]any{
				"review":     true,
				"pr_number":  float64(1508),
				"batch_id":   runID,
				"branch":     "review/pr-1508",
				"agent_path": "agents/review.md",
			},
		},
	}

	run := v.runFromState("/tmp", state, nil, nil, nil)

	if run.IssueLabel != "Review of #1508" {
		t.Fatalf("IssueLabel=%q, want %q", run.IssueLabel, "Review of #1508")
	}
	if run.IssueNumber != 0 {
		t.Fatalf("IssueNumber=%d, want 0 (orphan review without resolved issue)", run.IssueNumber)
	}
	if run.PRNumber != 1508 {
		t.Fatalf("PRNumber=%d, want 1508", run.PRNumber)
	}
	if run.RunID != runID {
		t.Fatalf("RunID=%q, want %q (unchanged)", run.RunID, runID)
	}
}

// TestPortalRunsView_RunFromState_ReviewWithoutIssueOrPRFallsBackToRunID
// pins the exotic fallback: a review run that has neither an issue
// number nor a PR number keeps the raw RunID as the main label. We
// don't fabricate PR=<none>.
func TestPortalRunsView_RunFromState_ReviewWithoutIssueOrPRFallsBackToRunID(t *testing.T) {
	v := &portalRunsView{}
	runID := "a0c19-260622193226-review"
	state := events.RunState{
		RunID: runID,
		Started: events.Event{
			Type:      "run.started",
			RunID:     runID,
			Timestamp: time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC),
			Payload: map[string]any{
				"review":     true,
				"batch_id":   runID,
				"agent_path": "agents/review.md",
			},
		},
	}

	run := v.runFromState("/tmp", state, nil, nil, nil)

	if run.IssueLabel != runID {
		t.Fatalf("IssueLabel=%q, want %q (raw runID fallback when no PR and no issue)", run.IssueLabel, runID)
	}
}
