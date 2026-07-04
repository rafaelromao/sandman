package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/runid"
)

// abortSpy records AbortIssue invocations the per-run command socket
// forwards to the orchestrator. It is intentionally trivial: the
// regression tests in this file only need to assert that the abort
// request reached the right per-run socket and carried the expected
// Issue number for the row's shape.
type abortSpy struct {
	mu     sync.Mutex
	issues []int
}

func (s *abortSpy) AbortIssue(issueNumber int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues = append(s.issues, issueNumber)
	return nil
}

func (s *abortSpy) Snapshot() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, len(s.issues))
	copy(out, s.issues)
	return out
}

// startAbortCommandServer mimics daemon.CommandServer on the per-run
// socket: it accepts one connection, decodes the JSON abort request,
// forwards it to the spy, and writes back daemon.CommandResponse{ok}.
// Closing done signals the goroutine completed a round trip. The
// listener is registered for cleanup via t.Cleanup; the test does
// not need to close it.
func startAbortCommandServer(t *testing.T, sockPath string, spy *abortSpy) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req struct {
			Action string `json:"action"`
			Issue  int    `json:"issue"`
		}
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		_ = spy.AbortIssue(req.Issue)
		_ = json.NewEncoder(conn).Encode(daemon.CommandResponse{Status: "ok"})
		select {
		case done <- struct{}{}:
		default:
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
}

// stubPortalPeerPIDForAbort replaces portalPeerPID with a stub that
// returns a fake PID for the expected resolved per-run socket. The
// stub fails the test if the abort handler resolves to a different
// socket path, which is the regression that the slice is locking
// down.
func stubPortalPeerPIDForAbort(t *testing.T, expectedSock string) {
	t.Helper()
	prev := portalPeerPID
	t.Cleanup(func() { portalPeerPID = prev })
	portalPeerPID = func(sockPath string) (int, error) {
		if sockPath != expectedSock {
			t.Fatalf("expected resolved per-run socket %q, got %q", expectedSock, sockPath)
		}
		return 12345, nil
	}
}

// portalAbortBatchKindsFixture sets up the on-disk layout that the
// portal reads: a batches.json index entry pointing at the batch dir,
// a batch.sock listener, a per-run run.sock listener, the batch
// manifest, the per-row run.json, and a run.started event. It returns
// the repoRoot and the per-row RunID the portal row is keyed by.
//
// The repoRoot is created under /tmp with a short prefix rather than
// t.TempDir() because t.TempDir's full path can push the per-run
// socket path past the 108-byte Unix sun_path limit.
func portalAbortBatchKindsFixture(t *testing.T, opts portalAbortBatchKindsOpts) (repoRoot, perRowID string) {
	t.Helper()
	shortRoot, err := os.MkdirTemp("/tmp", "sm-abk-")
	if err != nil {
		t.Fatal(err)
	}
	repoRoot = shortRoot
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchDirName := opts.batchDirName
	if batchDirName == "" {
		batchDirName = opts.batchKey
	}
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchDirName)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if opts.batchManifest != nil {
		if err := daemon.WriteManifest(batchDir, *opts.batchManifest); err != nil {
			t.Fatal(err)
		}
	} else if opts.review {
		pr := opts.pr
		batchManifest := daemon.BatchManifest{
			RunKind:   "review",
			BatchId:   opts.batchKey,
			CreatedAt: time.Now().Add(-10 * time.Minute),
			PR:        &pr,
		}
		if opts.issueNumber > 0 {
			batchManifest.Issues = []int{opts.issueNumber}
		}
		if err := daemon.WriteManifest(batchDir, batchManifest); err != nil {
			t.Fatal(err)
		}
	} else if len(opts.issues) > 0 {
		if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: opts.issues, CreatedAt: time.Now().Add(-10 * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if opts.perRowID != "" {
		perRowDir := filepath.Join(batchDir, "runs", opts.perRowID)
		if err := os.MkdirAll(perRowDir, 0755); err != nil {
			t.Fatal(err)
		}
		if opts.perRowManifest != nil {
			if err := batchindex.WriteManifest(perRowDir, *opts.perRowManifest); err != nil {
				t.Fatal(err)
			}
		} else if opts.review {
			perRowManifest := batchindex.RunManifest{
				RunID:     opts.perRowID,
				BatchID:   opts.batchKey,
				Issue:     opts.issueNumber,
				PR:        opts.pr,
				Kind:      batchindex.KindReview,
				Branch:    "sandman/" + opts.branch + "-fix",
				CreatedAt: time.Now().Add(-10 * time.Minute),
				Status:    batchindex.RunManifestStatusActive,
			}
			if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
				t.Fatal(err)
			}
		} else {
			perRowManifest := batchindex.RunManifest{
				RunID:     opts.perRowID,
				BatchID:   opts.batchKey,
				Issue:     opts.issueNumber,
				Kind:      batchindex.KindIssue,
				Branch:    "sandman/" + opts.branch + "-fix",
				CreatedAt: time.Now().Add(-10 * time.Minute),
				Status:    batchindex.RunManifestStatusActive,
			}
			if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
				t.Fatal(err)
			}
		}
	}

	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Entries: []batchindex.Entry{
		{ID: opts.batchKey, Path: batchDir, Kind: opts.idxKind, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: opts.issues, PR: opts.pr},
	}}
	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := os.MkdirAll(filepath.Dir(idxPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	runID := opts.perRowID
	if runID == "" {
		runID = opts.batchKey
	}
	ev := events.Event{
		Type:      "run.started",
		Timestamp: startedAt,
		RunID:     runID,
		Issue:     opts.issueNumber,
		Payload:   map[string]any{"branch": "sandman/" + opts.branch + "-fix"},
	}
	if opts.review {
		ev.Payload["review"] = true
		if opts.pr > 0 {
			ev.Payload["pr_number"] = opts.pr
		}
	}
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{ev})
	return repoRoot, opts.perRowID
}

