package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

type portalTestOutput struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func newPortalTestOutput() *portalTestOutput {
	return &portalTestOutput{ready: make(chan struct{})}
}

func (o *portalTestOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.buf.Write(p)
	o.once.Do(func() { close(o.ready) })
	return n, err
}

func (o *portalTestOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func TestPortal_FindsRepoRootFromSubdir(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(repoRoot, "nested", "dir")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}

	found, err := findRepoRoot(subdir)
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	if found != repoRoot {
		t.Fatalf("expected repo root %q, got %q", repoRoot, found)
	}
}

func TestPortal_APIRescansRunsOnEachRequest(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	createUnixRunSocket(t, filepath.Join(repoRoot, ".sandman", "runs", "run-1", "run.sock"))

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	first := readPortalInstances(t, server.URL)
	if len(first) != 1 || first[0].Name != "run-1" {
		t.Fatalf("expected 1 run-1 instance, got %#v", first)
	}

	createUnixRunSocket(t, filepath.Join(repoRoot, ".sandman", "runs", "run-2", "run.sock"))

	second := readPortalInstances(t, server.URL)
	if len(second) != 2 {
		t.Fatalf("expected 2 instances after late start, got %#v", second)
	}
	if second[1].Name != "run-2" {
		t.Fatalf("expected late-starting run-2 to appear on next poll, got %#v", second)
	}
}

func TestPortal_IgnoresNonSocketRunFiles(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "runs", "run-file"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "runs", "run-file", "run.sock"), []byte("not a socket"), 0644); err != nil {
		t.Fatal(err)
	}

	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		t.Fatalf("discover instances: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("expected no instances for regular file, got %#v", instances)
	}
}

func TestPortal_IgnoresStaleSocketRunFiles(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// A finished batch leaves a run.sock inode with the socket bit set on
	// disk, but the process that owned it is gone. The portal must not
	// treat this as an active instance. The listener below exists only
	// so the socket file persists on disk with the socket bit set; the
	// liveness probe is stubbed to false so the listener's actual
	// dialability is irrelevant.
	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-stale-1")
	sockPath := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		t.Fatalf("discover instances: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("expected no instances for stale socket (no listener), got %#v", instances)
	}
}

func TestPortal_RunsAPI_OmitsRowsForFinishedBatchWithDeadSocket(t *testing.T) {
	// Long test names push t.TempDir() paths over the 108-byte Unix
	// socket limit, so create a short temp dir under /tmp directly.
	repoRoot, err := os.MkdirTemp("/tmp", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Seed a finished batch: a run.sock inode, a batch.json listing
	// two issues, and no run.started events for either issue. The
	// listener exists only so the socket file persists on disk with
	// the socket bit set; the liveness probe is stubbed to false so
	// the listener's actual dialability is irrelevant.
	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-42-1")
	sockPath := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42, 43}, CreatedAt: time.Now().Add(-2 * time.Minute)}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 0 {
		t.Fatalf("expected no rows for a finished batch with a dead socket, got %#v", runs)
	}
}

func TestPortal_LoadPortalRunsMergesActiveAndCompletedRuns(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	activeSock := filepath.Join(repoRoot, ".sandman", "runs", "run-1-100", "run.sock")
	if err := os.MkdirAll(filepath.Dir(activeSock), 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(filepath.Dir(activeSock), daemon.BatchManifest{Issues: []int{1, 2, 3, 4}, CreatedAt: batchStartedAt}); err != nil {
		t.Fatal(err)
	}
	activeLn, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = activeLn.Close() })
	go func() {
		conn, err := activeLn.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("[issue-1] 12:00:00 live output\n[issue-2] 12:00:00 hidden output\n"))
		_ = conn.Close()
	}()

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.started", Timestamp: batchStartedAt.Add(2 * time.Minute), RunID: "run-2", Issue: 2, Payload: map[string]any{"branch": "sandman/2-fix"}},
		{Type: "run.finished", Timestamp: batchStartedAt.Add(3 * time.Minute), RunID: "run-2", Issue: 2, Payload: map[string]any{"status": "success", "branch": "sandman/2-fix"}},
		{Type: "run.blocked", Timestamp: batchStartedAt.Add(4 * time.Minute), RunID: "run-3", Issue: 3, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.started", Timestamp: batchStartedAt.Add(-30 * time.Minute), RunID: "run-9", Issue: 9, Payload: map[string]any{"branch": "sandman/9-fix"}},
		{Type: "run.finished", Timestamp: batchStartedAt.Add(-25 * time.Minute), RunID: "run-9", Issue: 9, Payload: map[string]any{"status": "success", "branch": "sandman/9-fix"}},
	})

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "1.log"), []byte("issue one log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "2.log"), []byte("\x1b[0missue two log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "9.log"), []byte("issue nine log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 5 {
		t.Fatalf("expected 5 runs, got %#v", runs)
	}
	byIssue := map[int]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}
	if run := byIssue[1]; run.Kind != "active" || run.IssueLabel != "#1" || run.Status != "running" {
		t.Fatalf("unexpected active run: %#v", run)
	}
	if run := byIssue[1]; !strings.Contains(run.Log, "live output") || strings.Contains(run.Log, "hidden output") || strings.Contains(run.Log, "\x1b[") {
		t.Fatalf("expected filtered active log, got %#v", run.Log)
	}
	if run := byIssue[2]; run.Status != "success" || run.Kind != "active" || !strings.Contains(run.Log, "issue two log") {
		t.Fatalf("unexpected completed run: %#v", run)
	}
	if run := byIssue[3]; run.Status != "blocked" || !strings.Contains(run.Log, "Blocked by #99.") {
		t.Fatalf("unexpected blocked run: %#v", run)
	}
	if run := byIssue[4]; run.Status != "queued" || !strings.Contains(run.Log, "Queued. Waiting to start.") {
		t.Fatalf("unexpected queued run: %#v", run)
	}
	if run := byIssue[9]; run.Status != "success" || run.Kind != "completed" || !strings.Contains(run.Log, "issue nine log") {
		t.Fatalf("unexpected historical completed run: %#v", run)
	}
}

func TestPortal_ReviewRunShowsReviewingStatus(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run-review-1", "run-normal-1"} {
		runDir := filepath.Join(repoRoot, ".sandman", "runs", runID)
		if err := os.MkdirAll(runDir, 0755); err != nil {
			t.Fatal(err)
		}
		sockPath := filepath.Join(runDir, "run.sock")
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-review-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "review": true, "pr_number": 42}},
		{Type: "run.started", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-normal-1", Issue: 2, Payload: map[string]any{"branch": "sandman/2-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d: %#v", len(runs), runs)
	}
	byIssue := map[int]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}

	reviewRun, ok := byIssue[1]
	if !ok {
		t.Fatal("expected review run for issue 1")
	}
	if reviewRun.Status != "reviewing" {
		t.Fatalf("expected status 'reviewing' for active review run, got %q", reviewRun.Status)
	}
	if !reviewRun.Review {
		t.Fatal("expected Review=true for review run")
	}
	if reviewRun.PRNumber != 42 {
		t.Fatalf("expected PRNumber=42 for review run, got %d", reviewRun.PRNumber)
	}
	if reviewRun.IssueLabel == "prompt-only" {
		t.Fatalf("expected review run to avoid prompt-only label, got %q", reviewRun.IssueLabel)
	}
	if reviewRun.Kind != "active" {
		t.Fatalf("expected kind 'active' for reviewing run, got %q", reviewRun.Kind)
	}

	normalRun, ok := byIssue[2]
	if !ok {
		t.Fatal("expected normal run for issue 2")
	}
	if normalRun.Status != "running" {
		t.Fatalf("expected status 'running' for normal active run, got %q", normalRun.Status)
	}
	if normalRun.Review {
		t.Fatal("expected Review=false for normal run")
	}
	if normalRun.IssueLabel != "#2" {
		t.Fatalf("expected IssueLabel #2 for normal run, got %q", normalRun.IssueLabel)
	}
}

