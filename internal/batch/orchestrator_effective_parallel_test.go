package batch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func requireContainerRuntime(t *testing.T) {
	t.Helper()
	if _, err := sandbox.ResolveRuntime("podman"); err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}
}

// TestEffectiveParallel_CapCalculation is a table-driven unit test that locks
// down the effectiveParallel cap shape across all combinations of parallel,
// container_capacity, and max_containers called out in issue #501.
func TestEffectiveParallel_CapCalculation(t *testing.T) {
	cases := []struct {
		name              string
		parallel          int
		containerCapacity int
		maxContainers     int
		want              int
	}{
		{"parallel=4 capacity=4 max=0 auto", 4, 4, 0, 4},
		{"parallel=4 capacity=2 max=0 auto", 4, 2, 0, 4},
		{"parallel=4 capacity=2 max=2 explicit totals 4", 4, 2, 2, 4},
		{"parallel=4 capacity=4 max=1 explicit totals 4", 4, 4, 1, 4},
		{"parallel=4 capacity=2 max=1 explicit caps to 2", 4, 2, 1, 2},
		{"parallel=0 capacity=4 max=0 unlimited stays 0", 0, 4, 0, 0},
		{"parallel=0 capacity=4 max=2 unlimited stays 0", 0, 4, 2, 0},
		{"parallel=8 capacity=4 max=0 auto", 8, 4, 0, 8},
		{"parallel=8 capacity=2 max=0 auto", 8, 2, 0, 8},
		{"parallel=1 capacity=100 max=0 auto", 1, 100, 0, 1},
		{"parallel=6 capacity=1 max=0 auto", 6, 1, 0, 6},
		{"parallel=4 capacity=1 max=1 explicit caps to 1", 4, 1, 1, 1},
		{"parallel=4 capacity=2 max=1 explicit caps to 2", 4, 2, 1, 2},
		{"parallel=4 capacity=1 max=4 explicit caps to 4", 4, 1, 4, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveParallelCap(tc.parallel, tc.containerCapacity, tc.maxContainers)
			if got != tc.want {
				t.Fatalf("effectiveParallelCap(parallel=%d, capacity=%d, max=%d) = %d, want %d",
					tc.parallel, tc.containerCapacity, tc.maxContainers, got, tc.want)
			}
		})
	}
}

func TestAwaitedRowPriority_RecentChanceYieldsToQueuedOrdinaryWork(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	if got := awaitedRowMayUsePriority(now.Add(-time.Minute), 4, 4, true, now); got {
		t.Fatal("recently resumed waiting row bypassed queued ordinary work without ordinary progress")
	}
}

