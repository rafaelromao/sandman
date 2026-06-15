package cmd

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// newTestDeps returns Dependencies wired with test doubles. The
// default review command is overridden to "/oc review" so the
// review daemon guard (issue #383) is bypassed by default. Tests
// that need to exercise the guard must build their own
// Dependencies. Tests that need a live .sandman/review.sock
// (e.g. the run-guard-bypass tests) should also build their own
// Dependencies or set ReviewCommand: "/oc review" explicitly.
func newTestDeps() Dependencies {
	return Dependencies{
		BatchRunner:  &fakeBatchRunner{},
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
		Renderer:     &prompt.Engine{},
		IssuePicker:  &fakeIssuePicker{},
		IsTTY:        func() bool { return false },
	}
}

// newSandmanDir creates a temp dir with a live
// .sandman/review.sock listener. The caller must t.Chdir(dir)
// before invoking a command whose default review command is
// "/sandman review" so the run/continue/auto guard finds the
// live socket. Returns the dir for tests that need to inspect it.
func newSandmanDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", ReviewSocketPath(sandmanDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return dir
}

// fakeStore is a test double for config.Store.
type fakeStore struct {
	config *config.Config
	err    error
}

func (f *fakeStore) Load() (*config.Config, error) {
	return f.config, f.err
}

func (f *fakeStore) Save(cfg *config.Config) error {
	f.config = cfg
	return f.err
}

// fakeEventLog is a test double for events.EventLog.
type fakeEventLog struct {
	events []events.Event
	logged []events.Event
	err    error
}

func (f *fakeEventLog) Log(event events.Event) error {
	f.logged = append(f.logged, event)
	return f.err
}
func (f *fakeEventLog) Read() ([]events.Event, error) {
	return f.events, f.err
}
func (f *fakeEventLog) RemoveEventsByIssue(issueNumber int) error { return f.err }

// fakeBatchRunner is a test double for batch.Runner.
type fakeBatchRunner struct {
	result *batch.Result
	err    error
}

func (f *fakeBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	return f.result, f.err
}

// fakeIssuePicker is a test double for IssuePicker.
type fakeIssuePicker struct {
	issues []int
	err    error
}

func (f *fakeIssuePicker) Select(issues []github.Issue) ([]int, error) {
	return f.issues, f.err
}

// fakeGitRunner is a test double for gitRunner.
type fakeGitRunner struct {
	removeWorktreeErr          error
	removeWorktreeCalls        []string
	pruneAndDeleteBranchErr    error
	pruneAndDeleteBranchCalls  []string
	removeOrphanBranchesErr    error
	removeOrphanBranchesCount  int
	removeOrphanBranchesCalled bool
}

func (f *fakeGitRunner) removeWorktree(path string) error {
	f.removeWorktreeCalls = append(f.removeWorktreeCalls, path)
	if f.removeWorktreeErr != nil {
		return f.removeWorktreeErr
	}
	_ = os.RemoveAll(path)
	return nil
}

func (f *fakeGitRunner) pruneAndDeleteBranch(branch string) error {
	f.pruneAndDeleteBranchCalls = append(f.pruneAndDeleteBranchCalls, branch)
	return f.pruneAndDeleteBranchErr
}

func (f *fakeGitRunner) removeOrphanBranches() (int, error) {
	f.removeOrphanBranchesCalled = true
	return f.removeOrphanBranchesCount, f.removeOrphanBranchesErr
}

func TestRootHelpListsAllCommands(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"--help"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	commands := []string{"init", "run", "status", "history", "clean", "config", "attach", "portal", "review", "archive", "stranded"}
	for _, cmd := range commands {
		if !strings.Contains(out, cmd) {
			t.Errorf("help output missing command %q", cmd)
		}
	}
}

func TestInitViaRoot_CreatesSandmanFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetIn(strings.NewReader(""))
	rootCmd.SetArgs([]string{"init", "--build-tools", "generic"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "config.yaml")); err != nil {
		t.Errorf("config.yaml not created: %v", err)
	}
}

func TestRunPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"run"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when no issues provided")
	}
}

func TestRun_ParallelFlagParsed(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"run", "--parallel", "2", "42"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStatusNoActiveRuns(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"status"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "No active runs") {
		t.Errorf("expected 'No active runs', got:\n%s", buf.String())
	}
}

func TestHistoryNoCompletedRuns(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"history"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "No completed runs") {
		t.Errorf("expected 'No completed runs', got:\n%s", buf.String())
	}
}

func TestContinue_IsUnknownCommand(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"continue"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected unknown command error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("expected unknown command error, got %v", err)
	}
}

func TestClean_NoFlagsReturnsErrorFromRoot(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"clean"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when no filter flag provided")
	}
}

func TestConfigPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"config"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "Available Commands:") {
		t.Errorf("config did not print subcommand help")
	}
}

func TestCommandsAreIsolated(t *testing.T) {
	// Verify that each command construction is independent
	deps1 := newTestDeps()
	deps2 := newTestDeps()

	root1 := NewRootCmd(deps1)
	root2 := NewRootCmd(deps2)

	// Modifying one should not affect the other
	root1.SetArgs([]string{"status"})
	root2.SetArgs([]string{"history"})

	var buf1, buf2 bytes.Buffer
	root1.SetOut(&buf1)
	root2.SetOut(&buf2)

	_ = root1.Execute()
	_ = root2.Execute()

	if strings.Contains(buf1.String(), "history") {
		t.Error("root1 output should not contain history command output")
	}
	if strings.Contains(buf2.String(), "status") {
		t.Error("root2 output should not contain status command output")
	}
}
