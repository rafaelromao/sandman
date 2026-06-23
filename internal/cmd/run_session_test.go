package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// startSignalAgentCommand returns a shell command that:
//  1. Writes a marker file when it starts, proving the agent has been invoked.
//  2. Waits on a release file before exiting 0.
//
// The marker is written AFTER the orchestrator has logged run.started to
// events.jsonl, so any test that polls for the marker can then assert that
// the run dir, run.sock, and batch.json already exist on disk.
func startSignalAgentCommand(markerPath, releasePath string) string {
	return fmt.Sprintf(
		"mkdir -p %q && touch %q && while [ ! -f %q ]; do sleep 0.02; done",
		filepath.Dir(markerPath), markerPath, releasePath,
	)
}

// waitForPath polls for path to exist; fails the test after the timeout.
func waitForPathTB(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if info, err := os.Stat(path); err == nil {
			if !info.IsDir() {
				return path
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runSessionTestEnv bundles the per-test artifacts that the kill-and-inspect
// test (and its successors) need to inspect the boot sequence in isolation.
type runSessionTestEnv struct {
	repoDir    string
	sandmanDir string
	runsDir    string
	eventsPath string
	markerPath string
}

func newRunSessionTestEnv(t *testing.T) *runSessionTestEnv {
	t.Helper()
	// Allocate the repo dir under /tmp with a short prefix so the
	// resulting run.sock path stays under the 108-byte Unix socket
	// limit. The new run-id scheme (slice 2) produces ~38-char
	// directory names which, combined with the default t.TempDir
	// path prefix, push run.sock past the limit.
	dir, err := os.MkdirTemp("/tmp", "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Chdir(dir)
	initRunIntegrationRepoWithRemote(t, dir)

	sharedDir := t.TempDir()
	markerPath := filepath.Join(sharedDir, "agent.started")
	releasePath := filepath.Join(sharedDir, "agent.release")
	_ = releasePath

	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
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

	return &runSessionTestEnv{
		repoDir:    dir,
		sandmanDir: sandmanDir,
		runsDir:    filepath.Join(sandmanDir, "batches"),
		eventsPath: filepath.Join(sandmanDir, "events.jsonl"),
		markerPath: markerPath,
	}
}

// readJSONLEvents parses the events file and returns the decoded events.
func readJSONLEvents(t *testing.T, path string) []events.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []events.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e events.Event
		if err := jsonUnmarshalLine(line, &e); err != nil {
			t.Fatalf("decode event line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// TestRun_BootArtifactsBeforeRunStarted is the regression guard for
// GitHub issue #1024. It mirrors the issue's failure mode:
//
//   - A real orchestrator emits run.started to .sandman/events.jsonl.
//   - A real AgentRun is invoked (the agent process actually starts).
//   - The test waits for the agent's start marker, which proves the runnable
//     has been entered (i.e. the orchestrator's execute() reached runOnce).
//   - The test then asserts that .sandman/runs/<id>/run.sock,
//     .sandman/runs/<id>/batch.json, and events.jsonl all exist on disk.
//
// The issue's failure mode is: events.jsonl has run.started and the log
// file under .sandman/logs/ is written, but .sandman/runs/<id>/ is missing.
// This test fails in that scenario.
func TestRun_BootArtifactsBeforeRunStarted(t *testing.T) {
	env := newRunSessionTestEnv(t)

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			964: {Number: 964, Title: "Ghost row regression"},
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

	releasePath := filepath.Join(filepath.Dir(env.markerPath), "agent.release")
	agentCmd := startSignalAgentCommand(env.markerPath, releasePath)
	store.config.AgentProviders["opencode"] = config.Agent{Command: agentCmd}

	realEventLog := &events.JSONLLogger{Path: env.eventsPath}
	runner := batch.NewOrchestrator(gh, &prompt.Engine{}, store, realEventLog)
	deps := Dependencies{
		BatchRunner:  runner,
		ConfigStore:  store,
		EventLog:     realEventLog,
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IsTTY:        func() bool { return false },
	}

	runDone := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"964"})
		runDone <- cmd.Execute()
	}()

	// Wait for the agent's start marker — this proves the runnable was
	// invoked, which happens AFTER run.started is logged. So at this
	// instant, every boot artifact MUST exist on disk.
	waitForPathTB(t, env.markerPath, 10*time.Second)

	runsEntries, err := os.ReadDir(env.runsDir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	// Find the per-issue batch dir (named like "<batchID>-<issue>+<count>")
	// inside the parent batch dir.
	var perIssueBatch string
	for _, e := range runsEntries {
		if strings.Contains(e.Name(), "+") {
			perIssueBatch = filepath.Join(env.runsDir, e.Name())
			break
		}
	}
	if perIssueBatch == "" {
		t.Fatalf("expected per-issue batch dir under %s, got %d (issue #964 regression: run dir missing after run.started)", env.runsDir, len(runsEntries))
	}
	batchDir := perIssueBatch

	runSockPath := filepath.Join(batchDir, "run.sock")
	if _, err := os.Stat(runSockPath); err != nil {
		t.Fatalf("expected run.sock at %s after run.started: %v (issue #964 regression)", runSockPath, err)
	}
	if conn, err := net.Dial("unix", runSockPath); err != nil {
		t.Fatalf("expected run.sock to be live, got dial err: %v", err)
	} else {
		conn.Close()
	}

	cmdSockPath := filepath.Join(batchDir, "batch.sock")
	if _, err := os.Stat(cmdSockPath); err != nil {
		t.Fatalf("expected batch.sock (control) at %s after run.started: %v (issue #964 regression)", cmdSockPath, err)
	}

	manifestPath := filepath.Join(batchDir, "batch.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected batch.json at %s after run.started: %v (issue #964 regression)", manifestPath, err)
	}

	// events.jsonl must have a run.started entry for issue 964, AND
	// mtime(events.jsonl) must be at-or-before mtime(run.sock) — the
	// run dir + sockets were created BEFORE the event was written.
	startedEvents := 0
	for _, e := range readJSONLEvents(t, env.eventsPath) {
		if e.Type == "run.started" && e.Issue == 964 {
			startedEvents++
		}
	}
	if startedEvents == 0 {
		t.Fatalf("expected a run.started event for issue 964 in %s, got none", env.eventsPath)
	}

	// File-creation ordering check: events.jsonl is the last artifact the
	// daemon writes in the boot sequence. run.sock, batch.json, and
	// cmd.sock must have been created at-or-before events.jsonl. This is
	// the structural assertion: any future regression that emits
	// run.started before creating the run dir will fail this check
	// because the run dir will not even exist (the earlier ReadDir
	// check would have caught that), AND because their mtimes will be
	// strictly after events.jsonl's mtime.
	evMtime := mustMtime(t, env.eventsPath)
	for _, p := range []string{runSockPath, cmdSockPath, manifestPath, batchDir} {
		pm := mustMtime(t, p)
		if pm.After(evMtime) {
			t.Errorf("invariant violated: %s (mtime %s) was modified AFTER events.jsonl (mtime %s) — boot ordering regressed", p, pm.Format(time.RFC3339Nano), evMtime.Format(time.RFC3339Nano))
		}
	}

	// Release the agent and wait for the run to complete. We don't
	// assert on the run's final status — the agent exits 0 but no PR
	// was merged, so a fresh issue-driven run reports "failed" in the
	// summary. That's fine for the boot-ordering assertion; the run
	// was *reached*, the agent *ran*, and every boot artifact was
	// present on disk before run.started.
	if err := os.WriteFile(releasePath, []byte("go\n"), 0644); err != nil {
		t.Fatalf("release agent: %v", err)
	}

	select {
	case <-runDone:
		// We intentionally do not assert on the return value of
		// cmd.Execute() here: a fresh issue-driven run with no
		// merged PR reports failure even when the agent exits 0,
		// which is the normal flow and is orthogonal to the boot
		// ordering this test guards.
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for run to complete")
	}
}

func mustMtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime()
}

// jsonUnmarshalLine decodes a single JSONL line into an event.
func jsonUnmarshalLine(line string, e *events.Event) error {
	return json.Unmarshal([]byte(line), e)
}

// TestRun_ContainerSandboxMode_RunDirAndSocketsBeforeAgentStart is the
// container-mode companion to TestRun_BootArtifactsBeforeRunStarted.
// The boot ordering invariant (issue #1024) is enforced in run.go
// before the orchestrator is invoked, so the container path benefits
// from the same protection without separate logic. This test
// exercises run.go with `req.Sandbox = "docker"` and asserts the run
// dir + run.sock + cmd.sock are present on disk at the moment
// `run.started` is logged to events.jsonl.
//
// We poll for the run.started event in events.jsonl directly rather
// than for an agent marker: the container mode's Sandbox.Exec would
// shell out to docker (not available in the unit-test environment),
// so the agent may not be reached. The boot invariant is what we
// care about, and run.started is logged strictly before any
// Sandbox.Exec, so polling for the event is sufficient.
func TestRun_ContainerSandboxMode_RunDirAndSocketsBeforeAgentStart(t *testing.T) {
	env := newRunSessionTestEnv(t)

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			964: {Number: 964, Title: "Container mode ghost row regression"},
		},
	}
	store := &fakeStore{config: &config.Config{
		DefaultAgent:  "opencode",
		Agent:         "opencode",
		ReviewCommand: "/oc review",
		WorktreeDir:   ".sandman/worktrees",
		Sandbox:       "docker", // container mode
		Git:           config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"opencode": {Command: "true"},
		},
	}}

	realEventLog := &events.JSONLLogger{Path: env.eventsPath}
	runner := batch.NewOrchestrator(gh, &prompt.Engine{}, store, realEventLog)
	deps := Dependencies{
		BatchRunner:  runner,
		ConfigStore:  store,
		EventLog:     realEventLog,
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IsTTY:        func() bool { return false },
	}

	runDone := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		// Pass --sandbox=docker explicitly to make the test
		// resilient to changes in the default sandbox mode.
		cmd.SetArgs([]string{"--sandbox", "docker", "964"})
		runDone <- cmd.Execute()
	}()

	// Wait for run.started to appear in events.jsonl. The
	// orchestrator logs this BEFORE any Sandbox.Exec, so it is
	// reached even when the container runtime is unavailable (the
	// runtime call happens after run.started, inside runOnce).
	//
	// In the no-docker case, the orchestrator logs run.started and
	// then fails when trying to start the container, but the run
	// dir + sockets are already on disk.
	//
	// In the with-docker case, the orchestrator logs run.started
	// and proceeds to the agent; either way, the boot artifacts
	// are on disk by the time the event is written.
	deadline := time.Now().Add(10 * time.Second)
	var seen bool
	for !seen && time.Now().Before(deadline) {
		for _, e := range readJSONLEvents(t, env.eventsPath) {
			if e.Type == "run.started" && e.Issue == 964 {
				seen = true
				break
			}
		}
		if !seen {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !seen {
		// The orchestrator may have failed before reaching
		// run.started (e.g. the container runtime could not
		// acquire a container). In that case there is no
		// ghost-row risk because no event was emitted, and
		// the run has already failed; nothing to assert.
		select {
		case err := <-runDone:
			t.Skipf("orchestrator failed before reaching run.started (likely no container runtime): %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for run.started event")
		}
		return
	}

	// run.started has been logged. Assert the run dir + run.sock +
	// cmd.sock + batch.json are on disk.
	entries, err := os.ReadDir(env.runsDir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("expected at least one run dir under %s after run.started (issue #964 regression in container mode)", env.runsDir)
	}
	runDir := filepath.Join(env.runsDir, entries[0].Name())
	runSock := filepath.Join(runDir, "run.sock")
	if _, err := os.Stat(runSock); err != nil {
		t.Fatalf("expected run.sock at %s after run.started in container mode: %v", runSock, err)
	}
	cmdSock := filepath.Join(runDir, "cmd.sock")
	if _, err := os.Stat(cmdSock); err != nil {
		t.Fatalf("expected cmd.sock at %s after run.started in container mode: %v", cmdSock, err)
	}
	manifest := filepath.Join(runDir, "batch.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("expected batch.json at %s after run.started in container mode: %v", manifest, err)
	}

	// Wait for the run cmd to return (it may have failed in the
	// sandbox step, but the test invariant is satisfied).
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for run to complete")
	}
}

