package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// spyBatchRunner records the Request it receives.
type spyBatchRunner struct {
	called bool
	req    batch.Request
	result *batch.Result
	err    error
}

func (s *spyBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	s.called = true
	s.req = req
	return s.result, s.err
}

// fakeGitHubClient is a test double for github.Client.
type fakeGitHubClient struct {
	issues             map[int]*github.Issue
	fetchRelease       map[int]<-chan struct{}
	fetchReleaseAfter  map[int]int
	fetchCount         map[int]int
	mu                 sync.Mutex
	fetchIssueError    error
	searchIssuesQuery  string
	searchIssuesResult []github.Issue
	searchIssuesError  error
}

func (f *fakeGitHubClient) FetchIssue(number int) (*github.Issue, error) {
	f.mu.Lock()
	if f.fetchCount == nil {
		f.fetchCount = make(map[int]int)
	}
	f.fetchCount[number]++
	count := f.fetchCount[number]
	release := f.fetchRelease[number]
	f.mu.Unlock()

	if f.fetchIssueError != nil {
		return nil, f.fetchIssueError
	}
	threshold := 1
	if f.fetchReleaseAfter != nil {
		if after, ok := f.fetchReleaseAfter[number]; ok {
			threshold = after
		}
	}
	if release != nil && count > threshold {
		<-release
	}
	if issue, ok := f.issues[number]; ok {
		return issue, nil
	}
	return &github.Issue{Number: number}, nil
}

func (f *fakeGitHubClient) FetchIssueDependencies(number int) ([]int, error) {
	if issue, ok := f.issues[number]; ok {
		return issue.BlockedBy, nil
	}
	return nil, nil
}

func (f *fakeGitHubClient) SearchIssues(query string) ([]github.Issue, error) {
	f.searchIssuesQuery = query
	return f.searchIssuesResult, f.searchIssuesError
}

func newRunDeps(runner batch.Runner) Dependencies {
	return Dependencies{
		BatchRunner:  runner,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
	}
}

func TestRun_SingleIssueInvokesBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
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

func TestRun_MultipleIssuesInvokesBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1", "2", "3"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []int{1, 2, 3}
	if len(spy.req.Issues) != len(want) {
		t.Errorf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_ParallelFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--parallel", "2", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Parallel != 2 {
		t.Errorf("expected parallel=2, got %d", spy.req.Parallel)
	}
}

func TestRun_StartDelayFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--start-delay", "7", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.StartDelaySet {
		t.Fatal("expected start delay override to be marked as set")
	}
	if spy.req.StartDelay != 7*time.Second {
		t.Errorf("expected start delay=7s, got %s", spy.req.StartDelay)
	}
}

func TestRun_StartDelayNegativeValueRejected(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--start-delay", "-1", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for negative start delay")
	}
	if !strings.Contains(err.Error(), "start_delay must be 0 or greater") {
		t.Fatalf("expected validation error, got %v", err)
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
}

func TestRun_ModelFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--model", "gpt-4.1", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Model != "gpt-4.1" {
		t.Errorf("expected model=gpt-4.1, got %q", spy.req.Model)
	}
}

func TestRun_LoadConfigError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{err: errors.New("config not found")}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when config load fails")
	}
	if spy.called {
		t.Error("expected batch runner not to be called when config load fails")
	}
}

func TestRun_NoIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no issues provided")
	}
	if spy.called {
		t.Error("expected batch runner not to be called when no issues provided")
	}
}

func TestRun_HelpMentionsPromptOnlyMode(t *testing.T) {
	deps := newRunDeps(&spyBatchRunner{result: &batch.Result{}})

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "prompt-only mode") {
		t.Fatalf("expected help to mention prompt-only mode, got:\n%s", out)
	}
	if !strings.Contains(out, "{{ISSUE_NUMBER}}") {
		t.Fatalf("expected help to mention ISSUE_NUMBER gating, got:\n%s", out)
	}
}

