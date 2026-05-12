package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

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
	searchIssuesQuery  string
	searchIssuesResult []github.Issue
	searchIssuesError  error
}

func (f *fakeGitHubClient) FetchIssue(number int) (*github.Issue, error) {
	return nil, nil
}

func (f *fakeGitHubClient) SearchIssues(query string) ([]github.Issue, error) {
	f.searchIssuesQuery = query
	return f.searchIssuesResult, f.searchIssuesError
}

func TestRun_SingleIssueInvokesBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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

func TestRun_LoadConfigError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{err: errors.New("config not found")},
		EventLog:    &fakeEventLog{},
	}

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
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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

func TestRun_PrintsSummaryOnSuccess(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "success", Branch: "sandman/43-new-feature"},
		},
	}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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

func TestRun_PreserveFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--preserve", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.Preserve {
		t.Errorf("expected Preserve=true, got false")
	}
}

func TestRun_NoPreserveFlagDefaultsToFalse(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Preserve {
		t.Errorf("expected Preserve=false, got true")
	}
}

func TestRun_NoParallelFlagDefaultsToZero(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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

func TestRun_DebugFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--debug", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.Debug {
		t.Errorf("expected Debug=true, got false")
	}
}

func TestRun_ConfigParallelDefault(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", DefaultParallel: 8}},
		EventLog:    &fakeEventLog{},
	}

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
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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

func TestRun_InteractiveFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--interactive", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.Interactive {
		t.Errorf("expected Interactive=true, got false")
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

func TestRun_InteractiveWithMultipleIssuesReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--interactive", "1", "2"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --interactive with multiple issues")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
}

func TestRun_CombineArgsWithLabelReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when combining args with --label")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
}

func TestRun_NextFlagDelegatesLowestIssue(t *testing.T) {
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
	cmd.SetArgs([]string{"--next"})

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

func TestRun_NextFlagWithCountDelegatesN(t *testing.T) {
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
	cmd.SetArgs([]string{"--next=2"})

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

func TestRun_NextFlagWithFewerAvailableDelegatesAll(t *testing.T) {
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
	cmd.SetArgs([]string{"--next=5"})

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

func TestRun_NextFlagNoIssuesReturnsError(t *testing.T) {
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
	cmd.SetArgs([]string{"--next"})

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

func TestRun_NextFlagZeroCountReturnsError(t *testing.T) {
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
	cmd.SetArgs([]string{"--next=0"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --next 0")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--next count must be at least 1") {
		t.Errorf("expected '--next count must be at least 1' error, got: %v", err)
	}
}

func TestRun_NextFlagWithArgsReturnsError(t *testing.T) {
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
	cmd.SetArgs([]string{"--next", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when combining --next with args")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "cannot combine --next with issue arguments") {
		t.Errorf("expected mutual exclusivity error, got: %v", err)
	}
}

func TestRun_NextFlagWithLabelReturnsError(t *testing.T) {
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
	cmd.SetArgs([]string{"--next", "--label", "bug"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when combining --next with --label")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "cannot combine --next with issue arguments, --label or --query") {
		t.Errorf("expected mutual exclusivity error, got: %v", err)
	}
}

func TestRun_NextFlagWithQueryReturnsError(t *testing.T) {
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
	cmd.SetArgs([]string{"--next", "--query", "is:open"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when combining --next with --query")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "cannot combine --next with issue arguments, --label or --query") {
		t.Errorf("expected mutual exclusivity error, got: %v", err)
	}
}

func TestRun_InteractiveWithNextGreaterThanOneReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
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
	cmd.SetArgs([]string{"--interactive", "--next=2"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --interactive with --next 2")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--interactive requires exactly one issue") {
		t.Errorf("expected interactive error, got: %v", err)
	}
}

func TestRun_NextFlagNegativeCountReturnsError(t *testing.T) {
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
	cmd.SetArgs([]string{"--next=-1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --next -1")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--next count must be at least 1") {
		t.Errorf("expected '--next count must be at least 1' error, got: %v", err)
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
	cmd.SetArgs([]string{"--query", "is:open author:me"})

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
	if gh.searchIssuesQuery != "is:open author:me" {
		t.Errorf("expected search query 'is:open author:me', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_IsolatedContainersFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--sandbox", "docker", "--isolated-containers", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.IsolatedContainers {
		t.Errorf("expected IsolatedContainers=true, got false")
	}
	if spy.req.Sandbox != "docker" {
		t.Errorf("expected sandbox=docker, got %q", spy.req.Sandbox)
	}
}

func TestRun_PromptFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "custom template", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "custom template" {
		t.Errorf("expected PromptFlag='custom template', got %q", spy.req.PromptConfig.PromptFlag)
	}
}

func TestRun_TemplateFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--template", "./my-prompt.md", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.TemplateFlag != "./my-prompt.md" {
		t.Errorf("expected TemplateFlag='./my-prompt.md', got %q", spy.req.PromptConfig.TemplateFlag)
	}
}

func TestRun_PromptArgFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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

func TestRun_PromptConfigDefaultsEmpty(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

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
}

func TestRun_PromptAndTemplateFlagsCombined(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "inline", "--template", "./t.md", "--prompt-arg", "K=V", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "inline" {
		t.Errorf("expected PromptFlag='inline', got %q", spy.req.PromptConfig.PromptFlag)
	}
	if spy.req.PromptConfig.TemplateFlag != "./t.md" {
		t.Errorf("expected TemplateFlag='./t.md', got %q", spy.req.PromptConfig.TemplateFlag)
	}
	if spy.req.PromptConfig.PromptArgs["K"] != "V" {
		t.Errorf("expected K=V, got K=%q", spy.req.PromptConfig.PromptArgs["K"])
	}
}
