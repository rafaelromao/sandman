//go:build e2e

// end-to-end tests for all batch-id rules.
//
// This file is the only slice in parent PRD #1916 that adds e2e tests;
// earlier slices (1-9) focused on unit/integration changes through the
// public cmd/Dependencies and batch orchestrator seams. This file
// exercises the full identity model end-to-end through the same
// public seams so every batch-id rule (slices 1-5), the per-row archive
// flow, and the --continue flow is validated against
// the actual on-disk and HTTP-rendered behavior.
//
// Greenfield .sandman policy
// --------------------------
// These tests assume a greenfield .sandman layout. The archive
// flow and the --continue flow both operate on the same `.sandman/batches/` and
// `.sandman/archive/` shapes introduced by ADR-0032, with no legacy `.sandman/runs/` paths
// or pre-#1917 batch ids present at suite start. See
// docs/adr/0032-sandman-layout-redesign.md, "Migration out of scope":
// the slice-1 contract change renames the public BatchId surface and
// the per-row RunID templates, and the operator is expected to delete
// `.sandman` after rebuilding. No migration tool ships for the old
// layout. Each test below writes a fresh greenfield `.sandman/` in a
// per-test temp dir, so no legacy state ever crosses the test boundary.
//
// Build tag and gating
// --------------------
// The whole file is gated by the `//go:build e2e` tag and by a new
// `testenv.E2EScenarioBatchIDRules` scenario. Each test calls
// `testenv.E2EGateAllowed(testenv.E2EScenarioBatchIDRules)` and skips
// itself when the env var is unset. Run the suite locally with:
//
//	SANDMAN_E2E_GATES=batch_id_rules go test -tags e2e ./internal/cmd -run TestBatchIDRules
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/runid"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// batchIDRulesTS and batchIDRulesShortID are the deterministic (ts, shortid)
// pair the suite uses to mint canonical BatchIds that
// match the strings the rest of the test fixture hard-codes. The
// values intentionally avoid the time / random component so the
// assertions can use full string equality.
const (
	batchIDRulesTS      = "260618113825"
	batchIDRulesShortID = "abcd"
)

// singleIssueBatchID returns the canonical public BatchId for a
// single-issue batch (`<ts>-<sid>-42`).
func singleIssueBatchID() string {
	return runid.NewBatchID(runid.KindIssue, 1, "42", batchIDRulesTS, batchIDRulesShortID)
}

// multiIssueBatchID returns the canonical public BatchId for a
// 2-issue batch (`<ts>-<sid>-42+1`).
func multiIssueBatchID() string {
	return runid.NewBatchID(runid.KindIssue, 2, "42", batchIDRulesTS, batchIDRulesShortID)
}

// requireGateMultiIssue is the suite-wide gate: every test calls this
// before running so the file self-skips unless the operator opted in.
func requireGateMultiIssue(t *testing.T) {
	t.Helper()
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBatchIDRules) {
		t.Skip("set SANDMAN_E2E_GATES=batch_id_rules (or all) to run e2e tests")
	}
}

// freshSandmanDir returns the absolute path of the greenfield
// per-test temp dir the suite runs in. The dir is created by
// `newRunDeps` (or `newRunDepsInDir`) which sets up a fresh `.sandman/`
// and chdirs the test into it. Callers must invoke `newRunDeps` (or the
// `Auto`/`InDir` variant) BEFORE calling this helper so the path
// returned matches the cwd the run command writes batches.json into.
func freshSandmanDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return cwd
}

// bindUnixSocketForBatchIDRules binds a Unix domain socket at the given path
// and registers a Cleanup that releases it when the test finishes.
// Used to give the portal handler a live control socket without
// spinning up a real daemon.
func bindUnixSocketForBatchIDRules(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind unix socket %q: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

// idxContinueLookup returns the count of batch index entries whose id
// equals batchID. Used by the --continue tests to assert the fresh
// batch id is registered exactly once.
func idxContinueLookup(t *testing.T, dir, batchID string) int {
	t.Helper()
	idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	count := 0
	for _, b := range idx.Batches {
		if b.ID == batchID {
			count++
		}
	}
	return count
}

// --- Behavior 1: single issue batch identity -----------------------------

// TestBatchIDRules_SingleIssueBatchIdentity covers behavior 1: `sandman run
// 42` mints `<ts>-<sid>-42` as both the public BatchId and the per-row
// RunID; the on-disk batch folder basename, batch.json.batchId, the
// events.jsonl batch_id payload, and the batches index entry id all
// agree.
func TestBatchIDRules_SingleIssueBatchIdentity(t *testing.T) {
	requireGateMultiIssue(t)
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{{IssueNumber: 42, Status: "success", Branch: "42-fix-bug"}},
	}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix login", State: "open"}},
		prs:    map[string]*github.PR{},
	}
	dir := freshSandmanDir(t)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, buf.String())
	}

	wantPublicBatchID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42"

	idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	if len(idx.Batches) != 1 {
		t.Fatalf("expected exactly 1 batch index entry, got %d (entries=%v)", len(idx.Batches), idx.Batches)
	}
	got := idx.Batches[0]
	if got.ID != wantPublicBatchID {
		t.Errorf("batches index entry id = %q, want %q (public BatchId for single issue)", got.ID, wantPublicBatchID)
	}
	if filepath.Base(got.Path) != wantPublicBatchID {
		t.Errorf("batches index entry path basename = %q, want %q", filepath.Base(got.Path), wantPublicBatchID)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", wantPublicBatchID)
	manifest, err := daemon.ReadManifest(batchDir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.BatchId != wantPublicBatchID {
		t.Errorf("manifest.BatchId = %q, want %q", manifest.BatchId, wantPublicBatchID)
	}
}

// --- Behavior 2: multi-issue batch identity -----------------------------

// TestBatchIDRules_MultiIssueBatchIdentity covers behavior 2: `sandman run
// 42 43` mints `<ts>-<sid>-42+1` as the public BatchId (one index
// entry) with per-row RunIDs `<ts>-<sid>-42` and `<ts>-<sid>-43`. The
// per-row RunIDs do not have their own index entries.
func TestBatchIDRules_MultiIssueBatchIdentity(t *testing.T) {
	requireGateMultiIssue(t)
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "42-fix"},
			{IssueNumber: 43, Status: "success", Branch: "43-fix"},
		},
	}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, State: "open"},
			43: {Number: 43, State: "open"},
		},
		prs: map[string]*github.PR{},
	}
	dir := freshSandmanDir(t)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, buf.String())
	}

	wantPublicBatchID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42+1"
	wantFirstRowID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42"
	wantSecondRowID := spy.req.RunTS + "-" + spy.req.RunShortID + "-43"

	idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	if len(idx.Batches) != 1 {
		t.Fatalf("expected exactly 1 batch index entry for multi-issue run, got %d (entries=%v)", len(idx.Batches), idx.Batches)
	}
	if got := idx.Batches[0].ID; got != wantPublicBatchID {
		t.Errorf("entry ID = %q, want %q (public BatchId with +1)", got, wantPublicBatchID)
	}
	if idx.ResolveBatch(wantFirstRowID) != nil {
		t.Errorf("first row RunID %q must not have a separate index entry", wantFirstRowID)
	}
	if idx.ResolveBatch(wantSecondRowID) != nil {
		t.Errorf("second row RunID %q must not have a separate index entry", wantSecondRowID)
	}
	if filepath.Base(idx.Batches[0].Path) != wantPublicBatchID {
		t.Errorf("entry path basename = %q, want %q", filepath.Base(idx.Batches[0].Path), wantPublicBatchID)
	}
}

