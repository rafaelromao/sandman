package batch

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestVerifySandboxFactory_RealWrapsNewWorktreeSandbox(t *testing.T) {
	t.Parallel()
	repoPath := "/tmp/repo"
	worktreeBase := "/tmp/sandman-worktrees"
	f := &realVerifySandboxFactory{}
	sb, err := f.NewVerifySandbox(repoPath, worktreeBase, "run-42")
	if err != nil {
		t.Fatalf("NewVerifySandbox returned error: %v", err)
	}
	if sb == nil {
		t.Fatal("expected non-nil sandbox")
	}
	var _ sandbox.Sandbox = sb
}

func TestFakeVerifySandboxFactory_ReturnsConfiguredSandbox(t *testing.T) {
	t.Parallel()
	want := &fakeSandbox{workDir: "/tmp/fake-worktree"}
	f := &fakeVerifySandboxFactory{sandbox: want}
	got, err := f.NewVerifySandbox("/tmp/repo", "/tmp/wt-base", "run-1")
	if err != nil {
		t.Fatalf("NewVerifySandbox returned error: %v", err)
	}
	if got != want {
		t.Fatalf("expected the fake's configured sandbox back, got %v", got)
	}
}
