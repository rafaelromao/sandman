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
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func TestRetry_NoArgsReturnsError(t *testing.T) {
	deps := newTestDeps()
	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no issue provided")
	}
}

func TestRetry_InvalidIssueReturnsError(t *testing.T) {
	deps := newTestDeps()
	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"abc"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when invalid issue provided")
	}
}

func TestRetry_NoPreviousRunReturnsError(t *testing.T) {
	deps := newTestDeps()
	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no previous run exists")
	}
}

func TestRetry_LooksUpLastRunAndInvokesBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix-bug"}},
		{Type: "run.finished", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"status": "failure"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 42 {
		t.Errorf("expected issues [42], got %v", spy.req.Issues)
	}
}

func TestRetry_UsesBranchFromPreviousRun(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix-bug"}},
		{Type: "run.finished", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"status": "failure"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Branches[42] != "sandman/42-fix-bug" {
		t.Errorf("expected branch sandman/42-fix-bug, got %q", spy.req.Branches[42])
	}
}

func TestRetry_WorksWithUnfinishedRun(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix-bug"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called for unfinished run")
	}
	if spy.req.Branches[42] != "sandman/42-fix-bug" {
		t.Errorf("expected branch sandman/42-fix-bug, got %q", spy.req.Branches[42])
	}
}

func TestRetry_ReplaysStoredPromptOverrides(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "custom.md")
	if err := os.WriteFile(templatePath, []byte("template"), 0644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{
			"branch":              "sandman/42-fix-bug",
			"prompt_source_type":  "template",
			"prompt_source_value": templatePath,
			"prompt_args":         map[string]any{"FOO": "bar"},
			"review_command":      "/custom review",
		}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/current review"}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.TemplateFlag != templatePath {
		t.Fatalf("expected template replay, got %q", spy.req.PromptConfig.TemplateFlag)
	}
	if spy.req.PromptConfig.PromptArgs["FOO"] != "bar" {
		t.Fatalf("expected prompt arg replay, got %v", spy.req.PromptConfig.PromptArgs)
	}
	if spy.req.PromptConfig.ReviewCommand != "/custom review" {
		t.Fatalf("expected review command replay, got %q", spy.req.PromptConfig.ReviewCommand)
	}
	if !spy.req.PromptConfig.ReviewCommandSet {
		t.Fatalf("expected review command flag to be preserved")
	}
}

func TestRetry_ReplaysInlinePromptOverride(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{
			"branch":              "sandman/42-fix-bug",
			"prompt_source_type":  "prompt",
			"prompt_source_value": "inline: {{ISSUE_NUMBER}}",
		}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/current review"}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "inline: {{ISSUE_NUMBER}}" {
		t.Fatalf("expected prompt replay, got %q", spy.req.PromptConfig.PromptFlag)
	}
}

func TestRetry_UsesCurrentConfigWhenNoPromptOverrides(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix-bug", "prompt_source_type": "current"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/current review"}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "" {
		t.Fatalf("expected no prompt replay, got %q", spy.req.PromptConfig.PromptFlag)
	}
	if spy.req.PromptConfig.TemplateFlag != "" {
		t.Fatalf("expected no template replay, got %q", spy.req.PromptConfig.TemplateFlag)
	}
	if len(spy.req.PromptConfig.PromptArgs) != 0 {
		t.Fatalf("expected no prompt args replay, got %v", spy.req.PromptConfig.PromptArgs)
	}
	if spy.req.PromptConfig.ReviewCommand != "/current review" {
		t.Fatalf("expected current review command, got %q", spy.req.PromptConfig.ReviewCommand)
	}
}

func TestRetry_ReplaysStoredModelFromConfig(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix-bug", "model": "gpt-config"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", ReviewCommand: "/current review"}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Model != "gpt-config" {
		t.Fatalf("expected stored model replay, got %q", spy.req.Model)
	}
}

func TestRetry_ReplaysStoredModelFromOverride(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix-bug", "model": "gpt-override"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", ReviewCommand: "/current review"}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Model != "gpt-override" {
		t.Fatalf("expected stored override replay, got %q", spy.req.Model)
	}
}

func TestRetry_FailsWhenStoredTemplateMissing(t *testing.T) {
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{
			"branch":              "sandman/42-fix-bug",
			"prompt_source_type":  "template",
			"prompt_source_value": "./missing-template.md",
		}},
	}}
	deps := Dependencies{
		BatchRunner: batch.NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}}, &prompt.Engine{}, &fakeStore{config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, ReviewCommand: "/current review"}}, nil),
		ConfigStore: &fakeStore{config: &config.Config{DefaultAgent: "opencode", Agent: "opencode", ReviewCommand: "/current review"}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewRetryCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when template path is missing")
	}
	if !strings.Contains(err.Error(), "stored template path") {
		t.Fatalf("expected template read failure, got %v", err)
	}
}