type portalAbortBatchKindsOpts struct {
	batchKey       string
	batchDirName   string // optional: overrides batchKey as the on-disk dir name (post-#1675 entry id may differ from batchDirName for multi-issue)
	perRowID       string
	idxKind        batchindex.Kind
	issues         []int
	pr             int
	issueNumber    int
	branch         string
	review         bool
	batchManifest  *daemon.BatchManifest
	perRowManifest *batchindex.RunManifest
}

// TestPortal_AbortEndpoint_SingleIssueRunRow_ResolvesPerRunSocket is
// the tracer bullet for the slice: a single-issue `sandman run` row
// where n=1, so the batch entry id carries a "+1" suffix and the
// per-row id does not. The abort handler takes the `runID !=
// batchKey` branch (portal.go:353), resolves perRunID to the per-row
// id, and dispatches the abort request to the per-run socket at
// `<batchDir>/runs/<perRowID>/run.sock`. The orchestrator must
// receive `Issue=42`.
func TestPortal_AbortEndpoint_SingleIssueRunRow_ResolvesPerRunSocket(t *testing.T) {
	if !portalAbortSupported() {
		t.Skip("abort unsupported on this platform")
	}
	ts := "260618113825"
	shortid := "abcd"
	batchKey := runid.NewBatchID(runid.KindIssue, 1, "42", ts, shortid)
	perRowID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)
	if perRowID == batchKey {
		t.Fatalf("fixture invariant: perRowID %q must differ from batchKey %q", perRowID, batchKey)
	}

	repoRoot, _ := portalAbortBatchKindsFixture(t, portalAbortBatchKindsOpts{
		batchKey:    batchKey,
		perRowID:    perRowID,
		idxKind:     batchindex.KindIssue,
		issues:      []int{42},
		issueNumber: 42,
		branch:      "42",
	})

	perRunSock := filepath.Join(repoRoot, ".sandman", "batches", batchKey, "runs", perRowID, "run.sock")
	spy := &abortSpy{}
	startAbortCommandServer(t, perRunSock, spy)
	stubPortalPeerPIDForAbort(t, perRunSock)

	prevStale := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = prevStale })

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/abort", strings.NewReader(`{"runKey":"`+perRowID+`","issue":42}`))
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

	calls := spy.Snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected spy to receive 1 abort, got %d (issues=%v)", len(calls), calls)
	}
	if calls[0] != 42 {
		t.Fatalf("expected orchestrator to receive Issue=42, got %d", calls[0])
	}
}