func TestRun_PromptOnlyAllowsNoIssueSelection(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		setup func(*Dependencies)
	}{
		{
			name: "inline prompt",
			args: []string{"--prompt", "Return only OK."},
			setup: func(deps *Dependencies) {
				deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}
			},
		},
		{
			name: "template file",
			args: func() []string {
				dir := t.TempDir()
				path := dir + "/prompt.md"
				if err := os.WriteFile(path, []byte("Return only OK."), 0644); err != nil {
					t.Fatalf("write template: %v", err)
				}
				return []string{"--template", path}
			}(),
			setup: func(deps *Dependencies) {
				deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(spy)
			tt.setup(&deps)

			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !spy.called {
				t.Fatal("expected batch runner to be called")
			}
			if len(spy.req.Issues) != 0 {
				t.Fatalf("expected no issues, got %v", spy.req.Issues)
			}
		})
	}
}

func TestRun_PromptOnlyRejectsSubstitutedIssuePlaceholders(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "{{PROMPT_BODY}}", "--prompt-arg", "PROMPT_BODY=Issue {{ISSUE_TITLE}}"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "prompt requires issue selection but no issue selection was provided") {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
}

func TestRun_CustomPromptWithIssueSelectionStillUsesIssueDrivenFlow(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "inline prompt", args: []string{"--prompt", "Return only OK.", "42"}},
		{name: "template file", args: func() []string {
			dir := t.TempDir()
			path := dir + "/prompt.md"
			if err := os.WriteFile(path, []byte("Return only OK."), 0644); err != nil {
				t.Fatalf("write template: %v", err)
			}
			return []string{"--template", path, "42"}
		}()},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(spy)

			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tt.args)

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
		})
	}
}

func TestRun_PromptOnlyStillRequiresIssueNumberWhenPromptUsesIt(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "inline prompt", args: []string{"--prompt", "Issue {{ISSUE_TITLE}}"}},
		{name: "template file", args: func() []string {
			dir := t.TempDir()
			path := dir + "/prompt.md"
			if err := os.WriteFile(path, []byte("Issue {{ISSUE_BODY}}"), 0644); err != nil {
				t.Fatalf("write template: %v", err)
			}
			return []string{"--template", path}
		}()},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(spy)
			deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}

			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "prompt requires issue selection but no issue selection was provided") {
				t.Fatalf("unexpected error: %v", err)
			}
			if spy.called {
				t.Fatal("expected batch runner not to be called")
			}
		})
	}
}

func TestRun_PrintsSummaryOnSuccess(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "success", Branch: "sandman/43-new-feature"},
		},
	}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Summary: 2 succeeded, 0 failed") {
		t.Errorf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#42  success  sandman/42-fix-bug") {
		t.Errorf("expected issue 42 in summary, got:\n%s", out)
	}
}

func TestRun_PrintsSummaryOnPartialFailure(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "failure", Branch: "sandman/43-broken"},
		},
	}, err: errors.New("1 of 2 runs failed")}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when some runs fail")
	}

	out := buf.String()
	if !strings.Contains(out, "Summary: 1 succeeded, 1 failed") {
		t.Errorf("expected partial failure summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#43  failure  sandman/43-broken") {
		t.Errorf("expected issue 43 failure in summary, got:\n%s", out)
	}
}

func TestRun_PrintsSummaryWithBlockedRuns(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "failure", Branch: "sandman/43-broken"},
			{IssueNumber: 100, Status: "blocked", Branch: "sandman/100-dependent"},
		},
	}, err: errors.New("1 of 3 runs failed")}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43", "100"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when some runs fail")
	}

	out := buf.String()
	if !strings.Contains(out, "Summary: 1 succeeded, 1 failed, 1 blocked") {
		t.Errorf("expected blocked summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#100  blocked  sandman/100-dependent") {
		t.Errorf("expected issue 100 blocked in summary, got:\n%s", out)
	}
}

