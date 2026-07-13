package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureRule_IsNoOpWhenRuleAlreadyPresent(t *testing.T) {
	dir := t.TempDir()

	existing := []byte("node_modules/\n.vscode/\n.sandman/\nbuild/\n")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), existing, 0644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	gw := NewDefaultGitignoreRuleWriter()
	if err := gw.EnsureRule(dir, ".sandman/"); err != nil {
		t.Fatalf("EnsureRule returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}

	if string(got) != string(existing) {
		t.Fatalf("gitignore byte content changed\nwant:\n%s\ngot:\n%s", existing, got)
	}
}

func TestEnsureRule_CreatesFileWhenMissing(t *testing.T) {
	dir := t.TempDir()

	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("pre-condition: .gitignore must not exist, got stat err=%v", err)
	}

	gw := NewDefaultGitignoreRuleWriter()
	if err := gw.EnsureRule(dir, ".sandman/"); err != nil {
		t.Fatalf("EnsureRule returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}

	if string(got) != ".sandman/\n" {
		t.Fatalf("expected new file to contain only the rule line, got:\n%s", got)
	}
}

func TestEnsureRule_AppendsRuleWithoutDisturbingExistingContent(t *testing.T) {
	dir := t.TempDir()

	const existing = "node_modules/\n.vscode/\nbuild/\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	gw := NewDefaultGitignoreRuleWriter()
	if err := gw.EnsureRule(dir, ".sandman/"); err != nil {
		t.Fatalf("EnsureRule returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}

	want := existing + ".sandman/\n"
	if string(got) != want {
		t.Fatalf("gitignore unexpected\nwant:\n%s\ngot:\n%s", want, got)
	}

	for _, line := range []string{"node_modules/", ".vscode/", "build/"} {
		if !strings.Contains(string(got), line+"\n") {
			t.Fatalf("preexisting rule %q was disturbed or missing: %s", line, got)
		}
	}
}

func TestEnsureRule_AppendsRuleWhenExistingFileMissingTrailingNewline(t *testing.T) {
	dir := t.TempDir()

	const existing = "node_modules/\n.vscode/"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	gw := NewDefaultGitignoreRuleWriter()
	if err := gw.EnsureRule(dir, ".sandman/"); err != nil {
		t.Fatalf("EnsureRule returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}

	want := "node_modules/\n.vscode/\n.sandman/\n"
	if string(got) != want {
		t.Fatalf("gitignore unexpected\nwant:\n%s\ngot:\n%s", want, got)
	}
}
