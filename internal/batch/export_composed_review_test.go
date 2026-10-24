package batch

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

// Test-only exports for the external composed managed-review proof
// (composed_review_flow_test.go, package batch_test). That proof must import
// the review daemon, which itself imports batch, so it cannot live in this
// package. These helpers expose existing in-package fakes and the documented
// WithRunSessionOpts seam without widening the production API.

// ComposedTestRunSessionOpts injects the worktree head observation through
// the documented WithRunSessionOpts seam.
func ComposedTestRunSessionOpts(currentHead func(workDir string) (string, error)) OrchestratorOpt {
	return WithRunSessionOpts(runSessionOptions{currentHead: currentHead})
}

// ComposedTestSandboxFactory creates directory-backed fake sandboxes under
// the configured worktree base.
type ComposedTestSandboxFactory = reviewWaitSchedulerSandboxFactory

// ComposedTestConfigStore returns a config.Store that always loads cfg.
func ComposedTestConfigStore(cfg *config.Config) config.Store {
	return &fakeConfigStore{config: cfg}
}

// ComposedTestInitGitRepo initializes dir as a git repository on main with
// a bare origin.
func ComposedTestInitGitRepo(t *testing.T, dir string) {
	t.Helper()
	initGitRepo(t, dir)
}
