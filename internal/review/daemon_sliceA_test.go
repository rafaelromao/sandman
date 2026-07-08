package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// sliceASeenLoader records how often the on-disk scan helpers were called
// for the seen-cache hydration. Tests install it before constructing the
// daemon and assert against the counters after running processPR.
type sliceASeenLoader struct {
	batchesIndexLoadCalls atomic.Int64
	reviewStateReadCalls  atomic.Int64
}

func (s *sliceASeenLoader) Install(t *testing.T) {
	t.Helper()
	prevLoader := seenCacheLoader
	prevReader := seenStateReader
	seenCacheLoader = s.loadBatchesIndex
	seenStateReader = s.readReviewState
	t.Cleanup(func() {
		seenCacheLoader = prevLoader
		seenStateReader = prevReader
	})
}

func (s *sliceASeenLoader) loadBatchesIndex(baseDir string) (*batchindex.Index, error) {
	s.batchesIndexLoadCalls.Add(1)
	return batchindex.Load(daemon.BatchesIndexPath(baseDir))
}

func (s *sliceASeenLoader) readReviewState(runDir string) (batchindex.ReviewState, error) {
	s.reviewStateReadCalls.Add(1)
	return batchindex.ReadReviewState(runDir)
}

// seedPriorReviewEntry writes a prior-batch review-state.json plus an
// index entry so cache hydration has a (prNumber, commentID) pair to
// discover. Used by the slice-A regression tests.
//
// The on-disk layout matches the canonical per-row shape from
// ADR-0030 / issue #1551: `<batch>/runs/<rowID>/run.json` and
// `<batch>/runs/<rowID>/review-state.json`. The legacy
// `runs/review/` alias folder is no longer used by the hydration
// path.
//
// status is the literal status written into the seeded
// SeenComment entry. slice-A passes "success" (matches the
// pre-existing fixtures); slice-S4 passes "pending" to exercise
// the rehydrate walker.
func seedPriorReviewEntry(t *testing.T, baseDir, batchID string, prNumber int, commentID, status string) {
	t.Helper()
	batchesDir := filepath.Join(baseDir, "batches")
	batchPath := filepath.Join(batchesDir, batchID)
	rowID := deriveReviewRowID(batchID, prNumber)
	runDir := filepath.Join(batchPath, "runs", rowID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("create prior run dir: %v", err)
	}
	state := batchindex.ReviewState{
		PR: prNumber,
		SeenComments: []batchindex.SeenComment{
			{CommentID: commentID, Status: status, Timestamp: time.Now()},
		},
	}
	if err := batchindex.WriteReviewState(runDir, state); err != nil {
		t.Fatalf("write prior review state: %v", err)
	}
	manifest := batchindex.RunManifest{
		RunID:     rowID,
		BatchID:   batchID,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(runDir, manifest); err != nil {
		t.Fatalf("write prior run manifest: %v", err)
	}
	idxPath := daemon.BatchesIndexPath(baseDir)
	idx, err := batchindex.Load(idxPath)
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	idx.AddBatch(batchindex.Batch{
		ID:   batchID,
		Path: batchPath,
		Kind: batchindex.KindReview,
		PR:   prNumber,
	})
	if err := idx.Save(idxPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}
}

// deriveReviewRowID computes the per-row RunID a review launch would
// have minted for a prior batch whose batch directory is `batchID`,
// given that the PR does not link an issue (the conservative shape
// used by tests that pre-seed prior review state).
func deriveReviewRowID(batchID string, prNumber int) string {
	parts := strings.SplitN(batchID, "-", 3)
	if len(parts) < 3 {
		return batchID
	}
	ts := parts[0]
	sid := parts[1]
	return fmt.Sprintf("%s-%s-PR%d", ts, sid, prNumber)
}

// TestDaemon_SeenCacheHydratedAtConstruction pins the slice-A behavior
// that the Daemon's seenCache is populated at New from the on-disk
// review-state.json files referenced by .sandman/batches.json. After
// construction, processPR for a PR whose only trigger matches a cached
// (prNumber, commentID) pair skips the launch without re-reading the
// batches index or any review-state.json file.
func TestDaemon_SeenCacheHydratedAtConstruction(t *testing.T) {
	const (
		prNumber  = 42
		commentID = "cached-100"
	)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: time.Now()}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "T", Body: "B"}},
	}
	runner := &capturedRequest{}

	// Seed the prior review-state.json BEFORE constructing the daemon
	// so the cache hydration at New actually sees it (mirrors the
	// production lifecycle: daemon restarts and reads existing state).
	dir := t.TempDir()
	t.Chdir(dir)
	seedPriorReviewEntry(t, dir, "prior-batch-PR42", prNumber, commentID, "success")

	d := New(dir, gh, &prompt.Engine{}, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false, nil)

	counter := &sliceASeenLoader{}
	counter.Install(t)

	// Cache hydration happened at New: the (prNumber, commentID) pair
	// from the seeded review-state.json must already be present.
	if !d.IsTerminalSeen(prNumber, commentID) {
		t.Fatalf("seenCache should have (PR %d, %s) after construction, got %v", prNumber, commentID, d.seenCache)
	}

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if runner.calls != 0 {
		t.Errorf("expected no batch runs because the comment is in seenCache, got %d", runner.calls)
	}
	if got := counter.batchesIndexLoadCalls.Load(); got != 0 {
		t.Errorf("processPR should not re-read batches index when cache covers the PR, got %d load calls", got)
	}
	if got := counter.reviewStateReadCalls.Load(); got != 0 {
		t.Errorf("processPR should not re-read any review-state.json when cache covers the PR, got %d read calls", got)
	}
}

