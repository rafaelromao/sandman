package batch

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

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
		{"parallel=4 capacity=2 max=0 auto caps to 2", 4, 2, 0, 2},
		{"parallel=4 capacity=2 max=2 explicit totals 4", 4, 2, 2, 4},
		{"parallel=4 capacity=4 max=1 explicit totals 4", 4, 4, 1, 4},
		{"parallel=4 capacity=2 max=1 explicit caps to 2", 4, 2, 1, 2},
		{"parallel=0 capacity=4 max=0 unlimited stays 0", 0, 4, 0, 0},
		{"parallel=0 capacity=4 max=2 unlimited stays 0", 0, 4, 2, 0},
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

func (r *startOrderRunnable) Run(ctx context.Context, _ prompt.Renderer, _ string, _ prompt.RenderConfig) AgentRunResult {
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
		Sandbox:           "docker",
		WorktreeDir:       ".sandman/worktrees",
		ContainerCapacity: 1,
		MaxContainers:     0,
		Git:               config.GitConfig{BaseBranch: "main"},
		AgentProviders:    map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, nil)
	o.containerRuntimeFactory = &fakeContainerRuntimeFactory{starter: &fakeContainerStarter{}}
	o.runnableFactory = factory
	o.sandboxFactory = &freshSandboxFactory{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{
			Issues:               []int{1, 2, 3, 4},
			Sandbox:              "docker",
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
// window.
func TestBatch_StartOrderNotSerialisedWithParallelStart(t *testing.T) {
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
		Sandbox:           "docker",
		WorktreeDir:       ".sandman/worktrees",
		ContainerCapacity: 4,
		MaxContainers:     1,
		Git:               config.GitConfig{BaseBranch: "main"},
		AgentProviders:    map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, nil)
	o.containerRuntimeFactory = &fakeContainerRuntimeFactory{starter: &fakeContainerStarter{}}
	o.runnableFactory = factory
	o.sandboxFactory = &freshSandboxFactory{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{
			Issues:               []int{1, 2, 3, 4},
			Sandbox:              "docker",
			Parallel:             4,
			ContainerCapacity:    4,
			ContainerCapacitySet: true,
			MaxContainers:        1,
			MaxContainersSet:     true,
		})
	}()

	// With effectiveParallel=4, all 4 should be observed starting within a
	// single scheduling quantum. If the orchestrator introduced artificial
	// serialisation, this would take much longer.
	starts, ok := factory.waitForStarts(4, 500*time.Millisecond)
	<-done
	if !ok {
		t.Fatalf("only %d/4 issues started within 500ms (artificial serialisation?), got order %v", len(starts), starts)
	}
}
