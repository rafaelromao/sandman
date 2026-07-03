package review

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

func waitIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := d.WaitForIdle(ctx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
}

// TestDaemon_PerPRSlotTable_AllowsCrossPRConcurrency pins the slice-C
// behavior: with parallel_reviews=2 and three PRs, exactly two PRs
// acquire slots and run concurrently; the third PR's processPR
// returns without dropping the trigger (the slot pool is full).
// The trigger survives in ListPRComments and is processed on the
// next tick once a slot frees.
func TestDaemon_PerPRSlotTable_AllowsCrossPRConcurrency(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	afterReview := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{
			{Number: 11, State: "open"},
			{Number: 22, State: "open"},
			{Number: 33, State: "open"},
		},
		comments: map[int][]github.PRComment{
			11: {
				{ID: "c11", Body: "/sandman review", CreatedAt: now},
				{ID: "c11-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
			},
			22: {
				{ID: "c22", Body: "/sandman review", CreatedAt: now},
				{ID: "c22-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
			},
			33: {
				{ID: "c33", Body: "/sandman review", CreatedAt: now},
				{ID: "c33-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
			},
		},
		prFetch: map[int]*github.PR{
			11: {Number: 11, Title: "PR 11", Body: "Body 11"},
			22: {Number: 22, Title: "PR 22", Body: "Body 22"},
			33: {Number: 33, Title: "PR 33", Body: "Body 33"},
		},
	}
	started := make(chan int, 4)
	var startedCount int
	var countMu sync.Mutex
	release := map[int]chan struct{}{
		11: make(chan struct{}),
		22: make(chan struct{}),
		33: make(chan struct{}),
	}
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		countMu.Lock()
		startedCount++
		countMu.Unlock()
		started <- req.PRNumber
		<-release[req.PRNumber]
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 2,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// Tick returns immediately (async launch). Two PRs acquire slots
	// and launch background goroutines; the third exits without a slot.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	hits := map[int]bool{}
	first := <-started
	hits[first] = true
	second := <-started
	hits[second] = true
	queuedPR := 0
	for _, pr := range []int{11, 22, 33} {
		if !hits[pr] {
			queuedPR = pr
			break
		}
	}

	heldCount := 0
	for _, pr := range []int{11, 22, 33} {
		if d.IsSlotHeld(pr) {
			heldCount++
		}
	}
	if heldCount != 2 {
		t.Errorf("expected 2 slots held mid-flight, got %d (hits=%v, queued=%d)", heldCount, hits, queuedPR)
	}
	for _, pr := range []int{11, 22, 33} {
		if hits[pr] {
			if !d.IsSlotHeld(pr) {
				t.Errorf("PR %d started but IsSlotHeld=false", pr)
			}
		} else {
			if d.IsSlotHeld(pr) {
				t.Errorf("PR %d did not start yet but IsSlotHeld=true", pr)
			}
		}
	}

	close(release[first])
	close(release[second])
	waitIdle(t, d)

	for _, pr := range []int{first, second} {
		if d.IsSlotHeld(pr) {
			t.Errorf("slot should be released for PR %d after review completed", pr)
		}
	}

	countMu.Lock()
	prevCount := startedCount
	countMu.Unlock()
	if prevCount != 2 {
		t.Fatalf("after first tick, expected 2 RunBatch invocations, got %d", prevCount)
	}

	// Second tick: the queued PR's trigger is still in ListPRComments.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	select {
	case prNumber := <-started:
		if prNumber != queuedPR {
			t.Fatalf("expected queued PR %d to start on second tick, got PR %d", queuedPR, prNumber)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("queued PR %d did not start within 15s on second tick", queuedPR)
	}

	close(release[queuedPR])
	waitIdle(t, d)
	if d.slotHeldCount() != 0 {
		t.Errorf("slots not released after queued PR completed: slotHeldCount=%d", d.slotHeldCount())
	}
}

// TestDaemon_PerPRSlotTable_ReleasesOnCompletion asserts the slot
// table does not leak: after all in-flight reviews complete, an
// idle daemon reports no slots held.
func TestDaemon_PerPRSlotTable_ReleasesOnCompletion(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	afterReview := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{
			{Number: 41, State: "open"},
			{Number: 42, State: "open"},
		},
		comments: map[int][]github.PRComment{
			41: {
				{ID: "c41", Body: "/sandman review", CreatedAt: now},
				{ID: "c41-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
			},
			42: {
				{ID: "c42", Body: "/sandman review", CreatedAt: now},
				{ID: "c42-reply", Body: "## Summary\nLGTM", CreatedAt: afterReview},
			},
		},
		prFetch: map[int]*github.PR{
			41: {Number: 41, Title: "PR 41", Body: "Body 41"},
			42: {Number: 42, Title: "PR 42", Body: "Body 42"},
		},
	}
	started := make(chan int, 2)
	release := map[int]chan struct{}{
		41: make(chan struct{}),
		42: make(chan struct{}),
	}
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		started <- req.PRNumber
		<-release[req.PRNumber]
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 2,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case prNumber := <-started:
			if !d.IsSlotHeld(prNumber) {
				t.Fatalf("expected slot held for PR %d while RunBatch is in-flight", prNumber)
			}
			close(release[prNumber])
		case <-time.After(15 * time.Second):
			t.Fatal("did not see both PRs start within 15s")
		}
	}

	waitIdle(t, d)

	if d.IsSlotHeld(41) {
		t.Errorf("slot for PR 41 leaked: IsSlotHeld(41)=true after reviews completed")
	}
	if d.IsSlotHeld(42) {
		t.Errorf("slot for PR 42 leaked: IsSlotHeld(42)=true after reviews completed")
	}
	if got := d.slotHeldCount(); got != 0 {
		t.Errorf("daemon should hold zero slots when idle, got slotHeldCount=%d", got)
	}
}

