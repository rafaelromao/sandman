package cmd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

// TestPortal_AwaitEventShowsWaiting verifies that when a run has a current
// run.await event (no run.finished), the portal shows it as "waiting".
func TestPortal_AwaitEventShowsWaiting(t *testing.T) {
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
	if got.Status != "waiting" {
		t.Fatalf("expected status 'waiting' for active run with await event, got %q", got.Status)
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

	previousStaleCleaner := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = previousStaleCleaner })
	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	responseRuns := readPortalRuns(t, server.URL)
	var responseRun *portalRun
	for i := range responseRuns {
		if responseRuns[i].IssueNumber == 42 {
			responseRun = &responseRuns[i]
			break
		}
	}
	if responseRun == nil || responseRun.Status != "waiting" {
		t.Fatalf("expected /api/runs issue 42 to return waiting, got %#v", responseRuns)
	}

	eventLog := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}
	if err := eventLog.Log(events.Event{
		Type: "run.continued", Timestamp: awaitAt.Add(time.Minute), RunID: "1-42", Issue: 42,
		Payload: map[string]any{"branch": "42-fix-bug", "batch_id": "1-batch"},
	}); err != nil {
		t.Fatal(err)
	}
	runs, err = (&portalRunsView{}).compute(repoRoot, eventLog)
	if err != nil {
		t.Fatalf("compute after continuation: %v", err)
	}
	for i := range runs {
		if runs[i].IssueNumber == 42 {
			got = &runs[i]
			break
		}
	}
	if got.Status != "running" {
		t.Fatalf("expected continuation to clear waiting, got %q", got.Status)
	}
	if len(got.Events) != 3 {
		t.Fatalf("expected historical await event to remain in portal events, got %d events", len(got.Events))
	}

	if err := eventLog.Log(events.Event{
		Type: "run.await", Timestamp: awaitAt.Add(2 * time.Minute), RunID: "1-42", Issue: 42,
		Payload: map[string]any{"await_reason": "review-timeout", "branch": "42-fix-bug", "batch_id": "1-batch"},
	}); err != nil {
		t.Fatal(err)
	}
	runs, err = (&portalRunsView{}).compute(repoRoot, eventLog)
	if err != nil {
		t.Fatalf("compute after second await: %v", err)
	}
	for i := range runs {
		if runs[i].IssueNumber == 42 {
			got = &runs[i]
			break
		}
	}
	if got.Status != "waiting" {
		t.Fatalf("expected newer await to restore waiting, got %q", got.Status)
	}
	if len(got.Events) != 4 {
		t.Fatalf("expected both await events in portal details, got %d events", len(got.Events))
	}
}

func TestPortal_StartupPreservesAwaitingRunWithUnexpiredLease(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	batchID := "dead-await-portal"
	runID := "run-await-portal-42"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatalf("create batch directory: %v", err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: now.Add(-20 * time.Minute)}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}
	runDir := filepath.Join(batchDir, "runs", runID)
	if err := batchindex.WriteManifest(runDir, batchindex.RunManifest{
		RunID: runID, BatchID: batchID, Issue: 42,
		Status: batchindex.RunManifestStatusActive, CreatedAt: now.Add(-19 * time.Minute),
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}
	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{42})
	if err := daemon.RenewRunWait(batchDir, daemon.RunWait{
		Protocol: "run-wait/v1", RunID: runID, BatchID: batchID, Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "ci:17:head",
		OperationDeadline: now.Add(30 * time.Minute),
	}, now); err != nil {
		t.Fatalf("write awaiting snapshot: %v", err)
	}
	eventsPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, eventsPath, []events.Event{
		{Type: "run.started", RunID: runID, Issue: 42, Timestamp: now.Add(-19 * time.Minute), Payload: map[string]any{
			"batch_id": batchID, "branch": "42-fix", "base_branch": "main",
		}},
		{Type: "run.await", RunID: runID, Issue: 42, Timestamp: now.Add(-10 * time.Minute), Payload: map[string]any{
			"await_reason": "pending", "ci_wait": map[string]any{
				"deadline_unix_seconds": now.Add(30 * time.Minute).Unix(),
			},
		}},
	})

	handler := newPortalHandler(repoRoot)
	portal := handler.(*portalHandler)
	portal.waitForStaleCleanup()
	server := startPortalHTTPServer(t, handler)
	runs := readPortalRuns(t, server.URL)
	var got *portalRun
	for i := range runs {
		if runs[i].IssueNumber == 42 {
			got = &runs[i]
			break
		}
	}
	if got == nil || got.Status != "waiting" || got.FinishedAt != nil {
		t.Fatalf("portal row = %#v, want active waiting row", got)
	}
	logEvents, err := (&events.JSONLLogger{Path: eventsPath}).Read()
	if err != nil {
		t.Fatalf("read portal event log: %v", err)
	}
	for _, event := range logEvents {
		if event.RunID == runID && event.Type == "run.aborted" {
			t.Fatal("portal startup aborted an awaiting run with an unexpired lease")
		}
	}
	manifest, err := batchindex.ReadManifest(runDir)
	if err != nil {
		t.Fatalf("read run manifest after portal startup: %v", err)
	}
	if manifest.Status != batchindex.RunManifestStatusActive {
		t.Fatalf("run manifest status = %q, want active", manifest.Status)
	}
}

