package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestClean_NoFlagsReturnsError(t *testing.T) {
	deps := newTestDeps()
	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no filter flag provided")
	}
}

func TestClean_AllRemovesEverything(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	logDir := filepath.Join(dir, ".sandman", "logs")
	if err := os.MkdirAll(filepath.Join(worktreeDir, "sandman", "42-fix"), 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "42.log"), []byte("log"), 0644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
		EventLog:    &fakeEventLog{},
	}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("expected worktree dir to be removed")
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Errorf("expected log dir to be removed")
	}
}

func TestClean_SuccessRemovesOnlySuccessfulRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	if err := os.MkdirAll(filepath.Join(worktreeDir, "sandman", "42-fix"), 0755); err != nil {
		t.Fatalf("create worktree 42: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(worktreeDir, "sandman", "43-fix"), 0755); err != nil {
		t.Fatalf("create worktree 43: %v", err)
	}

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "success"}},
		{Type: "run.started", RunID: "run-43", Issue: 43, Payload: map[string]any{"branch": "sandman/43-fix"}},
		{Type: "run.finished", RunID: "run-43", Issue: 43, Payload: map[string]any{"status": "failure"}},
	}}
	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--success"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(worktreeDir, "sandman", "42-fix")); !os.IsNotExist(err) {
		t.Errorf("expected worktree 42 to be removed")
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, "sandman", "43-fix")); os.IsNotExist(err) {
		t.Errorf("expected worktree 43 to be preserved")
	}
}

func TestClean_FailedRemovesOnlyFailedRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	logDir := filepath.Join(dir, ".sandman", "logs")
	if err := os.MkdirAll(filepath.Join(worktreeDir, "sandman", "42-fix"), 0755); err != nil {
		t.Fatalf("create worktree 42: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(worktreeDir, "sandman", "43-fix"), 0755); err != nil {
		t.Fatalf("create worktree 43: %v", err)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "42.log"), []byte("log"), 0644); err != nil {
		t.Fatalf("create log 42: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "43.log"), []byte("log"), 0644); err != nil {
		t.Fatalf("create log 43: %v", err)
	}

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "success"}},
		{Type: "run.started", RunID: "run-43", Issue: 43, Payload: map[string]any{"branch": "sandman/43-fix"}},
		{Type: "run.finished", RunID: "run-43", Issue: 43, Payload: map[string]any{"status": "failure"}},
	}}
	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
		EventLog:    log,
	}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--failed"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(worktreeDir, "sandman", "42-fix")); os.IsNotExist(err) {
		t.Errorf("expected worktree 42 to be preserved")
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, "sandman", "43-fix")); !os.IsNotExist(err) {
		t.Errorf("expected worktree 43 to be removed")
	}
	if _, err := os.Stat(filepath.Join(logDir, "42.log")); os.IsNotExist(err) {
		t.Errorf("expected log 42 to be preserved")
	}
	if _, err := os.Stat(filepath.Join(logDir, "43.log")); !os.IsNotExist(err) {
		t.Errorf("expected log 43 to be removed")
	}
}
