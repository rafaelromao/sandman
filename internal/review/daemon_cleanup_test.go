package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// initReviewTestGitRepo creates a git repo at dir with one commit on main so
// `git worktree add` / `git branch` work. Mirrors initGitRepo in
// internal/batch/orchestrator_test.go; duplicated here to keep the review
// package self-contained. t.Skip if git is unavailable.
func initReviewTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	// Remove any pre-existing .git (defensive: t.TempDir() returns an
	// empty dir, but a leftover .git from a previous test run can
	// confuse `git init` in the rare case Go reuses a temp path).
	_ = os.RemoveAll(filepath.Join(dir, ".git"))
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable (%v: %s)", err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Skipf("git init succeeded but no .git directory: %v", err)
	}
	// Ensure HEAD is on main (init.defaultBranch varies between git
	// versions; older versions needed an explicit checkout -b main).
	checkout := exec.Command("git", "checkout", "-B", "main")
	checkout.Dir = dir
	_ = checkout.Run()
}

// stageReviewWorktree creates a worktree + branch in the test repo at
// worktreeDir/<branch>. Mirrors the layout the real WorktreeSandbox
// creates, so the cleanup function under test exercises real git state.
func stageReviewWorktree(t *testing.T, worktreeDir, branch string) {
	t.Helper()
	wtPath := filepath.Join(worktreeDir, branch)
	cmd := exec.Command("git", "worktree", "add", "-b", branch, wtPath, "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create worktree %s: %v: %s", wtPath, err, out)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree should exist: %v", err)
	}
}

// gitWorktreeHasBranch reports whether the test repo's `git worktree list`
// still references worktreeDir/<branch>.
func gitWorktreeHasBranch(t *testing.T, worktreeDir, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v: %s", err, out)
	}
	return strings.Contains(string(out), filepath.Join(worktreeDir, branch))
}

// gitBranchExists reports whether the test repo's `git branch --list`
// still contains the given branch.
func gitBranchExists(t *testing.T, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "branch", "--list", branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out)) != ""
}

func TestClearReviewArtifacts_HappyPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initReviewTestGitRepo(t, dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir worktreeDir: %v", err)
	}
	branch := "sandman/review-42-c1"
	stageReviewWorktree(t, worktreeDir, branch)

	if !gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Fatalf("precondition: worktree list should contain %s", branch)
	}
	if !gitBranchExists(t, branch) {
		t.Fatalf("precondition: branch list should contain %s", branch)
	}

	var logBuf strings.Builder
	ClearReviewArtifacts(branch, worktreeDir, &logBuf)

	if gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Errorf("expected worktree to be removed after ClearReviewArtifacts")
	}
	if gitBranchExists(t, branch) {
		t.Errorf("expected branch %s to be removed after ClearReviewArtifacts", branch)
	}
}

func TestClearReviewArtifacts_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initReviewTestGitRepo(t, dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir worktreeDir: %v", err)
	}

	var logBuf strings.Builder
	ClearReviewArtifacts("sandman/review-999-nonexistent", worktreeDir, &logBuf)

	if got := logBuf.String(); strings.Contains(got, "error") {
		t.Errorf("idempotent cleanup should not log errors, got: %s", got)
	}
}

func TestClearReviewArtifacts_EmptyBranchIsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initReviewTestGitRepo(t, dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	var logBuf strings.Builder
	ClearReviewArtifacts("", worktreeDir, &logBuf)
	if got := logBuf.String(); got != "" {
		t.Errorf("empty branch should produce no log output, got: %s", got)
	}
}

func TestClearReviewArtifacts_EmptyWorktreeDirIsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initReviewTestGitRepo(t, dir)

	var logBuf strings.Builder
	ClearReviewArtifacts("sandman/review-42-c1", "", &logBuf)
	if got := logBuf.String(); got != "" {
		t.Errorf("empty worktreeDir should produce no log output, got: %s", got)
	}
}