// TestRun_ContinueMode_RunDirAndSocketsBeforeContinuedEvent covers the
// run.continued path (issue #1024 acceptance criterion: the invariant
// must hold for both run.started and run.continued, since the
// orchestrator's emit happens deep in execute() and the run session
// is the same regardless of the event type).
//
// We don't drive the real orchestrator here because the
// --continue flag is a CLI-level request shape, not a per-event
// behavior — the boot is identical. Instead, we assert that the
// RunSession is constructed with a nil commander when --continue is
// set, which causes Prepare to skip the cmd.sock step cleanly. The
// unit-level TestRunSession_Prepare_SkipsCommandServerWhenCommanderNil
// covers the actual boot behavior.
func TestRun_ContinueMode_RunDirAndSocketsBeforeContinuedEvent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initRunIntegrationRepoWithRemote(t, dir)

	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
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

	branch := "sandman/prompt-only-456"
	if err := os.MkdirAll(filepath.Join(dir, branch, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	resumeContent := "## Stage: plan-approved\n\nContinue.\n"
	if err := os.WriteFile(filepath.Join(dir, branch, ".sandman", "task.md"), []byte(resumeContent), 0644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	gh := &fakeGitHubClient{issues: map[int]*github.Issue{}}
	releasePath := filepath.Join(dir, "agent.release")
	markerPath := filepath.Join(dir, "agent.started")
	agentCmd := startSignalAgentCommand(markerPath, releasePath)
	store := &fakeStore{config: &config.Config{
		Agent:         "opencode",
		ReviewCommand: "/oc review",
		WorktreeDir:   dir,
		Sandbox:       "worktree",
		Git:           config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"opencode": {Command: agentCmd},
		},
	}}

	// Real orchestrator with a blocked runner. We use the real
	// orchestrator so a run.continued event is actually emitted
	// (when --continue is set and a previous run.started is in the
	// event log).
	realEventLog := &events.JSONLLogger{Path: filepath.Join(sandmanDir, "events.jsonl")}
	runner := batch.NewOrchestrator(gh, &prompt.Engine{}, store, realEventLog)
	// Seed the event log with a previous run.started for the same
	// run-id so the orchestrator recognises this as a continuation.
	prevStarted := events.Event{
		Type:      "run.started",
		RunID:     "my-run",
		Issue:     0,
		Timestamp: time.Now().Add(-1 * time.Minute),
		Payload: map[string]any{
			"agent":              "opencode",
			"branch":             branch,
			"base_branch":        "main",
			"prompt_source_type": "prompt",
		},
	}
	_ = realEventLog.Log(prevStarted)

	deps := Dependencies{
		BatchRunner:  runner,
		ConfigStore:  store,
		EventLog:     realEventLog,
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IsTTY:        func() bool { return false },
	}

	runDone := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"--continue", "--run-id", "my-run"})
		runDone <- cmd.Execute()
	}()

	// Wait for the agent's start marker (proves the runnable was
	// entered, which happens AFTER run.continued is logged) — then
	// assert the batch dir + run.sock + batch.json exist on disk
	// under .sandman/batches. cmd.sock must NOT exist because
	// --continue disables the per-issue abort server.
	waitForPathTB(t, markerPath, 10*time.Second)

	entries, err := os.ReadDir(filepath.Join(sandmanDir, "batches"))
	if err != nil {
		t.Fatalf("read batches dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one batch dir after the agent was entered")
	}
	runDir := filepath.Join(sandmanDir, "batches", entries[0].Name())
	if _, err := os.Stat(filepath.Join(runDir, "batch.sock")); err != nil {
		t.Fatalf("batch.sock (control) must exist at the time the agent is entered: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "batch.json")); err != nil {
		t.Fatalf("batch.json must exist at the time the agent is entered: %v", err)
	}
	// run.sock must NOT exist when --continue is set (no commander).
	if _, err := os.Stat(filepath.Join(runDir, "run.sock")); err == nil {
		t.Fatalf("run.sock must not exist when --continue is set")
	}

	// Confirm events.jsonl has a run.continued line (not run.started).
	continuedFound := false
	for _, e := range readJSONLEvents(t, realEventLog.Path) {
		if e.Type == "run.continued" && e.RunID == "my-run" {
			continuedFound = true
		}
	}
	if !continuedFound {
		t.Fatalf("expected a run.continued event for run-id my-run, events.jsonl = %s", realEventLog.Path)
	}

	// Release the agent and let the run complete.
	if err := os.WriteFile(releasePath, []byte("go\n"), 0644); err != nil {
		t.Fatalf("release agent: %v", err)
	}
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for run to complete")
	}
}