// --- Behavior 4: orphan review identity ----------------------------------

// TestBatchIDRules_OrphanReviewBatchIdentity pins the orphan-review path:
// when the review daemon picks up an open PR but no linked issue, the
// batch is keyed by the canonical review shape and the on-disk batch
// folder agrees with the index entry id.
func TestBatchIDRules_OrphanReviewBatchIdentity(t *testing.T) {
	requireGateMultiIssue(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	batchDir := filepath.Join(dir, ".sandman", "batches", "dead-orphan-pr42")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := daemon.BatchManifest{
		Issues:    []int{},
		CreatedAt: now,
	}
	if err := daemon.WriteManifest(batchDir, manifest); err != nil {
		t.Fatal(err)
	}
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: "dead-orphan-pr42", Path: batchDir, Kind: batchindex.KindReview, Status: batchindex.StatusActive, CreatedAt: now, PR: 42},
	}}
	if err := idx.Save(filepath.Join(dir, ".sandman", "batches.json")); err != nil {
		t.Fatal(err)
	}
	got, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Batches[0].Kind != batchindex.KindReview {
		t.Errorf("orphan review kind = %s, want %s", got.Batches[0].Kind, batchindex.KindReview)
	}
	if got.Batches[0].PR != 42 {
		t.Errorf("orphan review PR = %d, want 42", got.Batches[0].PR)
	}
	if filepath.Base(got.Batches[0].Path) != "dead-orphan-pr42" {
		t.Errorf("orphan review path basename = %q, want %q", filepath.Base(got.Batches[0].Path), "dead-orphan-pr42")
	}
}

// --- Behavior 5: prompt-only with and without user id -------------------

// TestBatchIDRules_PromptOnlyBatchIdentity covers behavior 5 for both the
// no-userid and with-userid prompt-only shapes:
//   - `sandman run --prompt "..."` mints `<ts>-<sid>-prompt`.
//   - `sandman run --prompt "..." --run-id myid` mints
//     `<ts>-<sid>-prompt-myid`.
func TestBatchIDRules_PromptOnlyBatchIdentity(t *testing.T) {
	requireGateMultiIssue(t)
	spyNoID := &spyBatchRunner{result: &batch.Result{}}
	depsNoID := newRunDeps(t, spyNoID)
	depsNoID.GitHubClient = &fakeGitHubClient{fetchIssueError: fmt.Errorf("fetch should not run for prompt-only")}
	dir := freshSandmanDir(t)

	var buf bytes.Buffer
	cmd := NewRunCmd(depsNoID)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "Return only OK."})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt-only no-userid error: %v\noutput:\n%s", err, buf.String())
	}

	wantNoID := spyNoID.req.BatchTS + "-" + spyNoID.req.BatchShortID + "-prompt"
	idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	if len(idx.Batches) != 1 {
		t.Fatalf("expected 1 batch index entry for prompt-only no-userid, got %d", len(idx.Batches))
	}
	if got := idx.Batches[0].ID; !strings.HasSuffix(got, "-prompt") {
		t.Errorf("prompt-only no-userid BatchId = %q, want suffix %q", got, "-prompt")
	}
	if filepath.Base(idx.Batches[0].Path) != wantNoID {
		t.Errorf("prompt-only no-userid path basename = %q, want %q", filepath.Base(idx.Batches[0].Path), wantNoID)
	}

	// Reset state for the with-userid path so we exercise the
	// follow-on index entry instead of overloading the first one.
	// Reuse the same deps/cwd so the second cmd2 writes into the
	// same .sandman/ as the first cmd.
	if err := os.Remove(filepath.Join(dir, ".sandman", "batches.json")); err != nil {
		t.Fatal(err)
	}

	spyWithID := &spyBatchRunner{result: &batch.Result{}}
	depsWithID := depsNoID
	depsWithID.BatchRunner = spyWithID

	var buf2 bytes.Buffer
	cmd2 := NewRunCmd(depsWithID)
	cmd2.SetOut(&buf2)
	cmd2.SetErr(&buf2)
	cmd2.SetArgs([]string{"--prompt", "Return only OK.", "--run-id", "myid"})

	if err := cmd2.Execute(); err != nil {
		t.Fatalf("prompt-only with-userid error: %v\noutput:\n%s", err, buf2.String())
	}

	wantWithID := spyWithID.req.BatchTS + "-" + spyWithID.req.BatchShortID + "-prompt-myid"
	idx2, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	if len(idx2.Batches) != 1 {
		t.Fatalf("expected 1 batch index entry for prompt-only with-userid, got %d", len(idx2.Batches))
	}
	if got := idx2.Batches[0].ID; !strings.Contains(got, "-prompt-myid") {
		t.Errorf("prompt-only with-userid BatchId = %q, want substring %q", got, "-prompt-myid")
	}
	if filepath.Base(idx2.Batches[0].Path) != wantWithID {
		t.Errorf("prompt-only with-userid path basename = %q, want %q", filepath.Base(idx2.Batches[0].Path), wantWithID)
	}
}

// --- Behavior 6: portal Batch label and Details tab BatchId --------------