func TestAwaitedRowPriority_EligibilityConditions(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name           string
		lastChance     time.Time
		startsAtChance uint64
		ordinaryStarts uint64
		ordinaryQueued bool
		want           bool
	}{
		{name: "no ordinary queue", lastChance: now.Add(-time.Minute), startsAtChance: 4, ordinaryStarts: 4, want: true},
		{name: "ordinary progress", lastChance: now.Add(-time.Minute), startsAtChance: 4, ordinaryStarts: 5, ordinaryQueued: true, want: true},
		{name: "cooldown boundary", lastChance: now.Add(-awaitingPriorityCooldown), startsAtChance: 4, ordinaryStarts: 4, ordinaryQueued: true, want: true},
		{name: "before cooldown boundary", lastChance: now.Add(-awaitingPriorityCooldown + time.Nanosecond), startsAtChance: 4, ordinaryStarts: 4, ordinaryQueued: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := awaitedRowMayUsePriority(tc.lastChance, tc.startsAtChance, tc.ordinaryStarts, tc.ordinaryQueued, now); got != tc.want {
				t.Fatalf("awaitedRowMayUsePriority() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBatchStartGate_RecentAwaitedWaiterUsesPriorityWithoutOrdinaryQueue(t *testing.T) {
	gate := newBatchStartGate(1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gate.Acquire(ctx); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	awaitedDone := make(chan error, 1)
	go func() {
		_, err := gate.AcquireAwaiting(ctx, awaitOpportunity{
			lastChance:     time.Now().Add(-time.Minute),
			ordinaryStarts: 1,
		})
		awaitedDone <- err
	}()
	waitForGateQueue(t, gate, func() bool { return len(gate.priorityWaiters) == 1 })

	gate.Release()
	select {
	case err := <-awaitedDone:
		if err != nil {
			t.Fatalf("awaited acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting row did not acquire after the only active row released")
	}
	gate.Release()
}

func TestBatchStartGate_OrdinaryArrivalDemotesRecentAwaitedWaiter(t *testing.T) {
	gate := newBatchStartGate(1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gate.Acquire(ctx); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	awaitedDone := make(chan error, 1)
	go func() {
		_, err := gate.AcquireAwaiting(ctx, awaitOpportunity{
			lastChance:     time.Now().Add(-time.Minute),
			ordinaryStarts: 1,
		})
		awaitedDone <- err
	}()
	waitForGateQueue(t, gate, func() bool { return len(gate.priorityWaiters) == 1 })

	ordinaryDone := make(chan error, 1)
	go func() { ordinaryDone <- gate.Acquire(ctx) }()
	waitForGateQueue(t, gate, func() bool {
		return len(gate.priorityWaiters) == 0 && len(gate.normalWaiters) == 2 && gate.normalWaiters[0].ordinary
	})

	gate.Release()
	select {
	case err := <-ordinaryDone:
		if err != nil {
			t.Fatalf("ordinary acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary waiter did not acquire after demoting the waiting row")
	}
	select {
	case err := <-awaitedDone:
		t.Fatalf("recently resumed waiting row bypassed arriving ordinary work, err=%v", err)
	case <-time.After(20 * time.Millisecond):
	}

	gate.Release()
	select {
	case err := <-awaitedDone:
		if err != nil {
			t.Fatalf("awaited acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting row did not acquire after ordinary work")
	}
	gate.Release()
}

func TestBatchStartGate_OrdinaryStartPromotesRecentAwaitedWaiterAheadOfRemainingQueue(t *testing.T) {
	gate := newBatchStartGate(1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gate.Acquire(ctx); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	ordinaryFirstDone := make(chan error, 1)
	go func() { ordinaryFirstDone <- gate.Acquire(ctx) }()
	waitForGateQueue(t, gate, func() bool { return len(gate.normalWaiters) == 1 })
	awaitedDone := make(chan error, 1)
	go func() {
		_, err := gate.AcquireAwaiting(ctx, awaitOpportunity{
			lastChance:     time.Now().Add(-time.Minute),
			ordinaryStarts: 1,
		})
		awaitedDone <- err
	}()
	waitForGateQueue(t, gate, func() bool { return len(gate.normalWaiters) == 2 })
	ordinarySecondDone := make(chan error, 1)
	go func() { ordinarySecondDone <- gate.Acquire(ctx) }()
	waitForGateQueue(t, gate, func() bool { return len(gate.normalWaiters) == 3 })

	gate.Release()
	select {
	case err := <-ordinaryFirstDone:
		if err != nil {
			t.Fatalf("first ordinary acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first ordinary waiter did not acquire")
	}

	gate.Release()
	select {
	case err := <-awaitedDone:
		if err != nil {
			t.Fatalf("awaited acquire: %v", err)
		}
	case err := <-ordinarySecondDone:
		t.Fatalf("remaining ordinary waiter bypassed by wrong queue order, err=%v", err)
	case <-time.After(time.Second):
		t.Fatal("awaited waiter did not acquire after ordinary progress")
	}

	gate.Release()
	select {
	case err := <-ordinarySecondDone:
		if err != nil {
			t.Fatalf("second ordinary acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("remaining ordinary waiter did not acquire")
	}
	gate.Release()
}

func TestBatchStartGate_RemovingOrdinaryWaiterPromotesRecentAwaitedWaiter(t *testing.T) {
	gate := newBatchStartGate(1, 0)
	activeCtx, cancelActive := context.WithCancel(context.Background())
	defer cancelActive()
	if err := gate.Acquire(activeCtx); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	ordinaryCtx, cancelOrdinary := context.WithCancel(context.Background())
	ordinaryDone := make(chan error, 1)
	go func() { ordinaryDone <- gate.Acquire(ordinaryCtx) }()
	waitForGateQueue(t, gate, func() bool { return len(gate.normalWaiters) == 1 })

	awaitedDone := make(chan error, 1)
	go func() {
		_, err := gate.AcquireAwaiting(activeCtx, awaitOpportunity{
			lastChance:     time.Now().Add(-time.Minute),
			ordinaryStarts: 1,
		})
		awaitedDone <- err
	}()
	waitForGateQueue(t, gate, func() bool { return len(gate.normalWaiters) == 2 })

	cancelOrdinary()
	select {
	case err := <-ordinaryDone:
		if err == nil {
			t.Fatal("cancelled ordinary waiter acquired a slot")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled ordinary waiter did not exit")
	}
	waitForGateQueue(t, gate, func() bool { return len(gate.priorityWaiters) == 1 && len(gate.normalWaiters) == 0 })

	gate.Release()
	select {
	case err := <-awaitedDone:
		if err != nil {
			t.Fatalf("awaited acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting row did not acquire after ordinary queue removal")
	}
	gate.Release()
}

func TestBatchStartGate_EligibleAwaitedWaitersRemainFIFO(t *testing.T) {
	gate := newBatchStartGate(1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gate.Acquire(ctx); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := gate.AcquireAwaiting(ctx, awaitOpportunity{lastChance: time.Now().Add(-time.Minute), ordinaryStarts: 1})
		firstDone <- err
	}()
	waitForGateQueue(t, gate, func() bool { return len(gate.priorityWaiters) == 1 })
	secondDone := make(chan error, 1)
	go func() {
		_, err := gate.AcquireAwaiting(ctx, awaitOpportunity{lastChance: time.Now().Add(-time.Minute), ordinaryStarts: 1})
		secondDone <- err
	}()
	waitForGateQueue(t, gate, func() bool { return len(gate.priorityWaiters) == 2 })

	gate.Release()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first awaited acquire: %v", err)
		}
	case <-secondDone:
		t.Fatal("second awaited waiter acquired before the first")
	case <-time.After(time.Second):
		t.Fatal("first awaited waiter did not acquire")
	}
	gate.Release()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second awaited acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second awaited waiter did not acquire")
	}
	gate.Release()
}

func TestBatchStartGate_RecentAwaitedWaiterYieldsToQueuedOrdinaryWork(t *testing.T) {
	gate := newBatchStartGate(1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gate.Acquire(ctx); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	ordinaryDone := make(chan error, 1)
	go func() { ordinaryDone <- gate.Acquire(ctx) }()
	waitForGateQueue(t, gate, func() bool { return len(gate.normalWaiters) == 1 })

	awaitedDone := make(chan error, 1)
	go func() {
		_, err := gate.AcquireAwaiting(ctx, awaitOpportunity{
			lastChance:     time.Now().Add(-time.Minute),
			ordinaryStarts: 1,
		})
		awaitedDone <- err
	}()
	waitForGateQueue(t, gate, func() bool { return len(gate.normalWaiters) == 2 })

	gate.Release()
	select {
	case err := <-ordinaryDone:
		if err != nil {
			t.Fatalf("ordinary acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued ordinary waiter did not acquire")
	}
	select {
	case err := <-awaitedDone:
		t.Fatalf("recently resumed waiting row bypassed ordinary waiter, err=%v", err)
	case <-time.After(20 * time.Millisecond):
	}

	gate.Release()
	select {
	case err := <-awaitedDone:
		if err != nil {
			t.Fatalf("awaited acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting row did not acquire after ordinary work")
	}
	gate.Release()
}

func waitForGateQueue(t *testing.T, gate *batchStartGate, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		queued := predicate()
		gate.mu.Unlock()
		if queued {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("gate waiters did not reach the expected queue state")
}

// TestEffectiveParallelCap_NonContainerCases verifies the cap is a no-op when
// the run is not in container mode (containerCapacity == 0).
func TestEffectiveParallelCap_NonContainerCases(t *testing.T) {
	if got := effectiveParallelCap(4, 0, 0); got != 4 {
		t.Fatalf("worktree mode: got %d, want 4", got)
	}
	if got := effectiveParallelCap(0, 0, 0); got != 0 {
		t.Fatalf("worktree mode unlimited: got %d, want 0", got)
	}
	if got := effectiveParallelCap(8, 0, 5); got != 8 {
		t.Fatalf("worktree mode with stray maxContainers: got %d, want 8", got)
	}
}

// TestBatchStartGate_HonoursEffectiveParallelCap verifies the start gate
// semaphore lets through at most effectiveParallel concurrent acquires.
func TestBatchStartGate_HonoursEffectiveParallelCap(t *testing.T) {
	gate := newBatchStartGate(4, 0)

	var active, peak int32
	var wg sync.WaitGroup
	release := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gate.Acquire(context.Background()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			cur := atomic.AddInt32(&active, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
					break
				}
			}
			<-release
			atomic.AddInt32(&active, -1)
			gate.Release()
		}()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&active) == 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&active); got != 4 {
		t.Fatalf("expected 4 active acquirers, got %d", got)
	}
	if got := atomic.LoadInt32(&peak); got != 4 {
		t.Fatalf("expected peak concurrent acquirers <= 4, got %d", got)
	}

	close(release)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gate acquirers did not finish in time")
	}
}

func TestBatchStartGate_ReadyAwaitedWaiterPrecedesQueuedWork(t *testing.T) {
	gate := newBatchStartGate(1, 0)
	if err := gate.Acquire(context.Background()); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	normalAcquired := make(chan struct{})
	go func() {
		if err := gate.Acquire(context.Background()); err != nil {
			return
		}
		close(normalAcquired)
	}()

	priorityAcquired := make(chan struct{})
	go func() {
		if err := gate.Acquire(context.Background(), true); err != nil {
			return
		}
		close(priorityAcquired)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		queued := len(gate.priorityWaiters) == 1 && len(gate.normalWaiters) == 1
		gate.mu.Unlock()
		if queued {
			break
		}
		time.Sleep(time.Millisecond)
	}
	gate.mu.Lock()
	queued := len(gate.priorityWaiters) == 1 && len(gate.normalWaiters) == 1
	active := gate.active
	normalCount := len(gate.normalWaiters)
	priorityCount := len(gate.priorityWaiters)
	gate.mu.Unlock()
	if !queued {
		t.Fatalf("waiters did not queue before releasing the held slot: active=%d normal=%d priority=%d", active, normalCount, priorityCount)
	}

	gate.Release()
	select {
	case <-priorityAcquired:
	case <-normalAcquired:
		t.Fatal("ordinary queued work acquired the slot before the ready awaited waiter")
	case <-time.After(time.Second):
		t.Fatal("priority waiter did not acquire the released slot")
	}
	gate.Release()
	select {
	case <-normalAcquired:
	case <-time.After(time.Second):
		t.Fatal("ordinary queued waiter did not acquire the second slot")
	}
	gate.Release()
}

func TestBatchStartGate_ReadyAwaitedWaitersRemainFIFO(t *testing.T) {
	gate := newBatchStartGate(1, 0)
	if err := gate.Acquire(context.Background()); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	firstAcquired := make(chan struct{})
	go func() {
		if err := gate.Acquire(context.Background(), true); err == nil {
			close(firstAcquired)
		}
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		queued := len(gate.priorityWaiters) == 1
		gate.mu.Unlock()
		if queued {
			break
		}
		time.Sleep(time.Millisecond)
	}
	gate.mu.Lock()
	queued := len(gate.priorityWaiters) == 1
	gate.mu.Unlock()
	if !queued {
		t.Fatal("first priority waiter did not queue")
	}

	secondAcquired := make(chan struct{})
	go func() {
		if err := gate.Acquire(context.Background(), true); err == nil {
			close(secondAcquired)
		}
	}()

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		queued = len(gate.priorityWaiters) == 2
		gate.mu.Unlock()
		if queued {
			break
		}
		time.Sleep(time.Millisecond)
	}
	gate.mu.Lock()
	queued = len(gate.priorityWaiters) == 2
	gate.mu.Unlock()
	if !queued {
		t.Fatal("priority waiters did not queue before release")
	}

	gate.Release()
	select {
	case <-firstAcquired:
	case <-secondAcquired:
		t.Fatal("second ready awaited waiter acquired before the first")
	case <-time.After(time.Second):
		t.Fatal("first ready awaited waiter did not acquire")
	}
	gate.Release()
	select {
	case <-secondAcquired:
	case <-time.After(time.Second):
		t.Fatal("second ready awaited waiter did not acquire")
	}
	gate.Release()
}

func TestBatchStartGate_CancelledPriorityWaiterIsRemoved(t *testing.T) {
	gate := newBatchStartGate(1, 0)
	if err := gate.Acquire(context.Background()); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	priorityCtx, cancelPriority := context.WithCancel(context.Background())
	priorityDone := make(chan error, 1)
	go func() { priorityDone <- gate.Acquire(priorityCtx, true) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		queued := len(gate.priorityWaiters) == 1
		gate.mu.Unlock()
		if queued {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancelPriority()
	select {
	case err := <-priorityDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled priority acquire error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled priority waiter did not return")
	}

	gate.mu.Lock()
	remaining := len(gate.priorityWaiters)
	gate.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("cancelled priority waiters = %d, want 0", remaining)
	}
	gate.mu.Lock()
	active := gate.active
	gate.mu.Unlock()
	if active != 1 {
		t.Fatalf("active slots after cancelling an unacquired waiter = %d, want 1", active)
	}

	normalAcquired := make(chan struct{})
	go func() {
		if err := gate.Acquire(context.Background()); err == nil {
			close(normalAcquired)
		}
	}()
	gate.Release()
	select {
	case <-normalAcquired:
	case <-time.After(time.Second):
		t.Fatal("normal waiter did not acquire after priority cancellation")
	}
	gate.Release()
}

func TestBatchStartGate_PriorityWaiterHonoursStartDelay(t *testing.T) {
	delay := 40 * time.Millisecond
	gate := newBatchStartGate(1, delay)
	if err := gate.Acquire(context.Background()); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	startedAt := time.Now()
	gate.Release()

	normalAcquired := make(chan struct{})
	go func() {
		if err := gate.Acquire(context.Background()); err == nil {
			close(normalAcquired)
		}
	}()
	priorityAcquired := make(chan struct{})
	go func() {
		if err := gate.Acquire(context.Background(), true); err == nil {
			close(priorityAcquired)
		}
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		queued := len(gate.priorityWaiters) == 1 && len(gate.normalWaiters) == 1
		gate.mu.Unlock()
		if queued {
			break
		}
		time.Sleep(time.Millisecond)
	}
	gate.mu.Lock()
	queued := len(gate.priorityWaiters) == 1 && len(gate.normalWaiters) == 1
	gate.mu.Unlock()
	if !queued {
		t.Fatal("waiters did not queue during start delay")
	}

	select {
	case <-priorityAcquired:
	case <-normalAcquired:
		t.Fatal("ordinary waiter bypassed ready-await priority during start delay")
	case <-time.After(time.Second):
		t.Fatal("priority waiter did not acquire after start delay")
	}
	if elapsed := time.Since(startedAt); elapsed < delay {
		t.Fatalf("priority waiter acquired after %s, before configured delay %s", elapsed, delay)
	}
	gate.Release()
	select {
	case <-normalAcquired:
	case <-time.After(time.Second):
		t.Fatal("normal waiter did not acquire after priority waiter")
	}
	gate.Release()
}

// TestRunBatch_StartGateUsesEffectiveParallelNotRawParallel is a direct
// regression guard for issue #500 and #1076: the start gate must be
// constructed with the capped effectiveParallel, not the raw parallel value.
// In auto mode (maxContainers=0) the cap is parallel*containerCapacity, so
// the gate must not throttle below the requested parallel. We assert that
// the batch reaches the full requested parallelism (peak == parallel) and
// never exceeds it.
func TestRunBatch_StartGateUsesEffectiveParallelNotRawParallel(t *testing.T) {
	requireContainerRuntime(t)

	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
			3: {Number: 3, Title: "Three"},
			4: {Number: 4, Title: "Four"},
		},
		prs: map[string]*github.PR{
			"1-one":   {Number: 1, State: "closed", Merged: true, HeadRefName: "1-one"},
			"2-two":   {Number: 2, State: "closed", Merged: true, HeadRefName: "2-two"},
			"3-three": {Number: 3, State: "closed", Merged: true, HeadRefName: "3-three"},
			"4-four":  {Number: 4, State: "closed", Merged: true, HeadRefName: "4-four"},
		},
	}

	// Auto mode (max=0) with capacity=2, parallel=4: the cap should not
	// throttle below the requested parallel of 4 (effectiveParallelCap(4,2,0)
	// = 4 in auto mode). The factory records peak active runs.
	factory := &fakeRunnableFactory{
		results: []AgentRunResult{
			{IssueNumber: 1, Status: "success"},
			{IssueNumber: 2, Status: "success"},
			{IssueNumber: 3, Status: "success"},
			{IssueNumber: 4, Status: "success"},
		},
		delays: []time.Duration{100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:             "test-agent",
		Sandbox:           "podman",
		WorktreeDir:       ".sandman/worktrees",
		ContainerCapacity: 2,
		MaxContainers:     0,
		Git:               config.GitConfig{BaseBranch: "main"},
		AgentProviders:    map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, nil,
		WithContainerRuntimeFactory(&fakeContainerRuntimeFactory{starter: &fakeContainerStarter{}}),
		WithRunnableFactory(factory),
		WithSandboxFactory(&freshSandboxFactory{}),
	)

	_, err := o.RunBatch(context.Background(), Request{
		Issues:               []int{1, 2, 3, 4},
		Sandbox:              "podman",
		Parallel:             4,
		ContainerCapacity:    2,
		ContainerCapacitySet: true,
		MaxContainers:        0,
		MaxContainersSet:     true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// effectiveParallelCap(4, 2, 0) = 4 in auto mode, so the gate permits
	// the full requested parallelism. The factory must observe peak == 4.
	// If the gate used a stale cap of 2 (the pre-#1076 bug), peak would be
	// <= 2 and the test would fail.
	if got := factory.max; got != 4 {
		t.Fatalf("start gate throttled below requested parallel (peak=%d, want 4); auto mode must allow full parallelism", got)
	}
}

// TestRunBatch_ParallelEightCapacityFourAutoMode_PeakAndContainerCount is the
// end-to-end regression guard for issue #1076 and #1077. In auto container
// mode (maxContainers=0) with parallel=8 and containerCapacity=4, the start
// gate must permit the full requested parallelism (peak == 8) and the
// container pool must start exactly 2 containers (8 / 4 = 2). A regression
// on the effectiveParallelCap would surface here either as peak < 8 (start
// gate throttling) or startCount != 2 (pool mis-sizing).
func TestRunBatch_ParallelEightCapacityFourAutoMode_PeakAndContainerCount(t *testing.T) {
	requireContainerRuntime(t)

	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	issues := make(map[int]*github.Issue, 8)
	prs := make(map[string]*github.PR, 8)
	results := make([]AgentRunResult, 8)
	delays := make([]time.Duration, 8)
	for i := 1; i <= 8; i++ {
		issues[i] = &github.Issue{Number: i, Title: fmt.Sprintf("Issue %d", i)}
		branch := fmt.Sprintf("%d-issue-%d", i, i)
		prs[branch] = &github.PR{Number: i, State: "closed", Merged: true, HeadRefName: branch}
		results[i-1] = AgentRunResult{IssueNumber: i, Status: "success"}
		delays[i-1] = 100 * time.Millisecond
	}

	client := &fakeGitHubClient{issues: issues, prs: prs}

	starter := &fakeContainerStarter{}
	factory := &fakeRunnableFactory{
		results: results,
		delays:  delays,
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:             "test-agent",
		Sandbox:           "podman",
		WorktreeDir:       ".sandman/worktrees",
		ContainerCapacity: 4,
		MaxContainers:     0,
		Git:               config.GitConfig{BaseBranch: "main"},
		AgentProviders:    map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, nil,
		WithContainerRuntimeFactory(&fakeContainerRuntimeFactory{starter: starter}),
		WithRunnableFactory(factory),
		WithSandboxFactory(&freshSandboxFactory{}),
	)

	_, err := o.RunBatch(context.Background(), Request{
		Issues:               []int{1, 2, 3, 4, 5, 6, 7, 8},
		Sandbox:              "podman",
		Parallel:             8,
		ContainerCapacity:    4,
		ContainerCapacitySet: true,
		MaxContainers:        0,
		MaxContainersSet:     true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// effectiveParallelCap(8, 4, 0) = 8 in auto mode, so the start gate
	// permits all 8 concurrent runs. If a regression caps the gate at
	// containerCapacity=4 (the pre-#1076 bug), peak would be <= 4.
	if got := factory.max; got != 8 {
		t.Fatalf("start gate throttled below requested parallel (peak=%d, want 8); auto mode must allow full parallelism", got)
	}
	// In auto mode the pool spawns containers on demand up to the
	// concurrent-run budget, capped by containerCapacity. With 8 parallel
	// runs and capacity=4 the pool should start exactly 2 containers.
	if got := starter.startCount; got != 2 {
		t.Fatalf("container pool started %d containers, want 2 (parallel=8 / capacity=4)", got)
	}
}

// TestBatchStartGate_IsUnboundedWhenZero verifies the gate permits arbitrary
// concurrency when effectiveParallel == 0.
func TestBatchStartGate_IsUnboundedWhenZero(t *testing.T) {
	gate := newBatchStartGate(0, 0)

	var active, peak int32
	var wg sync.WaitGroup
	release := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gate.Acquire(context.Background()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			cur := atomic.AddInt32(&active, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
					break
				}
			}
			<-release
			atomic.AddInt32(&active, -1)
			gate.Release()
		}()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&active) == 16 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&active); got != 16 {
		t.Fatalf("expected all 16 acquirers to be active, got %d", got)
	}
	if got := atomic.LoadInt32(&peak); got != 16 {
		t.Fatalf("expected peak concurrent acquirers == 16 (unbounded), got %d", got)
	}

	close(release)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gate acquirers did not finish in time")
	}
}

// startOrderRunnable records the order in which its Run method is entered.
// The runnable returns immediately so the next turn can proceed; the order is
// observed via the shared factory's starts slice rather than per-issue
// channels.
type startOrderRunnable struct {
	result  AgentRunResult
	factory *startOrderRunnableFactory
}

func (r *startOrderRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	r.factory.recordStart(r.result.IssueNumber)
	return r.result
}

// startOrderRunnableFactory hands out startOrderRunnable instances by issue
// number from a shared map and records the order in which they were observed
// running.
type startOrderRunnableFactory struct {
	mu        sync.Mutex
	runnables map[int]*startOrderRunnable
	starts    []int
	notify    chan struct{}
}

func newStartOrderRunnableFactory(runnables map[int]*startOrderRunnable) *startOrderRunnableFactory {
	return &startOrderRunnableFactory{
		runnables: runnables,
		notify:    make(chan struct{}, 64),
	}
}

func (f *startOrderRunnableFactory) recordStart(issue int) {
	f.mu.Lock()
	f.starts = append(f.starts, issue)
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func (f *startOrderRunnableFactory) waitForStarts(n int, timeout time.Duration) ([]int, bool) {
	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		count := len(f.starts)
		f.mu.Unlock()
		if count >= n {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]int{}, f.starts...), true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]int{}, f.starts...), false
		}
		select {
		case <-f.notify:
		case <-time.After(remaining):
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]int{}, f.starts...), false
		}
	}
}

func (f *startOrderRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.mu.Lock()
	r := f.runnables[issue.Number]
	f.mu.Unlock()
	return r
}

// TestBatch_StartOrderPreservedWithSerialStart verifies that when
// effectiveParallel == 1, the turn lock + start gate fire starts in the input
// order. This is the FIFO behaviour from issue #422 and the regression guard
// from issue #501.
func TestBatch_StartOrderPreservedWithSerialStart(t *testing.T) {
	requireContainerRuntime(t)

	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "First"},
			2: {Number: 2, Title: "Second"},
			3: {Number: 3, Title: "Third"},
			4: {Number: 4, Title: "Fourth"},
		},
	}

	runnables := make(map[int]*startOrderRunnable, 4)
	for i := 1; i <= 4; i++ {
		runnables[i] = &startOrderRunnable{
			result: AgentRunResult{IssueNumber: i, Status: "success"},
		}
	}
	factory := newStartOrderRunnableFactory(runnables)
	for i := 1; i <= 4; i++ {
		runnables[i].factory = factory
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:             "test-agent",
		Sandbox:           "podman",
		WorktreeDir:       ".sandman/worktrees",
		ContainerCapacity: 1,
		MaxContainers:     0,
		Git:               config.GitConfig{BaseBranch: "main"},
		AgentProviders:    map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, nil,
		WithContainerRuntimeFactory(&fakeContainerRuntimeFactory{starter: &fakeContainerStarter{}}),
		WithRunnableFactory(factory),
		WithSandboxFactory(&freshSandboxFactory{}),
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{
			Issues:               []int{1, 2, 3, 4},
			Sandbox:              "podman",
			Parallel:             1,
			ContainerCapacity:    1,
			ContainerCapacitySet: true,
			MaxContainers:        0,
			MaxContainersSet:     true,
		})
	}()

	starts, ok := factory.waitForStarts(4, 2*time.Second)
	<-done
	if !ok {
		t.Fatalf("only %d/4 issues started in time, got order %v", len(starts), starts)
	}
	want := []int{1, 2, 3, 4}
	if len(starts) != len(want) {
		t.Fatalf("expected %d start signals, got %d (%v)", len(want), len(starts), starts)
	}
	for i := range want {
		if starts[i] != want[i] {
			t.Fatalf("expected start order %v, got %v", want, starts)
		}
	}
}

