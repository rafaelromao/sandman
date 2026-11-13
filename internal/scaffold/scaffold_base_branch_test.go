package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

// A repository's base branch is recorded once, at init, so every later Run
// uses it without a --base-branch flag.

func scaffoldBaseBranch(t *testing.T, dir string, opts Options, git *fakeGitOps) string {
	t.Helper()
	opts.BuildTools = "generic"
	s := &Scaffolder{GitOps: git, Gitignore: &fakeGitignoreRuleWriter{}, HooksDir: t.TempDir()}
	if err := s.Scaffold(dir, opts, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg.Git.BaseBranch
}

func TestScaffold_RecordsExplicitBaseBranch(t *testing.T) {
	got := scaffoldBaseBranch(t, t.TempDir(), Options{BaseBranch: "develop"}, &fakeGitOps{isRepo: true, defaultBranch: "trunk"})
	if got != "develop" {
		t.Fatalf("git.base_branch = %q, want the explicit develop", got)
	}
}

func TestScaffold_DefaultsBaseBranchToRepositoryDefault(t *testing.T) {
	got := scaffoldBaseBranch(t, t.TempDir(), Options{}, &fakeGitOps{isRepo: true, defaultBranch: "trunk"})
	if got != "trunk" {
		t.Fatalf("git.base_branch = %q, want the repository default trunk", got)
	}
}

func TestScaffold_FallsBackToMainWithoutRepositoryDefault(t *testing.T) {
	got := scaffoldBaseBranch(t, t.TempDir(), Options{}, &fakeGitOps{isRepo: true})
	if got != "main" {
		t.Fatalf("git.base_branch = %q, want main", got)
	}
}

func writeExistingConfig(t *testing.T, dir string, cfg *config.Config) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0o755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	if err := config.Save(filepath.Join(dir, ".sandman", "config.yaml"), cfg); err != nil {
		t.Fatalf("save existing config: %v", err)
	}
}

func TestScaffold_PreservedConfigKeepsItsBaseBranch(t *testing.T) {
	dir := t.TempDir()
	writeExistingConfig(t, dir, &config.Config{ReviewCommand: "/oc review", Git: config.GitConfig{BaseBranch: "release"}})
	got := scaffoldBaseBranch(t, dir, Options{}, &fakeGitOps{isRepo: true, defaultBranch: "trunk"})
	if got != "release" {
		t.Fatalf("git.base_branch = %q, want the preserved release", got)
	}
}

func TestScaffold_ExplicitBaseBranchUpdatesPreservedConfig(t *testing.T) {
	dir := t.TempDir()
	writeExistingConfig(t, dir, &config.Config{ReviewCommand: "/oc review", Git: config.GitConfig{BaseBranch: "main"}})
	got := scaffoldBaseBranch(t, dir, Options{BaseBranch: "develop"}, &fakeGitOps{isRepo: true})
	if got != "develop" {
		t.Fatalf("git.base_branch = %q, want develop", got)
	}
	cfg, err := config.Load(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.ReviewCommand != "/oc review" {
		t.Fatalf("review_command = %q, want the preserved /oc review", cfg.ReviewCommand)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestDefaultGitOps_DefaultBranchReadsOriginHead(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	if got := NewDefaultGitOps().DefaultBranch(repo); got != "" {
		t.Fatalf("DefaultBranch without origin/HEAD = %q, want empty", got)
	}
	gitIn(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	if got := NewDefaultGitOps().DefaultBranch(repo); got != "develop" {
		t.Fatalf("DefaultBranch = %q, want develop from origin/HEAD", got)
	}
}