func TestRun_PrintsSummaryWithBlockedRunsAndNoFailures(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 100, Status: "blocked", Branch: "sandman/100-dependent"},
			{IssueNumber: 101, Status: "blocked", Branch: "sandman/101-another-dependent"},
		},
	}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "100", "101"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed, 2 blocked") {
		t.Errorf("expected blocked summary without failures, got:\n%s", out)
	}
	if !strings.Contains(out, "#101  blocked  sandman/101-another-dependent") {
		t.Errorf("expected issue 101 blocked in summary, got:\n%s", out)
	}
}

func TestRun_PrintsWorktreeHintForCompletedRuns(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{Runs: []batch.AgentRunResult{{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug", WorktreePath: ".sandman/worktrees/sandman/42-fix-bug"}}}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "worktree: .sandman/worktrees/sandman/42-fix-bug") {
		t.Fatalf("expected worktree hint, got:\n%s", out)
	}
}

func TestRun_PrintsPromptOnlySummaryLabel(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{Runs: []batch.AgentRunResult{{Status: "success", Branch: "sandman/return-only-ok-123"}}}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "Return only OK."})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "prompt-only  success  sandman/return-only-ok-123") {
		t.Fatalf("expected prompt-only summary label, got:\n%s", out)
	}
}

func TestRun_NoParallelFlagDefaultsToZero(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Parallel != 0 {
		t.Errorf("expected parallel=0 to pass through to orchestrator, got %d", spy.req.Parallel)
	}
}

func TestRun_ConfigParallelDefault(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", DefaultParallel: 8}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Parallel != 8 {
		t.Errorf("expected parallel=8 from config default, got %d", spy.req.Parallel)
	}
}

func TestRun_SandboxFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--sandbox", "docker", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Sandbox != "docker" {
		t.Errorf("expected sandbox=docker, got %q", spy.req.Sandbox)
	}
}

func TestRun_BaseBranchFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", Git: config.GitConfig{BaseBranch: "trunk"}}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--base-branch", "release", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.BaseBranch != "release" {
		t.Errorf("expected base_branch=release, got %q", spy.req.BaseBranch)
	}
}

func TestRun_BaseBranchDefaultsToConfig(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", Git: config.GitConfig{BaseBranch: "trunk"}}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.BaseBranch != "trunk" {
		t.Errorf("expected base_branch=trunk, got %q", spy.req.BaseBranch)
	}
}

func TestRun_InteractiveFlagRejected(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--interactive", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for removed --interactive flag")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "unknown flag: --interactive") {
		t.Fatalf("expected unknown flag error, got %v", err)
	}
}

func TestRun_IncludeDependenciesResolvesBatchBeforeRunning(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "Groundwork"},
		},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--include-dependencies", "100"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if !reflect.DeepEqual(spy.req.Issues, []int{7, 42, 100}) {
		t.Fatalf("expected resolved issues [7 42 100], got %v", spy.req.Issues)
	}
	wantDeps := map[int][]int{
		7:   nil,
		42:  {7},
		100: {42},
	}
	if !reflect.DeepEqual(spy.req.Dependencies, wantDeps) {
		t.Fatalf("expected dependencies %v, got %v", wantDeps, spy.req.Dependencies)
	}
}

func TestRun_OpenExternalBlockersAreMarkedBlockedWithoutIncludeDependencies(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor"},
		},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"100"})

	err := cmd.Execute()
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(spy.req.Issues, []int{100}) {
		t.Fatalf("expected resolved issues [100], got %v", spy.req.Issues)
	}
	wantBlocked := map[int][]int{100: {42}}
	if !reflect.DeepEqual(spy.req.Blocked, wantBlocked) {
		t.Fatalf("expected blocked metadata %v, got %v", wantBlocked, spy.req.Blocked)
	}
}

func TestRun_DependencyCycleReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{100}},
		},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--include-dependencies", "100"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected dependency cycle error")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "dependency cycle detected: #100 -> #42 -> #100") {
		t.Fatalf("expected dependency cycle error, got %v", err)
	}
}

