package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

type repairRunnableFactory struct {
	run     *AgentRun
	sandbox sandbox.Sandbox
}

func (f *repairRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.run = NewAgentRun(issue, branch, sb)
	f.sandbox = sb
	return f.run
}

type forbiddenRepairSandboxFactory struct{ t *testing.T }

func (f forbiddenRepairSandboxFactory) NewSandbox(string, string, string, string, sandbox.Container) sandbox.Sandbox {
	f.t.Fatal("repair invoked ordinary sandbox preparation")
	return nil
}

func TestRunRepairConfiguredAgentAndArtifacts(t *testing.T) {
	dir := testenv.MkdirShort(t, "repair-")
	t.Chdir(dir) // Deliberately not a git repository: no base or identity exists.
	if err := os.MkdirAll(".sandman", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".sandman/task.md", []byte("parent task"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("opencode", []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > repair-launch.txt\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := &spyEventLog{}
	factory := &repairRunnableFactory{}
	cfg := &config.Config{DefaultAgent: "configured", Sandbox: "podman", Retries: 9,
		AgentProviders: map[string]config.Agent{"configured": {Preset: "opencode", Model: "provider/repair-model"}}}
	o := NewOrchestrator(nil, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(forbiddenRepairSandboxFactory{t}),
		WithRunSessionOpts(runSessionOptions{baseBranchSync: func(string, string) error { t.Fatal("repair synced base"); return nil }}))
	req := Request{RunID: "261009123456-abcd-prompt-health-repair", Retries: 8, PortalHidden: true,
		PromptConfig: prompt.RenderConfig{TaskPrompt: "diagnose and repair"}}
	result, err := o.RunRepair(t.Context(), req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || result.Runs[0].Status != "success" || result.Runs[0].RunID != req.RunID || result.Runs[0].RetriesTotal != 1 {
		t.Fatalf("result: %+v", result)
	}
	batchDir := filepath.Join(dir, ".sandman", "batches", req.RunID)
	manifest, err := daemon.ReadManifest(batchDir)
	if err != nil || manifest.BatchId != req.RunID || manifest.RunKind != "prompt-only" {
		t.Fatalf("repair batch manifest: %+v %v", manifest, err)
	}
	index, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil || len(index.Batches) != 1 || index.Batches[0].ID != req.RunID || index.Batches[0].Path != batchDir {
		t.Fatalf("repair index: %+v %v", index, err)
	}
	if factory.run.runFolder != filepath.Join(batchDir, "runs", req.RunID) {
		t.Fatalf("repair artifacts disagree with reserved batch: %s", factory.run.runFolder)
	}
	if factory.run.model != "provider/repair-model" || factory.run.preset != "opencode" {
		t.Fatalf("configured agent not preserved: %+v", factory.run)
	}
	if factory.run.sandbox.WorkDir() != dir {
		t.Fatalf("workdir = %q", factory.run.sandbox.WorkDir())
	}
	launch, err := os.ReadFile("repair-launch.txt")
	if err != nil || !strings.Contains(string(launch), "provider/repair-model") || !strings.Contains(string(launch), "diagnose and repair") {
		t.Fatalf("configured model did not reach execution: %q %v", launch, err)
	}
	parent, err := os.ReadFile(".sandman/task.md")
	if err != nil || string(parent) != "parent task" {
		t.Fatalf("parent task changed: %q %v", parent, err)
	}
	artifact := filepath.Join(factory.run.runFolder, "task.md")
	data, err := os.ReadFile(artifact)
	if err != nil || !strings.Contains(string(data), "diagnose and repair") {
		t.Fatalf("repair task: %q %v", data, err)
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) != 1 || states[0].RunID != req.RunID || states[0].Status() != "success" {
		t.Fatalf("lifecycle: %+v", states)
	}
	for _, e := range log.snapshot() {
		if e.Type == "run.retry" {
			t.Fatal("repair retried")
		}
	}
}

func TestRunRepairRejectsIssueAndReviewInput(t *testing.T) {
	o := NewOrchestrator(nil, nil, nil, nil)
	for _, req := range []Request{{Issues: []int{42}}, {Review: true}, {IssueNumber: 42}, {PRNumber: 73}} {
		if _, err := o.RunRepair(t.Context(), req, nil); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
}

func TestRunRepairCancelledBeforeAdmissionDoesNotPrepare(t *testing.T) {
	dir := testenv.MkdirShort(t, "repair-aborted-")
	t.Chdir(dir)
	o := NewOrchestrator(nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := o.RunRepair(ctx, Request{RunID: "repair-aborted", PromptConfig: prompt.RenderConfig{PromptFlag: "repair"}}, &config.Config{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled admission prepared artifacts: %v", err)
	}
}

func TestPromptOnlyOperationalStartupDiagnostic(t *testing.T) {
	dir := testenv.MkdirShort(t, "repair-fail-")
	t.Chdir(dir)
	cause := errors.New("stranded worktree: original actionable diagnostic")
	cfg := &config.Config{}
	o := NewOrchestrator(nil, &noopRenderer{}, nil, nil, WithErrorLog(io.Discard),
		WithRunSessionOpts(runSessionOptions{baseBranchSync: func(string, string) error { return nil }}))
	row := RowSpec{RunID: "startup-failure", BatchID: "startup-failure", Branches: map[int]string{0: "repair"}, RenderCfg: prompt.RenderConfig{TaskPrompt: "repair"}}
	bc := BatchConfig{Cfg: cfg, IdentityResolver: noopIdentityResolver(), AgentCfg: config.Agent{Command: "true"}}
	wrapped := fmt.Errorf("initialize worktree: %w", cause)
	result, started := o.runPromptOnlyRow(context.Background(), row, bc, &fakeSandboxFactory{sandbox: &fakeSandbox{startErr: wrapped}}, nil)
	if started || !errors.Is(result.OperationalError, cause) || !strings.Contains(result.OperationalError.Error(), wrapped.Error()) {
		t.Fatalf("lost diagnostic: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "OperationalError") {
		t.Fatalf("operational error serialized: %s %v", encoded, err)
	}
	_, err = o.runPromptOnly(t.Context(), cfg, "configured", bc.AgentCfg, noopIdentityResolver(),
		&fakeSandboxFactory{sandbox: &fakeSandbox{startErr: wrapped}}, nil,
		Request{RunID: row.RunID, PromptConfig: row.RenderCfg}, "main", 0, 1, 0, 0, "worktree", 0, false, 0, false, false, false, newBatchCoordinator(nil), o.layout)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), wrapped.Error()) {
		t.Fatalf("public prompt-only error lost diagnostic: %v", err)
	}
}

type repairStartWriter struct {
	once    sync.Once
	started chan struct{}
}

func (w *repairStartWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return len(p), nil
}

func TestRunRepairCancellationWaitsForProcess(t *testing.T) {
	dir := testenv.MkdirShort(t, "repair-cancel-")
	t.Chdir(dir)
	log := &spyEventLog{}
	factory := &repairRunnableFactory{}
	writer := &repairStartWriter{started: make(chan struct{})}
	cfg := &config.Config{Agent: "configured", Sandbox: "docker", AgentProviders: map[string]config.Agent{"configured": {Command: "printf 'started\\n'; sleep 60"}}}
	o := NewOrchestrator(nil, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(forbiddenRepairSandboxFactory{t}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := o.RunRepair(ctx, Request{RunID: "repair-cancel", OutputWriter: writer, PromptConfig: prompt.RenderConfig{TaskPrompt: "repair"}}, cfg)
		done <- outcome{result, err}
	}()
	select {
	case <-writer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("repair process never started")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, ErrAborted) || got.result.Runs[0].Status != "aborted" {
			t.Fatalf("outcome: %+v %v", got.result, got.err)
		}
		proc := factory.sandbox.Process()
		if proc == nil {
			t.Fatal("missing supervised process")
		}
		select {
		case <-proc.WaitDone():
		default:
			t.Fatal("RunRepair returned before process wait completed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("repair cancellation did not complete")
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) != 1 || states[0].RunID != "repair-cancel" || states[0].Status() != "aborted" {
		t.Fatalf("cancel lifecycle: %+v", states)
	}
}

func TestRunRepairForcesOneFailedAttempt(t *testing.T) {
	t.Chdir(testenv.MkdirShort(t, "repair-once-"))
	log := &spyEventLog{}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{Status: "failure"}, {Status: "success"}}}
	cfg := &config.Config{Agent: "configured", Retries: 9, AgentProviders: map[string]config.Agent{"configured": {Command: "true"}}}
	o := NewOrchestrator(nil, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory))
	result, err := o.RunRepair(t.Context(), Request{RunID: "repair-one-attempt", Retries: 9, PromptConfig: prompt.RenderConfig{TaskPrompt: "repair"}}, cfg)
	if err == nil || len(factory.created) != 1 || result.Runs[0].Status != "failure" || result.Runs[0].RetriesTotal != 1 {
		t.Fatalf("result: %+v err=%v launches=%d", result, err, len(factory.created))
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) != 1 || states[0].RunID != "repair-one-attempt" || states[0].Status() != "failure" {
		t.Fatalf("failure lifecycle: %+v", states)
	}
}

func TestRunRepairTaskPromptKeepsTemplateBracesLiteral(t *testing.T) {
	dir := testenv.MkdirShort(t, "repair-literal-")
	t.Chdir(dir)
	if err := os.MkdirAll(".sandman", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("opencode", []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > literal-launch.txt\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := &spyEventLog{}
	factory := &repairRunnableFactory{}
	cfg := &config.Config{DefaultAgent: "configured", Sandbox: "podman",
		AgentProviders: map[string]config.Agent{"configured": {Preset: "opencode", Model: "provider/repair-model"}}}
	o := NewOrchestrator(nil, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(forbiddenRepairSandboxFactory{t}))
	literal := "diagnose missing substitution keys: {{UNKNOWN_KEY}} and verify"
	req := Request{RunID: "261009123456-abcd-prompt-literal", PromptConfig: prompt.RenderConfig{TaskPrompt: literal}}
	result, err := o.RunRepair(t.Context(), req, cfg)
	if err != nil {
		t.Fatalf("literal TaskPrompt must not fail template rendering: %v", err)
	}
	if len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		t.Fatalf("result: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(factory.run.runFolder, "task.md"))
	if err != nil || !strings.Contains(string(data), "{{UNKNOWN_KEY}}") {
		t.Fatalf("literal braces lost: %q %v", data, err)
	}
}

func TestRunRepairRenderedPromptUsesReservedArtifacts(t *testing.T) {
	dir := testenv.MkdirShort(t, "repair-render-")
	t.Chdir(dir)
	if err := os.MkdirAll(".sandman", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".sandman/task.md", []byte("parent task"), 0644); err != nil {
		t.Fatal(err)
	}
	renderer := &spyPromptRenderer{result: "rendered diagnostic"}
	factory := &repairRunnableFactory{}
	cfg := &config.Config{Agent: "configured", AgentProviders: map[string]config.Agent{"configured": {Command: "test -f {{.PromptFile}}"}}}
	log := &spyEventLog{}
	o := NewOrchestrator(nil, renderer, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory))
	runID := "repair-rendered"
	runDir := o.layout.RunFolder(runID, runID)
	_, err := o.RunRepair(t.Context(), Request{RunID: runID, RunDir: o.layout.BatchDir(runID),
		BatchTS: orchTestRunTS, BatchShortID: orchTestRunShortID,
		PromptConfig: prompt.RenderConfig{PromptFlag: "diagnose", RenderedPromptFile: ".sandman/task.md"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(runDir, "task.md"))
	if err != nil || !strings.Contains(string(data), "rendered diagnostic") {
		t.Fatalf("rendered repair artifact: %q %v", data, err)
	}
	parent, err := os.ReadFile(".sandman/task.md")
	if err != nil || string(parent) != "parent task" {
		t.Fatalf("parent task changed: %q %v", parent, err)
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) != 1 || states[0].RunID != runID || states[0].Status() != "success" {
		t.Fatalf("reserved lifecycle identity changed: %+v", states)
	}
}
