package review

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_AsyncLaunch_TickReturnsImmediately exercises the core fix:
// tick returns within a bounded window even though the fake BatchRunner
// blocks on a release channel. The slot is held while the review is
// in-flight and freed after the review completes (verified via
// WaitForIdle).
func TestDaemon_AsyncLaunch_TickReturnsImmediately(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	afterReview := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{{Number: 5, State: "open"}},
		comments: map[int][]github.PRComment{
			5: {
				{ID: "c5", Body: "/sandman review", CreatedAt: now},
				{ID: "c5-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
			},
		},
		prFetch: map[int]*github.PR{5: {Number: 5, Title: "PR 5", Body: "Body 5"}},
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 1,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// Tick must return within 2s even though RunBatch blocks.
	tickDone := make(chan error, 1)
	go func() { tickDone <- d.tick(context.Background()) }()
	select {
	case err := <-tickDone:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not return within 2s — still blocked on RunBatch")
	}

	// Wait for the review goroutine to start.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("review did not start within 5s")
	}

	// Slot must be held while the review is in-flight.
	if !d.IsSlotHeld(5) {
		t.Fatal("slot for PR 5 should be held while review is in-flight")
	}

	// Release the review so the goroutine can complete.
	close(release)

	// WaitForIdle must return once the goroutine finishes and releases the slot.
	idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}

	if d.IsSlotHeld(5) {
		t.Fatal("slot for PR 5 should be released after review completed")
	}
}