func TestPortal_LiveReviewKeepsRunningAndReviewingPresentation(t *testing.T) {
	started := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	runs := (&portalRunsView{}).aggregateReviewChildren(paths.NewLayout(nil, t.TempDir()), []portalRun{
		{IssueNumber: 42, RunID: "impl-42", Key: "impl-42", Kind: "active", Status: "running", StartedAt: started},
		{IssueNumber: 42, RunID: "review-42", Key: "review-42", Kind: "active", Status: "reviewing", Review: true, StartedAt: started.Add(time.Minute)},
	})
	var implementation, review *portalRun
	for i := range runs {
		switch runs[i].RunID {
		case "impl-42":
			implementation = &runs[i]
		case "review-42":
			review = &runs[i]
		}
	}
	if implementation == nil || implementation.Status != "reviewing" || !implementation.ReviewLive {
		t.Fatalf("live review aggregate changed implementation presentation: %#v", implementation)
	}
	if review == nil || review.Status != "reviewing" {
		t.Fatalf("live review row presentation changed: %#v", review)
	}
}

func TestPortal_WaitingBadgeHasDedicatedStyle(t *testing.T) {
	html, err := os.ReadFile("portal.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(html)
	if !strings.Contains(source, ".badge.waiting {") || !strings.Contains(source, ".badge.waiting .dot {") {
		t.Fatal("expected portal to style waiting badges distinctly")
	}
}

func TestPortal_StartedCapacityContinuationStaysWaiting(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, prior := range []string{"run.queued", "run.started", "run.continued"} {
		t.Run(prior, func(t *testing.T) {
			state := events.ProjectRunStates([]events.Event{
				{Type: prior, Timestamp: startedAt, RunID: "row", Issue: 42, Payload: map[string]any{"initial_admission": true}},
				{Type: "run.capacity_queued", Timestamp: startedAt.Add(time.Minute), RunID: "row", Issue: 42, Payload: map[string]any{"ready_continuation": true}},
			})[0]
			view := &portalRunsView{now: func() time.Time { return startedAt.Add(time.Hour) }}
			row := view.runFromState(t.TempDir(), state, nil, nil, nil, nil)
			want := "waiting"
			if prior == "run.queued" {
				want = "queued"
			}
			if row.Status != want || row.FinishedAt != nil {
				t.Fatalf("after %s: status=%q finished=%v, want non-terminal %s", prior, row.Status, row.FinishedAt, want)
			}
		})
	}
}

func TestPortal_CapacityDelayReplacesExternalAwaitPhase(t *testing.T) {
	started := time.Now().UTC().Add(-time.Hour)
	state := events.ProjectRunStates([]events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: started},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: started.Add(time.Minute), Payload: map[string]any{"await_reason": "pending"}},
		{Type: "run.capacity_queued", RunID: "row", Issue: 42, Timestamp: started.Add(2 * time.Minute), Payload: map[string]any{"ready_continuation": true}},
	})[0]
	view := &portalRunsView{now: func() time.Time { return started.Add(time.Hour) }}
	row := view.runFromState(t.TempDir(), state, nil, nil, nil, nil)
	if state.IsAwaiting() || !state.IsCapacityQueued() || state.AwaitEvent == nil || row.Kind != "active" || row.Status != "waiting" || row.FinishedAt != nil || row.Duration != time.Minute.String() {
		t.Fatalf("capacity transition retained old phase or revised lifecycle: state=%+v row=%+v", state, row)
	}
}

func TestPortal_AwaitDurationPausesAndResumes(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	awaitAt := startedAt.Add(5 * time.Minute)
	resumedAt := awaitAt.Add(time.Hour)
	finishedAt := resumedAt.Add(7 * time.Minute)
	repoRoot := t.TempDir()

	awaiting := events.ProjectRunStates([]events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-portal-duration", Issue: 42},
		{Type: "run.await", Timestamp: awaitAt, RunID: "run-portal-duration", Issue: 42},
	})[0]
	view := &portalRunsView{now: func() time.Time { return awaitAt.Add(2 * time.Hour) }}
	row := view.runFromState(repoRoot, awaiting, nil, nil, nil, nil)
	if row.Status != "waiting" {
		t.Fatalf("status = %q, want waiting", row.Status)
	}
	if row.Duration != "5m0s" {
		t.Fatalf("waiting duration = %q, want frozen 5m0s", row.Duration)
	}
	view.now = func() time.Time { return awaitAt.Add(3 * time.Hour) }
	row = view.runFromState(repoRoot, awaiting, nil, nil, nil, nil)
	if row.Duration != "5m0s" {
		t.Fatalf("later waiting duration = %q, want unchanged 5m0s", row.Duration)
	}

	resumed := events.ProjectRunStates([]events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-portal-duration", Issue: 42},
		{Type: "run.await", Timestamp: awaitAt, RunID: "run-portal-duration", Issue: 42},
		{Type: "run.resumed", Timestamp: resumedAt, RunID: "run-portal-duration", Issue: 42},
	})[0]
	view.now = func() time.Time { return finishedAt }
	row = view.runFromState(repoRoot, resumed, nil, nil, nil, nil)
	if row.Duration != "12m0s" {
		t.Fatalf("resumed duration = %q, want 12m0s", row.Duration)
	}
}
