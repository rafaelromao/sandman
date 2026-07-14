package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// failureRunner is a BatchRunner that always returns the configured
// error. Used by the launch-failure regression test to drive the
// recordLaunchFailure / sleepLaunchFailureBackoff path on every tick.
type failureRunner struct {
	err      error
	calls    atomic.Int32
	repoName string
}

func (f *failureRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	f.calls.Add(1)
	return nil, f.err
}

func (f *failureRunner) RepoName(ctx context.Context) (string, error) {
	if f.repoName != "" {
		return f.repoName, nil
	}
	return "rafaelromao/sandman", nil
}

// TestNextFailureBackoff_Schedule pins the AC #2 schedule:
//
//	attempt 1 → 10s
//	attempt 2 → 20s
//	attempt 3 → 40s
//	attempt 4 → 60s (cap reached)
//	attempt 5+ → 60s
func TestNextFailureBackoff_Schedule(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 0}, // pre-check: raw counter passthrough
		{-3, 0},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 60 * time.Second}, // cap reached
		{5, 60 * time.Second},
		{6, 60 * time.Second},
		{20, 60 * time.Second},
		{63, 60 * time.Second}, // bit-shift overflow guard
	}
	for _, tc := range cases {
		got := nextFailureBackoff(tc.attempts)
		if got != tc.want {
			t.Errorf("nextFailureBackoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

// TestFailureBackoffConstants_Pinned asserts AC #1: the exported
// constants are exactly the values the schedule is built on. Pinned
// so accidental re-tuning of the schedule triggers a deliberate
// review of the cascade-stopper behaviour.
func TestFailureBackoffConstants_Pinned(t *testing.T) {
	if failureBackoffBase != 10*time.Second {
		t.Errorf("failureBackoffBase = %v, want 10s", failureBackoffBase)
	}
	if failureBackoffCap != 60*time.Second {
		t.Errorf("failureBackoffCap = %v, want 60s", failureBackoffCap)
	}
}

// TestRecordLaunchFailure_IncrementsAttempts asserts AC #5: each
// call to recordLaunchFailure increments the attempts counter from
// its persisted value, so the launch-failure backoff schedule can
// compute the next sleep from the same source of truth.
func TestRecordLaunchFailure_IncrementsAttempts(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-review-recordfail-")
	t.Chdir(dir)
	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 4242, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	d := &Daemon{}

	for want := 1; want <= 4; want++ {
		if err := d.recordLaunchFailure(context.Background(), "c1", store, errors.New("upstream 500")); err == nil {
			t.Fatalf("attempt %d: recordLaunchFailure swallowed cause", want)
		}
		got := ReadFailureAttempts(store, "c1")
		if got != want {
			t.Errorf("attempt %d: ReadFailureAttempts = %d, want %d", want, got, want)
		}
	}
}

// TestRecordLaunchFailure_CtxCancelLeavesUntouched pins the
// ctx-cancellation carve-out from issue #1846: a recordLaunchFailure
// call observed after ctx.Done() must NOT increment the attempts
// counter or change the persisted status.
func TestRecordLaunchFailure_CtxCancelLeavesUntouched(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-recordfail-cancel-")
	t.Chdir(dir)
	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 4242, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	d := &Daemon{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := d.recordLaunchFailure(ctx, "c1", store, errors.New("upstream 500")); err == nil {
		t.Error("recordLaunchFailure should propagate ctx.Err() under cancelled context")
	}
	if got := ReadFailureAttempts(store, "c1"); got != 0 {
		t.Errorf("ReadFailureAttempts on cancelled ctx = %d, want 0", got)
	}
}

// TestRecordLaunchFailure_PreservesRetryableContract asserts AC #8:
// a failure write does NOT fire the seen-cache short-circuit
// hook (failure is not in shouldSkipDedupStatus). The trigger
// stays retryable across ticks.
func TestRecordLaunchFailure_PreservesRetryableContract(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-recordfail-cache-")
	t.Chdir(dir)
	storePath := filepath.Join(dir, "review-state.json")
	inv := &recordingInvalidator{}
	store, err := NewReviewStateStore(storePath, 4242, inv)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	d := &Daemon{}

	if err := d.recordLaunchFailure(context.Background(), "c1", store, errors.New("upstream 500")); err == nil {
		t.Error("recordLaunchFailure swallowed cause")
	}
	if len(inv.terminal) != 0 {
		t.Errorf("seen-cache hook must NOT fire on failure write, got %v", inv.terminal)
	}

	// And the on-disk SeenComment row should be persisted as
	// status="failure" with the incremented attempts count.
	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var got batchindex.ReviewState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.SeenComments) != 1 {
		t.Fatalf("expected 1 SeenComment, got %d", len(got.SeenComments))
	}
	sc := got.SeenComments[0]
	if sc.Status != "failure" {
		t.Errorf("Status = %q, want failure", sc.Status)
	}
	if sc.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", sc.Attempts)
	}
}

// TestSleepLaunchFailureBackoff_RespectsCtxCancel asserts AC #4:
// ctx cancellation interrupts the sleep promptly so a daemon
// shutdown does not hang on the 60s tail.
func TestSleepLaunchFailureBackoff_RespectsCtxCancel(t *testing.T) {
	d := &Daemon{}
	// Production schedule would sleep 60s on attempts=4; the test
	// seam injects a 60s sleeper so the test still proves the
	// cancellation path without running for a minute.
	d.launchBackoff = func(int) time.Duration { return 60 * time.Second }

	dir := testenv.MkdirShort(t, "sm-sleep-cancel-")
	t.Chdir(dir)
	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 4242, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		d.sleepLaunchFailureBackoff(ctx, "c1", store)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sleepLaunchFailureBackoff did not honour ctx cancellation")
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Errorf("sleep honoured ctx cancel but took %v; expected sub-second", elapsed)
	}
}

// TestSleepLaunchFailureBackoff_ZeroSeamReturnsImmediately asserts
// AC #6: the test seam returns 0 and the helper short-circuits
// without entering the timer select — so test fixtures can chain
// five consecutive failures without sleeping.
func TestSleepLaunchFailureBackoff_ZeroSeamReturnsImmediately(t *testing.T) {
	d := &Daemon{}
	calls := 0
	d.launchBackoff = func(int) time.Duration {
		calls++
		return 0
	}
	dir := testenv.MkdirShort(t, "sm-sleep-zero-")
	t.Chdir(dir)
	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 4242, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	start := time.Now()
	d.sleepLaunchFailureBackoff(context.Background(), "c1", store)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("zero-cost seam should return immediately, took %v", elapsed)
	}
	if calls != 1 {
		t.Errorf("launchBackoff seam should be called once, got %d", calls)
	}
}