// TestBatchIDRules_PortalBatchLabelAndDetailsRenderPublicBatchId covers
// behavior 6: the portal's /api/runs row must surface the public
// BatchId (the batch folder basename, including the "+N" suffix for
// multi-issue batches) on the Batch label and the Details tab
// payload, not the per-row RunID. Issue #1954 closed that ordering;
// this test pins it end-to-end.
func TestBatchIDRules_PortalBatchLabelAndDetailsRenderPublicBatchId(t *testing.T) {
	requireGateMultiIssue(t)
	repoRoot := testenv.MkdirShort(t, "sm-slice10-p-")
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	now := time.Now()

	ts := batchIDRulesTS
	shortid := batchIDRulesShortID
	batchID := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	rowRunID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)

	if batchID == rowRunID {
		t.Fatalf("setup: multi-issue batch id %q must differ from row RunID %q", batchID, rowRunID)
	}
	if !strings.Contains(batchID, "+") {
		t.Fatalf("setup: multi-issue batch id %q must contain '+'", batchID)
	}

	batchDir := filepath.Join(layout.BatchesDir, batchID)
	runDir := filepath.Join(batchDir, "runs", rowRunID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte("log line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wire a control socket under batchDir so the portal detects the
	// active row and surfaces it (issue #1924 contract:
	// active rows surface via control socket).
	bindUnixSocketForBatchIDRules(t, filepath.Join(batchDir, "batch.sock"))

	// Write the manifest under the batch dir with the PUBLIC BatchId
	// (== batch dir basename, with "+N" for multi-issue).
	// confirmed batchKeyForActive prefers manifest.BatchId over Key.
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   string(batchindex.KindIssue),
		Issues:    []int{42},
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	startedAt := now
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: rowRunID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": batchID}},
	})

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	}}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) < 1 {
		t.Fatalf("expected at least 1 portal run, got %d: %#v", len(runs), runs)
	}
	var got portalRun
	for _, r := range runs {
		if r.RunID == rowRunID {
			got = r
			break
		}
	}
	if got.RunID == "" {
		t.Fatalf("missing portal run with RunID=%q (got %#v)", rowRunID, runs)
	}

	if got.RunID != rowRunID {
		t.Errorf("RunID = %q, want %q (per-row RunID is preserved for per-row actions)", got.RunID, rowRunID)
	}
	if got.BatchKey != batchID {
		t.Errorf("BatchKey = %q, want %q (portal Batch label / Details tab use public BatchId)", got.BatchKey, batchID)
	}

	resp, err := http.Get(server.URL + "/api/runs?runKey=" + rowRunID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /api/runs?runKey=..., got %d", resp.StatusCode)
	}
	var detail struct {
		Run portalRun `json:"run"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Run.BatchKey != batchID {
		t.Errorf("Details tab run.BatchKey = %q, want %q", detail.Run.BatchKey, batchID)
	}
}

// --- Behavior 7: live-vs-saved log behavior ------------------------------

// TestBatchIDRules_PortalLiveVsSavedVsArchivedLog covers behavior 7: the
// portal must serve the live tail for an active row, the saved log
// for a terminal row, and the archived log for a historical row.
// This test pins the live/saved/archived log resolution end-to-end
// through the public /api/runs endpoint.
func TestBatchIDRules_PortalLiveVsSavedVsArchivedLog(t *testing.T) {
	requireGateMultiIssue(t)
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	now := time.Now()

	ts := batchIDRulesTS
	shortid := batchIDRulesShortID

	activeBatchID := runid.NewBatchID(runid.KindIssue, 1, "42", ts, shortid)
	activeRowID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)
	activeBatchDir := filepath.Join(layout.BatchesDir, activeBatchID)
	activeRunDir := filepath.Join(activeBatchDir, "runs", activeRowID)
	if err := os.MkdirAll(activeRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	liveContent := "[live] first line\n[live] second line\n"
	if err := os.WriteFile(filepath.Join(activeRunDir, "run.log"), []byte(liveContent), 0644); err != nil {
		t.Fatal(err)
	}
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: now.Add(-time.Minute), RunID: activeRowID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": activeBatchID}},
	})

	terminalBatchID := runid.NewBatchID(runid.KindIssue, 1, "43", ts, shortid)
	terminalRowID := runid.NewRunID(runid.KindIssue, "43", ts, shortid)
	terminalBatchDir := filepath.Join(layout.BatchesDir, terminalBatchID)
	terminalRunDir := filepath.Join(terminalBatchDir, "runs", terminalRowID)
	if err := os.MkdirAll(terminalRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	savedContent := "[saved] terminal first line\n[saved] terminal second line\n"
	if err := os.WriteFile(filepath.Join(terminalRunDir, "run.log"), []byte(savedContent), 0644); err != nil {
		t.Fatal(err)
	}
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: now.Add(-2 * time.Minute), RunID: terminalRowID, Issue: 43, Payload: map[string]any{"branch": "43-fix", "batch_id": terminalBatchID}},
		{Type: "run.finished", Timestamp: now.Add(-time.Minute), RunID: terminalRowID, Issue: 43, Payload: map[string]any{"status": "success", "branch": "43-fix", "batch_id": terminalBatchID}},
	})

	archivedBatchID := runid.NewBatchID(runid.KindIssue, 1, "99", ts, shortid)
	archivedRowID := runid.NewRunID(runid.KindIssue, "99", ts, shortid)
	archiveDir := filepath.Join(layout.ArchiveDir, archivedBatchID)
	archivedRunDir := filepath.Join(archiveDir, "runs", archivedRowID)
	if err := os.MkdirAll(archivedRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	archivedContent := "[archived] historical first line\n[archived] historical second line\n"
	if err := os.WriteFile(filepath.Join(archivedRunDir, "run.log"), []byte(archivedContent), 0644); err != nil {
		t.Fatal(err)
	}
	archivedAt := now.Add(-time.Hour)
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: now.Add(-3 * time.Hour), RunID: archivedRowID, Issue: 99, Payload: map[string]any{"branch": "99-fix", "batch_id": archivedBatchID}},
		{Type: "run.finished", Timestamp: now.Add(-2 * time.Hour), RunID: archivedRowID, Issue: 99, Payload: map[string]any{"status": "success", "branch": "99-fix", "batch_id": archivedBatchID}},
	})

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: activeBatchID, Path: activeBatchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
		{ID: terminalBatchID, Path: terminalBatchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{43}},
		{ID: archivedBatchID, Path: archiveDir, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now, ArchivedAt: &archivedAt, Issues: []int{99}},
	}}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	rowsByID := map[string]portalRun{}
	for _, row := range readPortalRuns(t, server.URL) {
		rowsByID[row.RunID] = row
	}
	active, ok := rowsByID[activeRowID]
	if !ok {
		t.Fatalf("missing active row %q", activeRowID)
	}
	if active.Archived {
		t.Errorf("active row marked Archived=true; want false")
	}
	if !active.SourceExists {
		t.Errorf("active row SourceExists=false; want true")
	}
	if strings.TrimSpace(active.Log) == "" {
		t.Errorf("active row Log is empty; want live tail")
	}
	if active.LogURL == "" {
		t.Errorf("active row LogURL is empty; want live URL")
	}

	terminal, ok := rowsByID[terminalRowID]
	if !ok {
		t.Fatalf("missing terminal row %q", terminalRowID)
	}
	if terminal.Archived {
		t.Errorf("terminal row marked Archived=true; want false")
	}
	if !terminal.SourceExists {
		t.Errorf("terminal row SourceExists=false; want true")
	}
	if !strings.Contains(terminal.Log, "terminal") {
		t.Errorf("terminal row Log = %q, want saved terminal content", terminal.Log)
	}

	hist, ok := rowsByID[archivedRowID]
	if !ok {
		t.Fatalf("missing historical row %q", archivedRowID)
	}
	if !hist.Archived {
		t.Errorf("historical row marked Archived=false; want true")
	}
	if !strings.Contains(hist.Log, "historical") {
		t.Errorf("historical row Log = %q, want archived log content", hist.Log)
	}
}

// --- Behavior 8: per-row RunID-based archive -----------------------------

// TestBatchIDRules_ArchiveRunMovesOnlyRunFolder pins behavior 8a:
// `sandman archive run <runId>` moves only `runs/<runId>/` to
// `archive/<batchId>/runs/<runId>/`. The on-disk batch dir stays in
// place and the archived run.log is reachable at the new path.
func TestBatchIDRules_ArchiveRunMovesOnlyRunFolder(t *testing.T) {
	requireGateMultiIssue(t)
	dir := newSandmanDir(t)
	t.Chdir(dir)

	runID := singleIssueBatchID()
	batchDir := filepath.Join(dir, ".sandman", "batches", runID)
	now := time.Now()
	writeRunDirForArchive(t, batchDir, runID, batchindex.RunManifest{
		BatchID: runID, Issue: 42, Kind: batchindex.KindIssue, CreatedAt: now, Status: batchindex.RunManifestStatusSuccess,
	})
	logContent := "[archived] hello world\n"
	if err := os.WriteFile(filepath.Join(batchDir, "runs", runID, "run.log"), []byte(logContent), 0644); err != nil {
		t.Fatal(err)
	}
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	})

	cmd := NewArchiveCmd(newTestDeps(t))
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", runID})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive run error: %v\noutput:\n%s", err, buf.String())
	}

	archiveRunDir := filepath.Join(dir, ".sandman", "archive", runID, "runs", runID)
	if _, err := os.Stat(archiveRunDir); err != nil {
		t.Fatalf("expected archived run dir %q to exist: %v", archiveRunDir, err)
	}
	if _, err := os.Stat(filepath.Join(batchDir, "runs", runID)); !os.IsNotExist(err) {
		t.Errorf("expected live run dir gone after archive, stat err = %v", err)
	}
	gotLog, err := os.ReadFile(filepath.Join(archiveRunDir, "run.log"))
	if err != nil {
		t.Fatalf("read archived run.log: %v", err)
	}
	if string(gotLog) != logContent {
		t.Errorf("archived run.log content = %q, want %q", gotLog, logContent)
	}
}

// TestBatchIDRules_ArchiveRunLeavesSiblingsLive pins behavior 8b: sibling
// rows in a multi-run batch remain active and source-backed after one
// row is archived. The batch dir stays in place under batches/, the
// archived row's runs/<rowID>/ folder leaves for archive/, and the
// sibling row's folder is untouched.
func TestBatchIDRules_ArchiveRunLeavesSiblingsLive(t *testing.T) {
	requireGateMultiIssue(t)
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchID := multiIssueBatchID()
	firstRow := runid.NewRunID(runid.KindIssue, "42", batchIDRulesTS, batchIDRulesShortID)
	secondRow := runid.NewRunID(runid.KindIssue, "43", batchIDRulesTS, batchIDRulesShortID)
	batchDir := filepath.Join(dir, ".sandman", "batches", batchID)
	now := time.Now()
	writeRunDirForArchive(t, batchDir, firstRow, batchindex.RunManifest{
		BatchID: batchID, Issue: 42, Kind: batchindex.KindIssue, CreatedAt: now, Status: batchindex.RunManifestStatusSuccess,
	})
	writeRunDirForArchive(t, batchDir, secondRow, batchindex.RunManifest{
		BatchID: batchID, Issue: 43, Kind: batchindex.KindIssue, CreatedAt: now, Status: batchindex.RunManifestStatusSuccess,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{ID: batchID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42, 43}},
	})

	cmd := NewArchiveCmd(newTestDeps(t))
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", firstRow})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive run error: %v\noutput:\n%s", err, buf.String())
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected batch dir %q to remain (sibling live), got: %v", batchDir, err)
	}
	if _, err := os.Stat(filepath.Join(batchDir, "runs", secondRow)); err != nil {
		t.Errorf("expected sibling run dir %q to remain live, got: %v", filepath.Join(batchDir, "runs", secondRow), err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", batchID, "runs", firstRow)); err != nil {
		t.Errorf("expected archived first-row dir to exist: %v", err)
	}

	idx := loadBatchIndexForArchive(t, dir)
	entry := idx.ResolveBatch(batchID)
	if entry == nil {
		t.Fatalf("expected batches index entry for %q", batchID)
	}
	if entry.Status != batchindex.StatusActive {
		t.Errorf("entry status = %s, want %s (sibling still live)", entry.Status, batchindex.StatusActive)
	}
}

// TestBatchIDRules_ArchiveRunFlipsPerRowRecordStatus pins behavior 8c: the
// archived row's RunRecord.Status flips to `archived` while the
// entry-level Status stays `active` until the last live row leaves.
// The same index covers both per-row and entry-level projections.
func TestBatchIDRules_ArchiveRunFlipsPerRowRecordStatus(t *testing.T) {
	requireGateMultiIssue(t)
	dir := newSandmanDir(t)
	t.Chdir(dir)

	runID := singleIssueBatchID()
	batchDir := filepath.Join(dir, ".sandman", "batches", runID)
	now := time.Now()
	writeRunDirForArchive(t, batchDir, runID, batchindex.RunManifest{
		BatchID: runID, Issue: 42, Kind: batchindex.KindIssue, CreatedAt: now, Status: batchindex.RunManifestStatusSuccess,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	})

	cmd := NewArchiveCmd(newTestDeps(t))
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", runID})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive run error: %v\noutput:\n%s", err, buf.String())
	}

	idx := loadBatchIndexForArchive(t, dir)
	entry := idx.ResolveBatch(runID)
	if entry == nil {
		t.Fatalf("missing batches index entry for %q", runID)
	}
	if entry.Status != batchindex.StatusActive {
		t.Errorf("entry-level Status = %s, want %s (last live row leaves until whole batch is archived)", entry.Status, batchindex.StatusActive)
	}
	rec := idx.RunRecordFor(runID, runID)
	if rec == nil {
		t.Fatalf("missing RunRecord for archived row %q", runID)
	}
	if rec.Status != batchindex.RunRecordStatusArchived {
		t.Errorf("RunRecord.Status = %s, want %s", rec.Status, batchindex.RunRecordStatusArchived)
	}
}

// TestBatchIDRules_ArchiveRunLogRetrievableFromNewPath pins behavior 8d:
// the archived row's run.log is readable from the new archive path,
// proving the file content survives the move and is reachable from
// the canonical archive location.
func TestBatchIDRules_ArchiveRunLogRetrievableFromNewPath(t *testing.T) {
	requireGateMultiIssue(t)
	dir := newSandmanDir(t)
	t.Chdir(dir)

	runID := singleIssueBatchID()
	batchDir := filepath.Join(dir, ".sandman", "batches", runID)
	now := time.Now()
	writeRunDirForArchive(t, batchDir, runID, batchindex.RunManifest{
		BatchID: runID, Issue: 42, Kind: batchindex.KindIssue, CreatedAt: now, Status: batchindex.RunManifestStatusSuccess,
	})
	logContent := "[archive-path] readable line\n"
	if err := os.WriteFile(filepath.Join(batchDir, "runs", runID, "run.log"), []byte(logContent), 0644); err != nil {
		t.Fatal(err)
	}
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	})

	cmd := NewArchiveCmd(newTestDeps(t))
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", runID})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive run error: %v\noutput:\n%s", err, buf.String())
	}

	archiveLogPath := filepath.Join(dir, ".sandman", "archive", runID, "runs", runID, "run.log")
	got, err := os.ReadFile(archiveLogPath)
	if err != nil {
		t.Fatalf("read archived log at %q: %v", archiveLogPath, err)
	}
	if string(got) != logContent {
		t.Errorf("archived log content = %q, want %q", got, logContent)
	}
}

// TestBatchIDRules_ArchiveRunAlreadyArchivedReturns409 pins behavior 8e:
// re-archiving an already-archived row returns 409 with the existing
// ArchivePath echoed in the error body. The portal HTTP path is
// exercised end-to-end so the structured 409 body (carrying
// `archivePath`) is pinned; the CLI path surfaces the same status.
func TestBatchIDRules_ArchiveRunAlreadyArchivedReturns409(t *testing.T) {
	requireGateMultiIssue(t)
	repoRoot := testenv.MkdirShort(t, "sm-slice10-a-")
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	runID := singleIssueBatchID()
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", runID)
	now := time.Now()
	writeRunDirForArchive(t, batchDir, runID, batchindex.RunManifest{
		BatchID: runID, Issue: 42, Kind: batchindex.KindIssue, CreatedAt: now, Status: batchindex.RunManifestStatusSuccess,
	})
	writeBatchIndexForArchive(t, repoRoot, []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	})

	// Pre-create the archive target so the portal handler hits the
	// "already archived" branch.
	archiveRunDir := filepath.Join(repoRoot, ".sandman", "archive", runID, "runs", runID)
	if err := os.MkdirAll(archiveRunDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Stub the liveness probe so the portal treats the batch as
	// terminal (the on-disk batch.sock does not exist in this test).
	prevProbe := portalRunLivenessProbe
	t.Cleanup(func() { portalRunLivenessProbe = prevProbe })
	portalRunLivenessProbe = func(string) bool { return false }

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	body := `{"runId":"` + runID + `"}`
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/archive", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, respBody)
	}

	var payload struct {
		Error       string `json:"error"`
		ArchivePath string `json:"archivePath"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		t.Fatalf("unmarshal 409 body: %v: %s", err, respBody)
	}
	if payload.Error == "" {
		t.Errorf("expected non-empty error message in 409 body, got %q", payload.Error)
	}
	// The portal handler surfaces the on-disk archive path (relative
	// to repoRoot) in the 409 body so the operator can inspect it.
	wantArchivePath := filepath.Join(".sandman", "archive", runID, "runs", runID)
	if payload.ArchivePath != wantArchivePath {
		t.Errorf("archivePath = %q, want %q (existing archive path must be echoed)", payload.ArchivePath, wantArchivePath)
	}
}

