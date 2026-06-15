package cmd

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
)

func TestRequireReviewDaemon_BypassesWhenReviewCommandHasNoSandmanSubstring(t *testing.T) {
	dir := t.TempDir()
	if err := requireReviewDaemon("/oc review", dir); err != nil {
		t.Fatalf("expected nil for /oc review, got: %v", err)
	}
}

func TestRequireReviewDaemon_BypassesForCustomReviewCommand(t *testing.T) {
	dir := t.TempDir()
	if err := requireReviewDaemon("/custom-review", dir); err != nil {
		t.Fatalf("expected nil for /custom-review, got: %v", err)
	}
}

func TestRequireReviewDaemon_FailsWhenSandmanSubstringAndSocketMissing(t *testing.T) {
	dir := t.TempDir()
	err := requireReviewDaemon("/sandman review", dir)
	if err == nil {
		t.Fatal("expected error when /sandman review set and no socket")
	}
	if err.Error() != reviewGuardMessage {
		t.Errorf("unexpected error message\nwant:\n%s\ngot:\n%s", reviewGuardMessage, err.Error())
	}
}

func TestRequireReviewDaemon_FailsWhenSocketIsStaleFile(t *testing.T) {
	dir := t.TempDir()
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(sandmanDir, "review.sock")
	if err := os.WriteFile(stalePath, []byte("not a socket"), 0644); err != nil {
		t.Fatal(err)
	}
	err := requireReviewDaemon("/sandman review", sandmanDir)
	if err == nil {
		t.Fatal("expected error when socket file is stale (no listener)")
	}
	if !strings.Contains(err.Error(), "sandman review daemon is not running") {
		t.Errorf("expected guard message, got: %v", err)
	}
}

func TestRequireReviewDaemon_PassesWhenLiveSocketExists(t *testing.T) {
	dir := t.TempDir()
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(sandmanDir, "review.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	if err := requireReviewDaemon("/sandman review", sandmanDir); err != nil {
		t.Fatalf("expected nil for live socket, got: %v", err)
	}
}

// runGuardDeps returns Dependencies for run-guard tests, with a
// chdir into a temp dir that does NOT have a .sandman/review.sock.
// The provided cfg is the config the test wants the command to load.
func runGuardDeps(t testing.TB, runner batch.Runner, cfg *config.Config) Dependencies {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sm-guard-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return Dependencies{
		BatchRunner:  runner,
		ConfigStore:  &fakeStore{config: cfg},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
	}
}

func TestRun_GuardFiresWhenReviewCommandContainsSandmanAndNoSocket(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	cfg := &config.Config{Agent: "opencode", ReviewCommand: "/sandman review"}
	deps := runGuardDeps(t, spy, cfg)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from review guard, got nil")
	}
	if err.Error() != reviewGuardMessage {
		t.Errorf("unexpected error message\nwant:\n%s\ngot:\n%s", reviewGuardMessage, err.Error())
	}
	if spy.called {
		t.Errorf("expected batch runner NOT to be called, but it was")
	}
}

func TestRun_GuardBypassedWhenReviewCommandHasNoSandmanSubstring(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	cfg := &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}
	deps := runGuardDeps(t, spy, cfg)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Errorf("expected batch runner to be called")
	}
}

func TestRun_AutoGuardFiresWhenReviewCommandContainsSandmanAndNoSocket(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	cfg := &config.Config{Agent: "opencode", ReviewCommand: "/sandman review"}
	deps := runGuardDeps(t, spy, cfg)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from review guard, got nil")
	}
	if err.Error() != reviewGuardMessage {
		t.Errorf("unexpected error message\nwant:\n%s\ngot:\n%s", reviewGuardMessage, err.Error())
	}
	if spy.called {
		t.Errorf("expected batch runner NOT to be called for --auto, but it was")
	}
}