func TestRun_LabelFlagResolvesIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Bug A"},
			{Number: 2, Title: "Bug B"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{1, 2}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_TTYPickerSelectsIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 10, Title: "Issue A"},
			{Number: 20, Title: "Issue B"},
		},
	}
	picker := &fakeIssuePicker{issues: []int{10, 20}}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IssuePicker:  picker,
		IsTTY:        func() bool { return true },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{10, 20}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
}

func TestRun_NoArgsNoTTYReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no issues provided and not a TTY")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
}

func TestRun_CombinePlainArgsWithLabelUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 42, Title: "Bug A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42"})

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
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_CombinePlainArgsWithQueryUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 42, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--query", "label:bug", "42"})

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
	if gh.searchIssuesQuery != "(label:bug) is:open" {
		t.Errorf("expected search query '(label:bug) is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_RangeArgUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42:45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "" {
		t.Errorf("expected no search query for bounded ranges, got %q", gh.searchIssuesQuery)
	}
	want := []int{42, 43, 44, 45}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_RangeArgWithLabelUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 42, Title: "Bug A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42:45"})

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
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_RangeArgWithQueryUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 42, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--query", "label:bug is:open", "42:45"})

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
	if gh.searchIssuesQuery != "(label:bug is:open) is:open" {
		t.Errorf("expected search query '(label:bug is:open) is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_MixedArgsWithLabelUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 42, Title: "Bug A"},
			{Number: 44, Title: "Bug B"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42", "44"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 44}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_UnboundedEndRangeUsesQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 42, Title: "Issue A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42:"})

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
	if gh.searchIssuesQuery != "is:open" {
		t.Errorf("expected search query 'is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_UnboundedStartRangeUsesQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Issue A"},
			{Number: 45, Title: "Issue B"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{":45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "" {
		t.Errorf("expected no search query for bounded start-end range, got %q", gh.searchIssuesQuery)
	}
	want := make([]int, 45)
	for i := range want {
		want[i] = i + 1
	}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_MixedExactAndUnboundedRangePreservesExplicitIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 42, Title: "Issue A"},
			{Number: 43, Title: "Issue B"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"7", "42:"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{7, 42, 43}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "is:open" {
		t.Errorf("expected search query 'is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_LargeRangeRejectedBeforeExpansion(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1:1001"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for oversized range")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "more than 1000 issues") {
		t.Errorf("expected oversized range error, got: %v", err)
	}
}

func TestRun_PositionalSelectionWithLabelRejectsTruncatedSearchResults(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	results := make([]github.Issue, 1000)
	for i := range results {
		results[i] = github.Issue{Number: i + 1, Title: fmt.Sprintf("Issue %d", i+1)}
	}
	gh := &fakeGitHubClient{searchIssuesResult: results}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for truncated search results")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "search result limit") {
		t.Errorf("expected search result limit error, got: %v", err)
	}
}

func TestRun_RalphFlagDelegatesLowestIssue(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 3, Title: "Feature C"},
			{Number: 1, Title: "Feature A"},
			{Number: 2, Title: "Feature B"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{1}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	if spy.req.Issues[0] != 1 {
		t.Errorf("expected issue 1, got %d", spy.req.Issues[0])
	}
	if gh.searchIssuesQuery != "label:ready-for-agent is:open" {
		t.Errorf("expected search query 'label:ready-for-agent is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_RalphFlagWithCountDelegatesN(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 5, Title: "Feature E"},
			{Number: 2, Title: "Feature B"},
			{Number: 3, Title: "Feature C"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph=2"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{1, 2}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_RalphFlagWithFewerAvailableDelegatesAll(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 2, Title: "Feature B"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph=5"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{1, 2}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_RalphFlagNoIssuesReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph"})

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

func TestRun_RalphFlagZeroCountReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph=0"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --ralph 0")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--ralph count must be at least 1") {
		t.Errorf("expected '--ralph count must be at least 1' error, got: %v", err)
	}
}

func TestRun_RalphFlagWithArgsReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when combining --ralph with args")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "cannot combine --ralph with issue arguments") {
		t.Errorf("expected mutual exclusivity error, got: %v", err)
	}
}

func TestRun_RalphFlagWithLabelUsesLabelSearch(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Bug A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph", "--label", "bug"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_RalphFlagWithQueryUsesRawQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 3, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph", "--query", "label:bug is:open"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_RalphFlagWithLabelAndQueryReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph", "--label", "bug", "--query", "is:open"})

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

func TestResolveRalphIssues_PriorityPromptFileSelectsSelectionPhase(t *testing.T) {
	sandmanDir := t.TempDir()
	promptPath := filepath.Join(sandmanDir, "priority-selection-prompt.md")
	if err := os.WriteFile(promptPath, []byte("test"), 0644); err != nil {
		t.Fatalf("create prompt: %v", err)
	}

	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A"},
		},
	}

	_, err := resolveRalphIssues(context.Background(), gh, 1, "", "", sandmanDir, "", "", &config.Config{})
	if err == nil {
		t.Fatal("expected selection phase error")
	}
	if !strings.Contains(err.Error(), "resolve agent") {
		t.Errorf("expected agent resolution error (empty config), got: %v", err)
	}
}

func TestResolveRalphIssues_PriorityPromptFileAbsentUsesNumericSort(t *testing.T) {
	sandmanDir := t.TempDir()
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 3, Title: "Feature C"},
			{Number: 1, Title: "Feature A"},
		},
	}

	issues, err := resolveRalphIssues(context.Background(), gh, 1, "", "", sandmanDir, "", "", &config.Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0] != 1 {
		t.Errorf("expected [1], got %v", issues)
	}
}

