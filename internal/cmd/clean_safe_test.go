package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestValidateOwnedPath_RejectsUnsafeCandidates(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "owned")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(inside, "escape")); err != nil {
		t.Fatal(err)
	}

	for name, candidate := range map[string]string{
		"empty":       "",
		"root":        string(filepath.Separator),
		"relative":    filepath.Join("owned", "batch"),
		"traversal":   filepath.Join(root, "owned", "..", "..", "outside"),
		"symlink":     filepath.Join(inside, "escape", "batch"),
		"trustedroot": inside,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateOwnedPath(candidate, inside); err == nil {
				t.Fatalf("validateOwnedPath(%q) accepted unsafe path", candidate)
			}
		})
	}

	if err := validateOwnedPath(filepath.Join(inside, "missing", "batch"), inside); err != nil {
		t.Fatalf("absent owned path rejected: %v", err)
	}
}

func TestValidateOwnedPath_AllowsAbsentTrustedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing", "owned")
	candidate := filepath.Join(root, "batch")
	if err := validateOwnedPath(candidate, root); err != nil {
		t.Fatalf("absent trusted root rejected: %v", err)
	}
}

func TestValidateOrphanPaths_RejectsOutsideRoot(t *testing.T) {
	repo := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, repo)
	if err := validateOrphanPaths([]string{filepath.Join(t.TempDir(), "orphan")}, layout); err == nil {
		t.Fatal("outside orphan path was accepted")
	}
}

func TestExecuteClean_RetainsFailedAndRemovesSuccessfulEntries(t *testing.T) {
	repo := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, repo)
	good := filepath.Join(layout.BatchesDir, "good")
	bad := filepath.Join(layout.BatchesDir, "bad")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBatchIndex(t, repo, []batchindex.Batch{
		{ID: "good", Path: good, Status: batchindex.StatusArchived},
		{ID: "bad", Path: bad, Status: batchindex.StatusArchived},
	})

	remover := &fakeCleanupRemover{failPath: bad}

	actions := collectCleanActions(&batchindex.Index{Batches: []batchindex.Batch{
		{ID: "good", Path: good, Status: batchindex.StatusArchived},
		{ID: "bad", Path: bad, Status: batchindex.StatusArchived},
	}}, batchindex.StatusArchived)
	// These synthetic actions deliberately bypass manifest planning; the executor
	// still validates and retains the same index entries on failure.
	for i := range actions {
		actions[i].Err = nil
	}
	outcomes, err := executeClean(actions, &fakeGitRunner{}, layout, remover)
	if err == nil || len(outcomes) != 2 {
		t.Fatalf("expected one action error and two outcomes, outcomes=%v err=%v", outcomes, err)
	}
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Resolve("good") != nil || idx.Resolve("bad") == nil {
		t.Fatalf("expected good removed and bad retained: %+v", idx.Batches)
	}
}

func TestExecuteClean_AbsentWorktreeStillDeletesManifestBranch(t *testing.T) {
	repo := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, repo)
	batchPath := filepath.Join(layout.BatchesDir, "batch")
	if err := os.MkdirAll(batchPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.WorktreeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBatchIndex(t, repo, []batchindex.Batch{{ID: "batch", Path: batchPath, Status: batchindex.StatusArchived}})
	branch := "2278-batch"
	gr := &fakeGitRunner{}
	actions := []cleanAction{{
		BatchID: "batch", BatchPath: batchPath, Worktree: filepath.Join(layout.WorktreeDir, "missing"),
		Branch: branch, Status: batchindex.StatusArchived,
	}}
	if _, err := executeClean(actions, gr, layout, &fakeCleanupRemover{}); err != nil {
		t.Fatal(err)
	}
	if len(gr.deleteBranchCalls) != 1 || gr.deleteBranchCalls[0] != branch {
		t.Fatalf("expected branch cleanup for absent worktree, calls=%v", gr.deleteBranchCalls)
	}
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Resolve("batch") != nil {
		t.Fatal("successful cleanup should remove index entry")
	}
}

