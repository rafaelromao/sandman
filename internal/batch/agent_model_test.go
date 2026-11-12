package batch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const claudeAgentRefusal = `model "claude-agent/sonnet" cannot run on agent "opencode-v2"`

// launchRecorder stands in for the runnable and sandbox factories, so a
// launch the check fails to stop runs a fake instead of a real agent, and the
// test can tell it was reached.
type launchRecorder struct {
	mu        sync.Mutex
	runnables int
	sandboxes int
}

func (r *launchRecorder) NewRunnable(issue *github.Issue, _ string, _ sandbox.Sandbox) Runnable {
	r.mu.Lock()
	r.runnables++
	r.mu.Unlock()
	result := AgentRunResult{Status: "failure"}
	if issue != nil {
		result.IssueNumber = issue.Number
	}
	return &fakeRunnable{result: result}
}

func (r *launchRecorder) NewSandbox(string, string, string, string, sandbox.Container) sandbox.Sandbox {
	r.mu.Lock()
	r.sandboxes++
	r.mu.Unlock()
	return &fakeSandbox{}
}

func (r *launchRecorder) requireNothingLaunched(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runnables != 0 || r.sandboxes != 0 {
		t.Fatalf("a refused pairing reached launch: %d runnables, %d sandboxes", r.runnables, r.sandboxes)
	}
}

func requireNoSandmanState(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".sandman")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused launch prepared .sandman state: %v", err)
	}
}

// RunBatch is the last net under every launch path (run, continuation, the
// review daemon and the Portal): a refused pairing never reaches a batch.
func TestRunBatch_RefusesClaudeAgentModelOnAnotherAgent(t *testing.T) {
	dir := testenv.MkdirShort(t, "agent-model-")
	t.Chdir(dir)
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}}
	cfg := &config.Config{Agent: "opencode-v2", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	launches := &launchRecorder{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, nil,
		WithErrorLog(io.Discard), WithRunnableFactory(launches), WithSandboxFactory(launches))

	_, err := o.RunBatch(t.Context(), Request{Issues: []int{42}, Agent: "opencode-v2", Model: "claude-agent/sonnet"})
	if err == nil || !strings.Contains(err.Error(), claudeAgentRefusal) {
		t.Fatalf("RunBatch error = %v, want it to mention %q", err, claudeAgentRefusal)
	}
	launches.requireNothingLaunched(t)
	requireNoSandmanState(t, dir)
}

func TestRunRepair_RefusesClaudeAgentModelOnAnotherAgent(t *testing.T) {
	dir := testenv.MkdirShort(t, "agent-model-repair-")
	t.Chdir(dir)
	cfg := &config.Config{DefaultAgent: "opencode-v2", Sandbox: "podman",
		AgentProviders: map[string]config.Agent{"opencode-v2": {Preset: "opencode-v2", Model: "claude-agent/sonnet"}}}
	launches := &launchRecorder{}
	o := NewOrchestrator(nil, &noopRenderer{}, &fakeConfigStore{config: cfg}, nil,
		WithErrorLog(io.Discard), WithRunnableFactory(launches), WithSandboxFactory(launches))

	_, err := o.RunRepair(t.Context(), Request{RunID: "261009123456-abcd-prompt-health-repair",
		PromptConfig: prompt.RenderConfig{TaskPrompt: "diagnose and repair"}}, cfg)
	if err == nil || !strings.Contains(err.Error(), claudeAgentRefusal) {
		t.Fatalf("RunRepair error = %v, want it to mention %q", err, claudeAgentRefusal)
	}
	launches.requireNothingLaunched(t)
	requireNoSandmanState(t, dir)
}
