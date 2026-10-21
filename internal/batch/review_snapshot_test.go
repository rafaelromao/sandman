package batch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

type snapshotRunnableFactory struct {
	t     *testing.T
	head  string
	calls int
}

func (f *snapshotRunnableFactory) NewRunnable(_ *github.Issue, _ string, sb sandbox.Sandbox) Runnable {
	return &snapshotRunnable{factory: f, sb: sb}
}

type snapshotRunnable struct {
	factory *snapshotRunnableFactory
	sb      sandbox.Sandbox
}

func (r *snapshotRunnable) Run(_ context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	f := r.factory
	f.calls++
	if got := strings.TrimSpace(runGit(f.t, r.sb.WorkDir(), "rev-parse", "HEAD")); got != f.head {
		f.t.Errorf("reviewer sees %s, want PR head %s", got, f.head)
	}
	if data, err := os.ReadFile(filepath.Join(r.sb.WorkDir(), "feature.txt")); err != nil || string(data) != "PR change\n" {
		f.t.Errorf("reviewer cannot see fork change: %q, %v", data, err)
	}
	return AgentRunResult{Status: "success"}
}

func TestReviewSnapshotChecksOutForkHeadWithSeparateBase(t *testing.T) {
	for _, scenario := range []string{"fresh", "missing-ref", "wrong-head", "reused-wrong-head"} {
		t.Run(scenario, func(t *testing.T) { testReviewSnapshotCheckout(t, scenario) })
	}
}

func testReviewSnapshotCheckout(t *testing.T, scenario string) {
	dir := testenv.MkdirShort(t, "sm-head-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	parentHead := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "push", "origin", "main:release")
	fork := t.TempDir()
	runGit(t, fork, "clone", dir, ".")
	runGit(t, fork, "config", "user.name", "Test")
	runGit(t, fork, "config", "user.email", "test@example.com")
	runGit(t, fork, "checkout", "-b", "fork-feature")
	if err := os.WriteFile(filepath.Join(fork, "feature.txt"), []byte("PR change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, fork, "add", "feature.txt")
	runGit(t, fork, "commit", "-m", "fork change")
	head := strings.TrimSpace(runGit(t, fork, "rev-parse", "HEAD"))
	origin := strings.TrimSpace(runGit(t, dir, "remote", "get-url", "origin"))
	runGit(t, fork, "push", origin, "HEAD:refs/pull/36/head")
	if scenario == "missing-ref" {
		runGit(t, fork, "push", origin, ":refs/pull/36/head")
	}
	requested := head
	if scenario == "wrong-head" {
		requested = parentHead
	}
	if scenario == "reused-wrong-head" {
		stale := sandbox.NewWorktreeSandbox(dir, filepath.Join(dir, ".sandman", "worktrees"), "review-36-test", "main")
		if err := stale.Start(sandbox.SandboxStart{}); err != nil {
			t.Fatal(err)
		}
	}
	factory := &snapshotRunnableFactory{t: t, head: head}
	cfg := &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}
	o := NewOrchestrator(&fakeGitHubClient{}, &noopRenderer{}, &fakeConfigStore{config: cfg}, &spyEventLog{}, WithRunnableFactory(factory))
	_, err := o.RunBatch(context.Background(), Request{Review: true, PRNumber: 36, ReviewHeadSHA: requested, BaseBranch: "release", PromptConfig: prompt.RenderConfig{PromptFlag: "review", Branch: "review-36-test"}, RunID: "review-snapshot"})
	if scenario == "fresh" {
		if err != nil || factory.calls != 1 {
			t.Fatalf("reviewer launches = %d, error = %v", factory.calls, err)
		}
	} else if err == nil || factory.calls != 0 {
		t.Fatalf("invalid snapshot launched reviewer: calls = %d, error = %v", factory.calls, err)
	}
	if got := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")); got != parentHead {
		t.Fatalf("operator checkout changed to %s", got)
	}
}
