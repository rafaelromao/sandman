package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
)

// The orchestrator resolves Request.RunDir, which is relative to the
// repository root the command resolved, against Request.RepoRoot. Without
// deps.RepoRoot the command uses the Git toplevel, which differs from the
// process directory when sandman runs from a repository subdirectory.
func TestRun_PassesTheResolvedRepositoryRootToTheBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "nested", "pkg")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	deps.RepoRoot = ""

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "Return only OK."})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, buf.String())
	}

	if !filepath.IsAbs(spy.req.RepoRoot) {
		t.Fatalf("Request.RepoRoot = %q, want the absolute repository root", spy.req.RepoRoot)
	}
	if !sameDir(t, spy.req.RepoRoot, root) {
		t.Fatalf("Request.RepoRoot = %q, want the Git toplevel %q", spy.req.RepoRoot, root)
	}
	if _, err := os.Stat(filepath.Join(spy.req.RepoRoot, spy.req.RunDir, "batch.json")); err != nil {
		t.Fatalf("Request.RunDir %q does not resolve under Request.RepoRoot: %v", spy.req.RunDir, err)
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		t.Fatalf("resolve %q, %q: %v, %v", a, b, errA, errB)
	}
	return ra == rb
}