func TestClearReviewArtifacts_OnlyTouchesTargetBranch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initReviewTestGitRepo(t, dir)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir worktreeDir: %v", err)
	}

	target := "sandman/review-42-c1"
	other := "sandman/review-99-other"
	stageReviewWorktree(t, worktreeDir, target)
	stageReviewWorktree(t, worktreeDir, other)

	var logBuf strings.Builder
	ClearReviewArtifacts(target, worktreeDir, &logBuf)

	if gitBranchExists(t, target) {
		t.Errorf("expected target branch %s to be removed", target)
	}
	if !gitBranchExists(t, other) {
		t.Errorf("expected unrelated branch %s to be preserved", other)
	}
	if !gitWorktreeHasBranch(t, worktreeDir, other) {
		t.Errorf("expected unrelated worktree for %s to be preserved", other)
	}
}

// newReviewLaunchTestDaemon constructs a daemon whose cwd is a fresh
// git-initialized temp dir. It returns the daemon, the daemon's base
// dir (which is also the cwd), and the worktree subdir ready for the
// test to stage review worktrees in.
func newReviewLaunchTestDaemon(t *testing.T, gh GitHubClient, runner BatchRunner, cfg *config.Config) (*Daemon, string, string) {
	t.Helper()
	d, _, dir := newDaemonForTest(t, gh, runner, cfg)
	d.Config = cfg
	initReviewTestGitRepo(t, dir)
	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	d.Config.WorktreeDir = worktreeDir
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir worktreeDir: %v", err)
	}
	return d, dir, worktreeDir
}

func newReviewLaunchTestConfig() *config.Config {
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}
	return cfg
}

// TestLaunchReview_CleansUpWorktreeAndBranchOnSuccess is the end-to-end
// happy-path test for issue #1494: after launchReview returns successfully,
// the review worktree and branch must be gone from git, while the batch
// metadata directory under .sandman/ survives.
func TestLaunchReview_CleansUpWorktreeAndBranchOnSuccess(t *testing.T) {
	now := time.Now().UTC()
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		prFetch: map[int]*github.PR{
			42: {Number: 42, Title: "T", Body: "B"},
		},
	}

	var capturedRunDir string
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		capturedRunDir = req.RunDir
		return &batch.Result{}, nil
	})

	d, _, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.Clock = func() time.Time { return now }
	branch := reviewBranchName(42, "c1")
	stageReviewWorktree(t, worktreeDir, branch)

	reviewRunFolder, perRowRunID, rs, _, prepErr := d.prepareReviewRun(42, "c1")
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}

	if err := d.launchReview(context.Background(), 42, "", "c1", "", "", reviewRunFolder, perRowRunID, rs); err != nil {
		t.Fatalf("launchReview: %v", err)
	}

	if gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Errorf("expected review worktree to be removed after launchReview success")
	}
	if gitBranchExists(t, branch) {
		t.Errorf("expected review branch to be removed after launchReview success")
	}
	if _, err := os.Stat(capturedRunDir); err != nil {
		t.Errorf("batch directory should still exist after launchReview, but stat returned: %v", err)
	}

	// Acceptance criterion #1675: the batches index entry id must equal
	// the per-row RunID the orchestrator will emit for this review.
	// daemon.BatchesIndexPath(baseDir) = <baseDir>/batches.json (not
	// <baseDir>/.sandman/batches.json — the batches.json file lives at
	// the sandman root, not under .sandman/).
	idxPath := filepath.Join(worktreeDir, "..", "..", "batches.json")
	idx, err := batchindex.Load(idxPath)
	if err != nil {
		t.Fatalf("load batches index from %s: %v", idxPath, err)
	}
	if len(idx.Entries) != 1 {
		t.Fatalf("expected exactly 1 batch index entry, got %d (entries=%v)", len(idx.Entries), idx.Entries)
	}
	if got := idx.Entries[0].ID; got != perRowRunID {
		t.Errorf("entry ID = %q, want perRowRunID %q", got, perRowRunID)
	}
}