// TestDaemon_AsyncLaunch_CrossTickSlotAccumulation verifies the parallelism
// fix: with parallel_reviews=3, three consecutive ticks each launch a review
// on a different PR. All three slots are held simultaneously while the fake
// runner blocks — proving the slot pool fills across ticks.
func TestDaemon_AsyncLaunch_CrossTickSlotAccumulation(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	afterReview := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{
			{Number: 11, State: "open"},
			{Number: 22, State: "open"},
			{Number: 33, State: "open"},
		},
		comments: map[int][]github.PRComment{},
		prFetch: map[int]*github.PR{
			11: {Number: 11, Title: "PR 11", Body: "B"},
			22: {Number: 22, Title: "PR 22", Body: "B"},
			33: {Number: 33, Title: "PR 33", Body: "B"},
		},
	}
	var startedMu sync.Mutex
	startedPRs := map[int]bool{}
	started := make(chan int, 4)
	release := make(chan struct{})
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		startedMu.Lock()
		startedPRs[req.PRNumber] = true
		startedMu.Unlock()
		select {
		case started <- req.PRNumber:
		default:
		}
		<-release
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 3,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// Tick 1: only PR 11 has a trigger.
	gh.mu.Lock()
	gh.comments[11] = []github.PRComment{
		{ID: "c11", Body: "/sandman review", CreatedAt: now},
		{ID: "c11-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
	}
	gh.mu.Unlock()
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	waitForStarted(t, started, 11)

	// Tick 2: PR 22 gets a trigger while PR 11's review is still in-flight.
	gh.mu.Lock()
	gh.comments[22] = []github.PRComment{
		{ID: "c22", Body: "/sandman review", CreatedAt: now},
		{ID: "c22-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
	}
	gh.mu.Unlock()
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	waitForStarted(t, started, 22)

	// Tick 3: PR 33 gets a trigger while PRs 11+22 are still in-flight.
	gh.mu.Lock()
	gh.comments[33] = []github.PRComment{
		{ID: "c33", Body: "/sandman review", CreatedAt: now},
		{ID: "c33-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
	}
	gh.mu.Unlock()
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	waitForStarted(t, started, 33)

	// All three slots must be held simultaneously — the core fix.
	if got := d.slotHeldCount(); got != 3 {
		t.Errorf("expected 3 slots held simultaneously, got %d", got)
	}
	for _, pr := range []int{11, 22, 33} {
		if !d.IsSlotHeld(pr) {
			t.Errorf("PR %d slot should be held", pr)
		}
	}

	// Release all reviews and wait for idle.
	close(release)
	idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
	if got := d.slotHeldCount(); got != 0 {
		t.Errorf("expected 0 slots held after idle, got %d", got)
	}
}

// TestDaemon_AsyncLaunch_CommandPickupDuringInFlight verifies no command
// loss: a trigger on PR B is picked up on the next tick while PR A's review
// is still in-flight. Both reviews run concurrently.
func TestDaemon_AsyncLaunch_CommandPickupDuringInFlight(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	afterReview := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{
			{Number: 7, State: "open"},
			{Number: 8, State: "open"},
		},
		comments: map[int][]github.PRComment{
			7: {
				{ID: "c7", Body: "/sandman review", CreatedAt: now},
				{ID: "c7-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
			},
		},
		prFetch: map[int]*github.PR{
			7: {Number: 7, Title: "PR 7", Body: "B"},
			8: {Number: 8, Title: "PR 8", Body: "B"},
		},
	}
	started := make(chan int, 4)
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		select {
		case started <- req.PRNumber:
		default:
		}
		if req.PRNumber == 7 {
			<-releaseA
		} else {
			<-releaseB
		}
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 2,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// Tick 1: PR 7 has a trigger.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	waitForStarted(t, started, 7)

	// Add a trigger on PR 8 while PR 7's review is in-flight.
	gh.mu.Lock()
	gh.comments[8] = []github.PRComment{
		{ID: "c8", Body: "/sandman review", CreatedAt: now.Add(30 * time.Second)},
		{ID: "c8-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
	}
	gh.mu.Unlock()

	// Tick 2: PR 8's trigger must be picked up immediately (no command loss).
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	waitForStarted(t, started, 8)

	// Both slots held simultaneously.
	if !d.IsSlotHeld(7) {
		t.Error("PR 7 slot should be held (review still in-flight)")
	}
	if !d.IsSlotHeld(8) {
		t.Error("PR 8 slot should be held (review just launched)")
	}

	close(releaseA)
	close(releaseB)
	idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
}

// TestDaemon_AsyncLaunch_RunBatchErrorRetryable verifies that a RunBatch
// error marks the trigger as failure (retryable, NOT terminal-seen) and
// releases the slot.
func TestDaemon_AsyncLaunch_RunBatchErrorRetryable(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 9, State: "open"}},
		comments: map[int][]github.PRComment{
			9: {
				{ID: "c9", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{9: {Number: 9, Title: "PR 9", Body: "B"}},
	}
	started := make(chan struct{}, 1)
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		return nil, errors.New("sandbox failed")
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 1,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("review did not start")
	}

	idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}

	// Slot must be released.
	if d.IsSlotHeld(9) {
		t.Error("slot should be released after RunBatch error")
	}
	// Failure is retryable — must NOT be terminal-seen.
	if d.IsTerminalSeen(9, "c9") {
		t.Error("comment c9 must NOT be terminal-seen after RunBatch error (failure is retryable)")
	}
}

// TestDaemon_AsyncLaunch_GracefulShutdown verifies that Run's context
// cancellation waits for in-flight background goroutines and releases
// their slots before Run returns.
func TestDaemon_AsyncLaunch_GracefulShutdown(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	afterReview := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{{Number: 13, State: "open"}},
		comments: map[int][]github.PRComment{
			13: {
				{ID: "c13", Body: "/sandman review", CreatedAt: now},
				{ID: "c13-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
			},
		},
		prFetch: map[int]*github.PR{13: {Number: 13, Title: "PR 13", Body: "B"}},
	}
	started := make(chan struct{}, 1)
	ctxBlocked := make(chan struct{})
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			close(ctxBlocked)
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return nil, nil
		}
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 1,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// Use a manual trigger so Run doesn't auto-tick.
	trigger := make(chan struct{}, 1)
	d.Trigger = trigger

	runCtx, runCancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(runCtx) }()

	// Fire a tick to launch the review.
	trigger <- struct{}{}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("review did not start")
	}
	if !d.IsSlotHeld(13) {
		t.Fatal("slot should be held while review is in-flight")
	}

	// Cancel Run's context.
	runCancel()

	// Run must return after the goroutine finishes.
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s after ctx cancel")
	}

	// Slot must be released after shutdown.
	if d.IsSlotHeld(13) {
		t.Error("slot should be released after Run shutdown")
	}
}

// waitForStarted asserts that a specific PR's review starts within a
// bounded window.
func waitForStarted(t *testing.T, started <-chan int, want int) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("expected PR %d to start, got PR %d", want, got)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("PR %d did not start within 5s", want)
	}
}