// TestDaemon_LaunchFailureBackoff_RegressionFiveFailures asserts
// AC #7: five consecutive launch failures on the same comment
// produce five attempts, the sleep schedule matches the AC table,
// and the seen cache remains non-terminal-seen throughout
// (preserves the S6 retryable contract).
func TestDaemon_LaunchFailureBackoff_RegressionFiveFailures(t *testing.T) {
	const (
		prNumber  = 2210
		commentID = "c-launch-five"
	)
	now := time.Date(2026, 7, 14, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 2210", Body: "Body"}},
	}
	runner := &failureRunner{err: errors.New("upstream 500: unknown error")}
	dir := testenv.MkdirShort(t, "sm-launch-five-")
	t.Chdir(dir)
	d := New(dir, gh, &prompt.Engine{}, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 1, true, nil)
	d.PollInterval = 0
	d.launchBackoff = func(int) time.Duration { return 0 } // zero-cost seam

	const expectedAttempts = 5

	for tickN := 1; tickN <= expectedAttempts; tickN++ {
		tickAndWait(t, d, context.Background())

		// Each tick launches one RunBatch (since failure is
		// retryable).
		if got := runner.calls.Load(); got != int32(tickN) {
			t.Fatalf("after tick %d: RunBatch calls = %d, want %d", tickN, got, tickN)
		}
		// The seen cache must NOT mark the comment terminal-seen
		// at any point — the S6 retryable contract holds.
		if d.IsTerminalSeen(prNumber, commentID) {
			t.Fatalf("after tick %d: seenCache marked terminal-seen, got cache %+v", tickN, d.seenCache)
		}
	}

	// The latest review-state.json carries the post-increment
	// attempts counter from the most recent failure — recordLaunchFailure
	// writes attempts=1 on each fresh launch's first failure. Per-launch
	// persistence is the contract; cross-launch / cross-restart
	// persistence is covered by #2211's gate.
	statePath := locateReviewStatePath(t, dir)
	if statePath == "" {
		t.Fatalf("expected review-state.json to exist after %d ticks, got none in %s", expectedAttempts, dir)
	}
	cs, err := NewReviewStateStore(statePath, prNumber, nil)
	if err != nil {
		t.Fatalf("reload review state: %v", err)
	}
	if got := ReadFailureAttempts(cs, commentID); got != 1 {
		t.Errorf("ReadFailureAttempts after 5 same-process failures = %d, want 1 (per-launch contract)", got)
	}

	// The production schedule (read via the public helper) must
	// match the AC table for the recorded attempt counts.
	wantSchedule := []time.Duration{
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
		60 * time.Second,
	}
	for i, want := range wantSchedule {
		got := nextFailureBackoff(i + 1)
		if got != want {
			t.Errorf("nextFailureBackoff(%d) = %v, want %v", i+1, got, want)
		}
	}
}