func TestExecuteClean_BranchNotFoundIsSuccessful(t *testing.T) {
	repo := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, repo)
	batchPath := filepath.Join(layout.BatchesDir, "batch")
	if err := os.MkdirAll(batchPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBatchIndex(t, repo, []batchindex.Batch{{ID: "batch", Path: batchPath, Status: batchindex.StatusArchived}})
	actions := []cleanAction{{BatchID: "batch", BatchPath: batchPath, Branch: "sandman/batch", Status: batchindex.StatusArchived}}
	gr := &fakeGitRunner{deleteBranchErr: errors.New("fatal: branch 'sandman/batch' not found")}
	if _, err := executeClean(actions, gr, layout, &fakeCleanupRemover{}); err != nil {
		t.Fatalf("branch-not-found cleanup should succeed: %v", err)
	}
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Resolve("batch") != nil {
		t.Fatal("successful cleanup should remove index entry")
	}
}

func TestExecuteClean_BranchFailureRetainsEntry(t *testing.T) {
	repo := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, repo)
	batchPath := filepath.Join(layout.BatchesDir, "batch")
	if err := os.MkdirAll(batchPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBatchIndex(t, repo, []batchindex.Batch{{ID: "batch", Path: batchPath, Status: batchindex.StatusArchived}})
	gr := &fakeGitRunner{deleteBranchErr: errors.New("branch locked")}
	actions := []cleanAction{{BatchID: "batch", BatchPath: batchPath, Branch: "sandman/batch", Status: batchindex.StatusArchived}}
	if _, err := executeClean(actions, gr, layout, &fakeCleanupRemover{}); err == nil {
		t.Fatal("expected branch cleanup error")
	}
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Resolve("batch") == nil {
		t.Fatal("branch failure must retain index entry")
	}
}

func TestExecuteClean_UnavailablePlanDoesNotDeleteChangedEntry(t *testing.T) {
	for _, status := range []batchindex.Status{batchindex.StatusActive, batchindex.StatusArchived} {
		t.Run(string(status), func(t *testing.T) {
			repo := t.TempDir()
			layout := paths.NewLayout(&config.Config{}, repo)
			batchPath := filepath.Join(layout.BatchesDir, "batch")
			if err := os.MkdirAll(batchPath, 0o755); err != nil {
				t.Fatal(err)
			}
			writeBatchIndex(t, repo, []batchindex.Batch{{ID: "batch", Path: batchPath, Status: status}})

			remover := &fakeCleanupRemover{failPath: batchPath}
			actions := []cleanAction{{
				BatchID: "batch", BatchPath: batchPath, Status: batchindex.StatusUnavailable, IsUnavail: true,
			}}
			if outcomes, err := executeClean(actions, &fakeGitRunner{}, layout, remover); err != nil || len(outcomes) != 0 {
				t.Fatalf("changed entry should be skipped, outcomes=%v err=%v", outcomes, err)
			}
			idx, err := batchindex.Load(layout.BatchesIndexPath)
			if err != nil {
				t.Fatal(err)
			}
			if idx.Resolve("batch") == nil {
				t.Fatal("changed entry must remain in the index")
			}
		})
	}
}

func TestExecuteClean_RejectsUnsafeBranchBeforeGit(t *testing.T) {
	repo := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, repo)
	batchPath := filepath.Join(layout.BatchesDir, "batch")
	if err := os.MkdirAll(batchPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBatchIndex(t, repo, []batchindex.Batch{{ID: "batch", Path: batchPath, Status: batchindex.StatusArchived}})
	gr := &fakeGitRunner{}
	actions := []cleanAction{{BatchID: "batch", BatchPath: batchPath, Branch: "../main", Status: batchindex.StatusArchived}}
	if _, err := executeClean(actions, gr, layout, &fakeCleanupRemover{}); err == nil {
		t.Fatal("expected unsafe branch validation error")
	}
	if len(gr.deleteBranchCalls) != 0 {
		t.Fatalf("unowned branch should not invoke git, calls=%v", gr.deleteBranchCalls)
	}
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Resolve("batch") == nil {
		t.Fatal("branch validation failure must retain index entry")
	}
}