func TestPortal_LoadPortalRunsShowsReviewAndPromptOnlyLabels(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, ".sandman", "runs", "PR43")
	sockPath := filepath.Join(sockDir, "run.sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sockDir, "batch.json"), []byte(`{"issues":[],"runID":"PR43"}`), 0644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}()

	started := time.Now().Add(-5 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: started, RunID: "PR42", Issue: 0, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
		{Type: "run.finished", Timestamp: started.Add(1 * time.Minute), RunID: "PR42", Issue: 0, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true}},
		{Type: "run.started", Timestamp: started.Add(2 * time.Minute), RunID: "PR43", Issue: 0, Payload: map[string]any{"branch": "sandman/review-PR43", "review": true, "pr_number": 43}},
		{Type: "run.started", Timestamp: started.Add(2 * time.Minute), RunID: "run-prompt-1", Issue: 0, Payload: map[string]any{"branch": "sandman/prompt-only-1"}},
		{Type: "run.finished", Timestamp: started.Add(3 * time.Minute), RunID: "run-prompt-1", Issue: 0, Payload: map[string]any{"status": "success", "branch": "sandman/prompt-only-1"}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %#v", runs)
	}

	byRunID := make(map[string]portalRun, len(runs))
	for _, run := range runs {
		byRunID[run.RunID] = run
	}

	reviewRun, ok := byRunID["PR42"]
	if !ok {
		t.Fatal("expected completed review run with RunID PR42")
	}
	if reviewRun.IssueLabel != "PR42" {
		t.Fatalf("expected review run label PR42, got %q", reviewRun.IssueLabel)
	}
	if !reviewRun.Review {
		t.Fatal("expected review flag on completed review run")
	}

	activeReview, ok := byRunID["PR43"]
	if !ok {
		t.Fatal("expected active review run with RunID PR43")
	}
	if activeReview.IssueLabel != "PR43" {
		t.Fatalf("expected active review run label PR43, got %q", activeReview.IssueLabel)
	}
	if !activeReview.Review {
		t.Fatal("expected review flag on active review run")
	}

	promptOnly, ok := byRunID["run-prompt-1"]
	if !ok {
		t.Fatal("expected prompt-only run")
	}
	if promptOnly.IssueLabel != "prompt-only" {
		t.Fatalf("expected prompt-only label to stay prompt-only, got %q", promptOnly.IssueLabel)
	}
	if promptOnly.Review {
		t.Fatal("expected Review=false for prompt-only run")
	}
}

func TestPortal_RunsEndpoint_RoundTripsReasonForReviewAndAutoSelect(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	started := time.Now().Add(-5 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: started, RunID: "PR42", Issue: 0, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
		{Type: "run.finished", Timestamp: started.Add(1 * time.Minute), RunID: "PR42", Issue: 0, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true}},
		{Type: "run.started", Timestamp: started.Add(2 * time.Minute), RunID: "auto-select-1700000000000", Payload: map[string]any{"run_kind": "auto-select", "branch": "sandman/auto-select"}},
		{Type: "run.finished", Timestamp: started.Add(3 * time.Minute), RunID: "auto-select-1700000000000", Payload: map[string]any{"run_kind": "auto-select", "status": "success"}},
		{Type: "run.started", Timestamp: started.Add(4 * time.Minute), RunID: "run-issue-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: started.Add(5 * time.Minute), RunID: "run-issue-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix", "status": "success"}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Parse the raw response so we can assert on key presence/absence
	// without coupling to the Go struct.
	var payload struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	byRunID := make(map[string]map[string]any, len(payload.Runs))
	for _, r := range payload.Runs {
		rid, _ := r["runId"].(string)
		byRunID[rid] = r
	}

	review, ok := byRunID["PR42"]
	if !ok {
		t.Fatal("expected PR42 in JSON")
	}
	if reason, _ := review["reason"].(string); reason != "review" {
		t.Errorf("expected reason=review for PR42, got %v", review["reason"])
	}

	auto, ok := byRunID["auto-select-1700000000000"]
	if !ok {
		t.Fatal("expected auto-select run in JSON")
	}
	if reason, _ := auto["reason"].(string); reason != "auto-select" {
		t.Errorf("expected reason=auto-select, got %v", auto["reason"])
	}

	issue, ok := byRunID["run-issue-42-1"]
	if !ok {
		t.Fatal("expected issue-driven run in JSON")
	}
	if _, hasReason := issue["reason"]; hasReason {
		t.Errorf("expected reason key absent for issue-driven run, got %v", issue["reason"])
	}
}

func TestPortal_CompletedReviewRunShowsTerminalStatus(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-review-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "review": true, "pr_number": 42}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-review-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix", "review": true}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d: %#v", len(runs), runs)
	}
	run := runs[0]
	if run.Status != "success" {
		t.Fatalf("expected status 'success' for completed review run, got %q", run.Status)
	}
	if !run.Review {
		t.Fatal("expected Review=true for completed review run")
	}
	if run.Kind != "completed" {
		t.Fatalf("expected kind 'completed' for finished review run, got %q", run.Kind)
	}
}

func TestPortal_LoadPortalRunsTreatsAbortedAsTerminalAborted(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.aborted", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "aborted", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %#v", runs)
	}
	run := runs[0]
	if run.Kind != "completed" || run.Status != "aborted" {
		t.Fatalf("expected aborted run to project as completed aborted, got %#v", run)
	}
}

func TestPortal_LoadPortalRunsTreatsAbortedEventAsAbortedRegardlessOfPayloadStatus(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.aborted", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "failure", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %#v", runs)
	}
	run := runs[0]
	if run.Kind != "completed" || run.Status != "aborted" {
		t.Fatalf("expected aborted run to project as completed aborted, got %#v", run)
	}
}

func TestPortal_LoadPortalRuns_ShowsQueuedIssuesFromEvents(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: batchStartedAt.Add(2 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
		{Type: "run.queued", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "run-2", Issue: 2, Payload: map[string]any{"blocked_by": []int{1}}},
		{Type: "run.queued", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "run-3", Issue: 3, Payload: map[string]any{}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
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
		t.Fatalf("expected completed success run for issue 1, got kind=%q status=%q", run.Kind, run.Status)
	}
	if run := byIssue[2]; run.Kind != "completed" || run.Status != "queued" {
		t.Fatalf("expected completed queued run for issue 2, got kind=%q status=%q", run.Kind, run.Status)
	}
	if run := byIssue[3]; run.Kind != "completed" || run.Status != "queued" {
		t.Fatalf("expected completed queued run for issue 3, got kind=%q status=%q", run.Kind, run.Status)
	}
}

func TestPortal_AbortRunEndpointAbortsActiveRunAndRefreshesStatus(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-portal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-42-1")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	prevAbort := portalRunAborter
	t.Cleanup(func() { portalRunAborter = prevAbort })
	portalRunAborter = func(ctx context.Context, repoRootArg, runKey string, issueNumber int) error {
		if repoRootArg != repoRoot {
			t.Fatalf("expected repo root %q, got %q", repoRoot, repoRootArg)
		}
		if runKey != "run-42-1" {
			t.Fatalf("expected run key run-42-1, got %q", runKey)
		}
		if issueNumber != 42 {
			t.Fatalf("expected issue 42, got %d", issueNumber)
		}
		writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
			{Type: "run.aborted", Timestamp: time.Now(), RunID: "run-42-1", Issue: 42, Payload: map[string]any{"status": "aborted", "branch": "sandman/42-fix"}},
		})
		return os.RemoveAll(runDir)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/abort", strings.NewReader(`{"runKey":"run-42-1","issue":42}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 1 {
		t.Fatalf("expected 1 projected run after stop, got %#v", runs)
	}
	run := runs[0]
	if run.Kind != "completed" || run.Status != "aborted" || run.IssueNumber != 42 {
		t.Fatalf("expected aborted run to project as completed aborted, got %#v", run)
	}
}