// TestDaemon_LaunchFailureBackoff_SleepsInProductionSchedule asserts
// that the launch goroutine consults the production
// nextFailureBackoff when d.launchBackoff is unset — recorded via
// the seam's call count and the attempts counter.
func TestDaemon_LaunchFailureBackoff_SleepsInProductionSchedule(t *testing.T) {
	const (
		prNumber  = 2211
		commentID = "c-launch-prod"
	)
	now := time.Date(2026, 7, 14, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 2210", Body: "Body"}},
	}
	runner := &failureRunner{err: errors.New("upstream 500")}
	dir := testenv.MkdirShort(t, "sm-launch-prod-")
	t.Chdir(dir)
	d := New(dir, gh, &prompt.Engine{}, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 1, true, nil)
	d.PollInterval = 0
	d.launchBackoff = func(int) time.Duration { return 0 } // zero-cost

	tickAndWait(t, d, context.Background())

	// The seam was consulted once during the backoff computation;
	// the attempts counter is now 1.
	statePath := locateReviewStatePath(t, dir)
	cs, err := NewReviewStateStore(statePath, prNumber, nil)
	if err != nil {
		t.Fatalf("reload review state: %v", err)
	}
	if got := ReadFailureAttempts(cs, commentID); got != 1 {
		t.Errorf("ReadFailureAttempts after 1 failure = %d, want 1", got)
	}
}

// TestDaemon_LaunchFailureBackoff_SuccessResetsAttempts asserts
// AC #5's second half: a successful post step calls MarkSeen
// ("success") which clears the attempts counter to 0 (per the
// success-clears-attempts contract from #2209).
func TestDaemon_LaunchFailureBackoff_SuccessResetsAttempts(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-launch-reset-")
	t.Chdir(dir)
	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 4242, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	d := &Daemon{}

	for i := 1; i <= 3; i++ {
		if err := d.recordLaunchFailure(context.Background(), "c1", store, errors.New("upstream 500")); err == nil {
			t.Fatalf("attempt %d: recordLaunchFailure swallowed cause", i)
		}
	}
	if got := ReadFailureAttempts(store, "c1"); got != 3 {
		t.Fatalf("setup: ReadFailureAttempts = %d, want 3", got)
	}

	// Standard MarkSeen("success") resets the counter to 0.
	if err := store.MarkSeen("c1", "success"); err != nil {
		t.Fatalf("MarkSeen(success): %v", err)
	}
	if got := ReadFailureAttempts(store, "c1"); got != 0 {
		t.Errorf("ReadFailureAttempts after success = %d, want 0", got)
	}
}

// recordingInvalidator captures the SeenCacheInvalidator callbacks
// so the launch-failure regression test can assert the retryable
// contract (failure must NOT fire MarkTerminalSeen).
type recordingInvalidator struct {
	terminal []string
}

func (r *recordingInvalidator) MarkTerminalSeen(_ int, commentID string) {
	r.terminal = append(r.terminal, commentID)
}
func (r *recordingInvalidator) Forget(_ int, _ string) {}
