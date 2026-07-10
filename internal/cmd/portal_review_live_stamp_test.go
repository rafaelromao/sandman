package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/paths"
)

// TestAggregateReviewChildren_LiveReviewStampsReviewLive pins issue #2109 slice
// 1 (B1): when a parent implementation row has at least one sibling review
// child with Status == "reviewing" (an in-flight review), the canonical parent
// row must carry the new ReviewLive=true server stamp so the JS counter line
// can render "N review(s) - In Progress" instead of the misleading
// "N review(s) - Unclear" fallback. The badge-flip path
// (summary.live && !isTerminalStatus(runs[idx].Status)) is unchanged: a
// non-terminal parent still flips to "reviewing" as before; a terminal parent
// is preserved. The new field is purely additive.
func TestAggregateReviewChildren_LiveReviewStampsReviewLive(t *testing.T) {
	repoRoot := t.TempDir()
	reviewStarted := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	implStarted := reviewStarted.Add(-1 * time.Minute)
	layout := paths.NewLayout(nil, repoRoot)

	liveReview := portalRun{
		IssueNumber: 2109, Review: true, RunID: "rev-live", Key: "rev-live",
		Kind: "active", Status: "reviewing",
		StartedAt: reviewStarted,
	}
	parent := portalRun{
		IssueNumber: 2109, RunID: "impl-2109", Key: "impl-2109",
		Kind: "active", Status: "running",
		StartedAt: implStarted,
	}

	runs := (&portalRunsView{}).aggregateReviewChildren(layout, []portalRun{parent, liveReview})

	var stamped *portalRun
	for i := range runs {
		if runs[i].RunID == "impl-2109" {
			stamped = &runs[i]
			break
		}
	}
	if stamped == nil {
		t.Fatalf("parent implementation row missing from aggregate output: %#v", runs)
	}
	if !stamped.ReviewLive {
		t.Fatalf("parent ReviewLive=false, want true when a sibling review has Status=\"reviewing\" (issue #2109 slice 1)")
	}
	if stamped.ReviewCount != 1 {
		t.Fatalf("parent ReviewCount=%d, want 1 (review child still aggregates)", stamped.ReviewCount)
	}
	if stamped.Status != "reviewing" {
		t.Fatalf("parent Status=%q, want %q (non-terminal parent still flips to \"reviewing\" when a live review child exists; existing badge-flip invariant preserved)", stamped.Status, "reviewing")
	}
}

// TestAggregateReviewChildren_TerminalReviewDoesNotStampReviewLive pins issue
// #2109 slice 2 (B2): when every review child has a terminal status, the
// canonical parent row must NOT carry ReviewLive=true. The field stays at the
// zero value so the omitempty JSON tag drops it from the wire entirely.
func TestAggregateReviewChildren_TerminalReviewDoesNotStampReviewLive(t *testing.T) {
	repoRoot := t.TempDir()
	reviewBatchID := "rev-term-2109"
	reviewRunID := "rev-term"
	reviewStarted := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	reviewFinished := reviewStarted.Add(2 * time.Minute)

	reviewRunDir := filepath.Join(repoRoot, ".sandman", "batches", reviewBatchID, "runs", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir review run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reviewRunDir, "decision.md"), []byte("## Decision\n**APPROVED**\n"), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)

	terminalReview := portalRun{
		IssueNumber: 2109, Review: true, RunID: reviewRunID, Key: reviewRunID,
		BatchKey: reviewBatchID, RunDir: reviewRunDir,
		Kind: "completed", Status: "success",
		StartedAt: reviewStarted, FinishedAt: &reviewFinished,
	}
	parent := portalRun{
		IssueNumber: 2109, RunID: "impl-2109", Key: "impl-2109",
		Kind: "completed", Status: "success",
		StartedAt: reviewStarted.Add(-1 * time.Minute), FinishedAt: &reviewFinished,
	}

	runs := (&portalRunsView{}).aggregateReviewChildren(layout, []portalRun{parent, terminalReview})

	var stamped *portalRun
	for i := range runs {
		if runs[i].RunID == "impl-2109" {
			stamped = &runs[i]
			break
		}
	}
	if stamped == nil {
		t.Fatalf("parent implementation row missing from aggregate output: %#v", runs)
	}
	if stamped.ReviewLive {
		t.Fatalf("parent ReviewLive=true, want false (no sibling review has Status=\"reviewing\"; the ReviewLive field must stay at zero when no review is in flight)")
	}
	if stamped.ReviewVerdict != "Approved" {
		t.Fatalf("parent ReviewVerdict=%q, want %q (terminal review verdict must still stamp from decision.md)", stamped.ReviewVerdict, "Approved")
	}
}
