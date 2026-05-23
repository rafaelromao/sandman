package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
)

type spyContinueBatchRunner struct {
	called bool
	req    batch.Request
	result *batch.Result
	err    error
}

func (s *spyContinueBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	s.called = true
	s.req = req
	return s.result, s.err
}

func TestContinue_NoArgsReturnsError(t *testing.T) {
	deps := newTestDeps()
	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no issue provided")
	}
}

func TestContinue_InvalidIssueReturnsError(t *testing.T) {
	deps := newTestDeps()
	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"abc", "finish the tests"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when invalid issue provided")
	}
}

func TestContinue_LooksUpLastRunAndInvokesBatchRunner(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyContinueBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "model": "gpt-4.1", "agent": "opencode", "review_command": "/custom review"}},
		{Type: "run.continued", RunID: "run-42-2", Issue: 42, Payload: map[string]any{"branch": branch, "model": "gpt-4.2", "agent": "pi", "review_command": "/custom review 2"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, ReviewCommand: "/current review", AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}, "pi": {Preset: "pi", Command: "true"}}}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "finish the tests"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 42 {
		t.Fatalf("expected issue 42, got %v", spy.req.Issues)
	}
	if !spy.req.Continuation {
		t.Fatal("expected continuation request")
	}
	if spy.req.PreviousRunID != "run-42-2" {
		t.Fatalf("expected previous run ID run-42-2, got %q", spy.req.PreviousRunID)
	}
	if spy.req.Branches[42] != branch {
		t.Fatalf("expected branch %q, got %q", branch, spy.req.Branches[42])
	}
	if spy.req.Model != "gpt-4.2" {
		t.Fatalf("expected model replay, got %q", spy.req.Model)
	}
	if spy.req.Agent != "pi" {
		t.Fatalf("expected agent replay, got %q", spy.req.Agent)
	}
	if spy.req.PromptConfig.ContinuePrompt != "finish the tests" {
		t.Fatalf("expected raw continue prompt, got %q", spy.req.PromptConfig.ContinuePrompt)
	}
	if spy.req.PromptConfig.ReviewCommand != "/custom review 2" {
		t.Fatalf("expected review command replay, got %q", spy.req.PromptConfig.ReviewCommand)
	}
	if !spy.req.PromptConfig.ReviewCommandSet {
		t.Fatal("expected review command flag to be preserved")
	}
}

func TestContinue_UsesFlagsToOverrideReplayedValues(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyContinueBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "model": "gpt-4.1", "agent": "opencode"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}, "pi": {Preset: "pi", Command: "true"}}}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--model", "gpt-override", "--agent", "pi", "42", "finish the tests"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Model != "gpt-override" {
		t.Fatalf("expected model override, got %q", spy.req.Model)
	}
	if spy.req.Agent != "pi" {
		t.Fatalf("expected agent override, got %q", spy.req.Agent)
	}
}

func TestContinue_FailsWhenWorktreeMissing(t *testing.T) {
	branch := "sandman/42-fix-bug"
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "agent": "opencode"}},
	}}
	deps := Dependencies{
		BatchRunner: &spyContinueBatchRunner{result: &batch.Result{}},
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: t.TempDir(), AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "finish the tests"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when worktree is missing")
	}
	if !strings.Contains(err.Error(), "sandman run") {
		t.Fatalf("expected run hint, got %v", err)
	}
}