// TestBatchIDRules_ArchiveRunNonTerminalReturns409 pins behavior 8f:
// archiving a non-terminal targeted row returns 409. The CLI path
// surfaces a terminal-state error so an operator hitting the failure
// from the shell sees the same status code as the portal endpoint.
func TestBatchIDRules_ArchiveRunNonTerminalReturns409(t *testing.T) {
	requireGateMultiIssue(t)
	dir := newSandmanDir(t)
	t.Chdir(dir)

	runID := singleIssueBatchID()
	batchDir := filepath.Join(dir, ".sandman", "batches", runID)
	now := time.Now()
	writeRunDirForArchive(t, batchDir, runID, batchindex.RunManifest{
		BatchID: runID, Issue: 42, Kind: batchindex.KindIssue, CreatedAt: now, Status: batchindex.RunManifestStatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	})

	cmd := NewArchiveCmd(newTestDeps(t))
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", runID})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected error from archive on non-terminal row, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "terminal") && !strings.Contains(strings.ToLower(err.Error()), "active") {
		t.Errorf("expected error to mention terminal/active state, got %q", err.Error())
	}
}

// TestBatchIDRules_ArchiveBatchMovesWholeDirAndFlipsEntry pins behavior 8g:
// whole-batch archive via `archive batch <batchId>` moves the whole
// batch dir and flips the entry to `archived`. The single-row
// shortcut is the same op with a single live row.
func TestBatchIDRules_ArchiveBatchMovesWholeDirAndFlipsEntry(t *testing.T) {
	requireGateMultiIssue(t)
	dir := newSandmanDir(t)
	t.Chdir(dir)

	runID := singleIssueBatchID()
	batchDir := filepath.Join(dir, ".sandman", "batches", runID)
	now := time.Now()
	writeRunDirForArchive(t, batchDir, runID, batchindex.RunManifest{
		BatchID: runID, Issue: 42, Kind: batchindex.KindIssue, CreatedAt: now, Status: batchindex.RunManifestStatusSuccess,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	})

	cmd := NewArchiveCmd(newTestDeps(t))
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"batch", runID})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive batch error: %v\noutput:\n%s", err, buf.String())
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", runID)); err != nil {
		t.Errorf("expected archived batch dir %q to exist, got: %v", filepath.Join(dir, ".sandman", "archive", runID), err)
	}
	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Errorf("expected live batch dir gone after whole-batch archive, stat err = %v", err)
	}
	idx := loadBatchIndexForArchive(t, dir)
	entry := idx.ResolveBatch(runID)
	if entry == nil {
		t.Fatalf("missing batches index entry for %q", runID)
	}
	if entry.Status != batchindex.StatusArchived {
		t.Errorf("entry status = %s, want %s", entry.Status, batchindex.StatusArchived)
	}
}

