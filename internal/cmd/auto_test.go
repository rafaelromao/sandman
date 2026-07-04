package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestRun_AutoFlag_EmitsAutoSelectEventsForAgentDrivenPath(t *testing.T) {
	sandmanDir := testenv.MkdirShort(t, "sm-auto-")
	t.Chdir(sandmanDir)
	if err := os.MkdirAll(".sandman", 0o755); err != nil {
		t.Fatalf("mkdir .sandman: %v", err)
	}
	promptPath := filepath.Join(".sandman", "auto-selection-prompt.md")
	if err := os.WriteFile(promptPath, []byte("priority prompt"), 0644); err != nil {
		t.Fatalf("create priority prompt: %v", err)
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A", Body: "A", Labels: []string{"bug"}},
			{Number: 2, Title: "Feature B", Body: "B", Labels: []string{"bug"}},
		},
	}
	log := &recordingEventLog{}
	cfg := &config.Config{
		Agent:         "opencode",
		ReviewCommand: "/oc review",
		AutoMaxCount:  5,
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {
			Command: fmt.Sprintf("echo '[1, 2]' > %s/selected-issues.json", filepath.Join(sandmanDir, ".sandman")),
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: cfg},
		EventLog:     log,
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}

	started, finished := findAutoSelectEvents(log)
	if started == nil {
		t.Fatal("expected a run.started event with auto-select-* RunID")
	}
	if finished == nil {
		t.Fatal("expected a run.finished event with auto-select-* RunID")
	}
	if started.RunID != finished.RunID {
		t.Fatalf("expected same RunID on started and finished, got %q vs %q", started.RunID, finished.RunID)
	}
	if got := autoSelectEventOrder(log); len(got) != 2 || got[0] != "run.started" || got[1] != "run.finished" {
		t.Fatalf("expected run.started before run.finished, got %v", got)
	}
	if status, _ := finished.Payload["status"].(string); status != "success" {
		t.Fatalf("expected finished status == success, got %v", finished.Payload["status"])
	}
	selected, ok := finished.Payload["selected"].([]int)
	if !ok || len(selected) != 2 || selected[0] != 1 || selected[1] != 2 {
		t.Fatalf("expected finished selected [1, 2], got %v", finished.Payload["selected"])
	}
}

func TestRun_AutoFlag_AgentFailurePropagatesErrorAndEmitsFailureFinished(t *testing.T) {
	sandmanDir := testenv.MkdirShort(t, "sm-auto-")
	t.Chdir(sandmanDir)
	if err := os.MkdirAll(".sandman", 0o755); err != nil {
		t.Fatalf("mkdir .sandman: %v", err)
	}
	promptPath := filepath.Join(".sandman", "auto-selection-prompt.md")
	if err := os.WriteFile(promptPath, []byte("priority prompt"), 0644); err != nil {
		t.Fatalf("create priority prompt: %v", err)
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 1, Title: "Feature A"}},
	}
	log := &recordingEventLog{}
	cfg := &config.Config{Agent: "opencode", ReviewCommand: "/oc review", AutoMaxCount: 5}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Command: "exit 1"},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: cfg},
		EventLog:     log,
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from agent failure")
	}
	if !strings.Contains(err.Error(), "selection agent failed") {
		t.Fatalf("expected agent-failure error to propagate, got: %v", err)
	}
	if spy.called {
		t.Fatal("expected batch runner NOT to be called when selection phase fails")
	}

	started, finished := findAutoSelectEvents(log)
	if started == nil {
		t.Fatal("expected a run.started event with auto-select-* RunID")
	}
	if finished == nil {
		t.Fatal("expected a run.finished event with auto-select-* RunID")
	}
	if status, _ := finished.Payload["status"].(string); status != "failure" {
		t.Fatalf("expected finished status == failure, got %v", finished.Payload["status"])
	}
	if reason, _ := finished.Payload["reason"].(string); reason == "" {
		t.Fatal("expected non-empty reason on failure")
	}
}

