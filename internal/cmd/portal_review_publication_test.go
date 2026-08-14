package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestPortal_Compute_PendingReviewPublicationHidesLocalVerdict(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const (
		issueNumber   = 2472
		reviewBatchID = "review-pending-batch"
		reviewRunID   = "review-pending-run"
	)
	startedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	reviewBatchDir := filepath.Join(repoRoot, ".sandman", "batches", reviewBatchID)
	reviewRunDir := filepath.Join(reviewBatchDir, "runs", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir review run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reviewRunDir, "decision.md"), []byte("## Decision\n**APPROVED**\n"), 0644); err != nil {
		t.Fatalf("write decision: %v", err)
	}
	if err := batchindex.WriteReviewState(reviewRunDir, batchindex.ReviewState{
		PR: issueNumber,
		SeenComments: []batchindex.SeenComment{{
			CommentID: "review-trigger",
			Status:    "pending",
			Timestamp: finishedAt,
		}},
	}); err != nil {
		t.Fatalf("write review state: %v", err)
	}

	index := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{{
			ID:        reviewBatchID,
			Path:      reviewBatchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        issueNumber,
		}},
	}
	if err := index.Save(filepath.Join(repoRoot, ".sandman", "batches.json")); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	eventsPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, eventsPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(-10 * time.Minute), RunID: "impl-run", Issue: issueNumber, Payload: map[string]any{
			"batch_id": "impl-batch",
			"branch":   "2472-fix",
		}},
		{Type: "run.finished", Timestamp: startedAt.Add(-8 * time.Minute), RunID: "impl-run", Issue: issueNumber, Payload: map[string]any{
			"batch_id": "impl-batch",
			"status":   "success",
		}},
		{Type: "run.started", Timestamp: startedAt, RunID: reviewRunID, Issue: issueNumber, Payload: map[string]any{
			"batch_id":     reviewBatchID,
			"review":       true,
			"pr_number":    issueNumber,
			"issue_number": issueNumber,
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: reviewRunID, Issue: issueNumber, Payload: map[string]any{
			"batch_id": reviewBatchID,
			"review":   true,
			"status":   "success",
		}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: eventsPath})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var parent *portalRun
	for i := range runs {
		if runs[i].IssueNumber == issueNumber && !runs[i].Review {
			parent = &runs[i]
			break
		}
	}
	if parent == nil {
		t.Fatalf("expected implementation row for #%d, got %#v", issueNumber, runs)
	}
	if !parent.ReviewPendingPublication {
		t.Fatalf("ReviewPendingPublication=false, want true for pending review publication")
	}
	if parent.ReviewVerdict != "" {
		t.Fatalf("ReviewVerdict=%q, want empty while publication is pending", parent.ReviewVerdict)
	}
	if parent.Status != "success" {
		t.Fatalf("implementation Status=%q, want success; pending publication must not rewrite the AgentRun status", parent.Status)
	}
}