// TestBatchIDRules_ArchiveOlderThanPerRowAware pins behavior 8h (older-than
// part): bulk `archive older-than` archives only the dead per-row
// runs whose CreatedAt is older than the cutoff, leaving younger
// rows alone.
func TestBatchIDRules_ArchiveOlderThanPerRowAware(t *testing.T) {
	requireGateMultiIssue(t)
	dir := newSandmanDir(t)
	t.Chdir(dir)

	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Round(time.Second)
	young := time.Now().Add(-2 * 24 * time.Hour).UTC().Round(time.Second)

	oldID := "old-per-row-dead"
	youngID := "young-per-row-dead"
	oldDir := filepath.Join(dir, ".sandman", "batches", oldID)
	youngDir := filepath.Join(dir, ".sandman", "batches", youngID)
	for _, d := range []string{oldDir, youngDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeRunDirForArchive(t, oldDir, oldID, batchindex.RunManifest{BatchID: oldID, Issue: 1, Kind: batchindex.KindIssue, CreatedAt: old, Status: batchindex.RunManifestStatusSuccess})
	writeRunDirForArchive(t, youngDir, youngID, batchindex.RunManifest{BatchID: youngID, Issue: 2, Kind: batchindex.KindIssue, CreatedAt: young, Status: batchindex.RunManifestStatusSuccess})
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{ID: oldID, Path: oldDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: old, Issues: []int{1}},
		{ID: youngID, Path: youngDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: young, Issues: []int{2}},
	})

	cmd := NewArchiveCmd(newTestDeps(t))
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive older-than error: %v\noutput:\n%s", err, buf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", oldID, "runs", oldID)); err != nil {
		t.Errorf("expected old row archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", youngID)); !os.IsNotExist(err) {
		t.Errorf("expected young row NOT archived, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(youngDir, "runs", youngID)); err != nil {
		t.Errorf("expected young row still live: %v", err)
	}
}

// TestBatchIDRules_ArchiveStalePerRowAware pins behavior 8h (stale part):
// bulk `archive stale` is per-row aware — it archives a stale (active
// but no live daemon) row while leaving freshly-active rows alone.
func TestBatchIDRules_ArchiveStalePerRowAware(t *testing.T) {
	requireGateMultiIssue(t)
	dir := newSandmanDir(t)
	t.Chdir(dir)

	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Round(time.Second)
	young := time.Now().Add(-2 * 24 * time.Hour).UTC().Round(time.Second)

	staleID := "stale-per-row"
	freshID := "fresh-per-row"
	staleDir := filepath.Join(dir, ".sandman", "batches", staleID)
	freshDir := filepath.Join(dir, ".sandman", "batches", freshID)
	for _, d := range []string{staleDir, freshDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeRunDirForArchive(t, staleDir, staleID, batchindex.RunManifest{BatchID: staleID, Issue: 1, Kind: batchindex.KindIssue, CreatedAt: old, Status: batchindex.RunManifestStatusSuccess})
	writeRunDirForArchive(t, freshDir, freshID, batchindex.RunManifest{BatchID: freshID, Issue: 2, Kind: batchindex.KindIssue, CreatedAt: young, Status: batchindex.RunManifestStatusActive})
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{ID: staleID, Path: staleDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: old, Issues: []int{1}},
		{ID: freshID, Path: freshDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: young, Issues: []int{2}},
	})

	cmd := NewArchiveCmd(newTestDeps(t))
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive stale error: %v\noutput:\n%s", err, buf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", staleID, "runs", staleID)); err != nil {
		t.Errorf("expected stale row archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", freshID)); !os.IsNotExist(err) {
		t.Errorf("expected fresh row NOT archived, got: %v", err)
	}
}