func TestAbortPortalRunSendsAbortRequestAndReturnsSuccess(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-42-1")
	activeSock := filepath.Join(runDir, "run.sock")
	abortSock := filepath.Join(runDir, "cmd.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	abortLn, err := net.Listen("unix", abortSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = abortLn.Close() })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	prevPeerPID := portalPeerPID
	t.Cleanup(func() {
		portalPeerPID = prevPeerPID
	})
	portalPeerPID = func(sockPath string) (int, error) {
		if sockPath != activeSock {
			t.Fatalf("expected socket %q, got %q", activeSock, sockPath)
		}
		return 12345, nil
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := abortLn.Accept()
			if err != nil {
				return
			}
			var req struct {
				Action string `json:"action"`
				Issue  int    `json:"issue"`
			}
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				conn.Close()
				continue
			}
			if req.Action != "abort" || req.Issue != 42 {
				t.Errorf("unexpected abort request: %#v", req)
				conn.Close()
				return
			}
			if err := json.NewEncoder(conn).Encode(daemon.CommandResponse{Status: "ok"}); err != nil {
				t.Errorf("write abort response: %v", err)
				conn.Close()
				return
			}
			conn.Close()
			close(done)
			return
		}
	}()

	if err := abortPortalRun(context.Background(), repoRoot, "run-42-1", 42); err != nil {
		t.Fatalf("abort portal run: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for abort response")
	}
}

func TestAbortPortalRun_ReturnsHTTPStatusCodes(t *testing.T) {
	t.Run("missing run", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		err := abortPortalRun(context.Background(), repoRoot, "run-42-1", 42)
		var abortErr *portalAbortError
		if !errors.As(err, &abortErr) {
			t.Fatalf("expected portalAbortError, got %v", err)
		}
		if abortErr.status != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", abortErr.status)
		}
	})

	t.Run("dial failure is sanitized", func(t *testing.T) {
		repoRoot, err := os.MkdirTemp("/tmp", "sm-abort-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-42-1")
		if err := os.MkdirAll(runDir, 0755); err != nil {
			t.Fatal(err)
		}
		activeSock := filepath.Join(runDir, "run.sock")
		cmdSock := filepath.Join(runDir, "cmd.sock")
		if err := os.WriteFile(cmdSock, []byte("offline"), 0644); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", activeSock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })

		writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
			{Type: "run.started", Timestamp: time.Now(), RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		})

		err = abortPortalRun(context.Background(), repoRoot, "run-42-1", 42)
		var abortErr *portalAbortError
		if !errors.As(err, &abortErr) {
			t.Fatalf("expected portalAbortError, got %v", err)
		}
		if abortErr.status != http.StatusBadGateway {
			t.Fatalf("expected 502, got %d", abortErr.status)
		}
		if strings.Contains(abortErr.message, "cmd.sock") {
			t.Fatalf("expected sanitized error, got %q", abortErr.message)
		}
	})

	t.Run("inactive issue", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		prevAbort := portalRunAborter
		t.Cleanup(func() { portalRunAborter = prevAbort })
		portalRunAborter = func(context.Context, string, string, int) error {
			return &portalAbortError{status: http.StatusConflict, message: "batch: no such issue"}
		}

		server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
		defer server.Close()

		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/abort", strings.NewReader(`{"runKey":"run-42-1","issue":9999}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("daemon silent", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		prevAbort := portalRunAborter
		t.Cleanup(func() { portalRunAborter = prevAbort })
		portalRunAborter = func(context.Context, string, string, int) error {
			return &portalAbortError{status: http.StatusBadGateway, message: "daemon silent"}
		}

		server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
		defer server.Close()

		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/abort", strings.NewReader(`{"runKey":"run-42-1","issue":42}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 502, got %d: %s", resp.StatusCode, body)
		}
	})
}

func TestAbortPortalRun_RejectsRunFromFinishedBatch(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-abort-stale-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-42-1")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	// Close the listener and remove the socket to simulate a dead daemon.
	_ = ln.Close()
	_ = os.Remove(activeSock)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	err = abortPortalRun(context.Background(), repoRoot, "run-42-1", 42)
	var abortErr *portalAbortError
	if !errors.As(err, &abortErr) {
		t.Fatalf("expected portalAbortError, got %v", err)
	}
	if abortErr.status != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", abortErr.status, abortErr.message)
	}
	if strings.TrimSpace(abortErr.message) == "" {
		t.Fatal("expected non-empty message")
	}
}

func TestPortal_RunsEndpointIncludesContinuedRun(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Now().Add(-25 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: time.Now().Add(-20 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
		{Type: "run.continued", Timestamp: time.Now().Add(-15 * time.Minute), RunID: "run-2", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-2", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
	})
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "1.log"), []byte("continued run log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %#v", runs)
	}
	byID := map[string]portalRun{}
	for _, run := range runs {
		byID[run.RunID] = run
	}
	continued, ok := byID["run-2"]
	if !ok {
		t.Fatal("expected continued run in API response")
	}
	if continued.IssueLabel != "#1" || continued.Status != "success" || continued.Kind != "completed" {
		t.Fatalf("unexpected continued run payload: %#v", continued)
	}
	if continued.Branch != "sandman/1-fix" || !strings.Contains(continued.Log, "continued run log") {
		t.Fatalf("expected continued run metadata, got %#v", continued)
	}
}

func TestPortal_PageExposesFiltersAndTabs(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{"Active", "Archive", "Log", "Events", "Details", "Actions", "data-rendered-json", "JSON.stringify(detailsData", "settings-toggle", "theme-picker", "poll-interval", "masthead-repo", `id="last-updated"`, "poll-health", "Sandman", "Sleep while your agents code", "Sandman Light", "Catppuccin", "Gruvbox", "Evergreen", "Tokyo Night", `const apiPath = "\/api\/runs";`, "const defaultTheme = 'sandman';", "html[data-theme=\"sandman\"]", `id="status-chips"`, `id="status-filter"`, `>All<`, `data-sort="status"`, `data-sort="started"`, `data-sort="duration"`, `id="active-batches"`, `id="archived-toggle"`, `syncFilterToggleButtons`} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:min(800, len(content))])
		}
	}
	// Status controls ship both desktop chip rail and mobile native select;
	// keep both markers and the initial 'All' option stable for the page
	// contract.
	// The data-action attributes live in the diff helper now.
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	diffHelper, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "portal_diff.js"))
	if err != nil {
		t.Fatalf("read portal_diff.js: %v", err)
	}
	for _, want := range []string{`'toggle-run'`, `'abort-run'`, `data-action`, `data-run-key`, `Abort`} {
		if !strings.Contains(string(diffHelper), want) {
			t.Fatalf("portal_diff.js missing %q", want)
		}
	}
	for _, banned := range []string{"command-row-hint", "row-hint", "Click row", `data-action="stop-run"`} {
		if strings.Contains(content, banned) {
			t.Fatalf("page should not contain %q\n%s", banned, content[:min(800, len(content))])
		}
	}
	if strings.Contains(content, ">Output<") {
		t.Fatalf("page should not expose Output tab\n%s", content[:min(800, len(content))])
	}
}

func TestPortal_PageUsesPlainEscapedTerminalRendering(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{"function renderTerminalContent(text)", "SandmanPortalDiff.highlightTerminalLog(value)"} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing terminal-rendering marker %q\n%s", want, content[:min(800, len(content))])
		}
	}
	for _, forbidden := range []string{"highlightTerminalText(", "term-protected"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("page must no longer contain %q after the plain-text renderer change\n%s", forbidden, content[:min(800, len(content))])
		}
	}
}

func TestPortal_PageIncludesAllClearEmptyStateMessage(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	if !strings.Contains(content, "All clear — 0 active") {
		t.Fatalf("page missing the all-clear empty-state contract\n%s", content[:min(1200, len(content))])
	}
}

func TestPortal_PageAbortedBadgeCSSIsDistinctFromArchived(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	if !strings.Contains(content, `.badge.aborted { background: color-mix(in oklch, var(--danger) 8%, var(--surface));`) {
		t.Fatalf("page missing the distinct aborted badge background rule\n%s", content[:min(1200, len(content))])
	}
	if strings.Contains(content, `.badge.archived`) {
		t.Fatalf("archived badge CSS must not remain on the page\n%s", content[:min(1200, len(content))])
	}
}

func TestPortal_PageMastheadShowsRepoAndUpdatedChip(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)

	if !strings.Contains(content, `class="masthead-repo"`) {
		t.Fatalf("page is missing masthead repo path\n%s", content[:min(800, len(content))])
	}
	if !strings.Contains(content, `id="last-updated"`) {
		t.Fatalf("page is missing the #last-updated element\n%s", content[:min(800, len(content))])
	}

	repoIdx := strings.Index(content, `class="masthead-repo"`)
	updatedIdx := strings.Index(content, `id="last-updated"`)
	if updatedIdx <= repoIdx {
		t.Fatalf("updated chip must appear after repo path in the rendered masthead\nrepoIdx=%d updatedIdx=%d", repoIdx, updatedIdx)
	}
}

