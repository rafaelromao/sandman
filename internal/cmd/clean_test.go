package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
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
		GitRunner:   &fakeGitRunner{},
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
		GitRunner:   &fakeGitRunner{},
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
		GitRunner:   &fakeGitRunner{},
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

func TestClean_FailedIncludesAbortedRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	logDir := filepath.Join(dir, ".sandman", "logs")
	if err := os.MkdirAll(filepath.Join(worktreeDir, "sandman", "42-abort"), 0755); err != nil {
		t.Fatalf("create worktree 42: %v", err)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "42.log"), []byte("log"), 0644); err != nil {
		t.Fatalf("create log 42: %v", err)
	}

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-abort"}},
		{Type: "run.aborted", RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "aborted", "branch": "sandman/42-abort"}},
	}}
	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
		EventLog:    log,
		GitRunner:   &fakeGitRunner{},
	}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--failed"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(worktreeDir, "sandman", "42-abort")); !os.IsNotExist(err) {
		t.Errorf("expected aborted worktree 42 to be removed under --failed")
	}
	if _, err := os.Stat(filepath.Join(logDir, "42.log")); !os.IsNotExist(err) {
		t.Errorf("expected aborted log 42 to be removed under --failed")
	}
	if !strings.Contains(buf.String(), "Cleaned 1 runs") {
		t.Errorf("expected output to confirm 1 run cleaned, got: %s", buf.String())
	}
}

func TestClean_Success_CallsGitRemoveWorktree(t *testing.T) {
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
	gr := &fakeGitRunner{}
	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
		EventLog:    log,
		GitRunner:   gr,
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

	if len(gr.removeWorktreeCalls) != 1 {
		t.Fatalf("expected 1 removeWorktree call, got %d", len(gr.removeWorktreeCalls))
	}
	expectedPath := filepath.Join(worktreeDir, "sandman", "42-fix")
	if gr.removeWorktreeCalls[0] != expectedPath {
		t.Errorf("expected removeWorktree(%q), got %q", expectedPath, gr.removeWorktreeCalls[0])
	}
	if len(gr.pruneAndDeleteBranchCalls) != 0 {
		t.Errorf("expected no pruneAndDeleteBranch calls, got %d", len(gr.pruneAndDeleteBranchCalls))
	}
}

func TestClean_Failed_CallsGitRemoveWorktree(t *testing.T) {
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
	gr := &fakeGitRunner{}
	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
		EventLog:    log,
		GitRunner:   gr,
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

	if len(gr.removeWorktreeCalls) != 1 {
		t.Fatalf("expected 1 removeWorktree call, got %d", len(gr.removeWorktreeCalls))
	}
	expectedPath := filepath.Join(worktreeDir, "sandman", "43-fix")
	if gr.removeWorktreeCalls[0] != expectedPath {
		t.Errorf("expected removeWorktree(%q), got %q", expectedPath, gr.removeWorktreeCalls[0])
	}
	if len(gr.pruneAndDeleteBranchCalls) != 0 {
		t.Errorf("expected no pruneAndDeleteBranch calls, got %d", len(gr.pruneAndDeleteBranchCalls))
	}
}

func TestClean_FallbackCallsPruneAndDelete(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "failure"}},
	}}
	gr := &fakeGitRunner{removeWorktreeErr: fmt.Errorf("worktree not found")}
	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
		EventLog:    log,
		GitRunner:   gr,
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

	if len(gr.removeWorktreeCalls) != 1 {
		t.Fatalf("expected 1 removeWorktree call, got %d", len(gr.removeWorktreeCalls))
	}
	if len(gr.pruneAndDeleteBranchCalls) != 1 {
		t.Fatalf("expected 1 pruneAndDeleteBranch call, got %d", len(gr.pruneAndDeleteBranchCalls))
	}
	if gr.pruneAndDeleteBranchCalls[0] != "sandman/42-fix" {
		t.Errorf("expected pruneAndDeleteBranch(%q), got %q", "sandman/42-fix", gr.pruneAndDeleteBranchCalls[0])
	}
}

func TestClean_All_CallsRemoveOrphanBranches(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	logDir := filepath.Join(dir, ".sandman", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "42.log"), []byte("log"), 0644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	gr := &fakeGitRunner{removeOrphanBranchesCount: 3}
	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: ".sandman/worktrees"}},
		EventLog:    &fakeEventLog{},
		GitRunner:   gr,
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

	if !gr.removeOrphanBranchesCalled {
		t.Errorf("expected removeOrphanBranches to be called")
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Errorf("expected log dir to be removed")
	}
	if !strings.Contains(buf.String(), "Cleaned 3 stale branches and logs") {
		t.Errorf("expected output to confirm 3 branches cleaned, got: %s", buf.String())
	}
}

func TestClean_Success_CleansPromptOnlyRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	if err := os.MkdirAll(filepath.Join(worktreeDir, "sandman", "prompt-only-123"), 0755); err != nil {
		t.Fatalf("create prompt-only worktree: %v", err)
	}

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-prompt", Payload: map[string]any{"branch": "sandman/prompt-only-123"}},
		{Type: "run.finished", RunID: "run-prompt", Payload: map[string]any{"status": "success", "branch": "sandman/prompt-only-123"}},
	}}
	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
		EventLog:    log,
		GitRunner:   &fakeGitRunner{},
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

	if _, err := os.Stat(filepath.Join(worktreeDir, "sandman", "prompt-only-123")); !os.IsNotExist(err) {
		t.Fatalf("expected prompt-only worktree to be removed")
	}
}

func TestClean_All_RemovesStaleRunOwnedSnapshots(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	inactive := filepath.Join(dir, ".sandman", "runs", "run-99-1")
	if err := os.MkdirAll(filepath.Join(inactive, "config"), 0755); err != nil {
		t.Fatalf("mkdir inactive config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inactive, "config", "host.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write host.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inactive, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}},
		EventLog:    &fakeEventLog{},
		GitRunner:   &fakeGitRunner{},
	}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(inactive, "config")); !os.IsNotExist(err) {
		t.Errorf("expected stale run-owned config/ to be removed under --all, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inactive, "batch.json")); err != nil {
		t.Errorf("expected inactive run manifest to be preserved (config snapshot is what gets removed): %v", err)
	}
	if !strings.Contains(buf.String(), "stale run snapshots") {
		t.Errorf("expected output to report stale run snapshots cleaned, got: %s", buf.String())
	}
}

func TestClean_Success_PreservesActiveRunSnapshots(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	active := filepath.Join(dir, ".sandman", "runs", "active-1")
	if err := os.MkdirAll(filepath.Join(active, "config"), 0755); err != nil {
		t.Fatalf("mkdir active config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(active, "config", "host.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write host.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(active, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}
	cmdServer := daemon.NewCommandServer(active, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("start command server: %v", err)
	}
	defer cmdServer.Stop()

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
		EventLog:    &fakeEventLog{},
		GitRunner:   &fakeGitRunner{},
	}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--success"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(active, "config")); err != nil {
		t.Errorf("expected active run config/ to be preserved, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(active, "batch.json")); err != nil {
		t.Errorf("expected active run manifest to be preserved, got: %v", err)
	}
}
