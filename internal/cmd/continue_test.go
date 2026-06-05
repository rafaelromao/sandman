package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
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
	contextPath := filepath.Join(dir, branch, ".sandman", "continuation-context.md")
	if err := os.MkdirAll(filepath.Dir(contextPath), 0755); err != nil {
		t.Fatalf("mkdir continuation dir: %v", err)
	}
	if err := os.WriteFile(contextPath, []byte("# Continuation Context\n\n## Completed\nInitial pass.\n"), 0644); err != nil {
		t.Fatalf("write continuation context: %v", err)
	}

	spy := &spyContinueBatchRunner{result: &batch.Result{}}
	log := &events.JSONLLogger{Path: filepath.Join(dir, "events.jsonl")}
	if err := log.Log(events.Event{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "model": "gpt-4.1", "agent": "opencode", "review_command": "/custom review", "parallel": 1, "start_delay": 3, "retries": 2, "sandbox": "worktree", "container_capacity": 1, "container_capacity_set": true, "max_containers": 2, "max_containers_set": true}}); err != nil {
		t.Fatalf("write run.started event: %v", err)
	}
	if err := log.Log(events.Event{Type: "run.continued", RunID: "run-42-2", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "model": "gpt-4.2", "agent": "pi", "review_command": "/custom review 2", "parallel": 7, "start_delay": 11, "retries": 4, "sandbox": "docker", "container_capacity": 3, "container_capacity_set": true, "max_containers": 5, "max_containers_set": true}}); err != nil {
		t.Fatalf("write run.continued event: %v", err)
	}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", DefaultModel: "openai/gpt-4.1", WorktreeDir: dir, ReviewCommand: "/current review", Git: config.GitConfig{BaseBranch: "trunk"}, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}, "pi": {Preset: "pi", Command: "true"}}}},
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
	if len(spy.req.ContinuePrompts) != 1 {
		t.Fatalf("expected 1 continue prompt, got %v", spy.req.ContinuePrompts)
	}
	if spy.req.BaseBranches[42] != "main" {
		t.Fatalf("expected base branch main, got %q", spy.req.BaseBranches[42])
	}
	if spy.req.PromptConfig.ContinuePrompt != "finish the tests" {
		t.Fatalf("expected shared bare prompt, got %q", spy.req.PromptConfig.ContinuePrompt)
	}
	if !spy.req.Continuation {
		t.Fatal("expected continuation request")
	}
	if spy.req.PreviousRunIDs[42] != "run-42-2" {
		t.Fatalf("expected previous run ID run-42-2 for issue 42, got %q", spy.req.PreviousRunIDs[42])
	}
	if spy.req.Branches[42] != branch {
		t.Fatalf("expected branch %q, got %q", branch, spy.req.Branches[42])
	}
	if spy.req.Model != "openai/gpt-4.1" {
		t.Fatalf("expected config default model, got %q", spy.req.Model)
	}
	if spy.req.BaseBranch != "main" {
		t.Fatalf("expected base branch replay, got %q", spy.req.BaseBranch)
	}
	if spy.req.Agent != "pi" {
		t.Fatalf("expected agent replay, got %q", spy.req.Agent)
	}
	if spy.req.Parallel != 7 {
		t.Fatalf("expected parallel replay, got %d", spy.req.Parallel)
	}
	if spy.req.StartDelay != 11*time.Second {
		t.Fatalf("expected start delay replay, got %s", spy.req.StartDelay)
	}
	if !spy.req.StartDelaySet {
		t.Fatal("expected start delay flag to be preserved")
	}
	if spy.req.Retries != 4 {
		t.Fatalf("expected retries replay, got %d", spy.req.Retries)
	}
	if spy.req.Sandbox != "docker" {
		t.Fatalf("expected sandbox replay, got %q", spy.req.Sandbox)
	}
	if spy.req.ContainerCapacity != 3 {
		t.Fatalf("expected container capacity replay, got %d", spy.req.ContainerCapacity)
	}
	if !spy.req.ContainerCapacitySet {
		t.Fatal("expected container capacity flag to be preserved")
	}
	if spy.req.MaxContainers != 5 {
		t.Fatalf("expected max containers replay, got %d", spy.req.MaxContainers)
	}
	if !spy.req.MaxContainersSet {
		t.Fatal("expected max containers flag to be preserved")
	}
	if !strings.Contains(spy.req.ContinuePrompts[42], "## Prior Context") {
		t.Fatalf("expected prior context section, got %q", spy.req.ContinuePrompts[42])
	}
	if strings.Contains(spy.req.ContinuePrompts[42], "# Continuation Context") {
		t.Fatalf("expected header stripped, got %q", spy.req.ContinuePrompts[42])
	}
	if !strings.Contains(spy.req.ContinuePrompts[42], "finish the tests") {
		t.Fatalf("expected new instruction, got %q", spy.req.ContinuePrompts[42])
	}
	if !strings.Contains(spy.req.ContinuePrompts[42], ".sandman/continuation-context.md") {
		t.Fatalf("expected update instruction, got %q", spy.req.ContinuePrompts[42])
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
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "model": "gpt-4.1", "agent": "opencode"}},
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
	cmd.SetArgs([]string{"--model", "gpt-override", "--agent", "pi", "--parallel", "9", "--start-delay", "12", "--retries", "5", "--sandbox", "worktree", "--container-capacity", "8", "--max-containers", "6", "42", "finish the tests"})

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
	if spy.req.Parallel != 9 {
		t.Fatalf("expected parallel override, got %d", spy.req.Parallel)
	}
	if spy.req.StartDelay != 12*time.Second {
		t.Fatalf("expected start delay override, got %s", spy.req.StartDelay)
	}
	if !spy.req.StartDelaySet {
		t.Fatal("expected start delay override to set flag")
	}
	if spy.req.Retries != 5 {
		t.Fatalf("expected retries override, got %d", spy.req.Retries)
	}
	if spy.req.Sandbox != "worktree" {
		t.Fatalf("expected sandbox override, got %q", spy.req.Sandbox)
	}
	if spy.req.ContainerCapacity != 8 {
		t.Fatalf("expected container capacity override, got %d", spy.req.ContainerCapacity)
	}
	if !spy.req.ContainerCapacitySet {
		t.Fatal("expected container capacity override to set flag")
	}
	if spy.req.MaxContainers != 6 {
		t.Fatalf("expected max containers override, got %d", spy.req.MaxContainers)
	}
	if !spy.req.MaxContainersSet {
		t.Fatal("expected max containers override to set flag")
	}
}