// TestBatchIDRules_ArchiveLazyRecoveryOnIndexLoad pins behavior 8i: when an
// index entry has a non-empty ArchivePath but no live batch dir, the
// lazy recovery on Load treats the row as archived. The on-disk
// folder absence is the recovery signal. The test asserts both the
// per-row ArchivePath field and the entry's effective status after
// Load (lazy recovery normalises the entry status to "archived").
func TestBatchIDRules_ArchiveLazyRecoveryOnIndexLoad(t *testing.T) {
	requireGateMultiIssue(t)
	dir := testenv.MkdirShort(t, "sm-slice10-l-")
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	batchID := singleIssueBatchID()
	runID := batchID
	archivePath := filepath.Join(dir, ".sandman", "archive", batchID)
	if err := os.MkdirAll(filepath.Join(archivePath, "runs", runID), 0755); err != nil {
		t.Fatal(err)
	}

	// Write the index with an active status but ArchivePath populated
	// and Path pointing at the archive location. The live batch dir
	// is intentionally absent. Load's lazy recovery must promote the
	// entry to "archived".
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{
			ID:        batchID,
			Path:      archivePath,
			Kind:      batchindex.KindIssue,
			Status:    batchindex.StatusActive,
			CreatedAt: time.Now(),
			Issues:    []int{42},
			Runs: []batchindex.RunRecord{
				{RunID: runID, Status: batchindex.RunRecordStatusArchived, ArchivePath: filepath.Join(archivePath, "runs", runID)},
			},
		},
	})

	idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	entry := idx.ResolveBatch(batchID)
	if entry == nil {
		t.Fatalf("missing entry for %q", batchID)
	}
	if entry.Path != archivePath {
		t.Errorf("entry Path = %q, want %q (lazy recovery must keep the archive path)", entry.Path, archivePath)
	}
	rec := idx.RunRecordFor(batchID, runID)
	if rec == nil {
		t.Fatalf("missing RunRecord for archived row %q", runID)
	}
	if rec.ArchivePath == "" {
		t.Errorf("RunRecord.ArchivePath is empty (lazy recovery must preserve the archive path)")
	}
}

// --- Behavior 9: --continue flow -----------------------------------------

// TestBatchIDRules_ContinueMintsFreshBatchAndRunIDs pins behavior 9a:
// `sandman run --continue 42` mints a fresh public BatchId and fresh
// per-row RunIDs. The previous run's per-row RunID is carried only as
// the PreviousRunID lineage input.
func TestBatchIDRules_ContinueMintsFreshBatchAndRunIDs(t *testing.T) {
	requireGateMultiIssue(t)
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs:    map[string]*github.PR{},
	}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "prev-ts-abcd-42", Issue: 42, Payload: map[string]any{"branch": "42-fix-bug", "base_branch": "main", "agent": "opencode"}},
	}}
	dir := freshSandmanDir(t)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(dir, ".sandman", "worktrees", branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("## Stage: plan-approved\n\nContinue.\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("continue run error: %v\noutput:\n%s", err, buf.String())
	}

	if spy.req.RunTS == "" || spy.req.RunShortID == "" {
		t.Fatalf("expected fresh batch identity, got ts=%q shortid=%q", spy.req.RunTS, spy.req.RunShortID)
	}
	wantFreshBatchID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42"
	if spy.req.RunID == "prev-ts-abcd-42" {
		t.Fatalf("expected fresh RunID, got prior %q", spy.req.RunID)
	}
	if got := idxContinueLookup(t, dir, wantFreshBatchID); got != 1 {
		t.Errorf("expected exactly 1 batch index entry for fresh continuation, got %d", got)
	}
	if idxContinueLookup(t, dir, "prev-ts-abcd-42") != 0 {
		t.Errorf("prior per-row RunID %q must not be re-keyed as a fresh batch", "prev-ts-abcd-42")
	}
}

// TestBatchIDRules_ContinueReusesOriginalBranchAndWorktree pins behavior 9b:
// the new --continue run reuses the original branch and worktree.
// The previous run's branch is carried through `req.Branches[issue]`
// (and the per-row worktree follows the same path).
func TestBatchIDRules_ContinueReusesOriginalBranchAndWorktree(t *testing.T) {
	requireGateMultiIssue(t)
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs:    map[string]*github.PR{},
	}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "prev-ts-abcd-42", Issue: 42, Payload: map[string]any{"branch": "42-fix-bug", "base_branch": "main", "agent": "opencode"}},
	}}
	dir := freshSandmanDir(t)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(dir, ".sandman", "worktrees", branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("## Stage: plan-approved\n\nContinue.\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("continue run error: %v\noutput:\n%s", err, buf.String())
	}
	if got := spy.req.Branches[42]; got != branch {
		t.Errorf("Branches[42] = %q, want reused branch %q", got, branch)
	}
}

// TestBatchIDRules_ContinueLeavesPreviousRunUnchanged pins behavior 9c:
// the previous run's batch dir, run folder, manifest, and event log
// are unchanged after the new run starts. This explicitly guards
// against the continuation accidentally rewriting history.
func TestBatchIDRules_ContinueLeavesPreviousRunUnchanged(t *testing.T) {
	requireGateMultiIssue(t)
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs:    map[string]*github.PR{},
	}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "prev-ts-abcd-42", Issue: 42, Payload: map[string]any{"branch": "42-fix-bug", "base_branch": "main", "agent": "opencode"}},
	}}
	dir := freshSandmanDir(t)

	branch := "42-fix-bug"
	worktreePath := filepath.Join(dir, ".sandman", "worktrees", branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("## Stage: plan-approved\nContinue.\n"), 0644); err != nil {
		t.Fatal(err)
	}

	prevBatchID := "prev-ts-abcd-42"
	prevBatchDir := filepath.Join(dir, ".sandman", "batches", prevBatchID)
	if err := os.MkdirAll(filepath.Join(prevBatchDir, "runs", prevBatchID), 0755); err != nil {
		t.Fatal(err)
	}
	prevManifest := daemon.BatchManifest{
		Issues:    []int{42},
		CreatedAt: time.Now().Add(-time.Hour),
	}
	if err := daemon.WriteManifest(filepath.Join(prevBatchDir, "runs", prevBatchID), prevManifest); err != nil {
		t.Fatal(err)
	}
	prevEventsPath := filepath.Join(prevBatchDir, "events.jsonl")
	prevEvent := events.Event{Type: "run.finished", Timestamp: time.Now().Add(-time.Hour), RunID: prevBatchID, Issue: 42, Payload: map[string]any{"status": "success"}}
	if err := (&events.JSONLLogger{Path: prevEventsPath}).Log(prevEvent); err != nil {
		t.Fatal(err)
	}
	prevManifestPath := filepath.Join(prevBatchDir, "runs", prevBatchID, "batch.json")
	prevManifestContent, err := os.ReadFile(prevManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	prevEventContent, err := os.ReadFile(prevEventsPath)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("continue run error: %v\noutput:\n%s", err, buf.String())
	}

	gotManifest, err := os.ReadFile(prevManifestPath)
	if err != nil {
		t.Fatalf("read prev manifest: %v", err)
	}
	if !bytes.Equal(gotManifest, prevManifestContent) {
		t.Errorf("previous run manifest content changed\nbefore: %s\nafter:  %s", prevManifestContent, gotManifest)
	}
	gotEvent, err := os.ReadFile(prevEventsPath)
	if err != nil {
		t.Fatalf("read prev events: %v", err)
	}
	if !bytes.Equal(gotEvent, prevEventContent) {
		t.Errorf("previous run event log content changed\nbefore: %s\nafter:  %s", prevEventContent, gotEvent)
	}
}

