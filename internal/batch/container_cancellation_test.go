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
}