func TestContinue_DoesNotUseDefaultModelForCustomAgent(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyContinueBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "custom"}}}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{
			Agent:        "custom",
			DefaultModel: "openai/gpt-4.1",
			WorktreeDir:  dir,
			AgentProviders: map[string]config.Agent{
				"custom": {Command: "true"},
			},
		}},
		EventLog: log,
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

	if spy.req.Model != "" {
		t.Fatalf("expected empty model for custom agent, got %q", spy.req.Model)
	}
}

func TestContinue_WarnsAndUsesBarePromptWhenContinuationContextMissing(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyContinueBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}}}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
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

	if spy.req.PromptConfig.ContinuePrompt != "finish the tests" {
		t.Fatalf("expected bare prompt, got %q", spy.req.PromptConfig.ContinuePrompt)
	}
	if !strings.Contains(buf.String(), "missing continuation context") {
		t.Fatalf("expected warning about missing context, got %q", buf.String())
	}
}

func TestContinue_FailsWhenPRMerged(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyContinueBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}}}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
		EventLog:    log,
		GitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{
			branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch},
		}},
	}

	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "finish the tests"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when PR is already merged")
	}
	want := "cannot continue issue #42: PR already merged (branch \"sandman/42-fix-bug\")"
	if err.Error() != want {
		t.Fatalf("expected %q, got %q", want, err.Error())
	}
	if spy.called {
		t.Fatal("expected batch runner NOT to be called when PR is merged")
	}
}

func TestContinue_DoesNotBlockWhenPRNotMerged(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   *github.PR
	}{
		{name: "open", pr: &github.PR{Number: 42, State: "open", HeadRefName: "sandman/42-fix-bug"}},
		{name: "closed-unmerged", pr: &github.PR{Number: 42, State: "closed", HeadRefName: "sandman/42-fix-bug"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			branch := "sandman/42-fix-bug"
			if err := os.MkdirAll(filepath.Join(dir, branch), 0755); err != nil {
				t.Fatalf("mkdir worktree: %v", err)
			}

			spy := &spyContinueBatchRunner{result: &batch.Result{}}
			log := &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}}}}
			deps := Dependencies{
				BatchRunner: spy,
				ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
				EventLog:    log,
				GitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{
					branch: tc.pr,
				}},
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
				t.Fatal("expected batch runner to be called when PR is not merged")
			}
		})
	}
}