// TestPrepareReviewRun_LinkedIssueRegistersPerRowRunID verifies that
// when the review daemon prepares a run for a PR with a linked issue,
// the batches index entry id equals the per-row RunID
// `<sid>-<ts>-<issue>-PR<pr>` (subject derived from the PR body's
// "Fixes #N" keyword). Mirrors #1675 §review with linked issue.
func TestPrepareReviewRun_LinkedIssueRegistersPerRowRunID(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		prFetch: map[int]*github.PR{
			42: {Number: 42, Title: "T", Body: "Fixes #99"},
		},
	}
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		return &batch.Result{}, nil
	})

	d, _, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.Clock = func() time.Time { return now }
	branch := reviewBranchName(42, "c1")
	stageReviewWorktree(t, worktreeDir, branch)

	reviewRunFolder, perRowRunID, rs, _, prepErr := d.prepareReviewRun(42, "c1")
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}
	if err := d.launchReview(context.Background(), 42, "", "c1", "", "", reviewRunFolder, perRowRunID, rs); err != nil {
		t.Fatalf("launchReview: %v", err)
	}

	// Per-row RunID must include the linked issue (subject = `99-PR42`).
	if !strings.Contains(perRowRunID, "-99-PR42") {
		t.Fatalf("expected perRowRunID to contain -99-PR42, got %q", perRowRunID)
	}

	idx, err := batchindex.Load(filepath.Join(worktreeDir, "..", "..", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	if len(idx.Entries) != 1 {
		t.Fatalf("expected exactly 1 batch index entry, got %d (entries=%v)", len(idx.Entries), idx.Entries)
	}
	if got := idx.Entries[0].ID; got != perRowRunID {
		t.Errorf("entry ID = %q, want perRowRunID %q", got, perRowRunID)
	}
}

// TestLaunchReview_CleansUpOnRunBatchFailure verifies that when RunBatch
// returns an error, launchReview still cleans up the worktree and branch.
func TestLaunchReview_CleansUpOnRunBatchFailure(t *testing.T) {
	now := time.Now().UTC()
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		prFetch: map[int]*github.PR{
			42: {Number: 42, Title: "T", Body: "B"},
		},
	}

	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		return nil, errors.New("batch exploded")
	})

	d, _, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.Clock = func() time.Time { return now }
	branch := reviewBranchName(42, "c1")
	stageReviewWorktree(t, worktreeDir, branch)

	reviewRunFolder, perRowRunID, rs, _, prepErr := d.prepareReviewRun(42, "c1")
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}

	err := d.launchReview(context.Background(), 42, "", "c1", "", "", reviewRunFolder, perRowRunID, rs)
	if err == nil {
		t.Fatal("expected launchReview to return the RunBatch error")
	}

	if gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Errorf("expected review worktree to be removed after launchReview error")
	}
	if gitBranchExists(t, branch) {
		t.Errorf("expected review branch to be removed after launchReview error")
	}
}

// TestLaunchReview_CleansUpOnContextCancellation verifies that when the
// context passed to RunBatch is cancelled, launchReview still cleans up
// the worktree and branch.
func TestLaunchReview_CleansUpOnContextCancellation(t *testing.T) {
	now := time.Now().UTC()
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		prFetch: map[int]*github.PR{
			42: {Number: 42, Title: "T", Body: "B"},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &batch.Result{}, nil
	})

	d, _, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.Clock = func() time.Time { return now }
	branch := reviewBranchName(42, "c1")
	stageReviewWorktree(t, worktreeDir, branch)

	reviewRunFolder, perRowRunID, rs, _, prepErr := d.prepareReviewRun(42, "c1")
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}

	err := d.launchReview(ctx, 42, "", "c1", "", "", reviewRunFolder, perRowRunID, rs)
	if err == nil {
		t.Fatal("expected launchReview to return a context.Canceled error")
	}

	if gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Errorf("expected review worktree to be removed after ctx cancel")
	}
	if gitBranchExists(t, branch) {
		t.Errorf("expected review branch to be removed after ctx cancel")
	}
}
