package cmd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortal_AwaitEventShowsInProgress verifies that when a run has
// run.await event (no run.finished), the portal shows it as "in progress".
func TestPortal_AwaitEventShowsInProgress(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "1-batch")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a socket so the batch is recognized as active (not dead).
	sockPath := daemon.BatchSocketPath(runDir)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addBatchToIndex(t, repoRoot, "1-batch", runDir, []int{42})

	startedAt := time.Now().Add(-5 * time.Minute)
	awaitAt := time.Now().Add(-2 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "1-42", Issue: 42, Payload: map[string]any{"branch": "42-fix-bug", "batch_id": "1-batch"}},
		{Type: "run.await", Timestamp: awaitAt, RunID: "1-42", Issue: 42, Payload: map[string]any{
			"await":         true,
			"await_reason":  "pending",
			"branch":        "42-fix-bug",
			"base_branch":   "main",
			"gate":          "pending",
			"blocker":       "external-gate",
			"retries_total": float64(0),
		}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	var got *portalRun
	for i := range runs {
		if runs[i].IssueNumber == 42 {
			got = &runs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("expected row for issue 42, got %d rows: %#v", len(runs), runs)
	}
	if got.Kind != "active" {
		t.Fatalf("expected kind 'active' for run with await event, got %q", got.Kind)
	}
	if got.Status != "running" {
		t.Fatalf("expected status 'running' for active run with await event, got %q", got.Status)
	}
	if got.FinishedAt != nil {
		t.Fatalf("expected nil FinishedAt for active run with await event, got %v", got.FinishedAt)
	}
	if got.IssueNumber != 42 {
		t.Fatalf("expected issue number 42, got %d", got.IssueNumber)
	}
	if got.Branch != "42-fix-bug" {
		t.Fatalf("expected branch '42-fix-bug', got %q", got.Branch)
	}
}