func TestReadSelectedIssues_ValidJSONReturnsNumbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte("[1, 2, 3]"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := readSelectedIssues(dir, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("expected %d at index %d, got %d", v, i, got[i])
		}
	}
}

func TestReadSelectedIssues_CapsAtMaxCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte("[1, 2, 3, 4, 5]"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := readSelectedIssues(dir, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestReadSelectedIssues_MissingFileReturnsError(t *testing.T) {
	dir := t.TempDir()

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "produced no output") {
		t.Errorf("expected 'produced no output' error, got: %v", err)
	}
}

func TestReadSelectedIssues_InvalidJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte("not-json"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid selection format") {
		t.Errorf("expected 'invalid selection format' error, got: %v", err)
	}
}

func TestReadSelectedIssues_NonArrayJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte(`{"key": "value"}`), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for non-array JSON")
	}
	if !strings.Contains(err.Error(), "invalid selection format") {
		t.Errorf("expected 'invalid selection format' error, got: %v", err)
	}
}

func TestReadSelectedIssues_EmptyArrayReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte("[]"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for empty array")
	}
	if !strings.Contains(err.Error(), "selected no issues") {
		t.Errorf("expected 'selected no issues' error, got: %v", err)
	}
}

func TestReadSelectedIssues_NonIntArrayReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte(`["a", "b"]`), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for non-int array")
	}
	if !strings.Contains(err.Error(), "invalid selection format") {
		t.Errorf("expected 'invalid selection format' error, got: %v", err)
	}
}

func TestRunSelectionPhase_AgentWritesSelectedIssuesAndReturnsNumbers(t *testing.T) {
	sandmanDir := t.TempDir()

	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A", Body: "Description A", Labels: []string{"bug"}},
			{Number: 2, Title: "Feature B", Body: "Description B", Labels: []string{"enhancement"}},
			{Number: 3, Title: "Feature C", Body: "Description C", Labels: []string{"bug"}},
		},
	}

	cfg := &config.Config{
		Agent: "test-agent",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {
			Command: fmt.Sprintf("echo '[2, 1]' > %s/selected-issues.json", sandmanDir),
		},
	}

	got, err := runSelectionPhase(context.Background(), gh, 5, "", "", sandmanDir, "test-agent", "", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{2, 1}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("expected %d at index %d, got %d", v, i, got[i])
		}
	}
}

