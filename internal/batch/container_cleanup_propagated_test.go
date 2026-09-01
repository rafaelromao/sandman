package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestContextRollover_CleanupErrorWrappingPreservedViaContainerPath(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}

	sbDir := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(sbDir, ".sandman"), 0o755); err != nil {
		t.Fatal(err)
	}

	// This sandbox mimics the real container path: it writes the detector
	// literal, blocks until cancellation, then returns a wrapped CleanupError
	// exactly as container_sandbox.go does: fmt.Errorf("container exec: %w", &CleanupError{...})
	sb := &wrappingCleanupSandbox{
		workDir:    sbDir,
		cleanupErr: fmt.Errorf("kill failed"),
	}
	factory := &wrappingCleanupFactory{sb: sb}

	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "bug"}}}
	sandboxFactory := sandboxFactoryFunc(func(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
		return sb
	})
	o := NewOrchestrator(client, &retryRenderer{result: "prompt"}, nil, eventLog,
		WithSandboxFactory(sandboxFactory),
		WithRunnableFactory(factory),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.BuiltInAgentPresets["opencode"].Agent("opencode"),
		IdentityResolver: noopIdentityResolver(),
		Retries:          0,
	}

	row := RowSpec{IssueNumber: 42, Branches: map[int]string{42: "42-bug"}, BaseBranch: "main"}
	result, started := o.newRunExecutor(context.Background(), bc, sandboxFactory, nil).Execute(context.Background(), row)
	if !started {
		t.Fatalf("expected start, result=%+v", result)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, ev := range logs {
		if ev.Type == "run.finished" {
			if _, ok := ev.Payload["cleanup_error"]; !ok {
				t.Fatalf("expected cleanup_error in payload, got %v", ev.Payload)
			}
			if ev.Payload["cleanup_error"] != "kill failed" {
				t.Fatalf("cleanup_error = %q, want 'kill failed'", ev.Payload["cleanup_error"])
			}
			return
		}
	}
	t.Fatalf("expected run.finished with cleanup_error, got %+v", logs)
}

type wrappingCleanupFactory struct{ sb *wrappingCleanupSandbox }

func (f *wrappingCleanupFactory) NewRunnable(issue *github.Issue, branch string, _ sandbox.Sandbox) Runnable {
	run := NewAgentRun(issue, branch, f.sb)
	run.preset = "opencode"
	return run
}

type wrappingCleanupSandbox struct {
	workDir    string
	cleanupErr error
}

func (s *wrappingCleanupSandbox) Start(sandbox.SandboxStart) error {
	return os.MkdirAll(filepath.Join(s.workDir, ".sandman"), 0o755)
}
func (s *wrappingCleanupSandbox) Exec(ctx context.Context, _ string, _, stderr io.Writer) error {
	_, _ = io.WriteString(stderr, "Error: prompt is too long\nError: prompt is too long\n")
	<-ctx.Done()
	// Wrap exactly like container_sandbox.go: fmt.Errorf("container exec: %w", &CleanupError{...})
	return fmt.Errorf("container exec: %w", &sandbox.CleanupError{Err: ctx.Err(), CleanupFail: s.cleanupErr})
}
func (s *wrappingCleanupSandbox) ExecInteractive(context.Context, string) error { return nil }
func (s *wrappingCleanupSandbox) Stop() error                                   { return nil }
func (s *wrappingCleanupSandbox) WorkDir() string                               { return s.workDir }
func (s *wrappingCleanupSandbox) RepoPath() string                              { return filepath.Dir(s.workDir) }
func (s *wrappingCleanupSandbox) Process() sandbox.Process                      { return nil }
func (s *wrappingCleanupSandbox) RestoreHostPaths() error                       { return nil }
func (s *wrappingCleanupSandbox) WritePrompt(content string) error {
	return os.WriteFile(filepath.Join(s.workDir, ".sandman", "task.md"), []byte(content), 0o644)
}