func TestPortal_PageExposesPollHealthPill(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)

	for _, want := range []string{`id="poll-health"`, `id="last-updated"`, "updatePollHealth", "pollFailCount", "poll-health-updated"} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing poll-health marker %q\n%s", want, content[:min(800, len(content))])
		}
	}
	if strings.Contains(content, "poll-health-label") {
		t.Fatalf("poll-health pill should no longer include live label\n%s", content[:min(800, len(content))])
	}
	// The pill must start neutral (no ok/warn class) so the first paint
	// does not falsely claim a healthy or failing poll.
	if strings.Contains(content, `poll-health ok`) || strings.Contains(content, `poll-health warn`) {
		t.Fatalf("poll-health pill must start without an ok/warn state class\n%s", content[:min(800, len(content))])
	}
}

func TestPortal_PageWiresPortalViewStatePersistence(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{"SandmanPortalState", "sandman.portal.view-state.v1:", "persistPortalViewState", "normalizePortalViewState", "const visibleRuns = state.runs.filter(shouldShowRun);", "getSelectedTab"} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:min(800, len(content))])
		}
	}
}

func TestPortal_PageWiresSubjectSelector(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{`data-action="set-subject"`, `findRunByIdentity`, `detail-subject-picker`, `runs: state.runs`} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:min(800, len(content))])
		}
	}
}

func TestPortal_PageExposesArchivedFilter(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{
		`showArchived: persistedPortalState.showArchived === true`,
		`activeBatches: persistedPortalState.activeBatches === true`,
		`showArchived: state.showArchived`,
		`activeBatchesToggle.addEventListener('click'`,
		`archivedToggle.addEventListener('click'`,
		`if (state.activeBatches) return run.kind === 'active';`,
		`if (state.showArchived) return !!run.archived;`,
		`if (run.archived) return false;`,
		`const visibleRuns = state.runs.filter(shouldShowRun)`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:min(800, len(content))])
		}
	}
}

func TestPortal_PageWiresLogScrollPreservation(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{"SandmanPortalScroll", "data-scroll-key", "portalScroll.capture", "portalScroll.restore"} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q for log scroll preservation\n%s", want, content[:min(800, len(content))])
		}
	}
}

func TestPortal_DetailPanelHasFixedHeightWithScroll(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)

	if !strings.Contains(content, ".detail-panel") {
		t.Fatalf("page missing .detail-panel CSS block")
	}
	if !strings.Contains(content, "max-height: clamp(420px, 68vh, 780px)") {
		t.Fatalf(".detail-panel missing max-height clamp to match min-height")
	}
	if !strings.Contains(content, ".detail-panel { min-height: 0; max-height: calc(100vh - 220px); overflow: auto; }") {
		t.Fatalf(".detail-panel missing max-height:calc(100vh - 220px) at 960px breakpoint")
	}
}

func TestPortal_SyntaxHighlightingHasNoSizeCutoff(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	if strings.Contains(content, "12000") {
		t.Fatalf("page should not contain old char limit cutoff (12000)")
	}
	if strings.Contains(content, "value.length > 12000") {
		t.Fatalf("page should not contain old size cutoff condition")
	}
}

func TestPortal_PageExposesRetryChipStyles(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)

	if strings.Contains(content, ".retry-chip") {
		t.Fatalf("page should no longer render retry chip CSS\n%s", content[:1000])
	}
}

func TestPortal_PageExposesRetryEventCardStyles(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{".retry-event-card", ".retry-log", ".retry-line"} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q CSS hook\n%s", want, content[:1000])
		}
	}
}

func TestPortal_PageExposesMobileExpandedRunPanelStyles(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{
		`@media (max-width: 960px)`,
		`.detail-panel { min-height: 0; max-height: calc(100vh - 220px); overflow: auto; }`,
		`@media (max-width: 760px)`,
		`.table-shell { border-radius: 16px; }`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:1000])
		}
	}
}

func TestPortal_PageExposesMobileRunDetailFactsLayout(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	start := strings.Index(content, `@media (max-width: 760px)`)
	if start < 0 {
		t.Fatalf("page missing mobile media query\n%s", content[:min(1000, len(content))])
	}
	block := content[start:]
	if end := strings.Index(block, `@media (prefers-reduced-motion: reduce)`); end >= 0 {
		block = block[:end]
	}
	for _, want := range []string{
		`.detail-meta {`,
		`grid-template-columns: repeat(2, minmax(0, 1fr));`,
		`gap: 8px 12px;`,
		`.detail-box h3 {`,
		`padding-left: 14px;`,
		`padding-right: 14px;`,
		`.kv span {`,
		`font-size: 10px;`,
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("mobile detail facts layout missing %q\n%s", want, block[:min(1000, len(block))])
		}
	}
}

func TestPortal_MobileTableShellIsNotFixedHeightTrap(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)

	if strings.Contains(content, ".table-shell { border-radius: 16px; height: 56px; overflow: auto; flex: 0 0 auto; }") {
		t.Fatalf("page traps .table-shell in a 56px scroller at the 760px breakpoint; remove the fixed-height shell so the runs table flows naturally on mobile")
	}
}

func TestPortal_LaunchEndpointIsRemoved(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/launch", "application/json", strings.NewReader(`{"command":"run"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestPortal_DownloadsLogFiles(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(repoRoot, ".sandman", "logs", "1.log")
	if err := os.WriteFile(logPath, []byte("full log\nline two\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	href := "/api/logs?path=" + url.QueryEscape(filepath.Join(".sandman", "logs", "1.log"))
	resp, err := http.Get(server.URL + href)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Fatalf("expected attachment download, got %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "full log\nline two\n" {
		t.Fatalf("unexpected log body: %q", string(body))
	}
}

func TestPortal_LogsRejectsPathTraversal(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	// Place a sensitive file outside the log directory that a path-traversal
	// request would otherwise reveal.
	if err := os.WriteFile(filepath.Join(repoRoot, "secret.txt"), []byte("TOP-SECRET"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	cases := []string{
		filepath.Join("..", "secret.txt"),
		filepath.Join("..", "..", "etc", "passwd"),
		"../secret.txt",
	}
	for _, p := range cases {
		href := "/api/logs?path=" + url.QueryEscape(p)
		resp, err := http.Get(server.URL + href)
		if err != nil {
			t.Fatalf("request for %q: %v", p, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read body for %q: %v", p, readErr)
		}
		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "TOP-SECRET") {
			t.Fatalf("path traversal %q leaked secret: status=%d body=%q", p, resp.StatusCode, body)
		}
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("path traversal %q should yield 4xx, got %d (body=%q)", p, resp.StatusCode, body)
		}
	}
}

func TestPortal_BindsToLocalhostAndFailsWhenPortBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	port := busy.Addr().(*net.TCPAddr).Port
	out := newPortalTestOutput()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runPortalServer(ctx, t.TempDir(), port, portalDefaultHost, out)
	}()

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "bind portal on "+portalDefaultHost) {
			t.Fatalf("expected bind error on loopback bind, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bind failure")
	}
}

func TestPortal_AbortRejectsOversizedBody(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	prevAbort := portalRunAborter
	t.Cleanup(func() { portalRunAborter = prevAbort })
	portalRunAborter = func(context.Context, string, string, int) error {
		return nil
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	oversized := strings.Repeat("a", 2*1024*1024)
	body := `{"runKey":"run-1","issue":1,"padding":"` + oversized + `"}`
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/abort", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("expected 4xx for oversized body, got %d", resp.StatusCode)
	}
}