func TestRunSelectionPhase_AgentFailureReturnsError(t *testing.T) {
	sandmanDir := t.TempDir()

	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A"},
		},
	}

	cfg := &config.Config{
		Agent: "test-agent",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {
			Command: "exit 1",
		},
	}

	_, err := runSelectionPhase(context.Background(), gh, 5, "", "", sandmanDir, "test-agent", "", cfg)
	if err == nil {
		t.Fatal("expected error from agent failure")
	}
	if !strings.Contains(err.Error(), "selection agent failed") {
		t.Errorf("expected agent failure error, got: %v", err)
	}
}

func TestSelectionPhase_FormatCandidateIssues(t *testing.T) {
	issues := []github.Issue{
		{Number: 1, Title: "Bug", Body: "Fix this bug", Labels: []string{"bug"}},
		{Number: 2, Title: "Feature", Body: "Add new feature", Labels: []string{"enhancement"}},
	}

	result := formatCandidateIssues(issues)
	if !strings.Contains(result, "#1") {
		t.Error("expected #1 in formatted output")
	}
	if !strings.Contains(result, "Bug") {
		t.Error("expected Bug in formatted output")
	}
	if !strings.Contains(result, "[bug]") {
		t.Error("expected [bug] in formatted output")
	}
	if !strings.Contains(result, "Fix this bug") {
		t.Error("expected body in formatted output")
	}
}

func TestRun_RalphFlagNegativeCountReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--ralph=-1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --ralph -1")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--ralph count must be at least 1") {
		t.Errorf("expected '--ralph count must be at least 1' error, got: %v", err)
	}
}

func TestRun_QueryFlagResolvesIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 3, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--query", "author:me"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 3 {
		t.Errorf("expected issues [3], got %v", spy.req.Issues)
	}
	if gh.searchIssuesQuery != "(author:me) is:open" {
		t.Errorf("expected search query '(author:me) is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_LabelAndQueryFlagsUseCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 3, Title: "Feature A"}},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "--query", "author:me"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "label:bug (author:me) is:open" {
		t.Errorf("expected search query 'label:bug (author:me) is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_ContainerFlagsPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--sandbox", "docker", "--container-capacity", "1", "--max-containers", "2", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.ContainerCapacitySet {
		t.Fatal("expected ContainerCapacitySet=true")
	}
	if spy.req.ContainerCapacity != 1 {
		t.Errorf("expected container_capacity=1, got %d", spy.req.ContainerCapacity)
	}
	if !spy.req.MaxContainersSet {
		t.Fatal("expected MaxContainersSet=true")
	}
	if spy.req.MaxContainers != 2 {
		t.Errorf("expected max_containers=2, got %d", spy.req.MaxContainers)
	}
	if spy.req.Sandbox != "docker" {
		t.Errorf("expected sandbox=docker, got %q", spy.req.Sandbox)
	}
}

func TestRun_MaxContainersAutoFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--max-containers", "0", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.MaxContainersSet {
		t.Fatal("expected MaxContainersSet=true")
	}
	if spy.req.MaxContainers != 0 {
		t.Errorf("expected max_containers=0, got %d", spy.req.MaxContainers)
	}
}

func TestRun_ContainerCapacityAutoFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--container-capacity", "0", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.ContainerCapacitySet {
		t.Fatal("expected ContainerCapacitySet=true")
	}
	if spy.req.ContainerCapacity != 0 {
		t.Errorf("expected container_capacity=0, got %d", spy.req.ContainerCapacity)
	}
}

func TestRun_InvalidContainerFlagsReturnError(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "container capacity less than one",
			args:    []string{"--container-capacity", "-1", "42"},
			wantErr: "container_capacity must be 0 or greater",
		},
		{
			name:    "negative max containers",
			args:    []string{"--max-containers", "-1", "42"},
			wantErr: "max_containers must be 0 or greater",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(spy)

			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
			if spy.called {
				t.Fatal("expected batch runner not to be called")
			}
		})
	}
}

func TestRun_PromptFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "custom template {{ISSUE_NUMBER}}", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "custom template {{ISSUE_NUMBER}}" {
		t.Errorf("expected PromptFlag='custom template {{ISSUE_NUMBER}}', got %q", spy.req.PromptConfig.PromptFlag)
	}
}

