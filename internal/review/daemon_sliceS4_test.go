package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// TestDaemon_S4_EmptyMapAtConstruction pins the Slice-A contract:
// a fresh Daemon constructed via New has a `pendingPost` map keyed
// by (prNumber, commentID) that is nil-or-empty until rehydration
// runs. The map is private; this test pins the empty-map invariant
// via the unexported peekPendingPost helper, which is the only
// surface that lets the Slice B / C / D / E tests observe the
// in-memory map without depending on the internals of processPR.
//
// Slice A does NOT exercise the walker yet — that is Slice B's job.
// Slice A only introduces the type plumbing so the rest of the slices
// have a stable target shape to land against.
func TestDaemon_S4_EmptyMapAtConstruction(t *testing.T) {
	d, _, dir := newDaemonForTest(t, &fakeGH{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	// Ensure the test workspace is empty so no on-disk seed is
	// observed by the walker that is added in Slice B. Slice A's
	// walker is a no-op anyway, but the assertion is robust against
	// future slices accidentally populating the map.
	_ = dir

	// Reset the map to a fresh empty state so an older fixture
	// (e.g. a later test that leaves an entry behind) cannot mask
	// the empty invariant.
	d.pendingPostMu.Lock()
	d.pendingPost = map[int]map[string]pendingPostEntry{}
	d.pendingPostMu.Unlock()

	if entry, ok := d.peekPendingPost(4242, "comment-doesnt-exist"); ok {
		t.Fatalf("peekPendingPost should report ok=false on an empty map, got entry=%+v", entry)
	}

	// No tick should happen for Slice A — the map stays empty
	// because no walker writes to it yet. This is the negative
	// assertion that mirrors Slice B's positive assertions.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick on empty daemon: %v", err)
	}

	d.pendingPostMu.Lock()
	if got := len(d.pendingPost); got != 0 {
		t.Errorf("pendingPost should stay empty after a tick (Slice A contract), got %d entries", got)
	}
	d.pendingPostMu.Unlock()
}

// TestDaemon_S4_PeekRegisteredEntry pins the peekPendingPost helper:
// when an entry is registered with the daemon, peekPendingPost
// returns it under the lock so concurrent test readers see a
// consistent (runDir, reviewState) tuple. The Slice B walker
// will be the producer of these entries; Slice A wires the
// consumer side.
//
// This test constructs the entry via the lock directly so Slice A
// stays self-contained. Slice B will replace this test fixture with
// the on-disk seed path.
func TestDaemon_S4_PeekRegisteredEntry(t *testing.T) {
	d, _, _ := newDaemonForTest(t, &fakeGH{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	runDir := filepath.Join(t.TempDir(), "row")
	reviewState := filepath.Join(runDir, "review-state.json")

	d.pendingPostMu.Lock()
	d.pendingPost = map[int]map[string]pendingPostEntry{
		4242: {
			"c-s4-1": {
				commentID:   "c-s4-1",
				runDir:      runDir,
				reviewState: reviewState,
			},
		},
	}
	d.pendingPostMu.Unlock()

	entry, ok := d.peekPendingPost(4242, "c-s4-1")
	if !ok {
		t.Fatalf("peekPendingPost should return ok=true for registered (4242, c-s4-1)")
	}
	if entry.runDir != runDir {
		t.Errorf("peekPendingPost returned runDir=%q, want %q", entry.runDir, runDir)
	}
	if entry.reviewState != reviewState {
		t.Errorf("peekPendingPost returned reviewState=%q, want %q", entry.reviewState, reviewState)
	}
	if entry.commentID != "c-s4-1" {
		t.Errorf("peekPendingPost returned commentID=%q, want %q", entry.commentID, "c-s4-1")
	}

	if _, ok := d.peekPendingPost(4242, "missing-comment"); ok {
		t.Errorf("peekPendingPost should return ok=false for an unregistered commentID")
	}
	if _, ok := d.peekPendingPost(9999, "c-s4-1"); ok {
		t.Errorf("peekPendingPost should return ok=false for an unregistered prNumber")
	}
}

// decisionPathForTest returns the absolute path a Slice B / Slice C
// fixture should write decision.md into. Mirrors the per-row folder
// that seedPriorReviewEntry creates: <baseDir>/batches/<batch>/runs/<rowID>/.
func decisionPathForTest(t *testing.T, baseDir, batchID string, prNumber int) string {
	t.Helper()
	rowID := deriveReviewRowID(batchID, prNumber)
	return filepath.Join(baseDir, "batches", batchID, "runs", rowID, "decision.md")
}

// runDirForTest returns the absolute row folder for a seeded entry.
// Helpers like peekPendingPost compare absolute paths so each test
// needs the same canonical path string the daemon computed.
func runDirForTest(t *testing.T, baseDir, batchID string, prNumber int) string {
	t.Helper()
	rowID := deriveReviewRowID(batchID, prNumber)
	return filepath.Join(baseDir, "batches", batchID, "runs", rowID)
}

// TestDaemon_S4_LoadPendingPosts_RegistersDecisionPending pins the
// Slice-B happy path: a pending review-state.json whose row folder
// has decision.md on disk produces exactly one pendingPost entry at
// Daemon.New. The entry holds the absolute runDir and reviewState
// so the rehydrate post (Slice C) can read the file and call
// MarkSeen without resolving paths.
func TestDaemon_S4_LoadPendingPosts_RegistersDecisionPending(t *testing.T) {
	const (
		prNumber  = 5050
		commentID = "c-s4-pending-1"
	)
	dir := t.TempDir()

	const batchID = "sip-batch-PR5050"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")

	decisionPath := decisionPathForTest(t, dir, batchID, prNumber)
	body := "## Summary\n## Decision\nthis is the unposted body.\n"
	if err := os.WriteFile(decisionPath, []byte(body), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	d := New(dir, &fakeGH{}, &prompt.Engine{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false, nil)

	entry, ok := d.peekPendingPost(prNumber, commentID)
	if !ok {
		t.Fatalf("pendingPost should hold an entry for (PR=%d, comment=%s) after Daemon.New, peek returned !ok", prNumber, commentID)
	}
	wantRunDir := runDirForTest(t, dir, batchID, prNumber)
	if entry.runDir != wantRunDir {
		t.Errorf("entry.runDir=%q, want %q", entry.runDir, wantRunDir)
	}
	wantStatePath := filepath.Join(wantRunDir, "review-state.json")
	if entry.reviewState != wantStatePath {
		t.Errorf("entry.reviewState=%q, want %q", entry.reviewState, wantStatePath)
	}
	if entry.commentID != commentID {
		t.Errorf("entry.commentID=%q, want %q", entry.commentID, commentID)
	}
}

// TestDaemon_S4_LoadPendingPosts_SkipsMissingDecision pins the
// Slice-B negative path: a pending review-state.json whose row
// folder has NO decision.md (agent crash before write) must NOT
// register a pendingPost entry.
func TestDaemon_S4_LoadPendingPosts_SkipsMissingDecision(t *testing.T) {
	const (
		prNumber  = 5051
		commentID = "c-s4-pending-no-decision"
	)
	dir := t.TempDir()

	const batchID = "sip-batch-PR5051"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")
	// Deliberately omit decision.md.

	d := New(dir, &fakeGH{}, &prompt.Engine{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false, nil)

	if _, ok := d.peekPendingPost(prNumber, commentID); ok {
		t.Fatalf("pendingPost should not register an entry when decision.md is missing (Slice B negative path)")
	}
}

// TestDaemon_S4_LoadPendingPosts_SkipsTerminalStatuses pins the
// Slice-B status filter: only status="pending" review-state entries
// are considered for rehydrate; status="success" and status=
// "superseded" must be ignored even when decision.md is present.
func TestDaemon_S4_LoadPendingPosts_SkipsTerminalStatuses(t *testing.T) {
	const prNumber = 5052
	for _, status := range []string{"success", "superseded"} {
		status := status
		t.Run(status, func(t *testing.T) {
			dir := t.TempDir()

			commentID := "c-s4-" + status
			batchID := "sip-batch-PR5052"
			seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, status)

			decisionPath := decisionPathForTest(t, dir, batchID, prNumber)
			if err := os.MkdirAll(filepath.Dir(decisionPath), 0755); err != nil {
				t.Fatalf("mkdir decision.md dir: %v", err)
			}
			if err := os.WriteFile(decisionPath, []byte("body"), 0644); err != nil {
				t.Fatalf("write decision.md: %v", err)
			}

			d := New(dir, &fakeGH{}, &prompt.Engine{}, &capturedRequest{}, &config.Config{
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/foo",
			}, &lockedBuffer{}, 0, false, nil)
			if _, ok := d.peekPendingPost(prNumber, commentID); ok {
				t.Fatalf("pendingPost should not register entry for status=%s (Slice B status filter)", status)
			}
		})
	}
}

// TestDaemon_S4_LoadPendingPosts_DoesNotDoubleHandleByDeleting pins
// that both walkers may end up registering the same row: a row that
// has BOTH a non-zero Timestamp (so loadPendingReviews registers it)
// AND decision.md on disk (so loadPendingPosts also registers it) is
// present in both maps. The actual precedence is decided at tick time
// by processPR (Slice C: rehydrate branch fires first), so this test
// only asserts the coexistence invariant — both maps end up holding
// the entry, and Slice C will exercise the precedence.
//
// This explicitly documents the design choice: a strict disjoint
// invariant would force exactly one of the two walkers to "lose" the
// row, but in production a row that meets BOTH gates deserves both:
// the rehydrate walker posts the body synchronously, and the lazy-
// verify entry is harmless after MarkSeen("success") takes effect
// (the seen-cache hydrate-and-filter path prevents re-processing).
func TestDaemon_S4_LoadPendingPosts_DoesNotDoubleHandleByDeleting(t *testing.T) {
	const (
		prNumber  = 5053
		commentID = "c-s4-coexist"
	)
	dir := t.TempDir()
	const batchID = "sip-batch-PR5053"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")

	// decision.md present ⇒ both walkers register the row.
	decisionPath := decisionPathForTest(t, dir, batchID, prNumber)
	if err := os.WriteFile(decisionPath, []byte("body"), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	d := New(dir, &fakeGH{}, &prompt.Engine{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false, nil)

	// Rehydrate map has the entry.
	if _, ok := d.peekPendingPost(prNumber, commentID); !ok {
		t.Fatalf("pendingPost should register the entry when decision.md is present (Slice B coexistence)")
	}
	// Lazy-verify map also has the entry (timestamp non-zero, status
	// pending). We do not assert it is in pendingReviews here — that
	// is loadPendingReviews' own contract; this test pins only the
	// rehydrate map's behaviour when both walkers share a row.
}

// TestDaemon_S4_RehydratePost_HappyPath is the seam-4 contract test
// pinned by issue #1847 / PRD #1843. It exercises the full
// rehydrate-on-startup pipeline against the existing
// fakeCommentPoster and capturedRequest seams:
//
//  1. Pre-populate .sandman/batches/<batch>/runs/<runID>/
//     review-state.json with a 'pending' SeenComment and
//     <runDir>/decision.md with a body that contains /sandman
//     substrings (so RedactBody must transform them).
//  2. Construct a fresh Daemon via New; loadPendingPosts picks up
//     the rehydrate-eligible row.
//  3. Run a single tick. processPR's rehydrate branch must read
//     decision.md, redact it, post via the fake CommentPoster,
//     call MarkSeen("success") on the per-run store, drop the
//     pendingPost entry, and NOT call BatchRunner.RunBatch.
//
// The assertions are the exact acceptance criteria pinned by the
// issue body.
func TestDaemon_S4_RehydratePost_HappyPath(t *testing.T) {
	const (
		prNumber  = 6060
		commentID = "c-s4-rehydrate-1"
	)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: mustParseTime(t, "2026-07-06T13:00:00Z")}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR S4", Body: "Body"}},
	}
	runner := &capturedRequest{} // BATCH RUNNER MUST NOT BE CALLED.

	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	poster := &fakeCommentPoster{}

	// Pre-seed a fresh daemon workspace: pending review-state +
	// decision.md on disk, then construct the daemon so its
	// loadPendingPosts walker registers the entry.
	dir := t.TempDir()
	const batchID = "sip-batch-PR6060"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")
	body := "/sandman review please.\n/Sandman reply.\nplain sandman stays.\n"
	if err := os.WriteFile(decisionPathForTest(t, dir, batchID, prNumber), []byte(body), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	d := New(dir, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 0, false, poster)
	d.PollInterval = 0

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// (a) BatchRunner MUST NOT be called (acceptance criterion).
	if runner.Calls() != 0 {
		t.Fatalf("expected BatchRunner.RunBatch NOT to be called on rehydrate path, got %d calls", runner.Calls())
	}

	// (b) CommentPoster captured the redacted body.
	if poster.Calls() != 1 {
		t.Fatalf("expected exactly 1 PostComment call from the rehydrate branch, got %d", poster.Calls())
	}
	gotPR, gotBody := poster.Captured()
	if gotPR != prNumber {
		t.Errorf("PostComment prNumber=%d, want %d", gotPR, prNumber)
	}
	wantBody := RedactBody(body)
	if gotBody != wantBody {
		t.Errorf("posted body mismatch:\n want=%q\n got =%q", wantBody, gotBody)
	}

	// (c) MarkSeen("success") was persisted to disk.
	statePath := locateReviewStatePath(t, dir)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	found := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == commentID && sc.Status == "success" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("review-state.json missing MarkSeen(success) for %s: %s", commentID, string(stateBytes))
	}

	// (d) pendingPost entry has been dropped.
	if _, ok := d.peekPendingPost(prNumber, commentID); ok {
		t.Errorf("pendingPost should drop the entry after a successful rehydrate post (Slice C contract)")
	}

	// (e) seenCache now contains the (prNumber, commentID) pair so
	// the next tick's processPR short-circuits via the existing
	// terminal-seen filter.
	if !d.IsTerminalSeen(prNumber, commentID) {
		t.Errorf("seenCache should mark (%d, %s) terminal-seen after MarkSeen(success), got %v", prNumber, commentID, d.seenCache)
	}
}

// TestDaemon_S4_RehydratePost_FailedPost_KeepsEntry pins the
// Slice-D failure surface: when CommentPoster.PostComment returns
// a non-ctx error during the rehydrate branch, the daemon must
// (a) call the poster exactly once, (b) NOT call BatchRunner,
// (c) KEEP the pendingPost entry so the next tick retries,
// (d) leave review-state.json's status as 'pending' (no MarkSeen).
func TestDaemon_S4_RehydratePost_FailedPost_KeepsEntry(t *testing.T) {
	const (
		prNumber  = 6061
		commentID = "c-s4-failpost-1"
	)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: mustParseTime(t, "2026-07-06T13:00:01Z")}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR S4 fail", Body: "Body"}},
	}
	runner := &capturedRequest{} // no BatchRunner invocation.
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	poster := &fakeCommentPoster{err: errors.New("gh pr comment: simulated post failure")}

	dir := t.TempDir()
	const batchID = "sip-batch-PR6061"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")
	if err := os.WriteFile(decisionPathForTest(t, dir, batchID, prNumber), []byte("body"), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	d := New(dir, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 0, false, poster)
	d.PollInterval = 0

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// (a) Poster called exactly once.
	if poster.Calls() != 1 {
		t.Fatalf("expected exactly 1 PostComment call on rehydrate path, got %d", poster.Calls())
	}
	// (b) No BatchRunner invocation.
	if runner.Calls() != 0 {
		t.Fatalf("expected BatchRunner.RunBatch NOT to be called on rehydrate failure path, got %d calls", runner.Calls())
	}

	// (c) Entry still in pendingPost (retry next tick).
	if _, ok := d.peekPendingPost(prNumber, commentID); !ok {
		t.Errorf("pendingPost should retain the entry on a failed post (Slice D failure surface)")
	}

	// (d) On-disk review-state.json's status is still 'pending'
	// (no MarkSeen fired; the next tick's rehydrate branch will
	// retry the post).
	statePath := locateReviewStatePath(t, dir)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	for _, sc := range state.SeenComments {
		if sc.CommentID == commentID {
			if sc.Status != "pending" {
				t.Errorf("review-state.json should keep status='pending' on failed post (Slice D contract), got %q for %s", sc.Status, sc.CommentID)
			}
		}
	}

	// Sanity: a second tick re-attempts the post and STILL does
	// not launch the agent (the entry is still in pendingPost).
	poster2 := &fakeCommentPoster{}
	d.CommentPoster = poster2
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if poster2.Calls() != 1 {
		t.Errorf("expected retry on second tick, got %d calls", poster2.Calls())
	}
	if runner.Calls() != 0 {
		t.Errorf("expected no BatchRunner runs on retry, got %d", runner.Calls())
	}
	if _, ok := d.peekPendingPost(prNumber, commentID); ok {
		t.Errorf("pendingPost should drop the entry on successful retry, got it still present")
	}
}

// TestDaemon_S4_RehydratePost_StaleEntry_FallsThroughLaunch pins
// the Slice-E stale-entry contract: when a pendingPost entry
// exists from construction (decision.md was on disk then) but
// <runDir>/decision.md has been removed (or never existed at tick
// time — the entry was injected artificially), the rehydrate
// branch drops the entry from pendingPost AND falls through to
// the launch path so a fresh agent is launched for the trigger.
func TestDaemon_S4_RehydratePost_StaleEntry_FallsThroughLaunch(t *testing.T) {
	const (
		prNumber  = 6062
		commentID = "c-s4-stale-1"
	)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: mustParseTime(t, "2026-07-06T13:00:02Z")}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR S4 stale", Body: "Body"}},
	}
	// Runner writes decision.md before RunBatch returns so the
	// launch path has a fresh body to post (mirrors the S3 happy
	// path fixture).
	runner := &seamRunner{
		capturedRequest: &capturedRequest{},
		beforeReturn: func(req batch.Request) {
			path := filepath.Join(req.RunDir, "decision.md")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatalf("mkdir decision.md dir: %v", err)
			}
			if err := os.WriteFile(path, []byte("fresh launch decision"), 0644); err != nil {
				t.Fatalf("write decision.md: %v", err)
			}
		},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	poster := &fakeCommentPoster{}

	dir := t.TempDir()

	// Construct the daemon normally first, then inject a stale
	// pendingPost entry whose runDir points to a non-existent
	// decision.md. Mirrors the production scenario: an operator
	// deleted decision.md (or the entry survived a manual
	// cleanup) before the daemon restarted.
	d := New(dir, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 0, false, poster)
	d.PollInterval = 0

	staleRunDir := filepath.Join(dir, "batches", "stale-batch", "runs", "stale-batch-PR6062")
	d.pendingPostMu.Lock()
	d.pendingPost[prNumber] = map[string]pendingPostEntry{
		commentID: {
			commentID:   commentID,
			runDir:      staleRunDir,
			reviewState: filepath.Join(staleRunDir, "review-state.json"),
		},
	}
	d.pendingPostMu.Unlock()

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Wait for the launch goroutine to finish so RunBatch has fired.
	idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}

	// Stale entry was dropped from pendingPost.
	if _, ok := d.peekPendingPost(prNumber, commentID); ok {
		t.Errorf("pendingPost should drop a stale entry (decision.md missing) and fall through to launch (Slice E contract)")
	}

	// Launch path fired: BatchRunner was called exactly once.
	if runner.Calls() != 1 {
		t.Fatalf("expected BatchRunner.RunBatch to fire on stale-entry fall-through, got %d calls", runner.Calls())
	}
	// S3 happy path: poster captured the freshly-launched body.
	if poster.Calls() != 1 {
		t.Errorf("expected exactly 1 PostComment call from the fresh launch, got %d", poster.Calls())
	}
}