func TestRun_AutoFlag_NumericFallbackPathEmitsNoAutoSelectEvents(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A"},
			{Number: 2, Title: "Feature B"},
		},
	}
	log := &recordingEventLog{}
	deps := newRunDepsAuto(t, spy)
	deps.GitHubClient = gh
	deps.EventLog = log

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if started, finished := findAutoSelectEvents(log); started != nil || finished != nil {
		t.Fatalf("expected no auto-select events on numeric-fallback path, got started=%+v finished=%+v", started, finished)
	}
}

func TestRun_AutoFlag_NegativeCountReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "-1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --count=-1")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--count must be 0 or greater") {
		t.Errorf("expected --count validation error, got: %v", err)
	}
}

func TestRun_AutoFlag_NoIssuesReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{searchIssuesResult: []github.Issue{}}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no ready-for-agent issues")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "no issues ready for agent") {
		t.Errorf("expected 'no issues ready for agent' error, got: %v", err)
	}
}

func TestRun_AutoFlag_WithLabelAndQueryReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--label", "bug", "--query", "is:open"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when combining --label with --query")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "cannot combine") {
		t.Errorf("expected mutual exclusivity error, got: %v", err)
	}
}

func TestRun_AutoFlag_RunIdConflictUsesAutoWording(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--run-id", "testrun"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error combining --auto with --run-id")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--auto") {
		t.Errorf("expected --auto in error message, got: %v", err)
	}
	if strings.Contains(err.Error(), "--ralph") {
		t.Errorf("error message should not contain --ralph, got: %v", err)
	}
}

func TestRun_RalphFlagNoLongerExists(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for removed --ralph flag")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
}