func TestRun_TemplateFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	dir := t.TempDir()
	templatePath := dir + "/my-prompt.md"
	if err := os.WriteFile(templatePath, []byte("template file {{ISSUE_NUMBER}}"), 0644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--template", templatePath, "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.TemplateFlag != templatePath {
		t.Errorf("expected TemplateFlag=%q, got %q", templatePath, spy.req.PromptConfig.TemplateFlag)
	}
}

func TestRun_PromptArgFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt-arg", "FOO=bar", "--prompt-arg", "BAZ=qux", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spy.req.PromptConfig.PromptArgs) != 2 {
		t.Fatalf("expected 2 prompt args, got %d", len(spy.req.PromptConfig.PromptArgs))
	}
	if spy.req.PromptConfig.PromptArgs["FOO"] != "bar" {
		t.Errorf("expected FOO=bar, got FOO=%q", spy.req.PromptConfig.PromptArgs["FOO"])
	}
	if spy.req.PromptConfig.PromptArgs["BAZ"] != "qux" {
		t.Errorf("expected BAZ=qux, got BAZ=%q", spy.req.PromptConfig.PromptArgs["BAZ"])
	}
}

func TestRun_PromptArgFlagInvalidFormat(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt-arg", "NOEQUALSSIGN", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid --prompt-arg format")
	}
	if !strings.Contains(err.Error(), "--prompt-arg") {
		t.Errorf("expected error about --prompt-arg, got: %v", err)
	}
}

func TestRun_PromptArgValidationHappensBeforeDependencyResolution(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch issue should not run")}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt-arg", "NOEQUALSSIGN", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid --prompt-arg format")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--prompt-arg") {
		t.Fatalf("expected prompt-arg validation error, got %v", err)
	}
	if strings.Contains(err.Error(), "fetch issue should not run") {
		t.Fatalf("expected prompt-arg validation before dependency resolution, got %v", err)
	}
}

func TestRun_PromptConfigDefaultsEmpty(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "" {
		t.Errorf("expected empty PromptFlag, got %q", spy.req.PromptConfig.PromptFlag)
	}
	if spy.req.PromptConfig.TemplateFlag != "" {
		t.Errorf("expected empty TemplateFlag, got %q", spy.req.PromptConfig.TemplateFlag)
	}
	if len(spy.req.PromptConfig.PromptArgs) != 0 {
		t.Errorf("expected empty PromptArgs, got %v", spy.req.PromptConfig.PromptArgs)
	}
	if spy.req.PromptConfig.ReviewCommand != "/oc review" {
		t.Errorf("expected default ReviewCommand, got %q", spy.req.PromptConfig.ReviewCommand)
	}
}

func TestRun_ReviewCommandFromConfigPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/config review"}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.ReviewCommand != "/config review" {
		t.Fatalf("expected config review command, got %q", spy.req.PromptConfig.ReviewCommand)
	}
	if !spy.req.PromptConfig.ReviewCommandSet {
		t.Fatal("expected review command to be recorded in run payload")
	}
}

func TestRun_PromptAndTemplateFlagsCombined(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	dir := t.TempDir()
	templatePath := dir + "/template.md"
	if err := os.WriteFile(templatePath, []byte("template file {{ISSUE_NUMBER}}"), 0644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "inline {{ISSUE_NUMBER}}", "--template", templatePath, "--prompt-arg", "K=V", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "inline {{ISSUE_NUMBER}}" {
		t.Errorf("expected PromptFlag='inline {{ISSUE_NUMBER}}', got %q", spy.req.PromptConfig.PromptFlag)
	}
	if spy.req.PromptConfig.TemplateFlag != templatePath {
		t.Errorf("expected TemplateFlag=%q, got %q", templatePath, spy.req.PromptConfig.TemplateFlag)
	}
	if spy.req.PromptConfig.PromptArgs["K"] != "V" {
		t.Errorf("expected K=V, got K=%q", spy.req.PromptConfig.PromptArgs["K"])
	}
}