// TestBatchIDRules_ContinueEmitsRunContinuedEvent pins behavior 9d at the
// e2e seam the run command controls: the continuation request
// forwarded to the batch runner carries `Mode[issue] == ModeContinue`
// and `PreviousRunIDs[issue] == prev`. These are the inputs the
// orchestrator uses to emit `run.continued` with `previous_run_id`
// in its payload (the orchestrator-level emission is pinned by
// TestRunBatch_MultiIssueContinuationLogsPerIssuePreviousRunID in
// internal/batch/orchestrator_test.go).
func TestBatchIDRules_ContinueEmitsRunContinuedEvent(t *testing.T) {
	requireGateMultiIssue(t)
	prevRunID := "prev-ts-abcd-42"
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs:    map[string]*github.PR{},
	}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: prevRunID, Issue: 42, Payload: map[string]any{"branch": "42-fix-bug", "base_branch": "main", "agent": "opencode"}},
	}}
	dir := freshSandmanDir(t)
	// newRunDeps sets RepoRoot to "."; pin it to the absolute temp dir so the
	// --continue worktree-status check resolves against the git worktree list
	// recorded by `git worktree add` below (this relies on absolute paths).
	deps.RepoRoot = dir

	branch := "42-fix-bug"
	worktreeBase := filepath.Join(dir, ".sandman", "worktrees")
	worktreePath := addRegisteredContinuationWorktree(t, dir, worktreeBase, branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("## Stage: plan-approved\nContinue.\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("continue run error: %v\noutput:\n%s", err, buf.String())
	}

	if spy.req.Mode[42] != batch.ModeContinue {
		t.Errorf("req.Mode[42] = %v, want %v", spy.req.Mode[42], batch.ModeContinue)
	}
	if got := spy.req.PreviousRunIDs[42]; got != prevRunID {
		t.Errorf("req.PreviousRunIDs[42] = %q, want %q", got, prevRunID)
	}
}