// TestDaemon_PerPRSlotTable_NewTriggerMidFlight_IsNotDropped pins
// acceptance criterion #3 (slice C): a launchReview for PR N is
// blocked in a controllable fake BatchRunner; mid-flight, a new
// /sandman review comment arrives on PR N. The in-flight review is
// released; the next tick must invoke RunBatch for the new commentID
// within a bounded wait — the trigger is not silently dropped.
func TestDaemon_PerPRSlotTable_NewTriggerMidFlight_IsNotDropped(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {
				{ID: "first", Body: "/sandman review", CreatedAt: now},
				{ID: "first-reply", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{7: {Number: 7, Title: "PR 7", Body: "Body 7"}},
	}
	var callsMu sync.Mutex
	calls := []string{}
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	startedFirst := make(chan struct{}, 1)
	startedSecond := make(chan struct{}, 1)
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		callsMu.Lock()
		cid := req.PromptConfig.Branch
		calls = append(calls, cid)
		callsMu.Unlock()
		if cid == "sandman/review-7-first" {
			select {
			case startedFirst <- struct{}{}:
			default:
			}
			<-releaseFirst
		} else if cid == "sandman/review-7-second" {
			select {
			case startedSecond <- struct{}{}:
			default:
			}
			<-releaseSecond
		} else {
			t.Errorf("unexpected PR branch: %s", cid)
		}
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 1,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// First tick launches the review asynchronously and returns immediately.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	select {
	case <-startedFirst:
	case <-time.After(15 * time.Second):
		t.Fatal("first review did not start within 15s")
	}
	if !d.IsSlotHeld(7) {
		t.Errorf("slot for PR 7 must be held mid-flight, got IsSlotHeld(7)=false")
	}

	// Mid-flight: a new /sandman review comment arrives on PR 7.
	gh.mu.Lock()
	gh.comments[7] = append(gh.comments[7],
		github.PRComment{
			ID:        "second",
			Body:      "/sandman review focus on perf",
			CreatedAt: now.Add(2 * time.Minute),
		},
		github.PRComment{
			ID:        "second-reply",
			Body:      "## Summary\nLGTM",
			CreatedAt: now.Add(3 * time.Minute),
		},
	)
	gh.mu.Unlock()

	// Second tick during in-flight — returns immediately (slot held).
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick during in-flight: %v", err)
	}

	if !d.IsSlotHeld(7) {
		t.Errorf("slot for PR 7 should still be held (in-flight review not yet released), got IsSlotHeld(7)=false")
	}
	callsMu.Lock()
	callsSoFar := append([]string{}, calls...)
	callsMu.Unlock()
	if len(callsSoFar) != 1 {
		t.Fatalf("expected 1 RunBatch call so far (in-flight only), got %v", callsSoFar)
	}

	// Release the in-flight review so the slot frees.
	close(releaseFirst)
	waitIdle(t, d)

	if d.IsSlotHeld(7) {
		t.Errorf("slot for PR 7 should be released after first review completed, got IsSlotHeld(7)=true")
	}

	if d.IsTerminalSeen(7, "second") {
		t.Errorf("comment 'second' must not be terminal-seen before being launched, got IsTerminalSeen(7,second)=true")
	}

	// Third tick — picks up the second trigger.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("third tick: %v", err)
	}
	select {
	case <-startedSecond:
	case <-time.After(15 * time.Second):
		callsMu.Lock()
		t.Fatalf("expected RunBatch for second comment within 15s, got calls=%v", append([]string{}, calls...))
	}

	if !d.IsSlotHeld(7) {
		t.Errorf("slot for PR 7 should be held during second review, got IsSlotHeld(7)=false")
	}

	close(releaseSecond)
	waitIdle(t, d)

	// Promotion tick: advance pending → success.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("promotion tick: %v", err)
	}

	if !d.IsTerminalSeen(7, "first") {
		t.Errorf("comment 'first' should be terminal-seen after promotion tick, got IsTerminalSeen(7,first)=false")
	}
	if !d.IsTerminalSeen(7, "second") {
		t.Errorf("comment 'second' should be terminal-seen after promotion tick")
	}

	if d.IsSlotHeld(7) {
		t.Errorf("slot for PR 7 leaked after second review completed")
	}
	if d.slotHeldCount() != 0 {
		t.Errorf("slot table still holds entries after second review completed: slotHeldCount=%d", d.slotHeldCount())
	}
}

