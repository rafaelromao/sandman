package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/paths"
)

// fakeGitOps is a tiny spy GitOps that records calls so tests can assert
// that Scaffolder routes Untrack through the injected seam rather than the
// default implementation.
type fakeGitOps struct {
	isRepo          bool
	untrackCalls    int
	lastUntrackRoot string
	lastUntrackPath string
	untrackErr      error
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
	ensureCalls  int
	lastRepoRoot string
	lastRule     string
	ensureErr    error
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

	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
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

	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("Scaffold returned error: %v", err)
	}

	if goSpy.untrackCalls != 0 {
		t.Fatalf("expected GitOps.Untrack NOT to be called when not in a repo, got %d calls", goSpy.untrackCalls)
	}
	if giSpy.ensureCalls != 1 {
		t.Fatalf("expected Gitignore.EnsureRule to still run, got %d calls", giSpy.ensureCalls)
	}
}

// TestScaffold_InstallsPreCommitHookThatBlocksSandmanPaths is the unit test
// for issue #2148 acceptance criteria 3 and 4: after sandman init,
// `git add -f .sandman/task.md && git commit` must be rejected by the
// installed pre-commit hook, even though -f bypasses the .gitignore rule.
func TestScaffold_InstallsPreCommitHookThatBlocksSandmanPaths(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".git", "hooks")

	s := &Scaffolder{
		Gitignore: &fakeGitignoreRuleWriter{},
		GitOps:    &fakeGitOps{isRepo: true},
		HooksDir:  hooksDir,
	}

	if err := s.Scaffold(tmp, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("Scaffold returned error: %v", err)
	}

	hookPath := filepath.Join(hooksDir, "pre-commit")
	contents, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("pre-commit hook must be installed at %q: %v", hookPath, err)
	}
	if !strings.Contains(string(contents), ".sandman/") {
		t.Fatalf("pre-commit hook must mention .sandman/ in its rejection check, got:\n%s", contents)
	}
	if info, err := os.Stat(hookPath); err != nil {
		t.Fatalf("stat hook: %v", err)
	} else if info.Mode()&0o111 == 0 {
		t.Fatalf("pre-commit hook must be executable, got mode %v", info.Mode())
	}
}

// TestScaffold_PreCommitHookExistsWarnsAndSkipsInstall pins the
// no-destructive behavior (M2 in the post-24h review): when a
// pre-commit hook already exists, installPreCommitHook returns nil
// (preserves the user's hook) AND emits a warning to Options.Writer so
// the operator is informed that AC3 may not be satisfied. Without the
// warning, AC3 (rejecting `git add -f .sandman/task.md`) is silently
// unmet when the user's existing hook does not include the sandman
// guard.
func TestScaffold_PreCommitHookExistsWarnsAndSkipsInstall(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Pre-existing hook that does NOT include the sandman guard.
	existingHook := "#!/usr/bin/env bash\necho 'user hook'\nexit 0\n"
	existingPath := filepath.Join(hooksDir, "pre-commit")
	if err := os.WriteFile(existingPath, []byte(existingHook), 0o755); err != nil {
		t.Fatalf("seed pre-commit: %v", err)
	}

	var warn bytes.Buffer
	s := &Scaffolder{
		Gitignore: &fakeGitignoreRuleWriter{},
		GitOps:    &fakeGitOps{isRepo: true},
		HooksDir:  hooksDir,
	}

	if err := s.Scaffold(tmp, Options{BuildTools: "generic", Writer: &warn}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("Scaffold returned error: %v", err)
	}

	// Existing hook content must be preserved verbatim.
	got, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatalf("read pre-commit: %v", err)
	}
	if string(got) != existingHook {
		t.Errorf("existing pre-commit hook was modified: got %q, want %q", string(got), existingHook)
	}

	// Operator must be warned so they know AC3 may be unmet.
	if !strings.Contains(warn.String(), "pre-commit hook already exists") {
		t.Errorf("expected warning about pre-commit conflict, got: %q", warn.String())
	}
	if !strings.Contains(warn.String(), hooksDir) {
		t.Errorf("warning should mention the hooks dir path %q, got: %q", hooksDir, warn.String())
	}
}

// stubPrompter and discardWriter were removed in favor of the
// package-local fakePrompter (scaffolder_test.go) and Options.Writer's
// built-in io.Discard default respectively.
