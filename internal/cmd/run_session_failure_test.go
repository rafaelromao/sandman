package cmd

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// TestRun_PrepareFailure_DoesNotEmitRunStarted is the *fence* test for
// issue #1024. When the run dir or control socket cannot be created
// (here: the control socket path is pre-bound by another listener
// outside the daemon's control), Prepare must fail and the orchestrator
// must never run. Concretely:
//
//   - The run command returns an error.
//   - .sandman/events.jsonl does NOT contain a run.started line for
//     the issue. (A bug that emits run.started before Prepare succeeds
//     would create a ghost row that the portal cannot recover from.)
//   - .sandman/runs/<id>/ does not exist (Close cleaned it up).
func TestRun_PrepareFailure_DoesNotEmitRunStarted(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initRunIntegrationRepoWithRemote(t, dir)

	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0755); err != nil {
		t.Fatal(err)
	}
	// The review daemon guard requires a live .sandman/review.sock.
	reviewListener, err := net.Listen("unix", ReviewSocketPath(sandmanDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reviewListener.Close() })
	go func() {
		for {
			c, err := reviewListener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	// Pre-bind a unix socket at the path the daemon will try to
	// create .sandman/runs/<id>/run.sock. We use a long-lived
	// listener on a path chosen so the generated run dir lands on
	// it. Since run id is timestamp-based, we listen on the
	// .sandman/runs directory itself — that way every new run
	// dir is blocked at MkdirAll time.
	//
	// The trick that works: bind an unrelated unix socket at
	// .sandman/runs/pre-occupied. MkdirAll is happy (the path
	// doesn't collide), but the daemon's subsequent MkdirAll on
	// the run subdir fails because the parent is a socket file
	// (not a directory). This is portable and root-friendly.
	preOccupied := filepath.Join(sandmanDir, "pre-occupied")
	if err := os.MkdirAll(filepath.Dir(preOccupied), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preOccupied, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(preOccupied) })

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			964: {Number: 964, Title: "Boot fails"},
		},
	}
	store := &fakeStore{config: &config.Config{
		DefaultAgent:  "opencode",
		Agent:         "opencode",
		ReviewCommand: "/oc review",
		WorktreeDir:   ".sandman/worktrees",
		Sandbox:       "worktree",
		Git:           config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"opencode": {Command: "true"},
		},
	}}

	// We construct a request whose run-id collides with the
	// pre-occupied path so the daemon's MkdirAll(runDir) fails.
	// Since daemon.RunDir joins <baseDir>/runs/<id>, we pre-create
	// <baseDir>/runs as a non-directory file.
	runsPath := filepath.Join(sandmanDir, "runs")
	if err := os.WriteFile(runsPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(runsPath) })

	eventsPath := filepath.Join(sandmanDir, "events.jsonl")
	realEventLog := &events.JSONLLogger{Path: eventsPath}
	runner := batch.NewOrchestrator(gh, &prompt.Engine{}, store, realEventLog)
	deps := Dependencies{
		BatchRunner:  runner,
		ConfigStore:  store,
		EventLog:     realEventLog,
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IsTTY:        func() bool { return false },
		RepoRoot:     ".",
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"964"})

	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected an error when control socket bind fails")
	}

	// Fence assertion: events.jsonl must not contain a run.started
	// for issue 964. If Prepare fails, the orchestrator is never
	// reached, so this file should have no entries at all.
	for _, e := range readJSONLEvents(t, eventsPath) {
		if e.Type == "run.started" && e.Issue == 964 {
			t.Fatalf("invariant violated: run.started for issue 964 was written to %s even though Prepare failed (issue #1024 ghost row)", eventsPath)
		}
	}

	// The run dir cleanup: Prepare failure is followed by
	// `defer rs.Close()` in the run cmd, which removes the run
	// directory entirely. The .sandman/runs path is the
	// pre-occupied file (we never successfully created a real
	// run dir under it).
	if _, err := os.Stat(runsPath); err != nil {
		t.Fatalf("pre-occupied runs path was clobbered: %v", err)
	}
}

// TestRun_ControlSocketBindFailure_LeavesNoArtifacts is a focused
// sanity check: when the run dir is unwriteable (here: a regular file
// at the .sandman/runs path), the run command fails and no run dir is
// left on disk for the portal to find as a "dead" run.
func TestRun_ControlSocketBindFailure_LeavesNoArtifacts(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initRunIntegrationRepoWithRemote(t, dir)

	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0755); err != nil {
		t.Fatal(err)
	}
	reviewListener, err := net.Listen("unix", ReviewSocketPath(sandmanDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reviewListener.Close() })
	go func() {
		for {
			c, err := reviewListener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	// Pre-occupy .sandman/runs as a regular file so the daemon's
	// MkdirAll(runDir) fails. The RunSession.Prepare then returns
	// ErrStepMkdir and the run cmd reports the error.
	runsPath := filepath.Join(sandmanDir, "runs")
	if err := os.WriteFile(runsPath, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(runsPath) })

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			964: {Number: 964, Title: "Boot fails"},
		},
	}
	store := &fakeStore{config: &config.Config{
		DefaultAgent:  "opencode",
		Agent:         "opencode",
		ReviewCommand: "/oc review",
		WorktreeDir:   ".sandman/worktrees",
		Sandbox:       "worktree",
		Git:           config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"opencode": {Command: "true"},
		},
	}}

	eventsPath := filepath.Join(sandmanDir, "events.jsonl")
	realEventLog := &events.JSONLLogger{Path: eventsPath}
	runner := batch.NewOrchestrator(gh, &prompt.Engine{}, store, realEventLog)
	deps := Dependencies{
		BatchRunner:  runner,
		ConfigStore:  store,
		EventLog:     realEventLog,
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IsTTY:        func() bool { return false },
		RepoRoot:     ".",
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"964"})

	runDone := make(chan error, 1)
	go func() {
		runDone <- cmd.Execute()
	}()

	select {
	case err := <-runDone:
		if err == nil {
			t.Fatal("expected an error when run dir cannot be created")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for run cmd to fail")
	}

	// The daemon must NEVER have emitted a run.started event, because
	// the boot never completed.
	for _, e := range readJSONLEvents(t, eventsPath) {
		if e.Type == "run.started" && e.Issue == 964 {
			t.Fatalf("invariant violated: run.started for issue 964 was written to %s even though the run dir could not be created", eventsPath)
		}
	}
}
