package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestClean_Stale_AloneAccepted(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	deps := Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}},
		EventLog:    &fakeEventLog{},
		GitRunner:   &fakeGitRunner{},
	}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected --stale alone to be accepted, got: %v", err)
	}
}

func TestClean_Stale_MutuallyExclusiveWithAll(t *testing.T) {
	deps := newTestDeps()
	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale", "--all"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --stale combined with --all")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("expected error to mention --stale, got: %v", err)
	}
}

func TestClean_Stale_MutuallyExclusiveWithSuccess(t *testing.T) {
	deps := newTestDeps()
	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale", "--success"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --stale combined with --success")
	}
}

func TestClean_Stale_MutuallyExclusiveWithFailed(t *testing.T) {
	deps := newTestDeps()
	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale", "--failed"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --stale combined with --failed")
	}
}

func TestClean_AllRemovesEverything(t *testing.T) {
	dir := newSandmanDir(t)
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
		RepoRoot:    dir,
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

func TestClean_AllRespectsRepoRootOverCWD(t *testing.T) {
	tempRepo := t.TempDir()
	other := t.TempDir()
	t.Chdir(other)

	worktreeDir := filepath.Join(tempRepo, ".sandman", "worktrees")
	logDir := filepath.Join(tempRepo, ".sandman", "logs")
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
		RepoRoot:    tempRepo,
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeDir}},
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

	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("expected worktree dir under supplied RepoRoot to be removed")
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Errorf("expected log dir under supplied RepoRoot to be removed")
	}
	if _, err := os.Stat(filepath.Join(other, ".sandman", "logs", "42.log")); !os.IsNotExist(err) {
		t.Errorf("did not expect CWD to be touched, but log under %s was created", other)
	}
}

func TestClean_SuccessRemovesOnlySuccessfulRuns(t *testing.T) {
	dir := newSandmanDir(t)
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
	dir := newSandmanDir(t)
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
	dir := newSandmanDir(t)
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
	dir := newSandmanDir(t)
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
	dir := newSandmanDir(t)
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
	dir := newSandmanDir(t)
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
	dir := newSandmanDir(t)
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
	dir := newSandmanDir(t)
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
	dir := newSandmanDir(t)
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
	dir := newSandmanDir(t)
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

// writeBatchManifest writes a batch.json manifest into a run directory
// under .sandman/runs/<runID> for the --stale tests.
func writeBatchManifest(t *testing.T, baseDir, runID string, issues []int, createdAt time.Time) {
	t.Helper()
	runDir := filepath.Join(baseDir, ".sandman", "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	manifest := daemon.BatchManifest{Issues: issues, CreatedAt: createdAt}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestRecoverStaleRuns_DeadBatchUnterminated_EmitsAborted(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-dead-1", []int{42, 43}, createdAt)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.started", RunID: "run-43", Issue: 43, Timestamp: started, Payload: map[string]any{"branch": "sandman/43-fix"}},
	}}

	var buf bytes.Buffer
	cmd := NewCleanCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}},
		EventLog:    log,
		GitRunner:   &fakeGitRunner{},
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 2 {
		t.Fatalf("expected 2 run.aborted events, got %d: %+v", got, log.logged)
	}
	for _, e := range log.logged {
		if e.Type != "run.aborted" {
			t.Errorf("expected type run.aborted, got %q", e.Type)
		}
		recovered, ok := e.Payload["recovered"].(bool)
		if !ok || !recovered {
			t.Errorf("expected payload.recovered=true, got %v", e.Payload)
		}
		if e.IssueRef == nil || (*e.IssueRef != 42 && *e.IssueRef != 43) {
			t.Errorf("expected IssueRef to point to 42 or 43, got %v", e.IssueRef)
		}
	}
	if !strings.Contains(buf.String(), "Recovered 2 stale runs as aborted across 1 dead directories.") {
		t.Errorf("expected summary, got: %s", buf.String())
	}
}

func TestRecoverStaleRuns_LiveBatch_NoEventEmitted(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	runDir := filepath.Join(dir, ".sandman", "runs", "run-live-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	manifest := daemon.BatchManifest{Issues: []int{42}, CreatedAt: createdAt}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cmdServer := daemon.NewCommandServer(runDir, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("start command server: %v", err)
	}
	defer cmdServer.Stop()

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
	}}

	var buf bytes.Buffer
	cmd := NewCleanCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}},
		EventLog:    log,
		GitRunner:   &fakeGitRunner{},
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 0 {
		t.Errorf("expected 0 logged events for live batch, got %d: %+v", got, log.logged)
	}
	if !strings.Contains(buf.String(), "Recovered 0 stale runs") {
		t.Errorf("expected summary to report 0 recovered, got: %s", buf.String())
	}
}

