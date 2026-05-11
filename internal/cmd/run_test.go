package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
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
