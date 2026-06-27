package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestMissingManifestIssues_ReturnsUnseenIssuesInManifestOrder(t *testing.T) {
	manifest := daemon.BatchManifest{Issues: []int{1, 2, 3}}
	seen := map[int]struct{}{1: {}}

	got := missingManifestIssues(manifest, seen)
	want := []int{2, 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missingManifestIssues() = %v, want %v", got, want)
	}
}

func TestMissingManifestIssues_ReturnsAllIssuesAndSkipsExcludedKinds(t *testing.T) {
	if got := missingManifestIssues(daemon.BatchManifest{Issues: []int{4, 5}}, nil); !reflect.DeepEqual(got, []int{4, 5}) {
		t.Fatalf("missingManifestIssues() with no seen issues = %v, want %v", got, []int{4, 5})
	}
	for _, runKind := range []string{"auto-select", "review"} {
		if got := missingManifestIssues(daemon.BatchManifest{Issues: []int{4, 5}, RunKind: runKind}, nil); len(got) != 0 {
			t.Fatalf("missingManifestIssues() for runKind %q = %v, want empty", runKind, got)
		}
	}
}

func TestPortal_DeadBatchSynthesizesNeverStartedMembers(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{1, 2, 3}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", batchDir, []int{1, 2, 3})
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d: %#v", len(runs), runs)
	}
	byIssue := map[int]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}
	if run := byIssue[1]; run.Kind != "completed" || run.Status != "success" {
		t.Fatalf("expected issue 1 to stay completed success, got %#v", run)
	}
	for _, issue := range []int{2, 3} {
		run := byIssue[issue]
		if run.Kind != "completed" || run.Status != "aborted" {
			t.Fatalf("expected synthesized issue %d to be completed aborted, got %#v", issue, run)
		}
	}
}

func TestPortal_LiveBatchKeepsNeverStartedMemberQueued(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "live-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42, 43}, CreatedAt: time.Now().Add(-10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))
	addBatchToIndex(t, repoRoot, "live-1", batchDir, []int{42, 43})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 live rows, got %d: %#v", len(runs), runs)
	}
	for _, run := range runs {
		if run.Kind != "active" || run.Status != "queued" {
			t.Fatalf("expected live never-started member to stay active queued, got %#v", run)
		}
	}
}

func TestPortal_MixedLiveDeadAndOrphanRowsStayDistinct(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-20 * time.Minute)
	liveDir := filepath.Join(repoRoot, ".sandman", "batches", "live-1")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(liveDir, daemon.BatchManifest{Issues: []int{7}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(liveDir, "batch.sock"))
	addBatchToIndex(t, repoRoot, "live-1", liveDir, []int{7})

	deadDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(deadDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(deadDir, daemon.BatchManifest{Issues: []int{1, 2}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", deadDir, []int{1, 2})

	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
		{Type: "run.started", Timestamp: startedAt.Add(3 * time.Minute), RunID: "orphan-99", Issue: 99, Payload: map[string]any{"branch": "sandman/99-fix"}},
		{Type: "run.aborted", Timestamp: startedAt.Add(4 * time.Minute), RunID: "orphan-99", Issue: 99, Payload: map[string]any{"status": "aborted", "branch": "sandman/99-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 4 {
		t.Fatalf("expected 4 rows, got %d: %#v", len(runs), runs)
	}
	byIssue := map[int]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}
	if run := byIssue[7]; run.Kind != "active" || run.Status != "queued" {
		t.Fatalf("expected live issue 7 to remain active queued, got %#v", run)
	}
	if run := byIssue[1]; run.Kind != "completed" || run.Status != "success" {
		t.Fatalf("expected issue 1 to stay completed success, got %#v", run)
	}
	if run := byIssue[2]; run.Kind != "completed" || run.Status != "aborted" {
		t.Fatalf("expected dead batch issue 2 to synthesize as completed aborted, got %#v", run)
	}
	if run := byIssue[99]; run.Kind != "completed" || run.Status != "aborted" {
		t.Fatalf("expected orphan issue 99 to remain completed aborted, got %#v", run)
	}
}
