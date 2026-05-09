package cmd

import (
	"bytes"
	"context"
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
	err    error
}

func (f *fakeEventLog) Log(event events.Event) error { return f.err }
func (f *fakeEventLog) Read() ([]events.Event, error) {
	return f.events, f.err
}

// fakeBatchRunner is a test double for batch.Runner.
type fakeBatchRunner struct {
	result *batch.Result
	err    error
}

func (f *fakeBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	return f.result, f.err
}

// fakeSandbox is a test double for sandbox.Sandbox.
type fakeSandbox struct{}

func (f *fakeSandbox) Start() error { return nil }
func (f *fakeSandbox) Exec(ctx context.Context, worktreePath string, command string) error {
	return nil
}
func (f *fakeSandbox) Stop() error     { return nil }
func (f *fakeSandbox) WorkDir() string { return "" }

// newTestDeps returns Dependencies wired with test doubles.
func newTestDeps() Dependencies {
	return Dependencies{
		BatchRunner:    &fakeBatchRunner{},
		ConfigStore:    &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:       &fakeEventLog{},
		SandboxManager: &fakeSandbox{},
		GitHubClient:   &github.CLIClient{},
		PromptRenderer: &prompt.Engine{},
	}
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
	commands := []string{"init", "run", "status", "history", "retry", "clean", "config"}
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
	rootCmd.SetArgs([]string{"init", "--lang", "go"})

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

func TestStatusPlaceholder(t *testing.T) {
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

	if !strings.Contains(buf.String(), "status is not yet implemented") {
		t.Errorf("status did not print placeholder message")
	}
}

func TestHistoryPlaceholder(t *testing.T) {
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

	if !strings.Contains(buf.String(), "history is not yet implemented") {
		t.Errorf("history did not print placeholder message")
	}
}

func TestRetryPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"retry"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "retry is not yet implemented") {
		t.Errorf("retry did not print placeholder message")
	}
}

func TestCleanPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"clean"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "clean is not yet implemented") {
		t.Errorf("clean did not print placeholder message")
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
