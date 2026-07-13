package batch

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

type VerifySandboxFactory interface {
	NewVerifySandbox(repoPath, worktreeBase, runID string) (sandbox.Sandbox, error)
}

type realVerifySandboxFactory struct{}

func (f *realVerifySandboxFactory) NewVerifySandbox(repoPath, worktreeBase, runID string) (sandbox.Sandbox, error) {
	if repoPath == "" || worktreeBase == "" || runID == "" {
		return nil, fmt.Errorf("verify sandbox factory: repoPath, worktreeBase, and runID are required")
	}
	return sandbox.NewWorktreeSandbox(repoPath, worktreeBase, "sandman/verify-"+runID, "origin/main"), nil
}

type fakeVerifySandboxFactory struct {
	sandbox sandbox.Sandbox
}

func (f *fakeVerifySandboxFactory) NewVerifySandbox(repoPath, worktreeBase, runID string) (sandbox.Sandbox, error) {
	if f.sandbox == nil {
		return nil, fmt.Errorf("fakeVerifySandboxFactory: no sandbox configured")
	}
	return f.sandbox, nil
}
