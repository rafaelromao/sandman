package cmd

import (
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortalRunsView_RunFromState_ReviewWithoutIssueNumberUsesReviewOfPRLabel
// is the tracer bullet for issue #1667: the main label of an orphan
// review run (review=true, issueNumber=0) must render as
// "Review of PR <prNumber>" instead of leaking the raw runID into the
// UI. Mirrors the orphan-with-issue shape (issue #1526,
// ADR-0029 §Review-only orphan label) but with the PR number standing
// on its own.
func TestPortalRunsView_RunFromState_ReviewWithoutIssueNumberUsesReviewOfPRLabel(t *testing.T) {
	v := &portalRunsView{}
	runID := "260622193226-a0c19-PR1508"
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

	if run.IssueLabel != "Review of PR 1508" {
		t.Fatalf("IssueLabel=%q, want %q", run.IssueLabel, "Review of PR 1508")
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
	runID := "260622193226-a0c19-review"
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