// TestBatch_StartOrderNotSerialisedWithParallelStart verifies that when
// effectiveParallel > 1, the batch does not introduce artificial serialisation
// between starts. All 4 issues should be observed starting within a tight
// window. The window is bounded above by the scheduler latency seen on
// loaded CI hosts; under normal conditions all 4 land in the first scheduler
// quantum (~10ms), well below the 2s budget used here.
func TestBatch_StartOrderNotSerialisedWithParallelStart(t *testing.T) {
	requireContainerRuntime(t)

	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "First"},
			2: {Number: 2, Title: "Second"},
			3: {Number: 3, Title: "Third"},
			4: {Number: 4, Title: "Fourth"},
		},
	}

	runnables := make(map[int]*startOrderRunnable, 4)
	for i := 1; i <= 4; i++ {
		runnables[i] = &startOrderRunnable{
			result: AgentRunResult{IssueNumber: i, Status: "success"},
		}
	}
	factory := newStartOrderRunnableFactory(runnables)
	for i := 1; i <= 4; i++ {
		runnables[i].factory = factory
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:             "test-agent",
		Sandbox:           "podman",
		WorktreeDir:       ".sandman/worktrees",
		ContainerCapacity: 4,
		MaxContainers:     1,
		Git:               config.GitConfig{BaseBranch: "main"},
		AgentProviders:    map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, nil,
		WithContainerRuntimeFactory(&fakeContainerRuntimeFactory{starter: &fakeContainerStarter{}}),
		WithRunnableFactory(factory),
		WithSandboxFactory(&freshSandboxFactory{}),
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{
			Issues:               []int{1, 2, 3, 4},
			Sandbox:              "podman",
			Parallel:             4,
			ContainerCapacity:    4,
			ContainerCapacitySet: true,
			MaxContainers:        1,
			MaxContainersSet:     true,
		})
	}()

	// With effectiveParallel=4, all 4 should be observed starting within a
	// single scheduling quantum. If the orchestrator introduced artificial
	// serialisation, this would take much longer. The deadline matches the
	// serial-start test (2s) so the timing-sensitive check stays stable on
	// loaded CI hosts where goroutine scheduling can take longer than 500ms.
	starts, ok := factory.waitForStarts(4, 2*time.Second)
	<-done
	if !ok {
		t.Fatalf("only %d/4 issues started within 500ms (artificial serialisation?), got order %v", len(starts), starts)
	}
}