// TestDaemon_ProcessPRScalesConstantlyWithPriorBatches asserts that
// processPR for a fresh PR does not depend on the number of prior
// review batches. With the cache hydration, the per-tick scan is
// short-circuited so Load + ReadReviewState counters stay at zero
// regardless of N (and timings stay within a generous guardrail).
func TestDaemon_ProcessPRScalesConstantlyWithPriorBatches(t *testing.T) {
	const (
		freshPR   = 99
		triggerID = "fresh-trigger"
	)
	comments := map[int][]github.PRComment{
		freshPR: {
			{ID: triggerID, Body: "/sandman review", CreatedAt: time.Now()},
			// Add a follow-up review comment so verifyReviewPosted
			// succeeds on the first attempt (no 5-second backoff).
			{ID: "review-reply", Body: "## Summary\nLGTM", CreatedAt: time.Now().Add(1 * time.Minute)},
		},
	}

	const largestN = 200
	type runResult struct {
		idxLoads   int64
		stateReads int64
		elapsed    time.Duration
	}
	measure := func(t *testing.T, priorCount int) runResult {
		t.Helper()
		gh := &fakeGH{
			prs:      []github.PR{{Number: freshPR, State: "open"}},
			comments: comments,
			prFetch:  map[int]*github.PR{freshPR: {Number: freshPR, Title: "T", Body: "B"}},
		}
		runner := &capturedRequest{}
		d, _, dir := newDaemonForTest(t, gh, runner, &config.Config{
			DefaultReviewAgent: "opencode",
			DefaultReviewModel: "opencode/foo",
		})
		// Set Clock so verifyReviewPosted finds the review-reply
		// without retrying.
		d.Clock = func() time.Time { return time.Now().Add(-1 * time.Minute) }
		for i := 0; i < priorCount; i++ {
			batchID := "prior-batch-PR" + itoa(freshPR, i)
			seedPriorReviewEntry(t, dir, batchID, freshPR, batchID+"-c", "success")
		}
		// Re-hydrate the cache after seeding so the production
		// invariant is asserted: a cache hydrated after seed data
		// is written and pre-warm the cache at the same level as
		// a freshly-restarted daemon would.
		if err := d.InvalidateSeenCache(); err != nil {
			t.Fatalf("InvalidateSeenCache: %v", err)
		}
		counter := &sliceASeenLoader{}
		counter.Install(t)

		start := time.Now()
		tickAndWait(t, d, context.Background())
		elapsed := time.Since(start)

		if runner.calls != 1 {
			t.Fatalf("expected exactly 1 batch run for the fresh PR (N=%d), got %d", priorCount, runner.calls)
		}
		return runResult{
			idxLoads:   counter.batchesIndexLoadCalls.Load(),
			stateReads: counter.reviewStateReadCalls.Load(),
			elapsed:    elapsed,
		}
	}

	var largest runResult
	for _, n := range []int{1, 50, largestN} {
		res := measure(t, n)
		t.Logf("N=%d elapsed=%v idxLoads=%d stateReads=%d", n, res.elapsed, res.idxLoads, res.stateReads)
		if res.idxLoads != 0 {
			t.Errorf("N=%d: processPR should not Load the batches index per tick, got %d loads", n, res.idxLoads)
		}
		if res.stateReads != 0 {
			t.Errorf("N=%d: processPR should not ReadReviewState per tick, got %d reads", n, res.stateReads)
		}
		if n == largestN {
			largest = res
		}
	}

	// Guardrail: even with N=largestN, the tick for a fresh PR must
	// complete within a generous wall-clock budget. The counter
	// assertions above are the primary signal; this is a safety net.
	if largest.elapsed > 5*time.Second {
		t.Errorf("processPR took %v with N=%d; expected well under 5s (cache hydration should make it constant)", largest.elapsed, largestN)
	}
}

