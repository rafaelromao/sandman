package sandbox

import (
	"os/exec"
	"strings"
	"testing"
)

// diffSubsetFixture creates a tiny git repo with two refs whose diff is
// stable. It returns the repo dir and the SHA of `b`. The repo's `a` ref
// is the empty initial commit; the `b` ref is the worktree ref with a
// single file change.
func diffSubsetFixture(t *testing.T) (repo string, a, b string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	sh := func(script string) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sh -c %q: %v: %s", script, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	run("commit", "-q", "--allow-empty", "-m", "init")
	a = strings.TrimSpace(run("rev-parse", "HEAD"))
	run("checkout", "-q", "-b", "feature")
	sh("echo alpha > a.txt")
	sh("echo beta > b.txt")
	run("add", "a.txt", "b.txt")
	run("commit", "-q", "-m", "feature change")
	b = strings.TrimSpace(run("rev-parse", "HEAD"))
	return repo, a, b
}

// TestDiffSubset_BuildsFileHunkSet pins the contract that DiffSubset turns
// `git diff a..b` into a flat set of (file, hunkHeader) entries — the
// shape T2's pre-filter compares against origin/main.
func TestDiffSubset_BuildsFileHunkSet(t *testing.T) {
	t.Parallel()
	repo, a, b := diffSubsetFixture(t)
	ds, err := DiffSubset(repo, a, b)
	if err != nil {
		t.Fatalf("DiffSubset: %v", err)
	}
	if len(ds.Files) == 0 {
		t.Fatalf("expected non-empty diff set, got 0 files")
	}
	names := map[string]bool{}
	for _, f := range ds.Files {
		names[f.Path] = true
	}
	if !names["a.txt"] || !names["b.txt"] {
		t.Fatalf("DiffSubset files = %v, want a.txt and b.txt", names)
	}
}

// TestDiffSubset_EmptyWhenRefsEqual pins the trivial case: when the
// two refs point at the same commit, the diff set is empty.
func TestDiffSubset_EmptyWhenRefsEqual(t *testing.T) {
	t.Parallel()
	repo, a, _ := diffSubsetFixture(t)
	ds, err := DiffSubset(repo, a, a)
	if err != nil {
		t.Fatalf("DiffSubset: %v", err)
	}
	if len(ds.Files) != 0 {
		t.Fatalf("expected empty diff set when refs equal, got %d files: %+v", len(ds.Files), ds.Files)
	}
}
