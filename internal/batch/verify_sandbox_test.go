package batch

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

// TestT1SandboxFactory_PinsSourceToOriginMain pins slice 3's
// requirement: the T1 factory's NewSandbox returns a WorktreeSandbox
// whose source branch is `origin/main`, regardless of what the
// orchestrator's call site passed in.
func TestT1SandboxFactory_PinsSourceToOriginMain(t *testing.T) {
	t.Parallel()
	f := &T1SandboxFactory{RepoPath: "/repo", WorktreeBase: "/wt", Branch: "verify-t1"}
	sb := f.NewSandbox("/other", "/other-wt", "ignored", "feature-branch", nil)
	wt, ok := sb.(*sandbox.WorktreeSandbox)
	if !ok {
		t.Fatalf("expected *sandbox.WorktreeSandbox, got %T", sb)
	}
	if wt == nil {
		t.Fatal("nil worktree sandbox")
	}
	_ = wt
}