// itoa formats a non-negative int. Used only to mint deterministic
// batch IDs in the perf regression test.
func itoa(_ int, i int) string {
	const digits = "0123456789"
	if i == 0 {
		return string(digits[0])
	}
	out := make([]byte, 0, 8)
	for i > 0 {
		out = append([]byte{digits[i%10]}, out...)
		i /= 10
	}
	return string(out)
}

// fakeInvalidator is a no-op test double for SeenCacheInvalidator.
type fakeInvalidator struct {
	onMark   func(prNumber int, commentID string)
	onForget func(prNumber int, commentID string)
}

func (f fakeInvalidator) MarkTerminalSeen(prNumber int, commentID string) {
	if f.onMark != nil {
		f.onMark(prNumber, commentID)
	}
}

func (f fakeInvalidator) Forget(prNumber int, commentID string) {
	if f.onForget != nil {
		f.onForget(prNumber, commentID)
	}
}

// TestDaemon_SeenCacheConcurrentReadWriteSafe pins the race fix
// (slice-A PR review): processPR must hold seenCacheMu.RLock across
// the per-trigger lookup so that sibling PR goroutines in the same
// tick can call MarkTerminalSeen / Forget concurrently without
// panicking. Without this guard, Go's runtime reports a fatal
// "concurrent map read and map write" on the inner
// map[commentID]bool.
func TestDaemon_SeenCacheConcurrentReadWriteSafe(t *testing.T) {
	const numPRs = 25
	prs := make([]github.PR, numPRs)
	comments := make(map[int][]github.PRComment, numPRs)
	prFetch := make(map[int]*github.PR, numPRs)
	for i := 0; i < numPRs; i++ {
		prs[i] = github.PR{Number: 100 + i, State: "open"}
		comments[100+i] = []github.PRComment{
			{ID: "c-" + itoa(0, i), Body: "/sandman review", CreatedAt: time.Now()},
		}
		prFetch[100+i] = &github.PR{Number: 100 + i, Title: "T", Body: "B"}
	}
	gh := &fakeGH{prs: prs, comments: comments, prFetch: prFetch}
	runner := &capturedRequest{}

	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: numPRs,
	})
	d.Clock = func() time.Time { return time.Now().Add(2 * time.Minute) }
	// Pre-populate the cache with all N PRs so each processPR call
	// holds the read lock for a non-trivial window.
	for i := 0; i < numPRs; i++ {
		d.MarkTerminalSeen(100+i, "seed")
	}

	// Tick fans out to one goroutine per PR; each reads from
	// seenCache under d.seenCacheMu.RLock. Meanwhile the test
	// goroutine concurrently mutates the cache. Without the lock
	// fix, the race detector fires here.
	done := make(chan struct{})
	go func() {
		d.MarkTerminalSeen(100, "foo")
		d.Forget(100, "foo")
		d.MarkTerminalSeen(101, "bar")
		d.Forget(101, "bar")
		close(done)
	}()

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	<-done
}

