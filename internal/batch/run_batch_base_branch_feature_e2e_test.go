package batch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// runGitIn is a test helper that runs a single `git` command in dir
// and fails the test on error.
func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (in %s): %v: %s", strings.Join(args, " "), dir, err, out)
	}
}

// gitOutputIn runs a single `git` command in dir and returns its
// combined output; it fails the test on error.
func gitOutputIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// stageBaseBranchFeatureRepoInDir prepares `localDir` as a git repo
// whose `origin` is a bare remote that holds two diverged branches
// (`main` and `feature-x`). It returns the feature-x tip SHA so the
// caller can compare the on-disk worktree's HEAD against it.
//
// The local repo IS the dir the test will chdir into and the
// orchestrator will operate on; this matches the production
// invariant that the run command's cwd is the repo root.
func stageBaseBranchFeatureRepoInDir(t *testing.T, localDir string) (featureTip string) {
	t.Helper()

	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("mkdir localDir: %v", err)
	}

	bareDir := t.TempDir()
	runGitIn(t, bareDir, "init", "--bare")

	seedDir := t.TempDir()
	runGitIn(t, seedDir, "init")
	runGitIn(t, seedDir, "config", "user.email", "test@test.com")
	runGitIn(t, seedDir, "config", "user.name", "Test")
	runGitIn(t, seedDir, "checkout", "-b", "main")
	runGitIn(t, seedDir, "commit", "--allow-empty", "-m", "main init")
	runGitIn(t, seedDir, "remote", "add", "origin", bareDir)
	runGitIn(t, seedDir, "push", "-u", "origin", "main")
	runGitIn(t, seedDir, "checkout", "-b", "feature-x")
	runGitIn(t, seedDir, "commit", "--allow-empty", "-m", "feature commit")
	runGitIn(t, seedDir, "push", "-u", "origin", "feature-x")
	runGitIn(t, seedDir, "checkout", "main")
	runGitIn(t, seedDir, "commit", "--allow-empty", "-m", "main-only commit")
	runGitIn(t, seedDir, "push", "origin", "main")

	runGitIn(t, localDir, "clone", "-b", "main", bareDir, ".")
	runGitIn(t, localDir, "config", "user.email", "test@test.com")
	runGitIn(t, localDir, "config", "user.name", "Test")
	runGitIn(t, localDir, "fetch", "origin", "feature-x")
	runGitIn(t, localDir, "checkout", "-b", "feature-x", "origin/feature-x")
	runGitIn(t, localDir, "checkout", "main")

	featureTip = gitOutputIn(t, localDir, "rev-parse", "origin/feature-x")
	mainTip := gitOutputIn(t, localDir, "rev-parse", "origin/main")
	if featureTip == mainTip {
		t.Fatalf("test precondition: main and feature-x must differ, both = %q", featureTip)
	}
	return featureTip
}

// TestRunBatch_BaseBranchFeature_CutsWorktreeFromFeatureBranch pins
// the end-to-end contract for the `--base-branch` CLI flag: when a
// caller passes a non-main reference (e.g. `feature-x`), the
// orchestrator must sync that branch from origin and cut the
// per-row worktree from it — not from the config's `git.base_branch`
// default (`main`) and not from a hard-coded fallback. The test
// runs the real Orchestrator end-to-end against a hermetic
// `git init`/`git clone` fixture with a bare remote, a fake
// `opencode` agent, and asserts the on-disk worktree HEAD equals
// the feature-x tip and differs from the main tip.
//
// Gate: SANDMAN_E2E_GATES=base_branch_feature (or all).
func TestRunBatch_BaseBranchFeature_CutsWorktreeFromFeatureBranch(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBaseBranchFeature) {
		t.Skip("set SANDMAN_E2E_GATES=base_branch_feature (or all) to run base-branch feature e2e")
	}

	dir := testenv.MkdirShort(t, "sm-base-branch-")
	featureTip := stageBaseBranchFeatureRepoInDir(t, dir)
	t.Chdir(dir)

	fakeBinDir := filepath.Join(dir, "fake-bin")
	if err := os.MkdirAll(fakeBinDir, 0755); err != nil {
		t.Fatalf("create fake bin dir: %v", err)
	}
	fakeOpencode := filepath.Join(fakeBinDir, "opencode")
	if err := os.WriteFile(fakeOpencode, []byte("#!/bin/sh\ntouch agent-ran.txt\n"), 0755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	configData := `agent: opencode
worktree_dir: .sandman/worktrees
sandbox: worktree
`
	if err := os.WriteFile(configPath, []byte(configData), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Cut from feature branch", Body: "Body."},
		},
		prs: map[string]*github.PR{
			"sandman/42-cut-from-feature-branch": mergedPR("sandman/42-cut-from-feature-branch", ""),
		},
	}
	store := &config.FileStore{Path: configPath}
	engine := &prompt.Engine{}
	o := NewOrchestrator(client, engine, store, nil)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, BaseBranch: "feature-x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(result.Runs))
	}
	if result.Runs[0].Status != "success" {
		t.Errorf("expected success, got %s", result.Runs[0].Status)
	}

	wtPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-cut-from-feature-branch")
	wtHeadCmd := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD")
	wtHeadOut, err := wtHeadCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve worktree HEAD: %v: %s", err, wtHeadOut)
	}
	wtTip := strings.TrimSpace(string(wtHeadOut))
	if wtTip != featureTip {
		mainTip := gitOutputIn(t, dir, "rev-parse", "origin/main")
		t.Errorf("worktree HEAD = %s, want feature-x tip %s (main tip was %s)", wtTip, featureTip, mainTip)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "agent-ran.txt")); err != nil {
		t.Errorf("expected agent marker in feature-x worktree, got err=%v", err)
	}
}