func TestRun_AutoFlag_PRDWithNoChildrenFailsBeforeSelection(t *testing.T) {
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n"

	repoDir := t.TempDir()
	sandmanDir := filepath.Join(repoDir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0o755); err != nil {
		t.Fatalf("create sandman dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandmanDir, "auto-selection-prompt.md"), []byte("custom prompt"), 0o644); err != nil {
		t.Fatalf("create auto-selection-prompt.md: %v", err)
	}

	agentDir := t.TempDir()
	// If the selection phase runs, the agent writes a sentinel file. The test
	// asserts the agent never runs.
	agentScript := `#!/bin/sh
set -eu
mkdir -p .sandman
echo "selection-ran" > .sandman/agent-ran.flag
exit 0
`
	if err := os.WriteFile(filepath.Join(agentDir, "opencode"), []byte(agentScript), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	t.Setenv("PATH", agentDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0o755); err != nil {
		t.Fatalf("create reviews dir: %v", err)
	}
	reviewListener, listenErr := net.Listen("unix", filepath.Join(sandmanDir, "reviews", "review.sock"))
	if listenErr != nil {
		t.Fatalf("listen review.sock: %v", listenErr)
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

	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Empty PRD", Body: prdBody, State: "open"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review", AutoMaxCount: 50}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	t.Chdir(repoDir)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for empty PRD, got nil")
	}
	if !strings.Contains(err.Error(), "no child issues for PRD #1") {
		t.Fatalf("expected 'no child issues for PRD #1' in error, got %q", err)
	}
	if spy.called {
		t.Error("expected batch runner NOT to be called when PRD resolution fails")
	}
	if _, statErr := os.Stat(filepath.Join(sandmanDir, "agent-ran.flag")); !os.IsNotExist(statErr) {
		t.Errorf("expected selection phase to NOT run for empty PRD, but agent-ran flag exists")
	}
}

func TestRun_AutoFlag_NestedPRDFailsBeforeSelection(t *testing.T) {
	outerBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n"
	innerBody := "## Problem Statement\n\nInner.\n\n## Solution\n\nInner.\n\n## User Stories\n\n1. Inner.\n\n## Parent\n\n#1\n"

	repoDir := t.TempDir()
	sandmanDir := filepath.Join(repoDir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0o755); err != nil {
		t.Fatalf("create sandman dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandmanDir, "auto-selection-prompt.md"), []byte("custom prompt"), 0o644); err != nil {
		t.Fatalf("create auto-selection-prompt.md: %v", err)
	}

	agentDir := t.TempDir()
	agentScript := `#!/bin/sh
set -eu
mkdir -p .sandman
echo "selection-ran" > .sandman/agent-ran.flag
exit 0
`
	if err := os.WriteFile(filepath.Join(agentDir, "opencode"), []byte(agentScript), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	t.Setenv("PATH", agentDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0o755); err != nil {
		t.Fatalf("create reviews dir: %v", err)
	}
	reviewListener, listenErr := net.Listen("unix", filepath.Join(sandmanDir, "reviews", "review.sock"))
	if listenErr != nil {
		t.Fatalf("listen review.sock: %v", listenErr)
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

	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Outer PRD", Body: outerBody, State: "open"},
			10: {Number: 10, Title: "Inner PRD", Body: innerBody, State: "open"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review", AutoMaxCount: 50}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	t.Chdir(repoDir)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for nested PRD, got nil")
	}
	if !strings.Contains(err.Error(), "nested PRD detected: #10") {
		t.Fatalf("expected 'nested PRD detected: #10' in error, got %q", err)
	}
	if spy.called {
		t.Error("expected batch runner NOT to be called when PRD resolution fails")
	}
	if _, statErr := os.Stat(filepath.Join(sandmanDir, "agent-ran.flag")); !os.IsNotExist(statErr) {
		t.Errorf("expected selection phase to NOT run for nested PRD, but agent-ran flag exists")
	}
}

func TestRun_AutoFlag_NonPRDPassthroughUntouched(t *testing.T) {
	regular42 := &github.Issue{Number: 42, State: "open", Title: "Issue 42", Body: "## What\n\nJust a regular issue."}
	regular43 := &github.Issue{Number: 43, State: "open", Title: "Issue 43", Body: "## What\n\nJust a regular issue."}

	repoDir := testenv.MkdirShort(t, "sm-auto-")
	sandmanDir := filepath.Join(repoDir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0o755); err != nil {
		t.Fatalf("create sandman dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandmanDir, "auto-selection-prompt.md"), []byte("custom prompt"), 0o644); err != nil {
		t.Fatalf("create auto-selection-prompt.md: %v", err)
	}

	agentDir := t.TempDir()
	agentScript := `#!/bin/sh
set -eu
if [ ! -f .sandman/selection-prompt.md ]; then
  echo "selection-prompt.md missing" >&2
  exit 2
fi
prompt=$(cat .sandman/selection-prompt.md)
if ! printf '%s\n' "$prompt" | grep -q '^#42 '; then
  echo "issue #42 missing from selection prompt" >&2
  exit 3
fi
if ! printf '%s\n' "$prompt" | grep -q '^#43 '; then
  echo "issue #43 missing from selection prompt" >&2
  exit 4
fi
mkdir -p .sandman
printf '[42, 43]\n' > .sandman/selected-issues.json
exit 0
`
	if err := os.WriteFile(filepath.Join(agentDir, "opencode"), []byte(agentScript), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	t.Setenv("PATH", agentDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0o755); err != nil {
		t.Fatalf("create reviews dir: %v", err)
	}
	reviewListener, listenErr := net.Listen("unix", filepath.Join(sandmanDir, "reviews", "review.sock"))
	if listenErr != nil {
		t.Fatalf("listen review.sock: %v", listenErr)
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

	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: regular42,
			43: regular43,
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review", AutoMaxCount: 50}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	t.Chdir(repoDir)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "42", "43"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 43}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_AutoFlag_QueryFilterExpandsPRD(t *testing.T) {
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	regularBody := "## What\n\nA regular open issue.\n"

	repoDir := testenv.MkdirShort(t, "sm-auto-")
	sandmanDir := filepath.Join(repoDir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0o755); err != nil {
		t.Fatalf("create sandman dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandmanDir, "auto-selection-prompt.md"), []byte("custom prompt"), 0o644); err != nil {
		t.Fatalf("create auto-selection-prompt.md: %v", err)
	}

	agentDir := t.TempDir()
	agentScript := `#!/bin/sh
set -eu
if [ ! -f .sandman/selection-prompt.md ]; then
  echo "selection-prompt.md missing" >&2
  exit 2
fi
if grep -q '^#1 ' .sandman/selection-prompt.md; then
  echo "PRD #1 leaked into selection prompt" >&2
  exit 3
fi
if ! grep -q '^#10 ' .sandman/selection-prompt.md; then
  echo "child #10 missing from selection prompt" >&2
  exit 4
fi
if ! grep -q '^#30 ' .sandman/selection-prompt.md; then
  echo "regular #30 missing from selection prompt" >&2
  exit 5
fi
mkdir -p .sandman
printf '[10, 30]\n' > .sandman/selected-issues.json
exit 0
`
	if err := os.WriteFile(filepath.Join(agentDir, "opencode"), []byte(agentScript), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	t.Setenv("PATH", agentDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0o755); err != nil {
		t.Fatalf("create reviews dir: %v", err)
	}
	reviewListener, listenErr := net.Listen("unix", filepath.Join(sandmanDir, "reviews", "review.sock"))
	if listenErr != nil {
		t.Fatalf("listen review.sock: %v", listenErr)
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

	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD", Body: prdBody, State: "open"},
			10: {Number: 10, Title: "Child 1", Body: childBody, State: "open"},
			30: {Number: 30, Title: "Regular", Body: regularBody, State: "open"},
		},
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "PRD", Body: prdBody, State: "open"},
			{Number: 30, Title: "Regular", Body: regularBody, State: "open"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review", AutoMaxCount: 50}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	t.Chdir(repoDir)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--query", "label:bug is:open"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{10, 30}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestResolveAutoIssues_PriorityPromptFileSelectsSelectionPhase(t *testing.T) {
	sandmanDir := t.TempDir()
	promptPath := filepath.Join(sandmanDir, "auto-selection-prompt.md")
	if err := os.WriteFile(promptPath, []byte("test"), 0644); err != nil {
		t.Fatalf("create prompt: %v", err)
	}

	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A"},
		},
	}

	_, _, _, err := resolveAutoIssues(context.Background(), gh, 1, []int{1}, sandmanDir, "", "", &config.Config{ReviewCommand: "/oc review"}, "", nil)
	if err == nil {
		t.Fatal("expected selection phase error")
	}
	if !strings.Contains(err.Error(), "resolve agent") {
		t.Errorf("expected agent resolution error (empty config), got: %v", err)
	}
}

func TestResolveAutoIssues_AutoPromptFileAbsentUsesNumericSort(t *testing.T) {
	sandmanDir := t.TempDir()
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 3, Title: "Feature C"},
			{Number: 1, Title: "Feature A"},
		},
	}

	issues, _, _, err := resolveAutoIssues(context.Background(), gh, 1, []int{1, 3}, sandmanDir, "", "", &config.Config{}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0] != 1 {
		t.Errorf("expected [1], got %v", issues)
	}
}

func TestAutoPromptFileExists_DetectsRenamedFile(t *testing.T) {
	dir := t.TempDir()
	if autoPromptFileExists(dir) {
		t.Fatal("expected false when file does not exist")
	}
	if err := os.WriteFile(filepath.Join(dir, "auto-selection-prompt.md"), []byte("x"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !autoPromptFileExists(dir) {
		t.Fatal("expected true after writing auto-selection-prompt.md")
	}

	legacy := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacy, "priority-selection-prompt.md"), []byte("x"), 0644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	if autoPromptFileExists(legacy) {
		t.Fatal("expected false when only the legacy filename is present")
	}
}

func TestResolveAutoIssues_UnlimitedCap(t *testing.T) {
	sandmanDir := t.TempDir()
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A"},
		},
	}

	issues, _, _, err := resolveAutoIssues(context.Background(), gh, 0, []int{1}, sandmanDir, "", "", &config.Config{}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue (unlimited cap), got %d: %v", len(issues), issues)
	}
}