func TestPortal_HTTPServerHasReadWriteIdleTimeouts(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := newPortalHTTPServer(repoRoot)
	if server.ReadTimeout == 0 {
		t.Fatal("expected ReadTimeout to be set on portal http.Server, got zero")
	}
	if server.ReadHeaderTimeout == 0 {
		t.Fatal("expected ReadHeaderTimeout to be set on portal http.Server, got zero")
	}
	if server.WriteTimeout == 0 {
		t.Fatal("expected WriteTimeout to be set on portal http.Server, got zero")
	}
	if server.IdleTimeout == 0 {
		t.Fatal("expected IdleTimeout to be set on portal http.Server, got zero")
	}
	if server.ReadTimeout != portalReadHeaderTimeout {
		t.Fatalf("expected ReadTimeout to match portalReadHeaderTimeout (%s), got %s", portalReadHeaderTimeout, server.ReadTimeout)
	}
	if server.WriteTimeout != portalWriteTimeout {
		t.Fatalf("expected WriteTimeout to match portalWriteTimeout (%s), got %s", portalWriteTimeout, server.WriteTimeout)
	}
	if server.IdleTimeout != portalIdleTimeout {
		t.Fatalf("expected IdleTimeout to match portalIdleTimeout (%s), got %s", portalIdleTimeout, server.IdleTimeout)
	}
}

func TestPortal_DefaultHostIsLoopback(t *testing.T) {
	if portalDefaultHost != "127.0.0.1" {
		t.Fatalf("expected portalDefaultHost to be %q, got %q", "127.0.0.1", portalDefaultHost)
	}
}

func TestPortal_OptInWildcardBind(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := newPortalTestOutput()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runPortalServer(ctx, repoRoot, 0, "0.0.0.0", out)
	}()

	select {
	case <-out.ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for portal startup")
	}
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("unexpected portal stop error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for portal shutdown")
		}
	}()

	match := regexp.MustCompile(`http://0\.0\.0\.0:(\d+)`).FindStringSubmatch(out.String())
	if len(match) != 2 {
		t.Fatalf("expected banner to report 0.0.0.0 bind when --host 0.0.0.0 is passed, got %q", out.String())
	}
	port, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse startup port: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/instances", port))
	if err != nil {
		t.Fatalf("portal request via loopback failed: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from wildcard-bound portal, got %d", resp.StatusCode)
	}
}

func TestPortal_PrintListeningURL(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := newPortalTestOutput()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runPortalServer(ctx, repoRoot, 0, portalDefaultHost, out)
	}()

	select {
	case <-out.ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for portal startup")
	}

	match := regexp.MustCompile(`http://127\.0\.0\.1:(\d+)`).FindStringSubmatch(out.String())
	if len(match) != 2 {
		cancel()
		t.Fatalf("startup output missing listening URL: %q", out.String())
	}
	port, err := strconv.Atoi(match[1])
	if err != nil {
		cancel()
		t.Fatalf("parse startup port: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/instances", port))
	if err != nil {
		cancel()
		t.Fatalf("portal request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("expected 200 from portal, got %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected portal stop error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for portal shutdown")
	}
}

