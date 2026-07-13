package scaffold

import (
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/paths"
)

// fakeGitOps is a tiny spy GitOps that records calls so tests can assert
// that Scaffolder routes Untrack through the injected seam rather than the
// default implementation.
type fakeGitOps struct {
	isRepo        bool
	untrackCalls  int
	lastUntrackRoot  string
	lastUntrackPath  string
	untrackErr    error
}

func (f *fakeGitOps) IsRepo(repoRoot string) bool { return f.isRepo }

func (f *fakeGitOps) Untrack(repoRoot, path string) error {
	f.untrackCalls++
	f.lastUntrackRoot = repoRoot
	f.lastUntrackPath = path
	return f.untrackErr
}

// fakeGitignoreRuleWriter records calls without touching disk and lets tests
// assert that Scaffolder also routes gitignore edits through its seam.
type fakeGitignoreRuleWriter struct {
	ensureCalls int
	lastRepoRoot string
	lastRule string
	ensureErr error
}

func (f *fakeGitignoreRuleWriter) EnsureRule(repoRoot, rule string) error {
	f.ensureCalls++
	f.lastRepoRoot = repoRoot
	f.lastRule = rule
	return f.ensureErr
}

func TestScaffold_UntracksSandmanDirWhenInsideRepo(t *testing.T) {
	dir := t.TempDir()

	goSpy := &fakeGitOps{isRepo: true}
	giSpy := &fakeGitignoreRuleWriter{}

	s := &Scaffolder{
		Gitignore: giSpy,
		GitOps:    goSpy,
	}

	if err := s.Scaffold(dir, Options{BuildTools: "generic", Writer: discardWriter{}}, stubPrompter{}); err != nil {
		t.Fatalf("Scaffold returned error: %v", err)
	}

	wantPath := filepath.ToSlash(paths.NewLayout(nil, dir).SandmanDir)

	if goSpy.untrackCalls != 1 {
		t.Fatalf("expected GitOps.Untrack to be called once, got %d", goSpy.untrackCalls)
	}
	if goSpy.lastUntrackPath != wantPath {
		t.Fatalf("expected GitOps.Untrack path=%q, got %q", wantPath, goSpy.lastUntrackPath)
	}
	if goSpy.lastUntrackRoot != dir {
		t.Fatalf("expected GitOps.Untrack root=%q, got %q", dir, goSpy.lastUntrackRoot)
	}

	if giSpy.ensureCalls != 1 {
		t.Fatalf("expected Gitignore.EnsureRule to be called once, got %d", giSpy.ensureCalls)
	}
	if giSpy.lastRule != ".sandman/" {
		t.Fatalf("expected Gitignore.EnsureRule rule=\".sandman/\", got %q", giSpy.lastRule)
	}
}

func TestScaffold_SkipsUntrackWhenNotInsideRepo(t *testing.T) {
	dir := t.TempDir()

	goSpy := &fakeGitOps{isRepo: false}
	giSpy := &fakeGitignoreRuleWriter{}

	s := &Scaffolder{
		Gitignore: giSpy,
		GitOps:    goSpy,
	}

	if err := s.Scaffold(dir, Options{BuildTools: "generic", Writer: discardWriter{}}, stubPrompter{}); err != nil {
		t.Fatalf("Scaffold returned error: %v", err)
	}

	if goSpy.untrackCalls != 0 {
		t.Fatalf("expected GitOps.Untrack NOT to be called when not in a repo, got %d calls", goSpy.untrackCalls)
	}
	if giSpy.ensureCalls != 1 {
		t.Fatalf("expected Gitignore.EnsureRule to still run, got %d calls", giSpy.ensureCalls)
	}
}

// stubPrompter accepts any question with positive confirm / first option.
type stubPrompter struct{}

func (stubPrompter) Confirm(string) (bool, error)             { return true, nil }
func (stubPrompter) Select(string, []string) (string, error) { return "", nil }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }