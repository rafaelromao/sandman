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
	"net/http/httptest"
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

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/testenv"
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

	batch1Path := filepath.Join(repoRoot, ".sandman", "batches", "run-1")
	createUnixRunSocket(t, filepath.Join(batch1Path, "batch.sock"))
	addBatchToIndex(t, repoRoot, "run-1", batch1Path, []int{1})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	first := readPortalInstances(t, server.URL)
	if len(first) != 1 || first[0].Name != "run-1" {
		t.Fatalf("expected 1 run-1 instance, got %#v", first)
	}

	batch2Path := filepath.Join(repoRoot, ".sandman", "batches", "run-2")
	createUnixRunSocket(t, filepath.Join(batch2Path, "batch.sock"))
	addBatchToIndex(t, repoRoot, "run-2", batch2Path, []int{2})

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
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "batches", "run-file"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches", "run-file", "batch.sock"), []byte("not a socket"), 0644); err != nil {
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

	// A finished batch leaves a batch.sock inode with the socket bit set on
	// disk, but the process that owned it is gone. The portal must not
	// treat this as an active instance. The listener below exists only
	// so the socket file persists on disk with the socket bit set; the
	// liveness probe is stubbed to false so the listener's actual
	// dialability is irrelevant.
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "run-stale-1")
	sockPath := filepath.Join(runDir, "batch.sock")
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

	// Seed a finished batch: a batch.sock inode, a batch.json listing
	// two issues, and no run.started events for either issue. The
	// listener exists only so the socket file persists on disk with
	// the socket bit set; the liveness probe is stubbed to false so
	// the listener's actual dialability is irrelevant.
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "run-42-1")
	sockPath := filepath.Join(runDir, "batch.sock")
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

func TestPortal_RunsAPI_SynthesizesOnlyMissingDeadBatchMembers(t *testing.T) {
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
		{Type: "run.started", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "batch_id": "dead-1"}},
		{Type: "run.finished", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
	})

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	runs := readPortalRuns(t, server.URL)
	// Event-backed issue 1 gets BatchKey from batch_id and dedup strips
	// the synthetic row, leaving exactly one row per issue.
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs (1 event-backed + 2 synthesized), got %d: %#v", len(runs), runs)
	}

	byIssue := map[int][]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = append(byIssue[run.IssueNumber], run)
	}
	if got := len(byIssue[1]); got != 1 {
		t.Fatalf("expected 1 row for issue 1 (event-backed), got %d: %#v", got, byIssue[1])
	}
	run := byIssue[1][0]
	if run.Kind != "completed" || run.Status != "success" || run.BatchKey != "dead-1" {
		t.Fatalf("expected issue 1 to stay completed success with batch key dead-1, got %#v", run)
	}
	for _, issue := range []int{2, 3} {
		if got := len(byIssue[issue]); got != 1 {
			t.Fatalf("expected exactly 1 synthesized row for issue %d, got %d: %#v", issue, got, byIssue[issue])
		}
		run := byIssue[issue][0]
		if run.Kind != "completed" || run.Status != "aborted" || run.BatchKey != "dead-1" {
			t.Fatalf("expected issue %d to synthesize as dead-batch completed aborted row, got %#v", issue, run)
		}
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
		runDir := filepath.Join(repoRoot, ".sandman", "batches", runID)
		if err := os.MkdirAll(runDir, 0755); err != nil {
			t.Fatal(err)
		}
		sockPath := filepath.Join(runDir, "batch.sock")
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
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, ".sandman", "batches", "PR43")
	sockPath := filepath.Join(sockDir, "batch.sock")
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
	addBatchToIndex(t, repoRoot, "PR43", sockDir, []int{})

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
	if reviewRun.IssueLabel != "Review of PR 42" {
		// Orphan review run (no resolved issue): the main label
		// uses the "Review of PR <n>" convention (issue #1667,
		// ADR-0029 §Review-only orphan label).
		t.Fatalf("expected review run label 'Review of PR 42', got %q", reviewRun.IssueLabel)
	}
	if !reviewRun.Review {
		t.Fatal("expected review flag on completed review run")
	}

	activeReview, ok := byRunID["PR43"]
	if !ok {
		t.Fatal("expected active review run with RunID PR43")
	}
	if activeReview.IssueLabel != "Review of PR 43" {
		t.Fatalf("expected active review run label 'Review of PR 43', got %q", activeReview.IssueLabel)
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
		{Type: "run.started", Timestamp: started.Add(4 * time.Minute), RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: started.Add(5 * time.Minute), RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix", "status": "success"}},
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

	issue, ok := byRunID["run-42-1"]
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
	// Queued runs render as completed/queued (see kindForRun, issue #1699):
	// wait-state rows no longer borrow the active-row chrome even though
	// the daemon has not picked them up yet. Status still tracks the
	// wait state; only the Kind flipped from "active" to "completed".
	if run := byIssue[2]; run.Kind != "completed" || run.Status != "queued" {
		t.Fatalf("expected completed queued run for issue 2, got kind=%q status=%q", run.Kind, run.Status)
	}
	if run := byIssue[3]; run.Kind != "completed" || run.Status != "queued" {
		t.Fatalf("expected completed queued run for issue 3, got kind=%q status=%q", run.Kind, run.Status)
	}
}

func TestPortal_AbortRunEndpointAbortsActiveRunAndRefreshesStatus(t *testing.T) {
	repoRoot := testenv.MkdirShort(t, "sm-portal-")
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "run-42-1")
	activeSock := filepath.Join(runDir, "batch.sock")
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
	repoRoot := testenv.MkdirShort(t, "sm-stop-")
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "run-42-1")
	runID := "run-42-1"
	runFolder := filepath.Join(batchDir, "runs", runID)
	activeSock := filepath.Join(batchDir, "batch.sock")
	abortSock := filepath.Join(runFolder, "run.sock")
	if err := os.MkdirAll(runFolder, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	runManifest := batchindex.RunManifest{Issue: 42}
	runManifestData, _ := json.Marshal(runManifest)
	if err := os.WriteFile(filepath.Join(runFolder, "run.json"), runManifestData, 0644); err != nil {
		t.Fatal(err)
	}
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: "run-42-1", Path: batchDir, Kind: "batch", Status: "active", CreatedAt: startedAt, Issues: []int{42}},
	}}
	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := os.MkdirAll(filepath.Dir(idxPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(idxPath); err != nil {
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
		if sockPath != abortSock {
			t.Fatalf("expected socket %q, got %q", abortSock, sockPath)
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
		repoRoot := testenv.MkdirShort(t, "sm-abort-")
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		batchDir := filepath.Join(repoRoot, ".sandman", "batches", "run-42-1")
		runID := "run-42-1"
		runFolder := filepath.Join(batchDir, "runs", runID)
		if err := os.MkdirAll(runFolder, 0755); err != nil {
			t.Fatal(err)
		}
		activeSock := filepath.Join(batchDir, "batch.sock")
		cmdSock := filepath.Join(runFolder, "run.sock")
		if err := os.WriteFile(cmdSock, []byte("offline"), 0644); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", activeSock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })

		runManifest := batchindex.RunManifest{Issue: 42}
		runManifestData, _ := json.Marshal(runManifest)
		if err := os.WriteFile(filepath.Join(runFolder, "run.json"), runManifestData, 0644); err != nil {
			t.Fatal(err)
		}

		idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
			{ID: runID, Path: batchDir, Kind: "batch", Status: "active", Issues: []int{42}},
		}}
		idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
		if err := idx.Save(idxPath); err != nil {
			t.Fatal(err)
		}

		writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
			{Type: "run.started", Timestamp: time.Now(), RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		})

		err = abortPortalRun(context.Background(), repoRoot, runID, 42)
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
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "run-42-1")
	activeSock := filepath.Join(runDir, "batch.sock")
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

func TestAbortPortalRun_QueuedRunEmitsRunAborted(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-abort-queued-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "run-42-1")
	batchKey := "run-42-1"
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}

	eventsPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, eventsPath, []events.Event{
		{Type: "run.queued", Timestamp: startedAt, RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix", "blocked_by": []any{41}}},
	})

	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchKey, Path: batchDir, Kind: "batch", Status: "active", CreatedAt: startedAt, Issues: []int{42}},
	}}
	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := os.MkdirAll(filepath.Dir(idxPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	err = abortPortalRun(context.Background(), repoRoot, batchKey, 42)
	if err != nil {
		t.Fatalf("abort queued run: %v", err)
	}

	logged, err := (&events.JSONLLogger{Path: eventsPath}).Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var aborted events.Event
	for _, e := range logged {
		if e.Type == "run.aborted" && e.Issue == 42 {
			aborted = e
			break
		}
	}
	if aborted.Type == "" {
		t.Fatalf("expected run.aborted event for issue 42, got events: %+v", logged)
	}
	if aborted.RunID != "run-42-1" {
		t.Fatalf("expected RunID=run-42-1, got %q", aborted.RunID)
	}
	status, ok := aborted.Payload["status"]
	if !ok {
		t.Fatal("expected status field in run.aborted payload")
	}
	if status != "aborted" {
		t.Fatalf("expected status=aborted, got %v", status)
	}
}

func TestAbortPortalRun_BlockedRunEmitsRunAborted(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-abort-blocked-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "run-42-1")
	batchKey := "run-42-1"
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}

	eventsPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, eventsPath, []events.Event{
		{Type: "run.blocked", Timestamp: startedAt, RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix", "blocked_by": []any{41}}},
	})

	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchKey, Path: batchDir, Kind: "batch", Status: "active", CreatedAt: startedAt, Issues: []int{42}},
	}}
	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := os.MkdirAll(filepath.Dir(idxPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	err = abortPortalRun(context.Background(), repoRoot, batchKey, 42)
	if err != nil {
		t.Fatalf("abort blocked run: %v", err)
	}

	logged, err := (&events.JSONLLogger{Path: eventsPath}).Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var aborted events.Event
	for _, e := range logged {
		if e.Type == "run.aborted" && e.Issue == 42 {
			aborted = e
			break
		}
	}
	if aborted.Type == "" {
		t.Fatalf("expected run.aborted event for issue 42, got events: %+v", logged)
	}
	if aborted.RunID != "run-42-1" {
		t.Fatalf("expected RunID=run-42-1, got %q", aborted.RunID)
	}
	status, ok := aborted.Payload["status"]
	if !ok {
		t.Fatal("expected status field in run.aborted payload")
	}
	if status != "aborted" {
		t.Fatalf("expected status=aborted, got %v", status)
	}
}

func TestPortal_QueuedOnlyRowHasActiveKindSoAbortRenders(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	batchKey := "b1"
	runDir := filepath.Join(repoRoot, ".sandman", "batches", batchKey)
	activeSock := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: batchStartedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, batchKey, runDir, []int{42})
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.queued", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "queued-run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix", "blocked_by": []int{99}}},
	})

	prev := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = prev })
	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	var queuedRow *portalRun
	for i := range runs {
		if runs[i].IssueNumber == 42 && runs[i].Status == "queued" {
			run := runs[i]
			queuedRow = &run
			break
		}
	}
	if queuedRow == nil {
		t.Fatalf("expected a queued row for issue 42, got runs: %#v", runs)
	}
	if queuedRow.Kind != "active" {
		t.Fatalf("expected queued row to have Kind='active' so Abort button renders, got Kind=%q", queuedRow.Kind)
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
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "run-2")
	if err := os.MkdirAll(filepath.Join(batchDir, "runs", "run-2"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "runs", "run-2", "run.log"), []byte("continued run log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))
	addBatchToIndex(t, repoRoot, "run-2", batchDir, []int{1})

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

func TestPortal_PageEmbedsTerminalGrammarInline(t *testing.T) {
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
	for _, want := range []string{"Prism.highlight", "Prism.languages['sandman-log']"} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing inline terminal grammar marker %q\n%s", want, content[:min(1200, len(content))])
		}
	}
	for _, forbidden := range []string{"cdn.jsdelivr", "prismjs", "unpkg.com"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("page must not reference external Prism assets: %q\n%s", forbidden, content[:min(1200, len(content))])
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
	start := strings.Index(content, `@media (max-width: 760px)`)
	if start < 0 {
		t.Fatalf("page missing mobile media query\n%s", content[:min(1000, len(content))])
	}
	block := content[start:]
	if end := strings.Index(block, `@media (prefers-reduced-motion: reduce)`); end >= 0 {
		block = block[:end]
	}
	if strings.Contains(block, "td[data-cell=\"issue-title\"] {\n        display: none;") {
		t.Fatalf("mobile media block still hides issue-title cell; the display: none rule on that selector must be removed\n%s", block[:min(1000, len(block))])
	}
	nineSixtyStart := strings.Index(content, `@media (max-width: 960px)`)
	if nineSixtyStart < 0 {
		t.Fatalf("page missing 960px media query\n%s", content[:min(1000, len(content))])
	}
	nineSixtyBlock := content[nineSixtyStart:]
	if end := strings.Index(nineSixtyBlock, `@media (max-width: 760px)`); end >= 0 {
		nineSixtyBlock = nineSixtyBlock[:end]
	}
	for _, want := range []string{
		"tbody tr.run-row td[data-cell=\"issue-title\"] {",
		"align-self: center;",
	} {
		if !strings.Contains(nineSixtyBlock, want) {
			t.Fatalf("960px media block missing %q on issue-title cell rule\n%s", want, nineSixtyBlock[:min(1000, len(nineSixtyBlock))])
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
	runLogDir := filepath.Join(repoRoot, ".sandman", "batches", "1", "runs", "1")
	if err := os.MkdirAll(runLogDir, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(runLogDir, "run.log")
	if err := os.WriteFile(logPath, []byte("full log\nline two\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	href := "/api/logs?path=" + url.QueryEscape(filepath.Join(".sandman", "batches", "1", "runs", "1", "run.log"))
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

func TestPortal_LogsPathPermittedEnforcement(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "batches", "batch-1", "runs", "run-1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches", "batch-1", "runs", "run-1", "run.log"), []byte("archived log content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "archive", "archived-batch-1", "runs", "archived-run-1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "archive", "archived-batch-1", "runs", "archived-run-1", "run.log"), []byte("archived log content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "batches", "batch-1", "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches", "batch-1", "config", "hosts.yml"), []byte("secrets\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), []byte("[]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "events.jsonl"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	cases := []struct {
		path       string
		wantStatus int
	}{
		{".sandman/batches/batch-1/runs/run-1/run.log", http.StatusOK},
		{".sandman/archive/archived-batch-1/runs/archived-run-1/run.log", http.StatusOK},
		{".sandman/batches.json", http.StatusBadRequest},
		{".sandman/events.jsonl", http.StatusBadRequest},
		{".sandman/batches/batch-1/config/hosts.yml", http.StatusBadRequest},
	}

	for _, tc := range cases {
		href := "/api/logs?path=" + url.QueryEscape(tc.path)
		resp, err := http.Get(server.URL + href)
		if err != nil {
			t.Fatalf("request for %q: %v", tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.wantStatus {
			t.Errorf("path %q: got %d, want %d", tc.path, resp.StatusCode, tc.wantStatus)
		}
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

func addBatchToIndex(t *testing.T, repoRoot, batchID, batchPath string, issues []int) {
	t.Helper()
	layout := paths.NewLayout(nil, repoRoot)
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	idx.AddBatch(batchindex.Batch{
		ID:        batchID,
		Path:      batchPath,
		Kind:      batchindex.KindIssue,
		Issues:    issues,
		CreatedAt: time.Now(),
	})
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
}

func addArchivedBatchToIndex(t *testing.T, repoRoot, batchID, archivePath string, issues []int) {
	t.Helper()
	layout := paths.NewLayout(nil, repoRoot)
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	idx.AddBatch(batchindex.Batch{
		ID:        batchID,
		Path:      archivePath,
		Kind:      batchindex.KindIssue,
		Issues:    issues,
		CreatedAt: time.Now(),
	})
	if err := idx.SetArchived(batchID, archivePath, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
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
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "active-1")
	activeSock := filepath.Join(runDir, "batch.sock")
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
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "active-1")
	activeSock := filepath.Join(runDir, "batch.sock")
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
	addBatchToIndex(t, repoRoot, "active-1", runDir, []int{42})

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
		// The live-batch row now lands at kind="active" via the
		// runFromActiveBatchIssue constructor (a queued member of an
		// active batch whose daemon is alive — SocketPath is
		// populated; the row IS still being processed). The historical
		// orphan signal here is the empty BatchKey distinguishing it
		// from the active batch's row.
		//
		// kindForRun's demotion (issue #1699) only applies to the
		// event-fold projection path (runFromState); the
		// live-instance constructor keeps Kind="active" because the
		// daemon is genuinely still attached.
		case run.Kind == "active" && run.Status == "queued":
			sawActiveQueued = true
		// Historical blocked rows render as kind="completed" after
		// issue #1699: kindForRun no longer borrows active-row chrome
		// for wait states. The BatchKey="" distinguishes the orphan
		// historical row from the active batch's row.
		case run.Kind == "completed" && run.Status == "blocked" && run.BatchKey == "":
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

// TestPortal_Compute_CrossBatchRowsLockIn locks in the #1542
// acceptance criterion 3 contract: rows for the same issue rendered
// from two distinct batches (one historical via the event log, one
// active via a live batch directory) must survive dedupRuns as two
// separate portal rows with distinct BatchKey values.
func TestPortal_Compute_CrossBatchRowsLockIn(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	historicalStartedAt := time.Now().Add(-2 * time.Hour)
	historicalTerminalAt := historicalStartedAt.Add(5 * time.Minute)
	activeStartedAt := time.Now().Add(-10 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "active-1")
	activeSock := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: activeStartedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addBatchToIndex(t, repoRoot, "active-1", runDir, []int{42})

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: historicalStartedAt, RunID: "historical-1-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-historical", "batch_id": "historical-1"}},
		{Type: "run.finished", Timestamp: historicalTerminalAt, RunID: "historical-1-42", Issue: 42, Payload: map[string]any{"status": "aborted", "branch": "sandman/42-historical", "batch_id": "historical-1"}},
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
		t.Fatalf("expected 2 rows for issue 42 (one historical, one active), got %d: %#v", len(issueRuns), issueRuns)
	}

	var sawHistorical, sawActive bool
	for _, run := range issueRuns {
		switch run.BatchKey {
		case "historical-1":
			sawHistorical = true
		case "active-1":
			sawActive = true
		default:
			t.Fatalf("unexpected BatchKey for issue 42: %q (run: %#v)", run.BatchKey, run)
		}
	}
	if !sawHistorical {
		t.Fatal("expected historical row with BatchKey \"historical-1\" for issue 42")
	}
	if !sawActive {
		t.Fatal("expected active row with BatchKey \"active-1\" for issue 42")
	}
}

func TestPortal_Compute_LiveBatchTerminalRowsAreCompleted(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		status    string
	}{
		{name: "success", eventType: "run.finished", status: "success"},
		{name: "failure", eventType: "run.finished", status: "failure"},
		{name: "aborted", eventType: "run.aborted", status: "aborted"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot, err := os.MkdirTemp("/tmp", "p")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
			if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
				t.Fatal(err)
			}

			startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
			terminalAt := startedAt.Add(2 * time.Minute)
			runDir := filepath.Join(repoRoot, ".sandman", "batches", "live-1")
			activeSock := filepath.Join(runDir, "batch.sock")
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
			addBatchToIndex(t, repoRoot, "live-1", runDir, []int{42})

			writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
				{Type: "run.started", Timestamp: startedAt, RunID: "live-1-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
				{Type: tc.eventType, Timestamp: terminalAt, RunID: "live-1-42", Issue: 42, Payload: map[string]any{"status": tc.status, "branch": "sandman/42-fix"}},
			})

			prev := portalStaleCleaner
			portalStaleCleaner = func(string) error { return nil }
			t.Cleanup(func() { portalStaleCleaner = prev })

			server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
			runs := readPortalRuns(t, server.URL)
			if len(runs) != 1 {
				t.Fatalf("expected 1 run, got %d: %#v", len(runs), runs)
			}
			if runs[0].Kind != "completed" {
				t.Fatalf("expected completed kind for live batch %s row, got %q", tc.name, runs[0].Kind)
			}
			if runs[0].Status != tc.status {
				t.Fatalf("expected %s status for live batch %s row, got %q", tc.status, tc.name, runs[0].Status)
			}
		})
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

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "run-380-1")
	activeSock := filepath.Join(runDir, "batch.sock")
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

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "active-1")
	activeSock := filepath.Join(runDir, "batch.sock")
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

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "active-1")
	activeSock := filepath.Join(runDir, "batch.sock")
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
	addBatchToIndex(t, repoRoot, "active-1", runDir, []int{42, 43})

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
		SocketPath:  filepath.Join(repoRoot, ".sandman", "batches", "active-1", "batch.sock"),
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

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, state, nil, nil, "", nil, nil)

	if run.Status != "aborted" {
		t.Fatalf("expected status 'aborted', got %q", run.Status)
	}
	if run.Log != "" {
		t.Fatalf("expected log '', got %q", run.Log)
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

	if len(result) != 2 {
		t.Fatalf("expected queued/blocked stripped and active+aborted to survive (2 rows), got %d: %#v", len(result), result)
	}
	gotKeys := make(map[string]bool, len(result))
	for _, r := range result {
		gotKeys[r.Key] = true
	}
	if !gotKeys["active-row"] {
		t.Fatalf("expected active-row to survive, got keys=%v", gotKeys)
	}
	if !gotKeys["aborted-row"] {
		t.Fatalf("expected aborted-row to survive, got keys=%v", gotKeys)
	}
	if gotKeys["blocked-row"] || gotKeys["queued-row"] {
		t.Fatalf("expected queued/blocked rows to be stripped, got keys=%v", gotKeys)
	}
}

func TestDedupPortalRunGroup_TieBrokenByLatestStartedAt(t *testing.T) {
	base := time.Now().Add(-10 * time.Minute)
	group := []portalRun{
		{Key: "aborted-earlier", Kind: "completed", Status: "aborted", IssueNumber: 42, StartedAt: base},
		{Key: "aborted-later", Kind: "completed", Status: "aborted", IssueNumber: 42, StartedAt: base.Add(5 * time.Minute)},
	}

	result := (&portalRunsView{}).dedupRunGroup(group)

	if len(result) != 2 {
		t.Fatalf("expected both aborted rows to survive (no priority collapse), got %d: %#v", len(result), result)
	}
	gotKeys := make(map[string]bool, len(result))
	for _, r := range result {
		gotKeys[r.Key] = true
	}
	if !gotKeys["aborted-earlier"] || !gotKeys["aborted-later"] {
		t.Fatalf("expected both aborted-earlier and aborted-later to survive, got keys=%v", gotKeys)
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
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	newBatchStart := time.Now().Add(-5 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "run-42-new")
	activeSock := filepath.Join(runDir, "batch.sock")
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
	addBatchToIndex(t, repoRoot, "run-42-new", runDir, []int{42})

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
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "run-99-other")
	activeSock := filepath.Join(runDir, "batch.sock")
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

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "run-42-current")
	activeSock := filepath.Join(runDir, "batch.sock")
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
	addBatchToIndex(t, repoRoot, "run-42-current", runDir, []int{42})

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

// TestPortal_BlockedRunInDeadBatch_KindIsCompleted is the issue #1699
// acceptance end-to-end test. The production symptom was a batch whose
// daemon had died (batch.sock gone) but whose batches.json index
// still reports status="active"; orphan run.blocked events for that
// batch's issues were rendering with kind="active" — same active-row
// tint, row-added highlight, and "Active Batches" filter entry as a
// genuinely running row. The fix demotes kindForRun so wait-state
// rows tag as kind="completed", with status still tracking the wait
// signal. This test wires the full setup — real .sandman/batches.json
// entry, a batch dir with no batch.sock, a run.blocked event — and
// drives portalRunsView.compute to assert the JSON-visible Kind.
func TestPortal_BlockedRunInDeadBatch_KindIsCompleted(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	batchID := "260702214402-b7cf-1640+13"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatalf("mkdir batch dir: %v", err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		Issues:    []int{42},
		BatchId:   batchID,
		CreatedAt: batchStartedAt,
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{42})
	// Deliberately no batch.sock in batchDir — the daemon is gone,
	// which is the production state for issue #1699.

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.blocked", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "blocked-run-42", Issue: 42, Payload: map[string]any{"blocked_by": []int{99}}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var issue42Run *portalRun
	for i := range runs {
		if runs[i].IssueNumber == 42 {
			issue42Run = &runs[i]
			break
		}
	}
	if issue42Run == nil {
		t.Fatalf("expected one row for issue 42, got %#v", runs)
	}
	if issue42Run.Status != "blocked" {
		t.Fatalf("Status = %q, want %q (run.blocked should still surface as status \"blocked\")", issue42Run.Status, "blocked")
	}
	if issue42Run.Kind != "completed" {
		t.Fatalf("Kind = %q, want %q (issue #1699: blocked wait-state rows must be kind=\"completed\" so the active-row chrome does not fire and the \"Active Batches\" filter does not surface them)", issue42Run.Kind, "completed")
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

func TestPortal_RunsSummary(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	layout := paths.NewLayout(nil, repoRoot)
	startedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	runID := "run-42-1234567890"
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	batchID := "run-42"
	runDir := filepath.Join(layout.BatchesDir, batchID, "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte("hello world log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{
				ID:        batchID,
				Path:      filepath.Join(layout.BatchesDir, batchID),
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: startedAt,
				Issues:    []int{42},
			},
		},
	}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs?summary=1")
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
	if len(payload.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(payload.Runs))
	}
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if etag == "" {
		t.Fatal("expected summary response ETag")
	}
	run := payload.Runs[0]
	if run.Log != "" {
		t.Errorf("summary response must omit Log, got %q", run.Log)
	}
	if len(run.Events) == 0 {
		t.Errorf("summary response must include Events for runs that have them, got 0 entries")
	}
	if run.LogURL != "" {
		t.Errorf("summary response must omit LogURL")
	}

	req304, err := http.NewRequest(http.MethodGet, server.URL+"/api/runs?summary=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req304.Header.Set("If-None-Match", etag)
	resp304, err := http.DefaultClient.Do(req304)
	if err != nil {
		t.Fatal(err)
	}
	defer resp304.Body.Close()
	if resp304.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", resp304.StatusCode)
	}
	if got := strings.TrimSpace(resp304.Header.Get("ETag")); got != etag {
		t.Fatalf("expected 304 ETag %q, got %q", etag, got)
	}
	body304, err := io.ReadAll(resp304.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body304) != 0 {
		t.Fatalf("expected empty 304 body, got %q", string(body304))
	}

	writePortalLog(t, layout.EventsLogPath, []events.Event{{Type: "run.note", Timestamp: finishedAt.Add(1 * time.Minute), RunID: runID, Issue: 42, Payload: map[string]any{"note": "summary-noop"}}})
	getPortalRunsIndex(repoRoot).Invalidate()

	reqNoop, err := http.NewRequest(http.MethodGet, server.URL+"/api/runs?summary=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	reqNoop.Header.Set("If-None-Match", etag)
	respNoop, err := http.DefaultClient.Do(reqNoop)
	if err != nil {
		t.Fatal(err)
	}
	defer respNoop.Body.Close()
	if respNoop.StatusCode != http.StatusOK {
		t.Fatalf("expected run.note to surface in summary events, got %d", respNoop.StatusCode)
	}
	if got := strings.TrimSpace(respNoop.Header.Get("ETag")); got == "" || got == etag {
		t.Fatalf("expected fresh ETag after run.note populated events, got %q (was %q)", got, etag)
	}
	var noopPayload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(respNoop.Body).Decode(&noopPayload); err != nil {
		t.Fatalf("decode noop summary: %v", err)
	}
	if len(noopPayload.Runs) != 1 || len(noopPayload.Runs[0].Events) < 2 {
		t.Fatalf("expected summary to carry run.note in events, got runs=%d events[0]=%d", len(noopPayload.Runs), len(noopPayload.Runs[0].Events))
	}

	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
		{Type: "run.started", Timestamp: finishedAt.Add(1 * time.Minute), RunID: "run-99-1234567891", Issue: 99, Payload: map[string]any{"branch": "sandman/99-fix"}},
	})
	getPortalRunsIndex(repoRoot).Invalidate()

	reqChanged, err := http.NewRequest(http.MethodGet, server.URL+"/api/runs?summary=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	reqChanged.Header.Set("If-None-Match", etag)
	respChanged, err := http.DefaultClient.Do(reqChanged)
	if err != nil {
		t.Fatal(err)
	}
	defer respChanged.Body.Close()
	if respChanged.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after change, got %d", respChanged.StatusCode)
	}
	if got := strings.TrimSpace(respChanged.Header.Get("ETag")); got == "" || got == etag {
		t.Fatalf("expected changed summary ETag, got %q", got)
	}
	bodyChanged, err := io.ReadAll(respChanged.Body)
	if err != nil {
		t.Fatal(err)
	}
	var changedPayload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.Unmarshal(bodyChanged, &changedPayload); err != nil {
		t.Fatalf("decode changed summary: %v", err)
	}
	if len(changedPayload.Runs) != 2 {
		t.Fatalf("expected 2 runs after change, got %d", len(changedPayload.Runs))
	}
}

func TestPortal_RunsSummary_ActiveRunLogMtimeChangesETag(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	layout := paths.NewLayout(nil, repoRoot)
	startedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	runID := "run-42-1234567890"
	batchID := "run-42"
	batchDir := filepath.Join(layout.BatchesDir, batchID)
	runDir := filepath.Join(batchDir, "runs", runID)
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte("hello active log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{42})
	writePortalLog(t, layout.EventsLogPath, []events.Event{{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}}})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs?summary=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if etag == "" {
		t.Fatal("expected summary response ETag")
	}
	var payload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Runs) != 1 {
		t.Fatalf("expected 1 active run, got %d", len(payload.Runs))
	}
	firstOutputAt := payload.Runs[0].LastOutputAt
	if firstOutputAt.IsZero() {
		t.Fatal("expected active run to carry LastOutputAt")
	}

	time.Sleep(portalRunsSnapshotTTL + 20*time.Millisecond)
	changedAt := firstOutputAt.Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(runDir, "run.log"), changedAt, changedAt); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/runs?summary=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected active log mtime change to return 200, got %d", resp2.StatusCode)
	}
	if got := strings.TrimSpace(resp2.Header.Get("ETag")); got == "" || got == etag {
		t.Fatalf("expected fresh ETag after active log mtime change, got %q", got)
	}
	var payload2 struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&payload2); err != nil {
		t.Fatal(err)
	}
	if len(payload2.Runs) != 1 {
		t.Fatalf("expected 1 active run after log mtime change, got %d", len(payload2.Runs))
	}
	if payload2.Runs[0].LastOutputAt == nil || !payload2.Runs[0].LastOutputAt.After(*firstOutputAt) {
		t.Fatalf("expected LastOutputAt to advance after log mtime change, got %v then %v", firstOutputAt, payload2.Runs[0].LastOutputAt)
	}
}

func TestPortal_RunsRunKey(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	layout := paths.NewLayout(nil, repoRoot)
	startedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	runID := "run-42-1234567890"
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	batchID := "run-42"
	runDir := filepath.Join(layout.BatchesDir, batchID, "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte("hello world log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{
				ID:        batchID,
				Path:      filepath.Join(layout.BatchesDir, batchID),
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: startedAt,
				Issues:    []int{42},
			},
		},
	}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs?runKey=" + runID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Run portalRun `json:"run"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Run.RunID != runID {
		t.Errorf("RunID = %q, want %q", payload.Run.RunID, runID)
	}
	if payload.Run.Log == "" {
		t.Errorf("runKey response should include full Log")
	}
}

func TestPortal_RunsRunKey_ReportsSnapshotFailuresAsInternalServerError(t *testing.T) {
	repoRoot := t.TempDir()
	handler := &portalHandler{
		repoRoot: repoRoot,
		runsIndex: &portalRunsIndex{
			repoRoot:     repoRoot,
			eventLogPath: repoRoot,
			view:         &portalRunsView{},
		},
		staleCleaner: portalStaleCleaner,
	}

	req, err := http.NewRequest(http.MethodGet, "/api/runs?runKey=run-missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.handleRuns(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for snapshot failure, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "read event log") && !strings.Contains(rec.Body.String(), "stat event log") {
		t.Fatalf("expected internal error body, got %s", rec.Body.String())
	}
}

func TestPortal_RunsAPI_OrphanActiveRunWithLiveBatchSocket(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchID := "orphan-batch-live"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(batchDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{101}, CreatedAt: time.Now().Add(-1 * time.Minute)}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	startedAt := time.Now().Add(-30 * time.Second)
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-orphan-1", Issue: 101, Payload: map[string]any{"branch": "sandman/101-fix", "batch_id": batchID}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 1 {
		t.Fatalf("expected 1 orphan active run, got %d: %#v", len(runs), runs)
	}
	run := runs[0]
	if run.Kind != "active" {
		t.Errorf("Kind = %q, want %q (orphan run with live batch socket should stay active)", run.Kind, "active")
	}
	if run.IssueNumber != 101 {
		t.Errorf("IssueNumber = %d, want %d", run.IssueNumber, 101)
	}
}

// TestPortal_OrphanActiveBatch_AllRowsRender is the regression test for
// issue #1464. A multi-issue batch whose entry was silently evicted
// from the batches index must still show one row per issue in the
// portal: the active issue with `kind=active status=running`, and every
// queued issue with `kind=active status=queued`. The on-disk batch
// dir, batch.sock, and manifest are all intact; only the index is
// missing the entry. The liveness probe returns true so the active
// path would normally run, but `discoverActiveRuns` will not find the
// batch in the index.
func TestPortal_OrphanActiveBatch_AllRowsRender(t *testing.T) {
	repoRoot := shortTempDir(t)
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const batchID = "orphn-260101000000-1014+6"
	issues := []int{1014, 1016, 135, 136, 137, 139}
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(batchDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Now().Add(-10 * time.Minute)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		Issues:    issues,
		BatchId:   batchID,
		RunKind:   "issue",
		CreatedAt: startedAt,
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	ev := []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: batchID + "-1014", Issue: 1014,
			Payload: map[string]any{"branch": "sandman/1014-fix", "batch_id": batchID}},
		{Type: "run.queued", Timestamp: startedAt.Add(1 * time.Second), RunID: batchID + "-1016", Issue: 1016,
			Payload: map[string]any{"batch_id": batchID, "issue_title": "slice 4 docs"}},
		{Type: "run.queued", Timestamp: startedAt.Add(2 * time.Second), RunID: batchID + "-135", Issue: 135,
			Payload: map[string]any{"batch_id": batchID, "issue_title": "Scaffold pinned rust BuildToolsPreset"}},
		{Type: "run.queued", Timestamp: startedAt.Add(3 * time.Second), RunID: batchID + "-136", Issue: 136,
			Payload: map[string]any{"batch_id": batchID, "issue_title": "Scaffold pinned java BuildToolsPreset"}},
		{Type: "run.queued", Timestamp: startedAt.Add(4 * time.Second), RunID: batchID + "-137", Issue: 137,
			Payload: map[string]any{"batch_id": batchID, "issue_title": "Scaffold pinned ruby BuildToolsPreset"}},
		{Type: "run.queued", Timestamp: startedAt.Add(5 * time.Second), RunID: batchID + "-139", Issue: 139,
			Payload: map[string]any{"batch_id": batchID, "issue_title": "Scaffold pinned elixir BuildToolsPreset"}},
	}
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, ev)

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	byIssue := map[int]portalRun{}
	for _, r := range runs {
		if r.IssueNumber > 0 {
			byIssue[r.IssueNumber] = r
		}
	}

	for _, issue := range issues {
		row, ok := byIssue[issue]
		if !ok {
			t.Fatalf("issue %d: no portal row returned", issue)
		}
		// Issue 1014 is genuinely running (run.started). All others
		// are queued wait-state rows. After issue #1699, queued rows
		// are kind="completed" rather than "active" — only the
		// running row keeps the active row kind.
		wantKind := "completed"
		wantStatus := "queued"
		if issue == 1014 {
			wantKind = "active"
			wantStatus = "running"
		}
		if row.Kind != wantKind {
			t.Errorf("issue %d: Kind=%q want %q (row=%#v)", issue, row.Kind, wantKind, row)
		}
		if row.Status != wantStatus {
			t.Errorf("issue %d: Status=%q want %q (row=%#v)", issue, row.Status, wantStatus, row)
		}
		if row.BatchKey != batchID {
			t.Errorf("issue %d: BatchKey=%q want %q (row=%#v)", issue, row.BatchKey, batchID, row)
		}
	}
}

// TestPortal_QueuedAndTerminalSameIssue_NotCollapsed is the second
// regression for issue #1464. A fresh run.queued for issue N in batch
// B must not be silently dropped by dedupRuns just because a previous
// terminal run for the same issue exists from a different batch A.
func TestPortal_QueuedAndTerminalSameIssue_NotCollapsed(t *testing.T) {
	repoRoot := shortTempDir(t)
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	oldBatchDir := filepath.Join(repoRoot, ".sandman", "batches", "old-1")
	if err := os.MkdirAll(oldBatchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(oldBatchDir, daemon.BatchManifest{
		Issues:    []int{1016},
		BatchId:   "old-1",
		RunKind:   "issue",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("write old manifest: %v", err)
	}
	addBatchToIndex(t, repoRoot, "old-1", oldBatchDir, []int{1016})

	const batchID = "new-1"
	newBatchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(newBatchDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(newBatchDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if err := daemon.WriteManifest(newBatchDir, daemon.BatchManifest{
		Issues:    []int{1016, 135},
		BatchId:   batchID,
		RunKind:   "issue",
		CreatedAt: time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("write new manifest: %v", err)
	}
	addBatchToIndex(t, repoRoot, batchID, newBatchDir, []int{1016, 135})

	startedAt := time.Now().Add(-1 * time.Minute)
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(-1 * time.Hour), RunID: "old-1-1016", Issue: 1016,
			Payload: map[string]any{"branch": "sandman/1016-fix", "batch_id": "old-1"}},
		{Type: "run.finished", Timestamp: startedAt.Add(-1 * time.Hour).Add(1 * time.Minute), RunID: "old-1-1016", Issue: 1016,
			Payload: map[string]any{"status": "aborted", "batch_id": "old-1"}},
		{Type: "run.queued", Timestamp: startedAt, RunID: batchID + "-1016", Issue: 1016,
			Payload: map[string]any{"batch_id": batchID, "issue_title": "slice 4 docs"}},
		{Type: "run.queued", Timestamp: startedAt, RunID: batchID + "-135", Issue: 135,
			Payload: map[string]any{"batch_id": batchID, "issue_title": "rust"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	byIssue := map[int][]portalRun{}
	for _, r := range runs {
		if r.IssueNumber > 0 {
			byIssue[r.IssueNumber] = append(byIssue[r.IssueNumber], r)
		}
	}

	if got := len(byIssue[1016]); got < 2 {
		t.Fatalf("issue 1016: expected at least 2 rows (old aborted + new queued), got %d: %#v", got, byIssue[1016])
	}

	var foundOldAborted, foundNewQueued bool
	for _, r := range byIssue[1016] {
		if r.BatchKey == "old-1" && r.Kind == "completed" && r.Status == "aborted" {
			foundOldAborted = true
		}
		if r.BatchKey == batchID && r.Kind == "active" && r.Status == "queued" {
			foundNewQueued = true
		}
	}
	if !foundOldAborted {
		t.Errorf("missing old aborted row for issue 1016 / batchKey=old-1; got %#v", byIssue[1016])
	}
	if !foundNewQueued {
		t.Errorf("missing fresh queued row for issue 1016 / batchKey=%q; got %#v", batchID, byIssue[1016])
	}
}

// TestPortal_MatchRunState_FallbackDoesNotBindOrphanIssueToPromptOnly
// is the regression for the third layered bug in issue #1464.
func TestPortal_MatchRunState_FallbackDoesNotBindOrphanIssueToPromptOnly(t *testing.T) {
	v := &portalRunsView{}

	promptOnly := portalActiveRun{
		Key:          "260101000000-abc1-auto-1",
		IssueNumber:  0,
		IssueNumbers: []int{},
		BatchID:      "260101000000-abc1-auto-1",
		RunID:        "260101000000-abc1-auto-1",
		StartedAt:    time.Now(),
	}

	issueState := events.RunState{
		RunID: "260101000500-abc2-1014",
		Started: events.Event{
			Type:      "run.started",
			Timestamp: time.Now().Add(-1 * time.Minute),
			RunID:     "260101000500-abc2-1014",
			Issue:     1014,
			Payload:   map[string]any{"branch": "sandman/1014-fix"},
		},
	}
	states := []events.RunState{issueState}
	used := []bool{false}

	idx := v.matchRunState(promptOnly, states, used)
	if idx != -1 {
		t.Fatalf("matchRunState bound a prompt-only instance to an issue-bound state; returned idx=%d (state=%#v)", idx, states[idx])
	}
}

// seedMixedActiveBatchIndex mirrors the createMixedBatchRunSocket helper
// pattern from portal_e2e_test.go (which lives behind //go:build e2e and
// is not linkable from this package without the tag). It writes a batch
// directory with a live Unix socket, a manifest covering every issue, and
// a batches-index entry whose literal "id" field is the empty string.
//
// The empty "id" is the pre-#1657 shape that exercises the
// activeKeyForActive fallback chain. Writing the JSON directly via
// os.WriteFile bypasses batchindex.Index.Add's canonicalizeBatchID,
// which would otherwise backfill the id from the path basename and
// hide the bug. Only the index id is empty; path and status="active"
// are set so discoverPortalInstances still finds the entry by socket
// probe.
func seedMixedActiveBatchIndex(t *testing.T, repoRoot, runName string, issues []int) {
	t.Helper()

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", runName)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatalf("create batch dir: %v", err)
	}

	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		Issues:    append([]int(nil), issues...),
		BatchId:   runName,
		RunKind:   "issue",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	ln, err := net.Listen("unix", filepath.Join(batchDir, "batch.sock"))
	if err != nil {
		t.Fatalf("listen batch.sock: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	layout := paths.NewLayout(nil, repoRoot)
	if err := os.MkdirAll(filepath.Dir(layout.BatchesIndexPath), 0755); err != nil {
		t.Fatalf("mkdir batches index dir: %v", err)
	}
	issuesJSON, err := json.Marshal(issues)
	if err != nil {
		t.Fatalf("marshal issues: %v", err)
	}
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UTC().Format(time.RFC3339Nano)
	indexLiteral := fmt.Sprintf(`{
  "version": 1,
  "entries": [
    {
      "id": "",
      "path": %q,
      "kind": "issue",
      "status": "active",
      "createdAt": %q,
      "issues": %s
    }
  ]
}
`, batchDir, createdAt, string(issuesJSON))
	if err := os.WriteFile(layout.BatchesIndexPath, []byte(indexLiteral), 0644); err != nil {
		t.Fatalf("write batches index: %v", err)
	}
}

// TestPortal_ActiveMixedBatch_AllIssuesRenderedAcrossStatuses is the
// regression repro for issue #1658. A single active batch covers 7
// distinct issues: 3 are still queued (run.queued events only) and 4
// are already running (run.started events). Hitting /api/runs must
// return 7 rows, one per issue, with the right status for each. The
// batches-index entry is written with an empty "id" so the repro
// exercises the activeKeyForActive fallback chain that issue #1657
// introduced and the empty-id pre-condition for the dead-batch
// synthesis edge case.
//
// The queued events deliberately carry an empty RunID to model the
// legacy event-log shape where a run.queued is recorded before the
// orchestrator knows the eventual run id. With the empty-id index
// entry, the active instance's per-row dedup falls through to the
// dead-batch synthesis path in seenIssuesForBatch, which skips rows
// whose runState.RunID is empty. The 3 queued issues then surface as
// `kind=completed status=aborted` synthesized rows instead of the
// expected `kind=active status=queued` rows. The 4 started events
// carry a non-empty RunID so they remain visible. The matching fix
// from issue #1659 must make the 3 queued rows render correctly.
func TestPortal_ActiveMixedBatch_AllIssuesRenderedAcrossStatuses(t *testing.T) {
	// shortTempDir keeps the unix batch.sock path well below the
	// 108-byte sun_path limit on macOS/Linux. portalRunsIndexes is a
	// package-level sync.Map keyed by repoRoot; collisions across
	// tests would serve stale snapshots, so clear the entry for this
	// isolated dir before constructing the handler.
	repoRoot := shortTempDir(t)
	portalRunsIndexes.Delete(repoRoot)

	// The portal handler spawns a stale-run cleaner on startup that
	// emits run.aborted for any queued-but-not-started event in the
	// log, corrupting the test's queued-event input. Disable it for
	// the duration of the test, matching the override pattern used by
	// TestPortal_AbortEndpoint_* in portal_abort_batch_kinds_test.go.
	prevStale := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = prevStale })

	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runName = "mixed-active-1"
	issues := []int{101, 102, 103, 104, 105, 106, 107}
	queued := []int{101, 102, 103}
	started := []int{104, 105, 106, 107}

	seedMixedActiveBatchIndex(t, repoRoot, runName, issues)
	batchID := runName

	pinnedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ev []events.Event
	for i, issue := range queued {
		ev = append(ev, events.Event{
			Type:      "run.queued",
			Timestamp: pinnedTime.Add(time.Duration(i) * time.Second),
			RunID:     "",
			Issue:     issue,
			Payload:   map[string]any{"batch_id": batchID},
		})
	}
	for i, issue := range started {
		ev = append(ev, events.Event{
			Type:      "run.started",
			Timestamp: pinnedTime.Add(time.Duration(len(queued)+i) * time.Second),
			RunID:     fmt.Sprintf("%s-%d", runName, issue),
			Issue:     issue,
			Payload: map[string]any{
				"branch":   fmt.Sprintf("sandman/%d-fix", issue),
				"batch_id": batchID,
			},
		})
	}
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, ev)

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runs := readPortalRuns(t, server.URL)

	byIssue := make(map[int]portalRun, len(runs))
	for _, r := range runs {
		if r.IssueNumber > 0 {
			byIssue[r.IssueNumber] = r
		}
	}

	present := make([]int, 0, len(byIssue))
	missing := make([]int, 0)
	for _, issue := range issues {
		if _, ok := byIssue[issue]; ok {
			present = append(present, issue)
		} else {
			missing = append(missing, issue)
		}
	}

	if got, want := len(runs), len(issues); got != want {
		t.Fatalf("expected %d portal rows for the %d active-batch issues, got %d\npresent=%v missing=%v\nruns=%#v",
			want, len(issues), got, present, missing, runs)
	}
	for _, issue := range missing {
		t.Errorf("issue %d: no portal row returned (issue is part of the active batch but is missing from /api/runs)", issue)
	}

	for _, issue := range issues {
		row, ok := byIssue[issue]
		if !ok {
			continue
		}
		wantKind := "active"
		wantStatus := "queued"
		for _, s := range started {
			if issue == s {
				wantStatus = "running"
				break
			}
		}
		if row.Kind != wantKind {
			t.Errorf("issue %d: Kind=%q want %q (row=%#v)", issue, row.Kind, wantKind, row)
		}
		if row.Status != wantStatus {
			t.Errorf("issue %d: Status=%q want %q (row=%#v)", issue, row.Status, wantStatus, row)
		}
	}
}