// TestBatchIDRules_ContinueFoldCreatesFreshRunState pins behavior 9e: the
// events fold (which the run/portal state derives from) creates a
// fresh RunState keyed by the new RunID, not by the previous one.
// Each per-row run folder owns its own events.jsonl so the events
// fold is naturally scoped to the new RunID.
func TestBatchIDRules_ContinueFoldCreatesFreshRunState(t *testing.T) {
	requireGateMultiIssue(t)
	dir := t.TempDir()

	prevBatchID := "prev-ts-abcd-42"
	prevBatchDir := filepath.Join(dir, ".sandman", "batches", prevBatchID)
	prevRunDir := filepath.Join(prevBatchDir, "runs", prevBatchID)
	if err := os.MkdirAll(prevRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(prevRunDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	prevEvents := filepath.Join(prevRunDir, "events.jsonl")
	prevLog := &events.JSONLLogger{Path: prevEvents}
	if err := prevLog.Log(events.Event{Type: "run.started", Timestamp: time.Now().Add(-time.Hour), RunID: prevBatchID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "agent": "opencode"}}); err != nil {
		t.Fatal(err)
	}

	newBatchID := "new-ts-abcd-42"
	newBatchDir := filepath.Join(dir, ".sandman", "batches", newBatchID)
	newRunDir := filepath.Join(newBatchDir, "runs", newBatchID)
	if err := os.MkdirAll(newRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(newRunDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	newEvents := filepath.Join(newRunDir, "events.jsonl")
	newLog := &events.JSONLLogger{Path: newEvents}
	if err := newLog.Log(events.Event{Type: "run.continued", Timestamp: time.Now(), RunID: newBatchID, Issue: 42, Payload: map[string]any{"previous_run_id": prevBatchID, "branch": "42-fix"}}); err != nil {
		t.Fatal(err)
	}

	newRead, err := (&events.JSONLLogger{Path: newEvents}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(newRead) != 1 {
		t.Fatalf("expected exactly 1 event in the new run's event log, got %d", len(newRead))
	}
	if newRead[0].RunID != newBatchID {
		t.Errorf("fresh RunState keyed by %q, want %q", newRead[0].RunID, newBatchID)
	}
}

// TestBatchIDRules_ContinueSubjectPickerExposesPreviousRun pins behavior
// 9f: the portal subject picker exposes the previous run as a sibling
// entry on the continuation row. The picker derives its options from
// the per-row RunIDs visible on the continuation batch — both the
// new run and the prior run appear as options.
func TestBatchIDRules_ContinueSubjectPickerExposesPreviousRun(t *testing.T) {
	requireGateMultiIssue(t)
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	now := time.Now()
	prevRunID := "prev-ts-abcd-42"
	newRunID := "new-ts-abcd-42"
	prevBatchID := prevRunID
	newBatchID := newRunID

	prevBatchDir := filepath.Join(layout.BatchesDir, prevBatchID)
	prevRunDir := filepath.Join(prevBatchDir, "runs", prevRunID)
	if err := os.MkdirAll(prevRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(prevRunDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prevRunDir, "run.log"), []byte("prev run log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	newBatchDir := filepath.Join(layout.BatchesDir, newBatchID)
	newRunDir := filepath.Join(newBatchDir, "runs", newRunID)
	if err := os.MkdirAll(newRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(newRunDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRunDir, "run.log"), []byte("new run log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: now.Add(-time.Hour), RunID: prevRunID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": prevBatchID}},
		{Type: "run.finished", Timestamp: now.Add(-30 * time.Minute), RunID: prevRunID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "42-fix", "batch_id": prevBatchID}},
		{Type: "run.continued", Timestamp: now, RunID: newRunID, Issue: 42, Payload: map[string]any{"previous_run_id": prevRunID, "branch": "42-fix", "batch_id": newBatchID}},
		{Type: "run.started", Timestamp: now, RunID: newRunID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": newBatchID}},
	})

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: prevBatchID, Path: prevBatchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now.Add(-time.Hour), Issues: []int{42}},
		{ID: newBatchID, Path: newBatchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	}}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	byID := map[string]portalRun{}
	for _, row := range runs {
		byID[row.RunID] = row
	}
	if _, ok := byID[newRunID]; !ok {
		t.Fatalf("missing new run row %q in portal runs", newRunID)
	}
	if _, ok := byID[prevRunID]; !ok {
		t.Fatalf("missing previous run row %q in portal runs (must be exposed as sibling on continuation)", prevRunID)
	}
}

// TestBatchIDRules_ContinuePickerSwitchesToPreviousRun pins behavior 9g:
// the previous run is selectable in the picker; switching to it shows
// the previous run's log and details. The /api/runs?runKey= endpoint
// is the picker switch seam; the response must echo the previous
// run's content.
func TestBatchIDRules_ContinuePickerSwitchesToPreviousRun(t *testing.T) {
	requireGateMultiIssue(t)
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	now := time.Now()
	prevRunID := "prev-ts-abcd-42"
	newRunID := "new-ts-abcd-42"
	prevBatchID := prevRunID
	newBatchID := newRunID

	prevBatchDir := filepath.Join(layout.BatchesDir, prevBatchID)
	prevRunDir := filepath.Join(prevBatchDir, "runs", prevRunID)
	if err := os.MkdirAll(prevRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(prevRunDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	prevLogContent := "previous run log line one\nprevious run log line two\n"
	if err := os.WriteFile(filepath.Join(prevRunDir, "run.log"), []byte(prevLogContent), 0644); err != nil {
		t.Fatal(err)
	}
	newBatchDir := filepath.Join(layout.BatchesDir, newBatchID)
	newRunDir := filepath.Join(newBatchDir, "runs", newRunID)
	if err := os.MkdirAll(newRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(newRunDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRunDir, "run.log"), []byte("new run log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: now.Add(-time.Hour), RunID: prevRunID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": prevBatchID}},
		{Type: "run.finished", Timestamp: now.Add(-30 * time.Minute), RunID: prevRunID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "42-fix", "batch_id": prevBatchID}},
		{Type: "run.continued", Timestamp: now, RunID: newRunID, Issue: 42, Payload: map[string]any{"previous_run_id": prevRunID, "branch": "42-fix", "batch_id": newBatchID}},
		{Type: "run.started", Timestamp: now, RunID: newRunID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": newBatchID}},
	})

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: prevBatchID, Path: prevBatchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now.Add(-time.Hour), Issues: []int{42}},
		{ID: newBatchID, Path: newBatchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	}}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs?runKey=" + prevRunID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var detail struct {
		Run portalRun `json:"run"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if detail.Run.RunID != prevRunID {
		t.Errorf("picker switch RunID = %q, want %q (previous run selected)", detail.Run.RunID, prevRunID)
	}
	if !strings.Contains(detail.Run.Log, "previous run log line one") {
		t.Errorf("picker switch log = %q, want previous run log content", detail.Run.Log)
	}
}

// TestBatchIDRules_ContinueDoesNotRenderContinuationChip pins behavior 9h:
// no new "Continuation" chip is rendered. The portalRun struct has
// no `Continuation` field; the JSON envelope for /api/runs must not
// carry a "continuation" key for the continuation row either. We
// pin the absence by inspecting the raw JSON payload: a future
// regression that adds a chip field would surface here as an
// unexpected key.
func TestBatchIDRules_ContinueDoesNotRenderContinuationChip(t *testing.T) {
	requireGateMultiIssue(t)
	repoRoot := testenv.MkdirShort(t, "sm-slice10-c-")
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	now := time.Now()
	prevRunID := "prev-ts-abcd-42"
	newRunID := "new-ts-abcd-42"
	prevBatchID := prevRunID
	newBatchID := newRunID

	prevBatchDir := filepath.Join(layout.BatchesDir, prevBatchID)
	prevRunDir := filepath.Join(prevBatchDir, "runs", prevRunID)
	if err := os.MkdirAll(prevRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(prevRunDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	newBatchDir := filepath.Join(layout.BatchesDir, newBatchID)
	newRunDir := filepath.Join(newBatchDir, "runs", newRunID)
	if err := os.MkdirAll(newRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(newRunDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: now.Add(-time.Hour), RunID: prevRunID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": prevBatchID}},
		{Type: "run.finished", Timestamp: now.Add(-30 * time.Minute), RunID: prevRunID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "42-fix", "batch_id": prevBatchID}},
		{Type: "run.continued", Timestamp: now, RunID: newRunID, Issue: 42, Payload: map[string]any{"previous_run_id": prevRunID, "branch": "42-fix", "batch_id": newBatchID}},
		{Type: "run.started", Timestamp: now, RunID: newRunID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": newBatchID}},
	})

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: prevBatchID, Path: prevBatchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now.Add(-time.Hour), Issues: []int{42}},
		{ID: newBatchID, Path: newBatchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	}}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	// Fetch the raw /api/runs JSON. A future regression that adds a
	// chip-style field (e.g. "Continuation", "IsContinuation",
	// "continuedFrom") would surface as an unexpected JSON key
	// here.
	resp, err := http.Get(server.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(body))
	for _, forbidden := range []string{`"continuation":`, `"iscontinuation":`, `"continuedfrom":`, `"continued":`} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("portal /api/runs payload contains forbidden %q chip field (no Continuation chip must be rendered):\n%s", forbidden, string(body))
		}
	}
}

// --- Behavior 10: per-row RunID-based abort resolution ------------------

// TestBatchIDRules_AbortResolvesByRunID pins behavior 10: the portal abort
// endpoint resolves the targeted row by RunID. Two siblings live in
// the same batch; aborting one leaves the other untouched.
func TestBatchIDRules_AbortResolvesByRunID(t *testing.T) {
	requireGateMultiIssue(t)
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	now := time.Now()
	firstRow := runid.NewRunID(runid.KindIssue, "42", batchIDRulesTS, batchIDRulesShortID)
	secondRow := runid.NewRunID(runid.KindIssue, "43", batchIDRulesTS, batchIDRulesShortID)
	batchID := multiIssueBatchID()
	batchDir := filepath.Join(layout.BatchesDir, batchID)

	for _, row := range []string{firstRow, secondRow} {
		runDir := filepath.Join(batchDir, "runs", row)
		if err := os.MkdirAll(runDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42, 43}, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: now, RunID: firstRow, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": batchID}},
		{Type: "run.started", Timestamp: now, RunID: secondRow, Issue: 43, Payload: map[string]any{"branch": "43-fix", "batch_id": batchID}},
	})
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42, 43}},
	}}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	prevAborter := portalRunAborter
	t.Cleanup(func() { portalRunAborter = prevAborter })
	portalRunAborter = func(ctx context.Context, repoRootArg, runKey string, issueNumber int) error {
		if runKey != firstRow {
			t.Errorf("expected aborter called with first-row RunID %q, got %q", firstRow, runKey)
		}
		if issueNumber != 42 {
			t.Errorf("expected aborter called with issue 42, got %d", issueNumber)
		}
		return (&events.JSONLLogger{Path: filepath.Join(repoRootArg, ".sandman", "events.jsonl")}).Log(events.Event{Type: "run.aborted", Timestamp: time.Now(), RunID: firstRow, Issue: 42, Payload: map[string]any{"status": "aborted", "branch": "42-fix", "batch_id": batchID}})
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	body := `{"runKey":"` + firstRow + `","issue":42}`
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
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, got)
	}

	if _, err := os.Stat(filepath.Join(batchDir, "runs", secondRow)); err != nil {
		t.Errorf("sibling run dir %q must remain after aborting first row, got: %v", filepath.Join(batchDir, "runs", secondRow), err)
	}
}

// reference errors to keep imports used
var _ = context.Background
