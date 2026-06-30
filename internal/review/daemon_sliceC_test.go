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

// TestDaemon_PerPRSlotTable_AllowsCrossPRConcurrency pins the slice-C
// behavior: with parallel_reviews=2 and three PRs, exactly two PRs
// acquire slots and run concurrently; the third PR's processPR
// returns without dropping the trigger (the slot pool is full).
// The trigger survives in ListPRComments and is processed on the
// next tick once a slot frees. The internal per-tick sem is gone;
// the slot table is daemon-scoped and survives across ticks.
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
	// Block every PR's RunBatch on its own per-PR release channel, so
	// any two of {11, 22, 33} run concurrently and the third waits
	// until the next tick (after a slot frees).
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
		ch := release[req.PRNumber]
		if ch == nil {
			ch = make(chan struct{})
			release[req.PRNumber] = ch
		}
		<-ch
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 2,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	done := make(chan error, 1)
	go func() {
		done <- d.tick(context.Background())
	}()

	// With parallel_reviews=2, exactly two of the three PRs acquire
	// slots and run concurrently. Which two depends on goroutine
	// scheduling; we accept any pair from {11, 22, 33}.
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

	// Exactly two PRs hold slots.
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

	// The first tick should complete after the two held PRs release
	// (the third PR's processPR exited early without a slot).
	close(release[first])
	close(release[second])

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not finish after releasing all reviews")
	}

	// After release, both slots must be free. The queued PR runs on
	// the NEXT tick.
	for _, pr := range []int{first, second} {
		if d.IsSlotHeld(pr) {
			t.Errorf("slot should be released for PR %d after review completed", pr)
		}
	}

	// Reset started-count tracking for the next tick.
	countMu.Lock()
	prevCount := startedCount
	countMu.Unlock()
	if prevCount != 2 {
		t.Fatalf("after first tick, expected 2 RunBatch invocations, got %d", prevCount)
	}

	// Second tick: the queued PR's trigger is still in ListPRComments
	// (it was not MarkSeen), so the next tick should pick it up.
	go func() { _ = d.tick(context.Background()) }()

	select {
	case prNumber := <-started:
		if prNumber != queuedPR {
			t.Fatalf("expected queued PR %d to start on second tick, got PR %d", queuedPR, prNumber)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("queued PR %d did not start within 5s on second tick", queuedPR)
	}

	// Release the queued PR's review and let the second tick finish.
	close(release[queuedPR])
	// The second tick is fire-and-forget here; wait briefly for it
	// to drain via the runner returning + MarkSeen + deferred slot release.
	// We poll IsSlotHeld with a bounded wait.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !d.IsSlotHeld(queuedPR) && d.slotHeldCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("queued PR %d slot did not release after review completed: IsSlotHeld=%v slotHeldCount=%d", queuedPR, d.IsSlotHeld(queuedPR), d.slotHeldCount())
}

// TestDaemon_PerPRSlotTable_ReleasesOnCompletion asserts the slot
// table does not leak: after all in-flight reviews complete, an
// idle daemon reports no slots held. Runners signal completion via
// a done channel that the test waits on before asserting (avoids
// racing the slot release on the runner goroutine).
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
	completion := map[int]chan struct{}{
		41: make(chan struct{}),
		42: make(chan struct{}),
	}
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		started <- req.PRNumber
		<-release[req.PRNumber]
		close(completion[req.PRNumber])
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 2,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	done := make(chan error, 1)
	go func() {
		done <- d.tick(context.Background())
	}()

	for i := 0; i < 2; i++ {
		select {
		case prNumber := <-started:
			if !d.IsSlotHeld(prNumber) {
				t.Fatalf("expected slot held for PR %d while RunBatch is in-flight", prNumber)
			}
			// Wait for completion channel close (or close it ourselves
			// after releasing the runner block).
			doneCh := completion[prNumber]
			// Release the runner, then wait for completion.
			close(release[prNumber])
			select {
			case <-doneCh:
			case <-time.After(5 * time.Second):
				t.Fatalf("runner for PR %d did not complete within 5s", prNumber)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("did not see both PRs start within 5s")
		}
	}

	// Wait for the tick to finish (releases happen inside launchReview's
	// deferred path which runs after RunBatch returns and after MarkSeen).
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not finish after releasing all reviews")
	}

	// Now the daemon is idle — no slot should be held.
	if d.IsSlotHeld(41) {
		t.Errorf("slot for PR 41 leaked: IsSlotHeld(41)=true after tick completed")
	}
	if d.IsSlotHeld(42) {
		t.Errorf("slot for PR 42 leaked: IsSlotHeld(42)=true after tick completed")
	}
	if got := d.slotHeldCount(); got != 0 {
		t.Errorf("daemon should hold zero slots when idle, got slotHeldCount=%d", got)
	}
}

