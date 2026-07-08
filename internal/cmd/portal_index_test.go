package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestPortalRunsIndex_InitializesWithEventsLogPath(t *testing.T) {
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)
	expectedPath := layout.EventsLogPath

	idx := getPortalRunsIndex(repoRoot)

	if idx.eventLogPath != expectedPath {
		t.Fatalf("expected eventLogPath %q, got %q", expectedPath, idx.eventLogPath)
	}

	if idx.repoRoot != repoRoot {
		t.Fatalf("expected repoRoot %q, got %q", repoRoot, idx.repoRoot)
	}
}

func TestPortalRunsIndex_ReadEvents_AppendsJSONLTail(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	firstTS := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	writePortalLog(t, logPath, []events.Event{{Type: "run.started", Timestamp: firstTS, RunID: "260618113825-abcd-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}}})

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
	writePortalLog(t, logPath, []events.Event{{Type: "run.finished", Timestamp: secondTS, RunID: "260618113825-abcd-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}}})
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
	idx.snapshotCache = []portalRun{{Key: "260618113825-abcd-1", RunID: "260618113825-abcd-1", Kind: "completed", Status: "success", IssueLabel: "#1"}}
	idx.mu.Unlock()

	run, err := portalRunForKey(repoRoot, "260618113825-abcd-1")
	if err != nil {
		t.Fatalf("portalRunForKey: %v", err)
	}
	if run.Key != "260618113825-abcd-1" || run.RunID != "260618113825-abcd-1" {
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
	idx.snapshotCache = []portalRun{{Key: "260618113825-abcd-99", RunID: "260618113825-abcd-99", Kind: "completed", Status: "success", IssueLabel: "#99"}}
	idx.mu.Unlock()

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run from shared index snapshot, got %#v", runs)
	}
	if runs[0].Key != "260618113825-abcd-99" {
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
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "260618113825-abcd-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(runDir, "batch.sock"))
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{860}, CreatedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	// Create index for the batch
	layout := paths.NewLayout(nil, repoRoot)
	batchIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{
				ID:        "260618113825-abcd-1",
				Path:      runDir,
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now().Add(-time.Minute),
				Issues:    []int{860},
			},
		},
	}
	if err := batchIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	view := (&portalRunsIndex{}).view
	if view == nil {
		view = &portalRunsView{}
	}
	first, err := view.discoverActiveRuns(repoRoot, nil)
	if err != nil {
		t.Fatalf("discoverActiveRuns first: %v", err)
	}
	if len(first) != 1 || !reflect.DeepEqual(first[0].IssueNumbers, []int{860}) {
		t.Fatalf("expected first manifest issues [860], got %#v", first)
	}

	beforeModTime := manifestModTime(t, runDir)
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{854}, CreatedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	waitForManifestModTimeAfter(t, runDir, beforeModTime, time.Second)
	second, err := view.discoverActiveRuns(repoRoot, nil)
	if err != nil {
		t.Fatalf("discoverActiveRuns second: %v", err)
	}
	if len(second) != 1 || !reflect.DeepEqual(second[0].IssueNumbers, []int{854}) {
		t.Fatalf("expected refreshed manifest issues [854], got %#v", second)
	}
}

// manifestModTime returns the on-disk modtime of the batch manifest
// at runDir. Used by tests that need to detect a re-write without
// resorting to a fixed time.Sleep between writes.
func manifestModTime(t *testing.T, runDir string) time.Time {
	t.Helper()
	info, err := os.Stat(daemon.ManifestPath(runDir))
	if err != nil {
		t.Fatalf("stat manifest at %s: %v", runDir, err)
	}
	return info.ModTime()
}

// waitForManifestModTimeAfter polls the batch manifest at runDir
// until its modtime is strictly after before (a deadline-bounded
// replacement for a fixed time.Sleep between manifest writes).
// Mirrors the poll-with-deadline shape used elsewhere in the test
// suite (waitForPathTB / waitForSocketTB).
func waitForManifestModTimeAfter(t *testing.T, runDir string, before time.Time, timeout time.Duration) {
	t.Helper()
	path := daemon.ManifestPath(runDir)
	deadline := time.Now().Add(timeout)
	for {
		info, err := os.Stat(path)
		if err == nil && info.ModTime().After(before) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s modtime to advance past %v", path, before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
