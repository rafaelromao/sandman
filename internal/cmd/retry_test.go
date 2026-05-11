package cmd

import (
	"bytes"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
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