type continuationFlowState struct {
	prompts  []string
	contexts []string
	step     int
}

func (s *continuationFlowState) nextContext() string {
	if s.step >= len(s.contexts) {
		return ""
	}
	context := s.contexts[s.step]
	s.step++
	return context
}

type continuationFlowBatchRunner struct {
	log         *fakeEventLog
	state       *continuationFlowState
	worktreeDir string
	runIndex    int
}

func (r *continuationFlowBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	issue := req.Issues[0]
	branch := req.Branches[issue]
	if branch == "" {
		branch = fmt.Sprintf("sandman/%d-fix-bug", issue)
	}
	runID := fmt.Sprintf("run-%d-%d", issue, r.runIndex)
	r.runIndex++
	worktreePath := filepath.Join(r.worktreeDir, branch)
	contextPath := filepath.Join(worktreePath, ".sandman", "continuation-context.md")
	if content := r.state.nextContext(); content != "" {
		if err := os.MkdirAll(filepath.Dir(contextPath), 0755); err == nil {
			_ = os.WriteFile(contextPath, []byte(content), 0644)
		}
	}
	eventType := "run.started"
	payload := map[string]any{"branch": branch, "base_branch": req.BaseBranch, "agent": req.Agent}
	if req.Continuation {
		eventType = "run.continued"
		payload = map[string]any{"branch": branch, "base_branch": req.BaseBranch, "previous_run_id": req.PreviousRunIDs[issue]}
		promptText := req.PromptConfig.ContinuePrompt
		if perIssuePrompt, ok := req.ContinuePrompts[issue]; ok {
			promptText = perIssuePrompt
		}
		r.state.prompts = append(r.state.prompts, promptText)
	}
	r.log.events = append(r.log.events, events.Event{Type: eventType, RunID: runID, Issue: issue, Payload: payload})
	return &batch.Result{Runs: []batch.AgentRunResult{{IssueNumber: issue, Status: "success", Branch: branch, WorktreePath: worktreePath}}}, nil
}

func TestContinue_ChainedContinuationFlow(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(dir, branch)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	state := &continuationFlowState{contexts: []string{
		"## Completed\nInitial run.\n",
		"## Completed\nFirst continue.\n",
		"## Completed\nSecond continue.\n",
	}}
	log := &fakeEventLog{}
	runner := &continuationFlowBatchRunner{log: log, state: state, worktreeDir: dir}
	deps := Dependencies{
		BatchRunner: runner,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
		EventLog:    log,
	}

	_, err := runner.RunBatch(context.Background(), batch.Request{Issues: []int{42}, Branches: map[int]string{42: branch}, Agent: "opencode", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("initial run failed: %v", err)
	}
	initialContext, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "continuation-context.md"))
	if err != nil {
		t.Fatalf("read initial context: %v", err)
	}
	if !strings.Contains(string(initialContext), "Initial run.") {
		t.Fatalf("expected initial context, got %q", string(initialContext))
	}

	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "finish the tests"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("first continue failed: %v", err)
	}
	firstContinueContext, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "continuation-context.md"))
	if err != nil {
		t.Fatalf("read first continue context: %v", err)
	}
	if !strings.Contains(string(firstContinueContext), "First continue.") {
		t.Fatalf("expected first continue context, got %q", string(firstContinueContext))
	}

	buf.Reset()
	cmd = NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "push the PR"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("second continue failed: %v", err)
	}
	secondContinueContext, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "continuation-context.md"))
	if err != nil {
		t.Fatalf("read second continue context: %v", err)
	}
	if !strings.Contains(string(secondContinueContext), "Second continue.") {
		t.Fatalf("expected second continue context, got %q", string(secondContinueContext))
	}

	if len(state.prompts) != 2 {
		t.Fatalf("expected 2 continue prompts, got %#v", state.prompts)
	}
	if !strings.Contains(state.prompts[0], "Initial run.") {
		t.Fatalf("expected first continue prompt to include initial context, got %q", state.prompts[0])
	}
	if !strings.Contains(state.prompts[1], "First continue.") {
		t.Fatalf("expected second continue prompt to include updated context, got %q", state.prompts[1])
	}
	if len(log.events) != 3 {
		t.Fatalf("expected 3 events, got %#v", log.events)
	}
	if log.events[0].Type != "run.started" || log.events[1].Type != "run.continued" || log.events[2].Type != "run.continued" {
		t.Fatalf("unexpected event sequence: %#v", []string{log.events[0].Type, log.events[1].Type, log.events[2].Type})
	}
	if log.events[1].Payload["previous_run_id"] != log.events[0].RunID {
		t.Fatalf("expected first continue to reference initial run, got %#v", log.events[1].Payload["previous_run_id"])
	}
	if log.events[2].Payload["previous_run_id"] != log.events[1].RunID {
		t.Fatalf("expected second continue to reference first continue, got %#v", log.events[2].Payload["previous_run_id"])
	}
}
func TestContinue_FailsWhenWorktreeMissing(t *testing.T) {
	branch := "sandman/42-fix-bug"
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}},
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

