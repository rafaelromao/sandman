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

func TestPortalRunsIndex_ReadEvents_AppendsJSONLTail(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	firstTS := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	writePortalLog(t, logPath, []events.Event{{Type: "run.started", Timestamp: firstTS, RunID: "abcd-260618113825-issue-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}}})

	idx := getPortalRunsIndex(repoRoot)
	first, err := idx.readEvents()
	if err != nil {
		t.Fatalf("readEvents first: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("expected 1 event, got %d", len(first))
	}
	info1, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.eventsOffset != info1.Size() {
		t.Fatalf("eventsOffset=%d, want first log size %d", idx.eventsOffset, info1.Size())
	}

	secondTS := firstTS.Add(2 * time.Minute)
	writePortalLog(t, logPath, []events.Event{{Type: "run.finished", Timestamp: secondTS, RunID: "abcd-260618113825-issue-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}}})
	second, err := idx.readEvents()
	if err != nil {
		t.Fatalf("readEvents second: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("expected 2 events after append, got %d", len(second))
	}
	info2, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.eventsOffset != info2.Size() {
		t.Fatalf("eventsOffset=%d, want appended log size %d", idx.eventsOffset, info2.Size())
	}
	if !second[1].Timestamp.Equal(secondTS) {
		t.Fatalf("second event timestamp=%v, want %v", second[1].Timestamp, secondTS)
	}
}

func TestPortalRunForKey_UsesSharedRunsIndex(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	idx := getPortalRunsIndex(repoRoot)
	idx.mu.Lock()
	idx.snapshotAt = time.Now()
	idx.snapshotCache = []portalRun{{Key: "abcd-260618113825-issue-1", RunID: "abcd-260618113825-issue-1", Kind: "completed", Status: "success", IssueLabel: "#1"}}
	idx.mu.Unlock()

	run, err := portalRunForKey(repoRoot, "abcd-260618113825-issue-1")
	if err != nil {
		t.Fatalf("portalRunForKey: %v", err)
	}
	if run.Key != "abcd-260618113825-issue-1" || run.RunID != "abcd-260618113825-issue-1" {
		t.Fatalf("unexpected run from shared index: %#v", run)
	}
}

func TestPortalHandler_RunsServesSharedRunsIndexSnapshot(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	idx := getPortalRunsIndex(repoRoot)
	idx.mu.Lock()
	idx.snapshotAt = time.Now()
	idx.snapshotCache = []portalRun{{Key: "abcd-260618113825-issue-99", RunID: "abcd-260618113825-issue-99", Kind: "completed", Status: "success", IssueLabel: "#99"}}
	idx.mu.Unlock()

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run from shared index snapshot, got %#v", runs)
	}
	if runs[0].Key != "abcd-260618113825-issue-99" {
		t.Fatalf("unexpected run key %q from /api/runs shared index snapshot", runs[0].Key)
	}
}

func TestPortalRunsIndex_DiscoverActiveRuns_RefreshesManifestCacheOnChange(t *testing.T) {
	repoRoot, err := os.MkdirTemp("", "portal-index-manifest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "abcd-260618113825-issue-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(runDir, "batch.sock"))
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{860}, CreatedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	idx := getPortalRunsIndex(repoRoot)
	first, err := idx.discoverActiveRuns(nil)
	if err != nil {
		t.Fatalf("discoverActiveRuns first: %v", err)
	}
	if len(first) != 1 || !reflect.DeepEqual(first[0].IssueNumbers, []int{860}) {
		t.Fatalf("expected first manifest issues [860], got %#v", first)
	}

	// Ensure the manifest modtime changes so the cache re-reads it.
	time.Sleep(20 * time.Millisecond)
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{854}, CreatedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	second, err := idx.discoverActiveRuns(nil)
	if err != nil {
		t.Fatalf("discoverActiveRuns second: %v", err)
	}
	if len(second) != 1 || !reflect.DeepEqual(second[0].IssueNumbers, []int{854}) {
		t.Fatalf("expected refreshed manifest issues [854], got %#v", second)
	}
}
