package batch

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestEmitTerminal_PROpenConflictingOverridesStatus(t *testing.T) {
	s := &runSession{
		o: &Orchestrator{
			githubClient: &fakeGitHubClient{},
			renderer:     &retryRenderer{result: "rendered prompt"},
			errorLog:     io.Discard,
			layout:       paths.NewLayout(&config.Config{}, t.TempDir()),
		},
		issueNumber: 42,
	}
	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 99, "CONFLICTING", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	s.o.eventLog = &events.JSONLLogger{Path: eventsPath}

	result := AgentRunResult{IssueNumber: 42, Branch: "sandman/42-fix-bug", Status: "success", RetriesTotal: 1}
	got := s.emitTerminal(context.Background(), "run-id", result)
	if got != "failure" {
		t.Fatalf("emitTerminal returned %q, want failure (CONFLICTING PR overrides success)", got)
	}

	logs, err := s.o.eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var terminal events.Event
	for _, e := range logs {
		if e.Type == "run.finished" {
			terminal = e
		}
	}
	if terminal.Type == "" {
		t.Fatalf("run.finished event not found in logs: %v", logs)
	}
	if payload, _ := terminal.Payload["merge_conflict"].(bool); !payload {
		t.Fatalf("run.finished payload merge_conflict = %v, want true (payload=%v)", terminal.Payload["merge_conflict"], terminal.Payload)
	}
	prNumber, ok := terminal.Payload["pr_number"].(float64)
	if !ok {
		t.Fatalf("run.finished payload pr_number has wrong type %T, want number", terminal.Payload["pr_number"])
	}
	if prNumber != 99 {
		t.Fatalf("run.finished payload pr_number = %v, want 99", prNumber)
	}
}

func TestEmitTerminal_PROpenCleanLeavesStatusUnchanged(t *testing.T) {
	s := &runSession{
		o: &Orchestrator{
			githubClient: &fakeGitHubClient{},
			renderer:     &retryRenderer{result: "rendered prompt"},
			errorLog:     io.Discard,
			layout:       paths.NewLayout(&config.Config{}, t.TempDir()),
		},
		issueNumber: 42,
	}
	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 17, "MERGEABLE", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	s.o.eventLog = &events.JSONLLogger{Path: eventsPath}

	result := AgentRunResult{IssueNumber: 42, Branch: "sandman/42-fix-bug", Status: "success", RetriesTotal: 1}
	got := s.emitTerminal(context.Background(), "run-id", result)
	if got != "success" {
		t.Fatalf("emitTerminal returned %q, want success (MERGEABLE PR should not override)", got)
	}

	logs, err := s.o.eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, e := range logs {
		if e.Type == "run.finished" {
			if _, ok := e.Payload["merge_conflict"]; ok {
				t.Fatalf("MERGEABLE PR should not produce merge_conflict payload, got %v", e.Payload)
			}
		}
	}
}

func TestEmitTerminal_NoOpenPRLeavesStatusUnchanged(t *testing.T) {
	s := &runSession{
		o: &Orchestrator{
			githubClient: &fakeGitHubClient{},
			renderer:     &retryRenderer{result: "rendered prompt"},
			errorLog:     io.Discard,
			layout:       paths.NewLayout(&config.Config{}, t.TempDir()),
		},
		issueNumber: 42,
	}
	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return false, 0, "", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	s.o.eventLog = &events.JSONLLogger{Path: eventsPath}

	result := AgentRunResult{IssueNumber: 42, Branch: "sandman/42-fix-bug", Status: "success", RetriesTotal: 1}
	got := s.emitTerminal(context.Background(), "run-id", result)
	if got != "success" {
		t.Fatalf("emitTerminal returned %q, want success (no PR should not change status)", got)
	}

	logs, err := s.o.eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, e := range logs {
		if e.Type == "run.finished" {
			if _, ok := e.Payload["merge_conflict"]; ok {
				t.Fatalf("no PR should not produce merge_conflict payload, got %v", e.Payload)
			}
		}
	}
}

func TestEmitTerminal_GHLookupErrorLeavesStatusUnchanged(t *testing.T) {
	s := &runSession{
		o: &Orchestrator{
			githubClient: &fakeGitHubClient{},
			renderer:     &retryRenderer{result: "rendered prompt"},
			errorLog:     io.Discard,
			layout:       paths.NewLayout(&config.Config{}, t.TempDir()),
		},
		issueNumber: 42,
	}
	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return false, 0, "", errors.New("gh not authenticated")
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	s.o.eventLog = &events.JSONLLogger{Path: eventsPath}

	result := AgentRunResult{IssueNumber: 42, Branch: "sandman/42-fix-bug", Status: "success", RetriesTotal: 1}
	got := s.emitTerminal(context.Background(), "run-id", result)
	if got != "success" {
		t.Fatalf("emitTerminal returned %q, want success (gh error must be a soft pass-through)", got)
	}
}