func TestRecoverStaleRuns_RunStartedBeforeManifestCreatedAt_RecoveredAsOrphan(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(-1 * time.Hour) // before CreatedAt — orphaned old run
	writeBatchManifest(t, dir, "run-old", []int{42}, createdAt)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
	}}

	var buf bytes.Buffer
	cmd := NewCleanCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}},
		EventLog:    log,
		GitRunner:   &fakeGitRunner{},
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 1 {
		t.Errorf("expected 1 logged event for orphaned run, got %d: %+v", got, log.logged)
	}
	if got := log.logged[0].Type; got != "run.aborted" {
		t.Errorf("expected run.aborted, got %s", got)
	}
}

func TestRecoverStaleRuns_AlreadyTerminated_NoEventEmitted(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-finished", []int{42}, createdAt)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", RunID: "run-42", Issue: 42, Timestamp: started.Add(time.Hour), Payload: map[string]any{"status": "success"}},
	}}

	var buf bytes.Buffer
	cmd := NewCleanCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}},
		EventLog:    log,
		GitRunner:   &fakeGitRunner{},
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 0 {
		t.Errorf("expected 0 logged events for terminated run, got %d: %+v", got, log.logged)
	}
}

func TestRecoverStaleRuns_ContinuedResetsStartedTimestamp(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	firstStart := createdAt.Add(-2 * time.Hour) // before CreatedAt
	continuedAt := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-cont-1", []int{42}, createdAt)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: firstStart, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.continued", RunID: "run-42", Issue: 42, Timestamp: continuedAt, Payload: map[string]any{"branch": "sandman/42-fix"}},
	}}

	var buf bytes.Buffer
	cmd := NewCleanCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}},
		EventLog:    log,
		GitRunner:   &fakeGitRunner{},
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 1 {
		t.Fatalf("expected 1 logged event for continued run inside window, got %d: %+v", got, log.logged)
	}
	if log.logged[0].Type != "run.aborted" {
		t.Errorf("expected type run.aborted, got %q", log.logged[0].Type)
	}
}

func TestRecoverStaleRuns_MultipleDeadBatches(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdA := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	createdB := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	writeBatchManifest(t, dir, "run-a", []int{1}, createdA)
	writeBatchManifest(t, dir, "run-b", []int{2}, createdB)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-1", Issue: 1, Timestamp: createdA.Add(time.Minute)},
		{Type: "run.started", RunID: "run-2", Issue: 2, Timestamp: createdB.Add(time.Minute)},
	}}

	var buf bytes.Buffer
	cmd := NewCleanCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}},
		EventLog:    log,
		GitRunner:   &fakeGitRunner{},
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 2 {
		t.Fatalf("expected 2 logged events across two dead batches, got %d", got)
	}
	if !strings.Contains(buf.String(), "Recovered 2 stale runs as aborted across 2 dead directories.") {
		t.Errorf("expected summary to count 2 dirs, got: %s", buf.String())
	}
}

func TestRecoverStaleRuns_JSONRoundTripPreservesIssue(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-rt-1", []int{42}, createdAt)

	logFile := filepath.Join(dir, ".sandman", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(logFile), 0755); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	logger := &events.JSONLLogger{Path: logFile}
	initial := []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
	}
	for _, e := range initial {
		if err := logger.Log(e); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	readBack, err := logger.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewCleanCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}},
		EventLog:    logger,
		GitRunner:   &fakeGitRunner{},
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	persisted, err := logger.Read()
	if err != nil {
		t.Fatalf("read events after recover: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("expected 2 persisted events (start + recovered abort), got %d", len(persisted))
	}
	var last events.Event
	for _, e := range persisted {
		last = e
	}
	if last.Type != "run.aborted" {
		t.Errorf("expected last persisted event to be run.aborted, got %q", last.Type)
	}
	if last.IssueRef == nil || *last.IssueRef != 42 {
		t.Errorf("expected IssueRef=42 in persisted run.aborted, got %v", last.IssueRef)
	}
	if recovered, _ := last.Payload["recovered"].(bool); !recovered {
		t.Errorf("expected payload.recovered=true in persisted run.aborted, got %v", last.Payload)
	}

	// Sanity: readBack had 1 event before the command, ensure recovery worked off the
	// in-memory Read() result, not the on-disk file.
	_ = readBack
}