// TestDaemon_ReleaseForgetsCacheEntry pins that the daemon's Forget
// drops the (prNumber, commentID) entry from the seen cache so a
// later processPR call considers the comment as fresh again. This is
// exercised on the production Daemon rather than ReviewStateStore
// because Forget is a contract on the SeenCacheInvalidator seam and
// needs to round-trip through the daemon's seenCacheMutex.
func TestDaemon_ReleaseForgetsCacheEntry(t *testing.T) {
	d, _, _ := newDaemonForTest(t, &fakeGH{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.MarkTerminalSeen(7, "to-forget")
	if !d.IsTerminalSeen(7, "to-forget") {
		t.Fatal("cache should have (PR 7, to-forget) after MarkTerminalSeen")
	}
	d.Forget(7, "to-forget")
	if d.IsTerminalSeen(7, "to-forget") {
		t.Fatal("cache should NOT have (PR 7, to-forget) after Forget")
	}
}

// TestDaemon_ReviewStateStore_MarkSeenInvalidatesCacheMidProcess pins
// criterion #6: a comment marked via ReviewStateStore.MarkSeen
// mid-process is honored by a subsequent processPR call without
// requiring a daemon restart. Under the post-#1846 / post-#1849
// (S6) contract, the first tick records the comment as `success` on
// the launching tick via the post step (issue #1846); the cache hook
// fires on success via MarkSeen, hydrating the in-memory cache so a
// second tick short-circuits on cache hits without re-reading the
// batches index or any review-state.json.
func TestDaemon_ReviewStateStore_MarkSeenInvalidatesCacheMidProcess(t *testing.T) {
	const (
		prNumber  = 33
		triggerID = "trigger-mid"
	)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {
				{ID: triggerID, Body: "/sandman review", CreatedAt: time.Now()},
				{ID: "review-reply", Body: "## Summary\nLGTM", CreatedAt: time.Now().Add(1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "T", Body: "B"}},
	}
	runner := newDecisionRunner()
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return time.Now().Add(-1 * time.Minute) }

	counter := &sliceASeenLoader{}
	counter.Install(t)

	// First tick: cache is empty, so processPR should launch a
	// batch and the S3 post step (issue #1846) records the trigger
	// as success directly via MarkSeen. tickAndWait blocks until
	// the async review goroutine completes so the runner.calls
	// assertion is stable AND the seen-cache hook has fired.
	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("first tick should launch exactly one batch, got %d", runner.calls)
	}
	if !d.IsTerminalSeen(prNumber, triggerID) {
		t.Fatalf("after first tick the S3 post step (issue #1846) should have hydrated the seen cache with success, got cache %v", d.seenCache)
	}

	// Second tick: the seen cache now short-circuits the trigger
	// (the post step's MarkSeen('success') fired MarkTerminalSeen).
	// runner.calls stays at 1 because no new launch happens.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("second tick must not re-launch the review (already terminal-seen via S3 post step), got %d total calls", runner.calls)
	}
	if !d.IsTerminalSeen(prNumber, triggerID) {
		t.Fatalf("after second tick the cache should have (%d, %s) from S3 success, got %v", prNumber, triggerID, d.seenCache)
	}

	// Reset the counters so the assertion below measures the third tick only.
	counter.batchesIndexLoadCalls.Store(0)
	counter.reviewStateReadCalls.Store(0)

	// Third tick: the cache should short-circuit the trigger comment.
	// runner.calls must not advance and the disk I/O counters must stay
	// at zero on the cache-covered PR.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("third tick: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("third tick must not launch a new batch for the cached trigger, got %d total calls", runner.calls)
	}
	if got := counter.batchesIndexLoadCalls.Load(); got != 0 {
		t.Errorf("third tick should not re-Load batches index, got %d loads (cache failed to short-circuit)", got)
	}
	if got := counter.reviewStateReadCalls.Load(); got != 0 {
		t.Errorf("third tick should not re-Read any review-state.json, got %d reads", got)
	}
}