func TestContinue_MultipleIssuesBuildsBranchesAndPreviousRunIDsMaps(t *testing.T) {
	dir := t.TempDir()
	branchA := "sandman/1-fix-a"
	branchB := "sandman/2-fix-b"
	for _, b := range []string{branchA, branchB} {
		if err := os.MkdirAll(filepath.Join(dir, b), 0755); err != nil {
			t.Fatalf("mkdir worktree %s: %v", b, err)
		}
	}
	for branch, context := range map[string]string{branchA: "## Completed\nFirst issue.\n", branchB: "## Completed\nSecond issue.\n"} {
		contextPath := filepath.Join(dir, branch, ".sandman", "continuation-context.md")
		if err := os.MkdirAll(filepath.Dir(contextPath), 0755); err != nil {
			t.Fatalf("mkdir continuation dir %s: %v", branch, err)
		}
		if err := os.WriteFile(contextPath, []byte(context), 0644); err != nil {
			t.Fatalf("write continuation context %s: %v", branch, err)
		}
	}

	spy := &spyContinueBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-1-a", Issue: 1, Payload: map[string]any{"branch": branchA, "base_branch": "main", "agent": "opencode"}},
		{Type: "run.continued", RunID: "run-1-b", Issue: 1, Payload: map[string]any{"branch": branchA, "base_branch": "main", "agent": "opencode"}},
		{Type: "run.started", RunID: "run-2-a", Issue: 2, Payload: map[string]any{"branch": branchB, "base_branch": "main", "agent": "opencode"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1", "2", "fix tests"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 2 || spy.req.Issues[0] != 1 || spy.req.Issues[1] != 2 {
		t.Fatalf("expected issues=[1 2], got %v", spy.req.Issues)
	}
	if len(spy.req.ContinuePrompts) != 2 {
		t.Fatalf("expected 2 continue prompts, got %v", spy.req.ContinuePrompts)
	}
	if spy.req.BaseBranches[1] != "main" || spy.req.BaseBranches[2] != "main" {
		t.Fatalf("expected base branches to replay main, got %#v", spy.req.BaseBranches)
	}
	if !strings.Contains(spy.req.ContinuePrompts[1], "First issue.") {
		t.Fatalf("expected issue 1 prompt to include its own context, got %q", spy.req.ContinuePrompts[1])
	}
	if !strings.Contains(spy.req.ContinuePrompts[2], "Second issue.") {
		t.Fatalf("expected issue 2 prompt to include its own context, got %q", spy.req.ContinuePrompts[2])
	}
	if spy.req.ContinuePrompts[1] == spy.req.ContinuePrompts[2] {
		t.Fatal("expected different prompts per issue")
	}
	if spy.req.Branches[1] != branchA {
		t.Fatalf("expected Branches[1]=%q, got %q", branchA, spy.req.Branches[1])
	}
	if spy.req.Branches[2] != branchB {
		t.Fatalf("expected Branches[2]=%q, got %q", branchB, spy.req.Branches[2])
	}
	if spy.req.PreviousRunIDs[1] != "run-1-b" {
		t.Fatalf("expected PreviousRunIDs[1]=run-1-b (latest for issue 1), got %q", spy.req.PreviousRunIDs[1])
	}
	if spy.req.PreviousRunIDs[2] != "run-2-a" {
		t.Fatalf("expected PreviousRunIDs[2]=run-2-a, got %q", spy.req.PreviousRunIDs[2])
	}
	if !spy.req.Continuation {
		t.Fatal("expected continuation request")
	}
}

func TestContinue_FailsFastWhenAnyWorktreeMissingForMultipleIssues(t *testing.T) {
	dir := t.TempDir()
	branchA := "sandman/1-fix-a"
	branchB := "sandman/2-fix-b"
	if err := os.MkdirAll(filepath.Join(dir, branchA), 0755); err != nil {
		t.Fatalf("mkdir worktree A: %v", err)
	}
	// Intentionally do NOT create branchB's worktree.

	spy := &spyContinueBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-1-a", Issue: 1, Payload: map[string]any{"branch": branchA, "base_branch": "main", "agent": "opencode"}},
		{Type: "run.started", RunID: "run-2-a", Issue: 2, Payload: map[string]any{"branch": branchB, "base_branch": "main", "agent": "opencode"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1", "2", "fix tests"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when one worktree is missing")
	}
	if spy.called {
		t.Fatal("expected batch runner NOT to be called when worktree missing (fail fast)")
	}
	if !strings.Contains(err.Error(), branchB) {
		t.Fatalf("expected error to mention missing worktree %q, got %v", branchB, err)
	}
}

func TestContinue_FailsWhenAnyIssueHasNoPreviousRun(t *testing.T) {
	dir := t.TempDir()
	branchA := "sandman/1-fix-a"
	if err := os.MkdirAll(filepath.Join(dir, branchA), 0755); err != nil {
		t.Fatalf("mkdir worktree A: %v", err)
	}

	spy := &spyContinueBatchRunner{result: &batch.Result{}}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-1-a", Issue: 1, Payload: map[string]any{"branch": branchA, "base_branch": "main", "agent": "opencode"}},
		// No events for issue 2.
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1", "2", "fix tests"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when issue has no previous run")
	}
	if spy.called {
		t.Fatal("expected batch runner NOT to be called when validation fails")
	}
	if !strings.Contains(err.Error(), "#2") {
		t.Fatalf("expected error to mention issue #2, got %v", err)
	}
}