func createUnixRunSocket(t *testing.T, sockPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func writePortalLog(t *testing.T, path string, entries []events.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	log := &events.JSONLLogger{Path: path}
	for _, entry := range entries {
		if err := log.Log(entry); err != nil {
			t.Fatal(err)
		}
	}
}

func readPortalInstances(t *testing.T, baseURL string) []portalInstance {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Instances []portalInstance `json:"instances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Instances
}

func readPortalRuns(t *testing.T, baseURL string) []portalRun {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Runs
}

type portalHTTPServer struct {
	URL   string
	Close func()
}

func startPortalHTTPServer(t *testing.T, handler http.Handler) *portalHTTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(ln) }()
	closeFn := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
	t.Cleanup(closeFn)
	return &portalHTTPServer{URL: "http://" + ln.Addr().String(), Close: closeFn}
}
func TestLoadPortalRuns_DedupsBlockedAndQueuedRows(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "queued-run", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.blocked", Timestamp: batchStartedAt.Add(2 * time.Minute), RunID: "blocked-run", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run after dedup, got %d: %#v", len(runs), runs)
	}
	if runs[0].Status != "blocked" {
		t.Fatalf("expected status 'blocked', got %q", runs[0].Status)
	}
	if runs[0].IssueNumber != 42 {
		t.Fatalf("expected issue 42, got %d", runs[0].IssueNumber)
	}
}

func TestLoadPortalRuns_DedupsActiveBatchAndQueuedEvent(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "runs", "active-1")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{7}, CreatedAt: batchStartedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: batchStartedAt.Add(30 * time.Second), RunID: "queued-run-7", Issue: 7, Payload: map[string]any{}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run after dedup, got %d: %#v", len(runs), runs)
	}
	if runs[0].IssueNumber != 7 {
		t.Fatalf("expected issue 7, got %d", runs[0].IssueNumber)
	}
}

func TestPortal_DedupKeepsActiveBatchAndHistoricalRows(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "runs", "active-1")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: batchStartedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: batchStartedAt.Add(11 * time.Minute), RunID: "queued-run-42", Issue: 42, Payload: map[string]any{}},
		{Type: "run.blocked", Timestamp: batchStartedAt.Add(-3 * time.Minute), RunID: "blocked-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	var issueRuns []portalRun
	for _, run := range runs {
		if run.IssueNumber == 42 {
			issueRuns = append(issueRuns, run)
		}
	}
	if len(issueRuns) != 2 {
		t.Fatalf("expected 2 rows for issue 42, got %d: %#v", len(issueRuns), issueRuns)
	}
	var sawActiveQueued, sawHistoricalBlocked bool
	for _, run := range issueRuns {
		switch {
		case run.Kind == "active" && run.Status == "queued":
			sawActiveQueued = true
		case run.Kind == "completed" && run.Status == "blocked":
			sawHistoricalBlocked = true
		default:
			t.Fatalf("unexpected row for issue 42: %#v", run)
		}
	}
	if !sawActiveQueued {
		t.Fatal("expected active queued row for issue 42")
	}
	if !sawHistoricalBlocked {
		t.Fatal("expected historical blocked row for issue 42")
	}
}

func TestPortal_KeepsCompletedRunsThatStartAfterAnOlderActiveBatch(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-portal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	activeStartedAt := time.Now().Add(-30 * time.Minute)
	completedStartedAt := time.Now().Add(-5 * time.Minute)
	completedFinishedAt := completedStartedAt.Add(2 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-380-1")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{380}, CreatedAt: activeStartedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: completedStartedAt, RunID: "run-558-1", Issue: 558, Payload: map[string]any{"branch": "sandman/558-fix"}},
		{Type: "run.finished", Timestamp: completedFinishedAt, RunID: "run-558-1", Issue: 558, Payload: map[string]any{"status": "failure", "branch": "sandman/558-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	byIssue := map[int]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}

	if _, ok := byIssue[558]; !ok {
		t.Fatalf("expected completed issue 558 to remain visible, got %#v", runs)
	}
	if run := byIssue[558]; run.Kind != "completed" || run.Status != "failure" {
		t.Fatalf("expected issue 558 to project as completed failure, got %#v", run)
	}
}

func TestPortal_BatchWithBlockedIssue_ShowsOneRow(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "runs", "active-1")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: batchStartedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "queued-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.blocked", Timestamp: batchStartedAt.Add(2 * time.Minute), RunID: "blocked-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
	})

	prev := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = prev })
	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 1 {
		t.Fatalf("expected portal to render exactly 1 row, got %d: %#v", len(runs), runs)
	}
	run := runs[0]
	if run.IssueNumber != 42 {
		t.Fatalf("expected issue 42, got %d", run.IssueNumber)
	}
	if run.Status != "blocked" {
		t.Fatalf("expected status 'blocked', got %q", run.Status)
	}
}

func TestPortal_BatchWithMixedBlockedAndQueued_ShowsBlockedAndQueuedSeparately(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-portal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "runs", "active-1")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42, 43}, CreatedAt: batchStartedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "queued-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.blocked", Timestamp: batchStartedAt.Add(2 * time.Minute), RunID: "blocked-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
	})

	prev := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = prev })
	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 2 {
		t.Fatalf("expected portal to render exactly 2 rows, got %d: %#v", len(runs), runs)
	}
	byIssue := map[int]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}
	blocked, ok := byIssue[42]
	if !ok {
		t.Fatalf("expected row for issue 42, got %#v", runs)
	}
	if blocked.Status != "blocked" {
		t.Fatalf("expected issue 42 status 'blocked', got %q", blocked.Status)
	}
	queued, ok := byIssue[43]
	if !ok {
		t.Fatalf("expected row for issue 43, got %#v", runs)
	}
	if queued.Status != "queued" {
		t.Fatalf("expected issue 43 status 'queued', got %q", queued.Status)
	}
}

func TestPortalRunFromActiveBatchIssue_AbortedRunHasAbortedByOperatorLog(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-5 * time.Minute)
	finishedAt := startedAt.Add(1 * time.Minute)
	active := portalActiveRun{
		Key:         "run-active",
		SocketPath:  filepath.Join(repoRoot, ".sandman", "runs", "active-1", "run.sock"),
		IssueNumber: 42,
		StartedAt:   startedAt,
		ModTime:     startedAt,
	}
	state := &events.RunState{
		RunID:   "run-42",
		Started: events.Event{Type: "run.started", Timestamp: startedAt, RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		Finished: &events.Event{
			Type:      "run.aborted",
			Timestamp: finishedAt,
			RunID:     "run-42",
			Issue:     42,
			Payload:   map[string]any{"status": "aborted", "branch": "sandman/42-fix"},
		},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, state, nil, nil, "", nil)

	if run.Status != "aborted" {
		t.Fatalf("expected status 'aborted', got %q", run.Status)
	}
	if run.Log != "Aborted by operator." {
		t.Fatalf("expected log 'Aborted by operator.', got %q", run.Log)
	}
}

func TestDedupPortalRunGroup_AbortedWinsOverActiveBlockedQueued(t *testing.T) {
	base := time.Now().Add(-10 * time.Minute)
	group := []portalRun{
		{Key: "active-row", Kind: "active", Status: "running", IssueNumber: 42, StartedAt: base.Add(1 * time.Minute)},
		{Key: "blocked-row", Kind: "completed", Status: "blocked", IssueNumber: 42, StartedAt: base.Add(2 * time.Minute)},
		{Key: "queued-row", Kind: "completed", Status: "queued", IssueNumber: 42, StartedAt: base.Add(3 * time.Minute)},
		{Key: "aborted-row", Kind: "completed", Status: "aborted", IssueNumber: 42, StartedAt: base},
	}

	result := (&portalRunsView{}).dedupRunGroup(group)

	if len(result) != 1 {
		t.Fatalf("expected aborted to win and return 1 row, got %d: %#v", len(result), result)
	}
	if result[0].Key != "aborted-row" {
		t.Fatalf("expected aborted-row to win, got key=%q status=%q", result[0].Key, result[0].Status)
	}
	if result[0].Status != "aborted" {
		t.Fatalf("expected aborted status, got %q", result[0].Status)
	}
}

func TestDedupPortalRunGroup_TieBrokenByLatestStartedAt(t *testing.T) {
	base := time.Now().Add(-10 * time.Minute)
	group := []portalRun{
		{Key: "aborted-earlier", Kind: "completed", Status: "aborted", IssueNumber: 42, StartedAt: base},
		{Key: "aborted-later", Kind: "completed", Status: "aborted", IssueNumber: 42, StartedAt: base.Add(5 * time.Minute)},
	}

	result := (&portalRunsView{}).dedupRunGroup(group)

	if len(result) != 1 {
		t.Fatalf("expected single row, got %d: %#v", len(result), result)
	}
	if result[0].Key != "aborted-later" {
		t.Fatalf("expected tie-break to pick latest StartedAt (aborted-later), got %q", result[0].Key)
	}
}

func TestDedupPortalRunGroup_AllZeroPriorityRowsAreUntouched(t *testing.T) {
	base := time.Now().Add(-10 * time.Minute)
	group := []portalRun{
		{Key: "success-row", Kind: "completed", Status: "success", IssueNumber: 42, StartedAt: base},
		{Key: "failure-row", Kind: "completed", Status: "failure", IssueNumber: 42, StartedAt: base.Add(1 * time.Minute)},
	}

	result := (&portalRunsView{}).dedupRunGroup(group)

	if len(result) != 2 {
		t.Fatalf("expected succeeded and failure rows to be untouched (2 rows), got %d: %#v", len(result), result)
	}
}

// TestPortal_ActiveRowSurvivesOlderAbortedAtNearSameTime locks in that an
// older aborted run whose Started.Timestamp falls inside the active batch's
// 1-second tolerance window does not steal the active batch's row. The active
// queued row must remain visible alongside the historical aborted row.
func TestPortal_ActiveRowSurvivesOlderAbortedAtNearSameTime(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	newBatchStart := time.Now().Add(-5 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-42-new")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: newBatchStart}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Older run whose Started.Timestamp is within the 1-second tolerance
	// window before newBatchStart. Without a fix, latestPortalRunStateForIssue
	// matches it and the active batch row becomes the historical aborted row.
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: newBatchStart.Add(-500 * time.Millisecond), RunID: "older-run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-old"}},
		{Type: "run.aborted", Timestamp: newBatchStart.Add(-300 * time.Millisecond), RunID: "older-run-42", Issue: 42, Payload: map[string]any{"status": "aborted", "branch": "sandman/42-old"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	var activeRow, abortedRow *portalRun
	for i := range runs {
		if runs[i].IssueNumber != 42 {
			continue
		}
		switch {
		case runs[i].Kind == "active":
			activeRow = &runs[i]
		case runs[i].Status == "aborted":
			abortedRow = &runs[i]
		}
	}
	if activeRow == nil {
		t.Fatalf("expected active row for issue 42 to remain visible, got runs: %#v", runs)
	}
	if abortedRow == nil {
		t.Fatalf("expected historical aborted row for issue 42 to remain visible, got runs: %#v", runs)
	}
}

// TestPortal_QueuedThenSuccessShowsSuccessAfterBatchEnds locks in that a run
// that emits run.queued (from the orchestrator main goroutine) and then
// run.started+run.finished (with a different RunID generated inside runSingle)
// is rendered as its terminal status after the batch ends, instead of
// lingering as queued.
func TestPortal_QueuedThenSuccessShowsSuccessAfterBatchEnds(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "queued-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.started", Timestamp: batchStartedAt.Add(3 * time.Minute), RunID: "started-run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: batchStartedAt.Add(8 * time.Minute), RunID: "started-run-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	var issue42Runs []portalRun
	for _, run := range runs {
		if run.IssueNumber == 42 {
			issue42Runs = append(issue42Runs, run)
		}
	}
	if len(issue42Runs) != 1 {
		t.Fatalf("expected 1 run for issue 42 after dedup, got %d: %#v", len(issue42Runs), issue42Runs)
	}
	if issue42Runs[0].Status != "success" {
		t.Fatalf("expected status 'success' (queued event should not linger), got %q", issue42Runs[0].Status)
	}
}

// TestPortal_QueuedAndBlockedAgentRunDedupsToBlocked locks in criterion 5:
// when an AgentRun emits run.queued (runID_A, from the orchestrator main
// goroutine) and run.blocked (runID_B, from runSingle's external blocker
// recheck) for the same issue, the portal must render exactly one row with
// the terminal blocked status, even when an unrelated active batch is running
// concurrently for a different issue. Before BatchKey, the historical rows
// for issue 42 were kept as-is in dedup because the active batch belonged to
// a different issue.
func TestPortal_QueuedAndBlockedAgentRunDedupsToBlocked(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-portal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	otherBatchStart := time.Now().Add(-1 * time.Minute)
	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-99-other")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{99}, CreatedAt: otherBatchStart}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	historicalStart := time.Now().Add(-30 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: historicalStart.Add(1 * time.Minute), RunID: "queued-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{77}}},
		{Type: "run.blocked", Timestamp: historicalStart.Add(2 * time.Minute), RunID: "blocked-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{77}}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	var issue42Runs []portalRun
	for _, run := range runs {
		if run.IssueNumber == 42 {
			issue42Runs = append(issue42Runs, run)
		}
	}
	if len(issue42Runs) != 1 {
		t.Fatalf("expected exactly 1 row for issue 42 after dedup, got %d: %#v", len(issue42Runs), issue42Runs)
	}
	if issue42Runs[0].Status != "blocked" {
		t.Fatalf("expected status 'blocked' (terminal wins over queued), got %q", issue42Runs[0].Status)
	}
}

// TestPortal_CurrentActiveSurvivesOlderAbortedFromAnotherBatch locks in criterion 6:
// when a current active batch is running for issue 42 and a historical aborted
// run for the same issue exists from a prior (long-finished) batch, the portal
// must keep both rows visible. The active row must not be deduped away by the
// older aborted row that lives in a different batch.
func TestPortal_CurrentActiveSurvivesOlderAbortedFromAnotherBatch(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-portal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	olderBatchStart := time.Now().Add(-2 * time.Hour)
	newBatchStart := time.Now().Add(-5 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-42-current")
	activeSock := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: newBatchStart}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: olderBatchStart, RunID: "older-run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-old"}},
		{Type: "run.aborted", Timestamp: olderBatchStart.Add(2 * time.Minute), RunID: "older-run-42", Issue: 42, Payload: map[string]any{"status": "aborted", "branch": "sandman/42-old"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	var activeRow, abortedRow *portalRun
	for i := range runs {
		if runs[i].IssueNumber != 42 {
			continue
		}
		switch {
		case runs[i].Kind == "active":
			activeRow = &runs[i]
		case runs[i].Status == "aborted":
			abortedRow = &runs[i]
		}
	}
	if activeRow == nil {
		t.Fatalf("expected current active row for issue 42 to remain visible, got runs: %#v", runs)
	}
	if abortedRow == nil {
		t.Fatalf("expected older aborted row for issue 42 to remain visible alongside active, got runs: %#v", runs)
	}
	if activeRow.BatchKey == "" {
		t.Fatalf("expected active row to carry BatchKey from active runDir, got %q", activeRow.BatchKey)
	}
	if abortedRow.BatchKey != "" {
		t.Fatalf("expected historical aborted row to carry empty BatchKey, got %q", abortedRow.BatchKey)
	}
}

// TestPortal_GenuinelyQueuedRunStaysQueued locks in criterion 7: a run that
// is genuinely waiting to start (only run.queued, no terminal event yet)
// must continue to render as queued even after the dedup-queued-when-non-queued
// filter is in place.
func TestPortal_GenuinelyQueuedRunStaysQueued(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "queued-only-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	var issue42Runs []portalRun
	for _, run := range runs {
		if run.IssueNumber == 42 {
			issue42Runs = append(issue42Runs, run)
		}
	}
	if len(issue42Runs) != 1 {
		t.Fatalf("expected 1 row for issue 42, got %d: %#v", len(issue42Runs), issue42Runs)
	}
	if issue42Runs[0].Status != "queued" {
		t.Fatalf("expected genuinely-queued run to stay queued, got %q", issue42Runs[0].Status)
	}
}

// TestPortal_QueuedThenFailureShowsFailureAfterBatchEnds parallels
// QueuedThenSuccess for the failure terminal status. Both success and failure
// are priority-0 in portalRunPriority, so before the queued-filter both would
// have lost to queued (priority 1).
func TestPortal_QueuedThenFailureShowsFailureAfterBatchEnds(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "queued-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.started", Timestamp: batchStartedAt.Add(3 * time.Minute), RunID: "started-run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: batchStartedAt.Add(8 * time.Minute), RunID: "started-run-42", Issue: 42, Payload: map[string]any{"status": "failure", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	var issue42Runs []portalRun
	for _, run := range runs {
		if run.IssueNumber == 42 {
			issue42Runs = append(issue42Runs, run)
		}
	}
	if len(issue42Runs) != 1 {
		t.Fatalf("expected 1 run for issue 42 after dedup, got %d: %#v", len(issue42Runs), issue42Runs)
	}
	if issue42Runs[0].Status != "failure" {
		t.Fatalf("expected status 'failure' (queued event should not linger), got %q", issue42Runs[0].Status)
	}
}

// TestPortal_BlockedThenSuccessShowsSuccessAfterBatchEnds locks in that a
// blocked placeholder for an issue (different RunID from the eventual
// run.started) must not linger as "blocked" once the issue's actual work has
// completed. The blocked event describes the wait state; the later started+
// finished events describe the real work. Production scenario: issue was
// queued then re-queued as blocked, then picked up by the batch and finished.
func TestPortal_BlockedThenSuccessShowsSuccessAfterBatchEnds(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.blocked", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "blocked-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.started", Timestamp: batchStartedAt.Add(3 * time.Minute), RunID: "started-run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: batchStartedAt.Add(8 * time.Minute), RunID: "started-run-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	var issue42Runs []portalRun
	for _, run := range runs {
		if run.IssueNumber == 42 {
			issue42Runs = append(issue42Runs, run)
		}
	}
	if len(issue42Runs) != 1 {
		t.Fatalf("expected 1 run for issue 42 after dedup, got %d: %#v", len(issue42Runs), issue42Runs)
	}
	if issue42Runs[0].Status != "success" {
		t.Fatalf("expected status 'success' (blocked event should not linger), got %q", issue42Runs[0].Status)
	}
}

// TestPortal_BlockedThenFailureShowsFailureAfterBatchEnds mirrors the success
// variant for failure terminal status.
func TestPortal_BlockedThenFailureShowsFailureAfterBatchEnds(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.blocked", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "blocked-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.started", Timestamp: batchStartedAt.Add(3 * time.Minute), RunID: "started-run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: batchStartedAt.Add(8 * time.Minute), RunID: "started-run-42", Issue: 42, Payload: map[string]any{"status": "failure", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	var issue42Runs []portalRun
	for _, run := range runs {
		if run.IssueNumber == 42 {
			issue42Runs = append(issue42Runs, run)
		}
	}
	if len(issue42Runs) != 1 {
		t.Fatalf("expected 1 run for issue 42 after dedup, got %d: %#v", len(issue42Runs), issue42Runs)
	}
	if issue42Runs[0].Status != "failure" {
		t.Fatalf("expected status 'failure' (blocked event should not linger), got %q", issue42Runs[0].Status)
	}
}

func TestPortal_StaleCleanerRunsOnceOnStartupAndNotOnPoll(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var calls atomic.Int64
	prev := portalStaleCleaner
	t.Cleanup(func() { portalStaleCleaner = prev })
	portalStaleCleaner = func(string) error {
		calls.Add(1)
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		close(finished)
		return nil
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("stale cleaner was not invoked on portal startup")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected stub to be inside its first call, got calls=%d", got)
	}

	close(release)
	<-finished

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 0 {
		t.Fatalf("expected 0 runs in fresh repo, got %d: %#v", len(runs), runs)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected stale cleaner to have been called exactly once after one poll, got %d", got)
	}

	for i := 0; i < 5; i++ {
		_ = readPortalRuns(t, server.URL)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected stale cleaner to remain at 1 call after extra polls, got %d", got)
	}
}

func TestPortal_StaleCleanerErrorDoesNotBlockServing(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	finished := make(chan struct{})
	prev := portalStaleCleaner
	t.Cleanup(func() { portalStaleCleaner = prev })
	portalStaleCleaner = func(string) error {
		<-release
		close(finished)
		return errors.New("boom")
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs")
	if err != nil {
		t.Fatalf("poll /api/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 even when stale cleaner is mid-failure, got %d", resp.StatusCode)
	}
	close(release)
	<-finished

	resp, err = http.Get(server.URL + "/api/runs")
	if err != nil {
		t.Fatalf("poll /api/runs after error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after stale cleaner errored, got %d", resp.StatusCode)
	}
}

func TestPortal_StaleCleanerRecoversDeadBatchBeforeFirstPoll(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	createdAt := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	started := createdAt.Add(2 * time.Minute)
	writeBatchManifest(t, repoRoot, "dead-batch", []int{42}, createdAt)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", RunID: "run-42-dead", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	prev := portalStaleCleaner
	t.Cleanup(func() { portalStaleCleaner = prev })

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	deadline := time.Now().Add(3 * time.Second)
	var runs []portalRun
	for time.Now().Before(deadline) {
		runs = readPortalRuns(t, server.URL)
		if len(runs) == 1 && runs[0].Status == "aborted" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run after cleanup, got %d: %#v", len(runs), runs)
	}
	if runs[0].Status != "aborted" {
		t.Fatalf("expected recovered run status 'aborted', got %q", runs[0].Status)
	}
	if runs[0].IssueNumber != 42 {
		t.Fatalf("expected recovered run for issue 42, got %d", runs[0].IssueNumber)
	}
}

func TestPortal_LoadPortalRunsMarksReviewRows(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-5 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: started, RunID: "run-review-17", Issue: 0, IssueRef: nil, Payload: map[string]any{"branch": "sandman/review-17-100", "review": true, "pr_number": float64(17), "review_focus": "focus on tests"}},
		{Type: "run.finished", Timestamp: started.Add(2 * time.Minute), RunID: "run-review-17", Issue: 0, IssueRef: nil, Payload: map[string]any{"status": "success", "branch": "sandman/review-17-100"}},
		{Type: "run.started", Timestamp: started.Add(1 * time.Minute), RunID: "run-impl-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: started.Add(3 * time.Minute), RunID: "run-impl-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %#v", runs)
	}
	var review, impl *portalRun
	for i := range runs {
		if runs[i].RunID == "run-review-17" {
			review = &runs[i]
		} else if runs[i].RunID == "run-impl-42" {
			impl = &runs[i]
		}
	}
	if review == nil {
		t.Fatal("expected review run in portal output")
	}
	if !review.Review {
		t.Errorf("expected review run to have Review=true, got %#v", review)
	}
	if review.PRNumber != 17 {
		t.Errorf("expected review run PRNumber=17, got %d", review.PRNumber)
	}
	if impl == nil {
		t.Fatal("expected implementation run in portal output")
	}
	if impl.Review {
		t.Errorf("expected implementation run to have Review=false, got %#v", impl)
	}
	if impl.PRNumber != 0 {
		t.Errorf("expected implementation run PRNumber=0, got %d", impl.PRNumber)
	}
}

func TestPortal_LoadPortalRunsReviewKindStaysActiveOrCompleted(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-5 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: started, RunID: "run-review-9", Issue: 0, IssueRef: nil, Payload: map[string]any{"branch": "sandman/review-9-1", "review": true, "pr_number": float64(9), "review_focus": ""}},
		{Type: "run.finished", Timestamp: started.Add(2 * time.Minute), RunID: "run-review-9", Issue: 0, IssueRef: nil, Payload: map[string]any{"status": "success", "branch": "sandman/review-9-1"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].Kind != "active" && runs[0].Kind != "completed" {
		t.Errorf("Kind must remain in {active, completed}; got %q", runs[0].Kind)
	}
}

func TestPortal_LoadPortalRunsKeepsParallelReviewRunsAndLogs(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	started := time.Now().Add(-5 * time.Minute)
	issueStarted := started.Add(-2 * time.Minute)
	branch17 := "sandman/review-17-fixed"
	branch18 := "sandman/review-18-fixed"
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: issueStarted, RunID: "issue-1177", Issue: 1177, Payload: map[string]any{"branch": "sandman/1177-fix"}},
		{Type: "run.finished", Timestamp: issueStarted.Add(30 * time.Second), RunID: "issue-1177", Issue: 1177, Payload: map[string]any{"status": "success", "branch": "sandman/1177-fix"}},
		{Type: "run.started", Timestamp: started, RunID: "review-17-a", Issue: 0, Payload: map[string]any{"branch": branch17, "review": true, "pr_number": 17}},
		{Type: "run.finished", Timestamp: started.Add(1 * time.Minute), RunID: "review-17-a", Issue: 0, Payload: map[string]any{"status": "success", "branch": branch17, "review": true}},
		{Type: "run.started", Timestamp: started.Add(2 * time.Minute), RunID: "review-17-b", Issue: 0, Payload: map[string]any{"branch": branch18, "review": true, "pr_number": 17}},
		{Type: "run.finished", Timestamp: started.Add(3 * time.Minute), RunID: "review-17-b", Issue: 0, Payload: map[string]any{"status": "success", "branch": branch18, "review": true}},
	})

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	view := &portalRunsView{}
	log17 := view.portalLogPathForRun(repoRoot, 0, branch17, "review-17-a", true, 17)
	log18 := view.portalLogPathForRun(repoRoot, 0, branch18, "review-17-b", true, 17)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "1177.log"), []byte("main run log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log17, []byte("review 17 log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log18, []byte("review 18 log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runs, err := view.compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected issue row plus 2 review runs, got %d: %#v", len(runs), runs)
	}
	byRunID := map[string]portalRun{}
	for _, run := range runs {
		byRunID[run.RunID] = run
	}
	if run, ok := byRunID["issue-1177"]; !ok {
		t.Fatalf("expected issue run 1177 to remain visible, got %#v", runs)
	} else if !strings.Contains(run.Log, "main run log") {
		t.Fatalf("expected issue run to keep 1177.log, got %#v", run)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".sandman", "logs", "PR17.log")); !os.IsNotExist(err) {
		t.Fatalf("expected shared PR17.log not to exist")
	}
	for _, runID := range []string{"review-17-a", "review-17-b"} {
		run, ok := byRunID[runID]
		if !ok {
			t.Fatalf("expected review run %s to remain visible, got %#v", runID, runs)
		}
		if !run.Review {
			t.Fatalf("expected %s to remain a review run, got %#v", runID, run)
		}
		wantLog := "review 17 log"
		if runID == "review-17-b" {
			wantLog = "review 18 log"
		}
		if !strings.Contains(run.Log, wantLog) {
			t.Fatalf("expected %s to keep %q, got %#v", runID, wantLog, run)
		}
		if strings.Contains(run.Log, "wrong review log") {
			t.Fatalf("expected %s not to load shared PR log, got %#v", runID, run)
		}
	}
}

func TestPortal_ReviewRunLogPathPrefersBranchLogWhenFinishedEventHasIssue(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	started := time.Now().Add(-5 * time.Minute)
	issueStarted := started.Add(-2 * time.Minute)
	branch := "sandman/review-1177-fixed"
	implBranch := "sandman/1177-fix"
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: issueStarted, RunID: "issue-1177", Issue: 1177, Payload: map[string]any{"branch": implBranch}},
		{Type: "run.finished", Timestamp: issueStarted.Add(30 * time.Second), RunID: "issue-1177", Issue: 1177, Payload: map[string]any{"status": "success", "branch": implBranch}},
		{Type: "run.started", Timestamp: started, RunID: "review-1177-1", Issue: 0, IssueRef: nil, Payload: map[string]any{"branch": branch, "review": true, "pr_number": float64(1187), "issue_number": float64(1177)}},
		{Type: "run.finished", Timestamp: started.Add(1 * time.Minute), RunID: "review-1177-1", Issue: 1177, Payload: map[string]any{"status": "success", "branch": branch, "review": true, "pr_number": float64(1187), "issue_number": float64(1177)}},
	})

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	view := &portalRunsView{}
	reviewLogPath := view.portalLogPathForRun(repoRoot, 1177, branch, "review-1177-1", true, 1187)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "1177.log"), []byte("main run log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewLogPath, []byte("review log for issue 1177\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runs, err := view.compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	var review *portalRun
	for i := range runs {
		if runs[i].RunID == "review-1177-1" {
			review = &runs[i]
			break
		}
	}
	if review == nil {
		t.Fatalf("expected review run review-1177-1 in portal output, got %#v", runs)
	}
	if !review.Review {
		t.Fatalf("expected review flag set, got %#v", review)
	}
	if review.PRNumber != 1187 {
		t.Fatalf("expected review PRNumber=1187, got %d", review.PRNumber)
	}
	if review.LogPath == "" || strings.HasSuffix(review.LogPath, "1177.log") {
		t.Fatalf("expected review run to use branch-based log path, got %q", review.LogPath)
	}
	if !strings.Contains(review.Log, "review log for issue 1177") {
		t.Fatalf("expected review run to load branch log content, got %#v", review)
	}
}

func TestPortal_RunsTableHasColgroupAndFixedLayout(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)

	if !strings.Contains(content, "<colgroup>") {
		t.Fatalf("page missing <colgroup> element in runs table")
	}

	colIDs := []string{"col-title", "col-badge", "col-started", "col-duration", "col-issue-title", "col-actions"}
	for _, id := range colIDs {
		if !strings.Contains(content, `<col class="`+id+`">`) {
			t.Fatalf("page missing <col class=%q> element in colgroup", id)
		}
	}

	if !strings.Contains(content, "table-layout: fixed") {
		t.Fatalf("page missing table-layout: fixed on runs <table>")
	}
}