// TestPortal_AbortEndpoint_ContinueReviewRow_ResolvesPerRunSocket
// covers a review row with a linked issue (continue review): the
// per-row id carries `<issue>-PR<n>` while the batch entry id is
// `PR<n>`. The abort handler takes the `runID != batchKey` branch
// (portal.go:353) and resolves perRunID to the per-row id. The
// per-run socket lives at `<batchDir>/runs/<perRowID>/run.sock` and
// the orchestrator must receive `Issue=42` (the linked issue, not
// the PR number, not 0).
func TestPortal_AbortEndpoint_ContinueReviewRow_ResolvesPerRunSocket(t *testing.T) {
	if !portalAbortSupported() {
		t.Skip("abort unsupported on this platform")
	}
	ts := "260618113825"
	shortid := "abcd"
	batchKey := runid.NewBatchID(runid.KindReview, 1, "99", ts, shortid)
	perRowID := runid.NewRunID(runid.KindReview, "42-PR99", ts, shortid)
	if perRowID == batchKey {
		t.Fatalf("fixture invariant: perRowID %q must differ from batchKey %q", perRowID, batchKey)
	}

	repoRoot, _ := portalAbortBatchKindsFixture(t, portalAbortBatchKindsOpts{
		batchKey:    batchKey,
		perRowID:    perRowID,
		idxKind:     batchindex.KindReview,
		pr:          99,
		issueNumber: 42,
		branch:      "42",
		review:      true,
	})

	perRunSock := filepath.Join(repoRoot, ".sandman", "batches", batchKey, "runs", perRowID, "run.sock")
	spy := &abortSpy{}
	startAbortCommandServer(t, perRunSock, spy)
	stubPortalPeerPIDForAbort(t, perRunSock)

	prevStale := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = prevStale })

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/abort", strings.NewReader(`{"runKey":"`+perRowID+`","issue":42}`))
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

	calls := spy.Snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected spy to receive 1 abort, got %d (issues=%v)", len(calls), calls)
	}
	if calls[0] != 42 {
		t.Fatalf("expected orchestrator to receive Issue=42 (linked issue), got %d", calls[0])
	}
}

// TestPortal_AbortEndpoint_ContinueIssueRunRow_ResolvesPerRunSocket
// covers a `--continue` issue run on a multi-issue batch. After #1675
// the per-row RunID is `<sid>-<ts>-<num>` (NOT the batch dir name
// `<sid>-<ts>-<num>+N`). The batch index entry id is the per-row RunID
// of the canonical row (first issue), so `run.RunID == run.BatchKey ==
// perRowID` (both equal the per-row RunID). The abort handler takes
// the `runID == batchKey` branch at portal.go:357; the discriminator
// must use `manifest.RunKind` to detect orphan reviews (where the
// per-row RunID is the batch dir name) instead of falling through to
// `filepath.Base(runDir)` for issue runs. The per-run socket lives at
// `<batchDir>/runs/<perRowID>/run.sock` and the orchestrator must
// receive `Issue=42`.
func TestPortal_AbortEndpoint_ContinueIssueRunRow_ResolvesPerRunSocket(t *testing.T) {
	if !portalAbortSupported() {
		t.Skip("abort unsupported on this platform")
	}
	ts := "260618113825"
	shortid := "abcd"
	batchDirName := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	perRowID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)
	if perRowID == batchDirName {
		t.Fatalf("fixture invariant: perRowID %q must differ from batchDirName %q", perRowID, batchDirName)
	}

	batchManifest := daemon.BatchManifest{
		Issues:    []int{42, 43},
		BatchId:   perRowID, // post-#1675: BatchId == per-row RunID for the canonical row
		RunKind:   "issue",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	repoRoot, _ := portalAbortBatchKindsFixture(t, portalAbortBatchKindsOpts{
		batchKey:      perRowID,     // index entry id == per-row RunID of canonical row
		batchDirName:  batchDirName, // ADR-0030 batch dir name `<sid>-<ts>-42+2`
		perRowID:      perRowID,
		idxKind:       batchindex.KindIssue,
		issues:        []int{42, 43},
		issueNumber:   42,
		branch:        "42",
		batchManifest: &batchManifest,
	})

	perRunSock := filepath.Join(repoRoot, ".sandman", "batches", batchDirName, "runs", perRowID, "run.sock")
	spy := &abortSpy{}
	startAbortCommandServer(t, perRunSock, spy)
	stubPortalPeerPIDForAbort(t, perRunSock)

	prevStale := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = prevStale })

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/abort", strings.NewReader(`{"runKey":"`+perRowID+`","issue":42}`))
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

	calls := spy.Snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected spy to receive 1 abort, got %d (issues=%v)", len(calls), calls)
	}
	if calls[0] != 42 {
		t.Fatalf("expected orchestrator to receive Issue=42, got %d", calls[0])
	}
}