// TestDaemon_PerPRSlotTable_HeldSlotReturnsSilently pins acceptance
// criterion #2 (slice C): when the slot for a PR is held by an
// in-flight review, processPR returns without dropping the trigger.
// The seen-cache stays non-terminal so the trigger is naturally
// picked up on the next tick.
func TestDaemon_PerPRSlotTable_HeldSlotReturnsSilently(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 9, State: "open"}},
		comments: map[int][]github.PRComment{
			9: {
				{ID: "early", Body: "/sandman review", CreatedAt: now},
				{ID: "early-reply", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{9: {Number: 9, Title: "PR 9", Body: "B"}},
	}
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var callsMu sync.Mutex
	calls := 0
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
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

	// First tick launches review asynchronously.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("first review did not start within 15s")
	}
	if !d.IsSlotHeld(9) {
		t.Fatal("slot for PR 9 should be held mid-flight")
	}

	// While the first review is in-flight, run another tick. With
	// parallel_reviews=1 and the slot held, processPR returns
	// silently without launching a duplicate review.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	callsMu.Lock()
	callsSoFar := calls
	callsMu.Unlock()
	if callsSoFar != 1 {
		t.Errorf("second tick must not launch a duplicate review while slot is held; expected 1 RunBatch call, got %d", callsSoFar)
	}

	if d.IsTerminalSeen(9, "early") {
		t.Errorf("comment 'early' must not be terminal-seen before MarkSeen; got IsTerminalSeen=true")
	}

	// Release the in-flight review.
	close(release)
	waitIdle(t, d)

	if d.IsSlotHeld(9) {
		t.Errorf("slot should be released after first review completed")
	}
	// Promotion tick: advance pending → success.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("promotion tick: %v", err)
	}
	if !d.IsTerminalSeen(9, "early") {
		t.Errorf("comment 'early' should be terminal-seen after success")
	}
}