// TestDaemon_PerPRSlotTable_NewTriggerMidFlight_IsNotDropped pins
// acceptance criterion #3 (slice C): a launchReview for PR N is
// blocked in a controllable fake BatchRunner; mid-flight, a new
// /sandman review comment arrives on PR N (mutated into fakeGH's
// existing comments map). The in-flight review is released; the
// next tick must invoke RunBatch for the new commentID within a
// bounded wait — the trigger is not silently dropped.
//
// The pre-existing per-tick `sem` would have logged "parallel limit
// reached, skipping" for the new comment because the slot pool was
// saturated; the per-PR slot table instead returns silently and
// preserves the trigger for the next tick (choice 2(b)).
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

	firstTick := make(chan error, 1)
	go func() {
		firstTick <- d.tick(context.Background())
	}()

	// Wait for the first review to start.
	select {
	case <-startedFirst:
	case <-time.After(5 * time.Second):
		t.Fatal("first review did not start within 5s")
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

	// Second tick during in-flight — should run quickly (slot-held path
	// returns silently).
	secondTickDone := make(chan error, 1)
	go func() {
		secondTickDone <- d.tick(context.Background())
	}()
	select {
	case err := <-secondTickDone:
		if err != nil {
			t.Fatalf("second tick during in-flight: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second tick during in-flight did not return within 2s")
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

	// Wait for the first tick to finish.
	select {
	case err := <-firstTick:
		if err != nil {
			t.Fatalf("first tick: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first tick did not finish after release")
	}

	if d.IsSlotHeld(7) {
		t.Errorf("slot for PR 7 should be released after first review completed, got IsSlotHeld(7)=true")
	}

	// Slice D lazy-verify: a successful launchReview records the trigger
	// as `pending`; the seen cache only marks it terminal after
	// promotePendingReviews observes the agent's review comment. So at
	// this point both "first" and "second" (which has not even been
	// launched yet) are non-terminal. We only assert that "second" is
	// not (yet) terminal-seen; "first" becomes terminal-seen after the
	// promotion tick that runs after both reviews complete.
	if d.IsTerminalSeen(7, "second") {
		t.Errorf("comment 'second' must not be terminal-seen before being launched, got IsTerminalSeen(7,second)=true")
	}

	// Third tick — fire it and wait for the second review to START
	// (do NOT wait for the tick to complete: the second RunBatch is
	// blocked on releaseSecond by design).
	thirdTickDone := make(chan error, 1)
	go func() {
		thirdTickDone <- d.tick(context.Background())
	}()
	select {
	case <-startedSecond:
	case <-time.After(5 * time.Second):
		callsMu.Lock()
		t.Fatalf("expected RunBatch for second comment to be invoked within 5s after slot release, got calls=%v", append([]string{}, calls...))
	}

	// Second review is now in-flight — slot is held again.
	if !d.IsSlotHeld(7) {
		t.Errorf("slot for PR 7 should be held during second review, got IsSlotHeld(7)=false")
	}

	// Release the second review so its tick can complete.
	close(releaseSecond)
	select {
	case err := <-thirdTickDone:
		if err != nil {
			t.Fatalf("third tick: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("third tick did not finish after releasing second review")
	}

	// Slice D lazy-verify contract: a successful launchReview records
	// the trigger as `pending`; the next tick's promotePendingReviews
	// advances pending → success after observing the agent's review
	// comment. Fire a promotion tick before asserting terminal-seen.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("promotion tick: %v", err)
	}

	// Both comments are now terminal-seen.
	if !d.IsTerminalSeen(7, "first") {
		t.Errorf("comment 'first' should be terminal-seen after promotion tick, got IsTerminalSeen(7,first)=false")
	}
	if !d.IsTerminalSeen(7, "second") {
		t.Errorf("comment 'second' should be terminal-seen after promotion tick")
	}

	// Slot should be released.
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

	firstTick := make(chan error, 1)
	go func() {
		firstTick <- d.tick(context.Background())
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first review did not start within 5s")
	}
	if !d.IsSlotHeld(9) {
		t.Fatal("slot for PR 9 should be held mid-flight")
	}

	// While the first review is in-flight, run another tick. With
	// parallel_reviews=1 and the slot held, processPR returns
	// silently without launching a duplicate review.
	secondTickDone := make(chan error, 1)
	go func() {
		secondTickDone <- d.tick(context.Background())
	}()
	select {
	case err := <-secondTickDone:
		if err != nil {
			t.Fatalf("second tick: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second tick did not return within 2s")
	}

	callsMu.Lock()
	callsSoFar := calls
	callsMu.Unlock()
	if callsSoFar != 1 {
		t.Errorf("second tick must not launch a duplicate review while slot is held; expected 1 RunBatch call, got %d", callsSoFar)
	}

	// Comment is not terminal-seen yet — the in-flight review has not
	// called MarkSeen.
	if d.IsTerminalSeen(9, "early") {
		t.Errorf("comment 'early' must not be terminal-seen before MarkSeen; got IsTerminalSeen=true")
	}

	// Release the in-flight review. After it completes, MarkSeen +
	// slot release run; the trigger becomes terminal-seen.
	close(release)
	select {
	case err := <-firstTick:
		if err != nil {
			t.Fatalf("first tick: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first tick did not finish after release")
	}

	if d.IsSlotHeld(9) {
		t.Errorf("slot should be released after first review completed")
	}
	// Slice D lazy-verify contract: a successful launchReview records
	// the trigger as `pending`; the next tick's promotePendingReviews
	// advances it to `success` after observing the agent's review
	// comment. Fire a promotion tick before asserting terminal-seen.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("promotion tick: %v", err)
	}
	if !d.IsTerminalSeen(9, "early") {
		t.Errorf("comment 'early' should be terminal-seen after success")
	}
}