// TestAbortPortalRun_OrphanReviewRow_ResolvesPerRunSocket covers an
// orphan review row (PR with no linked issue): the per-row id is
// `PR<n>` and equals the batch entry id, so the abort handler takes
// the `runID == batchKey` branch (portal.go:357). The batch manifest
// has `RunKind: review` and `PR: <n>` but no `Issues`; the row's
// `IssueNumber` is 0. The HTTP handler rejects `Issue <= 0`
// (portal_handler.go:152), so this test calls `abortPortalRun`
// directly with `issueNumber: 0` and asserts the spy receives
// `AbortIssue(0)`.
func TestAbortPortalRun_OrphanReviewRow_ResolvesPerRunSocket(t *testing.T) {
	if !portalAbortSupported() {
		t.Skip("abort unsupported on this platform")
	}
	ts := "260618113825"
	shortid := "abcd"
	batchKey := runid.NewBatchID(runid.KindReview, 1, "100", ts, shortid)
	perRowID := runid.NewBatchID(runid.KindReview, 1, "100", ts, shortid)
	if perRowID != batchKey {
		t.Fatalf("fixture invariant: orphan-review perRowID %q must equal batchKey %q", perRowID, batchKey)
	}

	prVal := 100
	batchManifest := daemon.BatchManifest{
		RunKind:   "review",
		BatchId:   batchKey,
		CreatedAt: time.Now().Add(-10 * time.Minute),
		PR:        &prVal,
	}
	repoRoot, _ := portalAbortBatchKindsFixture(t, portalAbortBatchKindsOpts{
		batchKey:      batchKey,
		perRowID:      perRowID,
		idxKind:       batchindex.KindReview,
		pr:            100,
		issueNumber:   0,
		branch:        "100",
		review:        true,
		batchManifest: &batchManifest,
	})

	perRunSock := filepath.Join(repoRoot, ".sandman", "batches", batchKey, "runs", batchKey, "run.sock")
	spy := &abortSpy{}
	startAbortCommandServer(t, perRunSock, spy)
	stubPortalPeerPIDForAbort(t, perRunSock)

	prevStale := portalStaleCleaner
	portalStaleCleaner = func(string) error { return nil }
	t.Cleanup(func() { portalStaleCleaner = prevStale })

	if err := abortPortalRun(context.Background(), repoRoot, batchKey, 0); err != nil {
		t.Fatalf("abort portal run for orphan review: %v", err)
	}

	calls := spy.Snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected spy to receive 1 abort, got %d (issues=%v)", len(calls), calls)
	}
	if calls[0] != 0 {
		t.Fatalf("expected orchestrator to receive Issue=0 (orphan review), got %d", calls[0])
	}
}