func TestValidateBranchName_AcceptsManifestBranches(t *testing.T) {
	for _, branch := range []string{
		"42-fix-bug",
		"operator/topic",
		"sandman/review-2381-12345",
	} {
		if err := validateBranchName(branch); err != nil {
			t.Errorf("validateBranchName(%q) = %v", branch, err)
		}
	}
}

func TestValidateBranchName_RejectsUnsafeBranch(t *testing.T) {
	for _, branch := range []string{"", "../main", "topic//branch", "topic..branch", "topic~branch"} {
		if err := validateBranchName(branch); err == nil {
			t.Errorf("validateBranchName(%q) unexpectedly accepted unsafe branch", branch)
		}
	}
}

func TestRealGitRunner_DeleteBranchDoesNotPruneSiblingRegistrations(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "commit", "--allow-empty", "-m", "init")

	worktreeBase := filepath.Join(repo, ".sandman", "worktrees")
	targetBranch := "42-target"
	siblingBranch := "43-sibling"
	targetPath := filepath.Join(worktreeBase, targetBranch)
	siblingPath := filepath.Join(worktreeBase, siblingBranch)
	runGit(t, repo, "worktree", "add", "-b", targetBranch, targetPath, "main")
	runGit(t, repo, "worktree", "add", "-b", siblingBranch, siblingPath, "main")

	siblingRegistration := strings.TrimSpace(runGit(t, siblingPath, "rev-parse", "--git-dir"))
	if err := os.WriteFile(filepath.Join(siblingRegistration, "gitdir"), []byte("/missing/container/path/.git\n"), 0o644); err != nil {
		t.Fatalf("make sibling registration appear prunable: %v", err)
	}
	if err := os.RemoveAll(targetPath); err != nil {
		t.Fatalf("remove target worktree directory: %v", err)
	}

	if err := newRealGitRunner(repo).deleteBranch(targetBranch, targetPath); err != nil {
		t.Fatalf("delete target branch: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repo, ".git", "worktrees", siblingBranch)); err != nil {
		t.Fatalf("sibling registration was pruned: %v", err)
	}
	if out, err := exec.Command("git", "-C", siblingPath, "status", "--short").CombinedOutput(); err != nil {
		t.Fatalf("sibling worktree became unusable: %v: %s", err, out)
	}
}

func TestCleanupOrphanedBatches_PartialFailureKeepsFailedIndexEntry(t *testing.T) {
	repo := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, repo)
	good := filepath.Join(layout.BatchesDir, "good")
	bad := filepath.Join(layout.BatchesDir, "bad")
	for _, path := range []string{good, bad} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "batch.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBatchIndex(t, repo, []batchindex.Batch{
		{ID: "good", Path: good}, {ID: "bad", Path: bad},
	})
	remover := &fakeCleanupRemover{failPath: bad}
	removed, err := cleanupOrphanedBatches(layout, &fakeEventLog{}, func(string) bool { return false }, remover)
	if err == nil || len(removed) != 1 || removed[0] != good {
		t.Fatalf("expected partial orphan cleanup, removed=%v err=%v", removed, err)
	}
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Resolve("good") != nil || idx.Resolve("bad") == nil {
		t.Fatalf("unexpected index after partial cleanup: %+v", idx.Batches)
	}
}

type fakeCleanupRemover struct {
	failPath string
}

func (f *fakeCleanupRemover) RemoveAll(path string) error {
	if path == f.failPath {
		return errors.New("permission denied")
	}
	return os.RemoveAll(path)
}
