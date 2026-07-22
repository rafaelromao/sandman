//go:build e2e

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestRun_ContinueFlag_PromptOnlyUsesCurrentConfig_E2E(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/prompt-only-456"
	if err := os.MkdirAll(filepath.Join(dir, branch, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	resumeContent := "## Stage: plan-approved\n\nContinue.\n"
	if err := os.WriteFile(filepath.Join(dir, branch, ".sandman", "task.md"), []byte(resumeContent), 0644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode-current",
		DefaultModel:  "openai/gpt-4.1",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{
			"opencode-current": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: "run-0-abc", Issue: 0, Payload: map[string]any{"agent": "opencode-stored", "model": "gpt-4.2", "review_command": "/stored review", "parallel": 9, "start_delay": 30, "start_delay_set": true, "retries": 7, "sandbox": "worktree", "container_capacity": 8, "container_capacity_set": true, "max_containers": 4, "max_containers_set": true, "run_idle_timeout": 999, "run_idle_timeout_set": true, "branch": branch, "base_branch": "main", "prompt_source_type": "prompt"}}}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "--run-id", "my-run"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.req.Mode == nil || spy.req.IssueMode(0) != batch.ModeContinue {
		t.Fatal("expected continuation request")
	}
	if spy.req.RunID != "my-run" {
		t.Fatalf("expected RunID=my-run, got %q", spy.req.RunID)
	}
	if spy.req.Issues != nil {
		t.Fatalf("expected prompt-only issues to be nil, got %#v", spy.req.Issues)
	}
	if spy.req.BaseBranch != "main" {
		t.Fatalf("expected BaseBranch=main, got %q", spy.req.BaseBranch)
	}
	if spy.req.PromptConfig.Branch != branch {
		t.Fatalf("expected PromptConfig.Branch=%q, got %q", branch, spy.req.PromptConfig.Branch)
	}
	if !strings.Contains(spy.req.PromptConfig.TaskPrompt, "## Stage: plan-approved") {
		t.Fatalf("expected verbatim resume prompt with prior ## Stage, got %q", spy.req.PromptConfig.TaskPrompt)
	}
	if strings.Contains(spy.req.PromptConfig.TaskPrompt, "## Prior Context") {
		t.Fatalf("expected verbatim resume prompt (not rewritten), got %q", spy.req.PromptConfig.TaskPrompt)
	}
	if strings.Contains(buf.String(), "warning: no task found") {
		t.Fatalf("did not expect missing-task warning, got %q", buf.String())
	}
	if spy.req.Agent != "opencode-current" {
		t.Fatalf("expected agent from current cfg, got %q", spy.req.Agent)
	}
	if spy.req.Model != "openai/gpt-4.1" {
		t.Fatalf("expected model from cfg.DefaultModel, got %q", spy.req.Model)
	}
	if spy.req.Parallel != 0 {
		t.Fatalf("expected parallel from current cfg (default 0), got %d", spy.req.Parallel)
	}
	if spy.req.StartDelay != 0 || spy.req.StartDelaySet {
		t.Fatalf("expected start delay unset, got %s set=%v", spy.req.StartDelay, spy.req.StartDelaySet)
	}
	if spy.req.RunIdleTimeout != 0 || spy.req.RunIdleTimeoutSet {
		t.Fatalf("expected run idle timeout unset, got %d set=%v", spy.req.RunIdleTimeout, spy.req.RunIdleTimeoutSet)
	}
	if spy.req.Retries != -1 {
		t.Fatalf("expected retries sentinel (-1) from current cfg, got %d", spy.req.Retries)
	}
	if spy.req.Sandbox != "" {
		t.Fatalf("expected sandbox from current cfg (default empty), got %q", spy.req.Sandbox)
	}
	if spy.req.ContainerCapacity != 0 || spy.req.ContainerCapacitySet {
		t.Fatalf("expected container capacity unset, got %d set=%v", spy.req.ContainerCapacity, spy.req.ContainerCapacitySet)
	}
	if spy.req.MaxContainers != 0 || spy.req.MaxContainersSet {
		t.Fatalf("expected max containers unset, got %d set=%v", spy.req.MaxContainers, spy.req.MaxContainersSet)
	}
	if spy.req.PromptConfig.ReviewCommand != "/oc review" {
		t.Fatalf("expected review command from cfg, got %q", spy.req.PromptConfig.ReviewCommand)
	}
}

func TestRun_ContinueFlag_WarnsWhenPromptOnlyTaskMissing(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/prompt-only-456"
	if err := os.MkdirAll(filepath.Join(dir, branch, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: "run-0-abc", Issue: 0, Payload: map[string]any{"agent": "opencode", "branch": branch, "base_branch": "main", "prompt_source_type": "prompt"}}}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "--run-id", "my-run"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "warning: no task found") {
		t.Fatalf("expected missing-task warning, got %q", buf.String())
	}
}