// TestReviewStateStore_MarkSeenFiresCacheHook pins the contract on the
// SeenCacheInvalidator seam. MarkSeen only fires MarkTerminalSeen when
// shouldSkipDedupStatus(status) is true (success/superseded). A
// "failure" save must NOT cache — preserving the rename-loser
// retry semantics from ADR-0034 §3.
func TestReviewStateStore_MarkSeenFiresCacheHook(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	called := map[string]int{}
	hook := fakeInvalidator{
		onMark:   func(pr int, id string) { called[fmt.Sprintf("m:%d:%s", pr, id)]++ },
		onForget: func(pr int, id string) { called[fmt.Sprintf("f:%d:%s", pr, id)]++ },
	}

	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 42, &hook)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	for _, status := range []string{"failure", "aborted"} {
		if err := store.MarkSeen("c-noncache", status); err != nil {
			t.Fatalf("MarkSeen(%q): %v", status, err)
		}
		if v := called["m:42:c-noncache"]; v != 0 {
			t.Errorf("MarkSeen(%q) should NOT fire the cache hook, fired %d times", status, v)
		}
	}

	for _, status := range []string{"success", "superseded"} {
		if err := store.MarkSeen("c-cache-"+status, status); err != nil {
			t.Fatalf("MarkSeen(%q): %v", status, err)
		}
	}
	if v := called["m:42:c-cache-success"]; v != 1 {
		t.Errorf("MarkSeen(success) should fire MarkTerminalSeen exactly once, got %d", v)
	}
	if v := called["m:42:c-cache-superseded"]; v != 1 {
		t.Errorf("MarkSeen(superseded) should fire MarkTerminalSeen exactly once, got %d", v)
	}

	// Release must fire Forget.
	store2, err := NewReviewStateStore(filepath.Join(dir, "review-state2.json"), 42, &hook)
	if err != nil {
		t.Fatalf("NewReviewStateStore 2: %v", err)
	}
	if !store2.TryClaim("c-release") {
		t.Fatal("TryClaim should succeed for fresh commentID")
	}
	store2.Release("c-release")
	if v := called["f:42:c-release"]; v != 1 {
		t.Errorf("Release should fire Forget exactly once, got %d", v)
	}
}

// TestReviewStateStore_MarkSeenSaveErrorLeavesCacheUntouched pins the
// advisory invariant: if the on-disk Save errors, the cache hook must
// not fire. The cache only short-circuits what is also persisted.
// The test injects a Save error via the reviewStateSave test seam
// because forcing the atomic-rename to fail reliably across runtimes
// is brittle (MkdirAll is permissive on root, /tmp permissions vary).
func TestReviewStateStore_MarkSeenSaveErrorLeavesCacheUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	prev := reviewStateSave
	t.Cleanup(func() { reviewStateSave = prev })

	reviewStateSave = func(_ *ReviewStateStore) error {
		return fmt.Errorf("synthetic save failure")
	}

	hook := fakeInvalidator{
		onMark: func(pr int, id string) {
			t.Errorf("MarkTerminalSeen fired unexpectedly for (%d, %s) on Save error", pr, id)
		},
	}

	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 42, &hook)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	if err := store.MarkSeen("c1", "success"); err == nil {
		t.Fatalf("expected MarkSeen to error on Save failure; got nil")
	}
}
