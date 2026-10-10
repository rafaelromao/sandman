package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestRunBatch_CancelPendingContainerStartup(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	started, queued, release := make(chan struct{}, 3), make(chan struct{}, 3), make(chan struct{})
	var once sync.Once
	starter := &fakeContainerStarter{startStarted: started, startRelease: release}
	log := &spyEventLog{}
	runnables := &fakeRunnableFactory{}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{
		1: {Number: 1, Title: "One"}, 2: {Number: 2, Title: "Two"}, 3: {Number: 3, Title: "Three"},
	}}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, log, WithErrorLog(io.Discard), WithContainerRuntimeFactory(&fakeContainerRuntimeFactory{starter: starter}),
		WithRunnableFactory(runnables), WithSandboxFactory(&trackingSandboxFactory{}),
		WithRunSessionOpts(runSessionOptions{startWaiterQueued: func(bool) { queued <- struct{}{} }}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		once.Do(func() { close(release) })
		waitForSignal(t, finished, "batch teardown")
	})
	go func() {
		defer close(finished)
		_, err := o.RunBatch(ctx, Request{Issues: []int{1, 2, 3}, Sandbox: "docker", Parallel: 3,
			ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 1, MaxContainersSet: true,
			RunIDs: map[int]string{1: "cancel-1", 2: "cancel-2", 3: "cancel-3"}})
		done <- err
	}()
	waitForSignal(t, started, "container startup entered")
	waitForSignal(t, queued, "sibling admission queued")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("expected batch abort, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("batch cancellation blocked on pending container startup")
	}
	states, err := events.ReadRunStates(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cancel-1", "cancel-2", "cancel-3"} {
		if states[id].Status() != "aborted" {
			t.Errorf("%s not aborted: %+v", id, states[id])
		}
		claim, err := daemon.ClaimRun(filepath.Join(dir, ".sandman"), id)
		if err != nil {
			t.Fatalf("claim not released: %v", err)
		}
		_ = claim.Close()
	}
	for _, event := range log.snapshot() {
		if event.Type == "run.started" || event.Type == "run.retry" {
			t.Errorf("unexpected execution after pending startup abort: %+v", event)
		}
	}
	if len(runnables.created) != 0 {
		t.Fatal("agent launched after startup cancellation")
	}
}

// observedAcquireContext gives a barrier at the caller's first live admission
// check, without depending on timing or exposing the pool's synchronization.
type observedAcquireContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *observedAcquireContext) Err() error {
	err := c.Context.Err()
	if err == nil {
		c.once.Do(func() { close(c.checked) })
	}
	return err
}

func TestContainerPool_CancelWaiterPreservesSibling(t *testing.T) {
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "pending-startup", false: "exhausted-capacity"}[pending], func(t *testing.T) {
			started, release := make(chan struct{}, 1), make(chan struct{})
			container := &fakeContainerForOrchestrator{id: "sibling-owned"}
			starter := &fakeContainerStarter{container: container, startStarted: started, startRelease: release}
			pool := newContainerPool(starter, "img", ".", sandbox.StartOptions{}, 1, 1)
			defer pool.Close()
			ownerCtx, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			owner := make(chan *containerLease, 1)
			go func() {
				lease, err := pool.Acquire(ownerCtx)
				if err != nil {
					t.Errorf("sibling acquisition: %v", err)
				}
				owner <- lease
			}()
			waitForSignal(t, started, "sibling startup entered")
			var held *containerLease
			if !pending {
				close(release)
				held = <-owner
			}
			waitCtx, cancel := context.WithCancel(t.Context())
			observed := &observedAcquireContext{Context: waitCtx, checked: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				lease, err := pool.Acquire(observed)
				if lease != nil {
					lease.Release()
				}
				done <- err
			}()
			waitForSignal(t, observed.checked, "waiter entered admission")
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("waiter cancellation: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("waiter did not independently cancel")
			}
			if pending {
				select {
				case <-owner:
					t.Fatal("canceling waiter interrupted sibling startup")
				default:
				}
				close(release)
				held = <-owner
			}
			if held == nil || held.container.ID() != "sibling-owned" || container.stopCalled {
				t.Fatal("sibling lost ownership")
			}
			held.Release()
			continued, err := pool.Acquire(t.Context())
			if err != nil || continued.container.ID() != "sibling-owned" {
				t.Fatalf("sibling could not continue: %v", err)
			}
			continued.Release()
			if starter.startCount != 1 || container.stopCalled {
				t.Fatal("waiter cancellation killed or replaced the sibling container")
			}
		})
	}
}

type cancelOnInspectContainer struct {
	fakeContainerForOrchestrator
	cancel context.CancelFunc
}

func (c *cancelOnInspectContainer) Alive() (bool, error) {
	c.cancel()
	return true, nil
}