func TestContinue_UsesVariadicSyntaxInUseString(t *testing.T) {
	cmd := NewContinueCmd(newTestDeps())
	if !strings.Contains(cmd.Use, "[issue-number...]") {
		t.Fatalf("expected Use to indicate variadic issue numbers, got %q", cmd.Use)
	}
}

func TestContinue_ExitsWithCode130OnAbort(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyContinueBatchRunner{
		result: &batch.Result{
			Runs: []batch.AgentRunResult{
				{IssueNumber: 42, Status: "aborted", Branch: branch},
			},
		},
		err: batch.ErrAborted,
	}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
		EventLog:    log,
	}

	var stdout, stderr bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"42", "finish the tests"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from aborted batch")
	}

	if !errors.Is(err, batch.ErrAborted) {
		t.Errorf("expected error to wrap batch.ErrAborted, got %v", err)
	}

	var coded *ExitCodedError
	if !errors.As(err, &coded) {
		t.Fatalf("expected *ExitCodedError, got %T: %v", err, err)
	}
	if coded.Code != 130 {
		t.Errorf("expected exit code 130, got %d", coded.Code)
	}
	if !strings.Contains(stderr.String(), "batch aborted by operator") {
		t.Errorf("expected 'batch aborted by operator' on stderr, got:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Summary: 0 succeeded, 1 aborted") {
		t.Errorf("expected aborted summary on stdout, got:\n%s", stdout.String())
	}
}

func TestContinue_PreservesRunBatchErrorMessage(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyContinueBatchRunner{
		result: &batch.Result{
			Runs: []batch.AgentRunResult{
				{IssueNumber: 42, Status: "failure", Branch: branch},
			},
		},
		err: errors.New("1 of 1 runs failed"),
	}
	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}},
		EventLog:    log,
	}

	var stdout, stderr bytes.Buffer
	cmd := NewContinueCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"42", "finish the tests"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from failed batch")
	}

	if !strings.Contains(err.Error(), "run batch: 1 of 1 runs failed") {
		t.Errorf("expected wrapped 'run batch' message, got %v", err)
	}

	var coded *ExitCodedError
	if errors.As(err, &coded) {
		t.Errorf("expected plain error (not *ExitCodedError) for non-abort failure, got %v", err)
	}
}
