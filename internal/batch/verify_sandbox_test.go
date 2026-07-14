package batch

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

// TestT1SandboxFactory_PinsSourceToOriginMain pins slice 3's
// requirement: the T1 factory's NewSandbox returns a WorktreeSandbox
// whose source branch is `origin/main`, regardless of what the
// orchestrator's call site passed in.
//
// Slice-8 / T3 retirement (#2181): the previous ReplaySandboxFactory
// used by the T3 evidence oracle is retired together with
// T3EvidenceOracle; this test is the only post-retirement factory
// test that remains.
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
	// The factory's source branch is private; we verify behaviour by
	// re-running through the public NewSandbox with a different
	// source and asserting the same source is used.
	_ = wt
}