func TestContainerPool_CancellationBeforeLeaseGrant(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	container := &cancelOnInspectContainer{fakeContainerForOrchestrator: fakeContainerForOrchestrator{id: "shared"}, cancel: cancel}
	pool := newContainerPool(&fakeContainerStarter{container: container}, "img", ".", sandbox.StartOptions{}, 2, 1)
	defer pool.Close()
	owner, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	lease, err := pool.Acquire(ctx)
	if lease != nil {
		lease.Release()
		t.Error("cancellation before grant returned a sibling-owned container")
	}
	if !errors.Is(err, context.Canceled) || container.stopCalled {
		t.Fatalf("canceled grant affected sibling: err=%v stopped=%v", err, container.stopCalled)
	}
}

func TestContainerPool_CancelSuccessfulStartupBeforePublication(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	created := &fakeContainerForOrchestrator{id: "unpublished"}
	starts := 0
	starter := &cancellationStarter{start: func(context.Context) (sandbox.Container, error) {
		starts++
		if starts == 1 {
			cancel()
			return created, nil
		}
		return &fakeContainerForOrchestrator{id: "sibling"}, nil
	}}
	pool := newContainerPool(starter, "img", ".", sandbox.StartOptions{}, 2, 1)
	defer pool.Close()
	lease, err := pool.Acquire(ctx)
	if lease != nil || !errors.Is(err, context.Canceled) || !created.stopCalled {
		t.Fatalf("canceled startup leaked lease/container: lease=%v err=%v stopped=%v", lease, err, created.stopCalled)
	}
	sibling, err := pool.Acquire(t.Context())
	if err != nil || sibling.container.ID() != "sibling" {
		t.Fatalf("reservation or wrong-container leak: %v", err)
	}
	sibling.Release()
}

func TestContainerPool_CancellationAfterGrantDoesNotRevokeSibling(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	container := &fakeContainerForOrchestrator{id: "shared"}
	pool := newContainerPool(&fakeContainerStarter{container: container}, "img", ".", sandbox.StartOptions{}, 2, 1)
	defer pool.Close()
	owner, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	sibling, err := pool.Acquire(t.Context())
	if err != nil || sibling.container != owner.container || container.stopCalled {
		t.Fatalf("granted shared ownership revoked: %v", err)
	}
	owner.Release()
	sibling.Release()
}

type blockingInspectContainer struct {
	fakeContainerForOrchestrator
	inspecting chan struct{}
	release    <-chan struct{}
}

func (c *blockingInspectContainer) AliveContext(ctx context.Context) (bool, error) {
	c.inspecting <- struct{}{}
	select {
	case <-c.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestContainerPool_CancelWaiterDuringSiblingInspection(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	container := &blockingInspectContainer{fakeContainerForOrchestrator: fakeContainerForOrchestrator{id: "shared"}, inspecting: make(chan struct{}, 3), release: release}
	pool := newContainerPool(&fakeContainerStarter{container: container}, "img", ".", sandbox.StartOptions{}, 2, 1)
	defer pool.Close()
	owner, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	liveCtx, cancelLive := context.WithCancel(t.Context())
	defer cancelLive()
	live := make(chan *containerLease, 1)
	go func() { lease, _ := pool.Acquire(liveCtx); live <- lease }()
	waitForSignal(t, container.inspecting, "live sibling inspection entered")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		lease, err := pool.Acquire(ctx)
		if lease != nil {
			lease.Release()
		}
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || container.stopCalled {
			t.Fatalf("waiter canceled sibling inspection: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter cancellation blocked behind sibling inspection")
	}
	once.Do(func() { close(release) })
	select {
	case lease := <-live:
		if lease == nil || lease.container.ID() != "shared" {
			t.Fatal("live sibling lost container")
		}
		lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("sibling inspection did not finish")
	}
}

type cancellationStarter struct {
	fakeContainerStarter
	start func(context.Context) (sandbox.Container, error)
	build func(context.Context) (string, error)
}

func (s *cancellationStarter) Start(ctx context.Context, _ string, _ string, _ sandbox.StartOptions) (sandbox.Container, error) {
	return s.start(ctx)
}

func (s *cancellationStarter) BuildImage(ctx context.Context, repo string) (string, error) {
	if s.build != nil {
		return s.build(ctx)
	}
	return s.fakeContainerStarter.BuildImage(ctx, repo)
}

func TestContainerPool_StartupOwnerCancellationRetainsReservationThroughCleanup(t *testing.T) {
	started, cleaning, releaseCleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseCleanup) })
	starts := 0
	starter := &cancellationStarter{start: func(ctx context.Context) (sandbox.Container, error) {
		starts++
		if starts == 1 {
			close(started)
			<-ctx.Done()
			close(cleaning)
			<-releaseCleanup
			return nil, ctx.Err()
		}
		return &fakeContainerForOrchestrator{id: "replacement"}, nil
	}}
	pool := newContainerPool(starter, "img", ".", sandbox.StartOptions{}, 2, 1)
	defer pool.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ownerDone := make(chan error, 1)
	go func() { _, err := pool.Acquire(ctx); ownerDone <- err }()
	waitForSignal(t, started, "startup owner entered")
	cancel()
	waitForSignal(t, cleaning, "startup cleanup entered")
	waitCtx, cancelWait := context.WithCancel(t.Context())
	defer cancelWait()
	observed := &observedAcquireContext{Context: waitCtx, checked: make(chan struct{})}
	waiterDone := make(chan *containerLease, 1)
	go func() {
		lease, err := pool.Acquire(observed)
		if err != nil {
			t.Errorf("replacement acquisition: %v", err)
		}
		waiterDone <- lease
	}()
	waitForSignal(t, observed.checked, "replacement waiting during cleanup")
	// At the admission barrier the owner is still cleaning, so no container
	// can be published and the startup reservation remains counted.
	select {
	case <-waiterDone:
		t.Fatal("capacity reused before cleanup finished")
	default:
	}
	releaseOnce.Do(func() { close(releaseCleanup) })
	if err := <-ownerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("startup owner cancellation: %v", err)
	}
	select {
	case lease := <-waiterDone:
		if lease == nil || lease.container.ID() != "replacement" {
			t.Fatal("live sibling did not acquire replacement")
		}
		lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("reservation leaked after cancellation")
	}
	if starts != 2 {
		t.Fatalf("container starts = %d, want 2", starts)
	}
}

