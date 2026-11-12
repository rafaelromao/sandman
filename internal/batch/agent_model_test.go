package batch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const claudeAgentRefusal = `model "claude-agent/sonnet" cannot run on agent "opencode-v2"`

// withoutRealAgents puts stub agent CLIs first on PATH, so a launch the check
// fails to stop runs a stub that exits non-zero instead of a real agent.
func withoutRealAgents(t *testing.T, dir string) {
	t.Helper()
	bin := filepath.Join(dir, "stub-bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"opencode", "claude"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 97\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	withoutRealAgents(t, dir)
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}}
	cfg := &config.Config{Agent: "opencode-v2", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, nil, WithErrorLog(io.Discard))

	_, err := o.RunBatch(t.Context(), Request{Issues: []int{42}, Agent: "opencode-v2", Model: "claude-agent/sonnet"})
	if err == nil || !strings.Contains(err.Error(), claudeAgentRefusal) {
		t.Fatalf("RunBatch error = %v, want it to mention %q", err, claudeAgentRefusal)
	}
	requireNoSandmanState(t, dir)
}

func TestRunRepair_RefusesClaudeAgentModelOnAnotherAgent(t *testing.T) {
	dir := testenv.MkdirShort(t, "agent-model-repair-")
	t.Chdir(dir)
	withoutRealAgents(t, dir)
	cfg := &config.Config{DefaultAgent: "opencode-v2", Sandbox: "podman",
		AgentProviders: map[string]config.Agent{"opencode-v2": {Preset: "opencode-v2", Model: "claude-agent/sonnet"}}}
	o := NewOrchestrator(nil, &noopRenderer{}, &fakeConfigStore{config: cfg}, nil, WithErrorLog(io.Discard))

	_, err := o.RunRepair(t.Context(), Request{RunID: "261009123456-abcd-prompt-health-repair",
		PromptConfig: prompt.RenderConfig{TaskPrompt: "diagnose and repair"}}, cfg)
	if err == nil || !strings.Contains(err.Error(), claudeAgentRefusal) {
		t.Fatalf("RunRepair error = %v, want it to mention %q", err, claudeAgentRefusal)
	}
	requireNoSandmanState(t, dir)
}
