package cmd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/testenv"
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
	writePortalLog(t, logPath, []events.Event{{Type: "run.started", Timestamp: firstTS, RunID: "260618113825-abcd-42", Issue: 42, Payload: map[string]any{"branch": "42-fix"}}})

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
	writePortalLog(t, logPath, []events.Event{{Type: "run.finished", Timestamp: secondTS, RunID: "260618113825-abcd-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "42-fix"}}})
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

func TestPortalRunsIndex_SnapshotRecomputesAfterAwaitedRunFinishes(t *testing.T) {
	repoRoot := testenv.MkdirShort(t, "portal-await-finish-")
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	layout := paths.NewLayout(nil, repoRoot)
	batchID := "260912164222-c538-466+6"
	runID := "260912164222-c538-466"
	batchDir := filepath.Join(layout.BatchesDir, batchID)
	runDir := filepath.Join(batchDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))
	startedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.FixedZone("-03", -3*60*60))
	awaitAt := startedAt.Add(5 * time.Minute)
	continuedAt := awaitAt.Add(time.Hour)
	secondContinuedAt := continuedAt.Add(time.Hour)
	finishedAt := secondContinuedAt.Add(7 * time.Minute)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		Issues:    []int{466},
		CreatedAt: startedAt,
		BatchId:   batchID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteRunManifest(batchDir, runID, batchindex.RunManifest{
		RunID:      runID,
		BatchID:    batchID,
		Issue:      466,
		Branch:     "466-fix",
		BaseBranch: "main",
		CreatedAt:  startedAt,
	}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{466})
	eventLog := &events.JSONLLogger{Path: layout.EventsLogPath}
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 466, Payload: map[string]any{
			"branch": "466-fix", "batch_id": batchID,
		}},
		{Type: "run.await", Timestamp: awaitAt, RunID: runID, Issue: 466, Payload: map[string]any{
			"await_reason": "pending", "branch": "466-fix", "batch_id": batchID,
		}},
	})

	idx := getPortalRunsIndex(repoRoot)
	initial, err := idx.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("initial snapshot: %v", err)
	}
	if len(initial) != 1 || initial[0].Status != "waiting" || initial[0].FinishedAt != nil {
		t.Fatalf("initial snapshot = %#v, want one waiting active row", initial)
	}
	previousStaleCleaner := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = previousStaleCleaner })
	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	initialAPI := readPortalRuns(t, server.URL)
	if len(initialAPI) != 1 || initialAPI[0].Status != "waiting" {
		t.Fatalf("initial /api/runs = %#v, want one waiting row", initialAPI)
	}

	for _, event := range []events.Event{
		{Type: "run.continued", Timestamp: continuedAt, RunID: runID, Issue: 466, Payload: map[string]any{
			"branch": "466-fix", "batch_id": batchID,
		}},
		{Type: "run.continued", Timestamp: secondContinuedAt, RunID: runID, Issue: 466, Payload: map[string]any{
			"branch": "466-fix", "batch_id": batchID,
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 466, Payload: map[string]any{
			"status": "success", "branch": "466-fix", "batch_id": batchID,
		}},
	} {
		if err := eventLog.Log(event); err != nil {
			t.Fatal(err)
		}
	}
	terminalAPI := readPortalRuns(t, server.URL)
	if len(terminalAPI) != 1 || terminalAPI[0].Status != "success" || terminalAPI[0].Kind != "completed" || terminalAPI[0].FinishedAt == nil {
		t.Fatalf("terminal /api/runs = %#v, want one completed success row", terminalAPI)
	}
	terminal, err := idx.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("terminal snapshot: %v", err)
	}
	if len(terminal) != 1 {
		t.Fatalf("terminal snapshot = %#v, want exactly one row", terminal)
	}
	if got := terminal[0]; got.Status != "success" || got.Kind != "completed" || got.FinishedAt == nil {
		t.Fatalf("terminal snapshot row = %#v, want completed success with terminal timestamp", got)
	}
	if got, want := terminal[0].Duration, "1h12m0s"; got != want {
		t.Fatalf("terminal duration = %q, want %q", got, want)
	}
	if len(terminal[0].Events) != 5 || terminal[0].Events[1].Type != "run.await" {
		t.Fatalf("terminal event history = %#v, want await evidence and all lifecycle events", terminal[0].Events)
	}
}

func TestPortalRunsIndex_SummaryRecomputesAfterAwaitedRunFinishes(t *testing.T) {
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)
	runID := "run-42-1234567890"
	startedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	awaitAt := startedAt.Add(5 * time.Minute)
	continuedAt := awaitAt.Add(time.Hour)
	finishedAt := continuedAt.Add(7 * time.Minute)
	eventLog := &events.JSONLLogger{Path: layout.EventsLogPath}
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "42-fix"}},
		{Type: "run.await", Timestamp: awaitAt, RunID: runID, Issue: 42, Payload: map[string]any{
			"await_reason": "pending", "branch": "42-fix",
		}},
	})
	idx := &portalRunsIndex{
		repoRoot:     repoRoot,
		eventLogPath: layout.EventsLogPath,
		view:         &portalRunsView{},
	}
	initial, err := idx.SummarySnapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("initial summary snapshot: %v", err)
	}
	if len(initial.Runs) != 1 || initial.Runs[0].Status != "waiting" || initial.ETag == "" {
		t.Fatalf("initial summary = %#v, want one waiting row with ETag", initial)
	}

	for _, event := range []events.Event{
		{Type: "run.continued", Timestamp: continuedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "42-fix"}},
	} {
		if err := eventLog.Log(event); err != nil {
			t.Fatal(err)
		}
	}
	terminal, err := idx.SummarySnapshot(context.Background(), initial.ETag)
	if err != nil {
		t.Fatalf("terminal summary snapshot: %v", err)
	}
	if terminal.NotModified || terminal.ETag == initial.ETag {
		t.Fatalf("terminal summary = %#v, want changed ETag and fresh response", terminal)
	}
	if len(terminal.Runs) != 1 {
		t.Fatalf("terminal summary runs = %#v, want one row", terminal.Runs)
	}
	if got := terminal.Runs[0]; got.Status != "success" || got.Kind != "completed" || got.FinishedAt == nil {
		t.Fatalf("terminal summary row = %#v, want completed success with terminal timestamp", got)
	}

	unchanged, err := idx.SummarySnapshot(context.Background(), terminal.ETag)
	if err != nil {
		t.Fatalf("unchanged summary snapshot: %v", err)
	}
	if !unchanged.NotModified || unchanged.ETag != terminal.ETag {
		t.Fatalf("unchanged summary = %#v, want 304-equivalent response", unchanged)
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