func TestRunBatch_CancelImageBuildReleasesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	configPath := filepath.Join(dir, "agent-config")
	if err := os.WriteFile(configPath, []byte("settings"), 0600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	starter := &cancellationStarter{build: func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}, start: func(context.Context) (sandbox.Container, error) {
		t.Error("container started after build abort")
		return nil, errors.New("unexpected startup")
	}}
	log := &spyEventLog{}
	runDir := filepath.Join(dir, ".sandman", "build-run")
	o := NewOrchestrator(&fakeGitHubClient{}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true", ConfigFiles: []string{configPath}}},
	}}, log, WithErrorLog(io.Discard), WithContainerRuntimeFactory(&fakeContainerRuntimeFactory{starter: starter}), WithSandboxFactory(&trackingSandboxFactory{}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(ctx, Request{Issues: []int{1}, Sandbox: "docker", RunDir: runDir})
		done <- err
	}()
	waitForSignal(t, started, "image build entered")
	if _, err := os.Stat(filepath.Join(runDir, "config")); err != nil {
		t.Fatalf("config snapshot not prepared before build: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("build cancellation lost context error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("batch blocked on canceled build")
	}
	if _, err := os.Stat(filepath.Join(runDir, "config")); !os.IsNotExist(err) {
		t.Fatalf("build config leaked: %v", err)
	}
	for _, event := range log.snapshot() {
		if event.RunID != "" {
			t.Fatalf("build before row ownership fabricated run events: %+v", event)
		}
	}
}

type startupOrderedContentClient struct {
	fakeGitHubClient
	startup <-chan struct{}
}

func (c *startupOrderedContentClient) FetchIssueContent(ctx context.Context, number int) (*github.Issue, error) {
	if number != 1 {
		select {
		case <-c.startup:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.FetchIssue(ctx, number)
}

func TestRunBatch_CancelWaitingRowAllowsSiblingSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	starts := 0
	starter := &cancellationStarter{start: func(ctx context.Context) (sandbox.Container, error) {
		starts++
		if starts == 1 {
			close(started)
		}
		select {
		case <-release:
			return &fakeContainerForOrchestrator{id: "shared"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	log := &spyEventLog{}
	client := &startupOrderedContentClient{startup: started, fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{
		1: {Number: 1, Title: "One"}, 2: {Number: 2, Title: "Two"},
	}, prs: map[string]*github.PR{
		"1-one": {Number: 1, State: "closed", Merged: true, HeadRefName: "1-one"},
	}}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, log, WithErrorLog(io.Discard), WithContainerRuntimeFactory(&fakeContainerRuntimeFactory{starter: starter}),
		WithRunnableFactory(&byIssueRunnableFactory{results: map[int]AgentRunResult{
			1: {IssueNumber: 1, Status: "success"},
		}}), WithSandboxFactory(&freshSandboxFactory{}))
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		once.Do(func() { close(release) })
		waitForSignal(t, finished, "batch teardown")
	})
	done := make(chan error, 1)
	go func() {
		defer close(finished)
		_, err := o.RunBatch(ctx, Request{Issues: []int{1, 2}, Sandbox: "docker", Parallel: 2,
			ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 1, MaxContainersSet: true,
			RunIDs: map[int]string{1: "sibling-1", 2: "waiter-2"}})
		done <- err
	}()
	waitForSignal(t, started, "first row startup")
	if err := o.AbortIssue(2); err != nil {
		t.Fatalf("abort waiting row: %v", err)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("expected partial batch abort, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("siblings did not finish after waiter abort")
	}
	states, err := events.ReadRunStates(log)
	if err != nil {
		t.Fatal(err)
	}
	for id, status := range map[string]string{"sibling-1": "success", "waiter-2": "aborted"} {
		if states[id].Status() != status {
			t.Errorf("%s status = %s, want %s", id, states[id].Status(), status)
		}
	}
	if starts != 1 {
		t.Fatalf("waiting-row abort replaced shared container: starts=%d", starts)
	}
}
