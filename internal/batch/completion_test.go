package batch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/prompt"
)

func TestReadTaskContent(t *testing.T) {
	dir := t.TempDir()
	taskPath := filepath.Join(dir, "task.md")
	content := `## Stage: plan-approved
## Last Skill: sandman-tdd
## Last Skill Status: complete
## Completed
Initial implementation done.

## Next Step
Continue the work.`
	if err := os.WriteFile(taskPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	raw, exists, err := ReadTaskContent(taskPath)
	if err != nil {
		t.Fatalf("ReadTaskContent: %v", err)
	}
	if !exists {
		t.Fatal("expected task doc to exist")
	}
	if !strings.Contains(raw, "Initial implementation done.") {
		t.Fatalf("expected content to contain task content, got %q", raw)
	}
}

func TestReadTaskContent_MissingFile_DefaultsToDefaultPrompt(t *testing.T) {
	dir := t.TempDir()
	taskPath := filepath.Join(dir, "nonexistent", "task.md")

	content, exists, err := ReadTaskContent(taskPath)
	if err != nil {
		t.Fatalf("ReadTaskContent: %v", err)
	}
	if exists {
		t.Fatal("expected exists=false for missing file")
	}
	if content != EmptyTaskTemplate {
		t.Fatalf("expected EmptyTaskTemplate to mirror DefaultPrompt, got %q", content[:min(80, len(content))])
	}
	if content != prompt.DefaultPrompt() {
		t.Fatalf("expected EmptyTaskTemplate to equal DefaultPrompt, got %q", content[:min(80, len(content))])
	}

	resume := prompt.ContinuationTaskPrompt(content)
	if !strings.Contains(resume, "# Task") {
		t.Fatalf("expected continuation prompt to start with '# Task', got:\n%s", firstLines(resume, 10))
	}
	if !strings.Contains(resume, "## Execution Checklist") {
		t.Fatalf("expected continuation prompt to preserve ## Execution Checklist, got:\n%s", firstLines(resume, 40))
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n... (truncated)"
}

func TestReadTaskContent_ReadError(t *testing.T) {
	orig := readFileFn
	readFileFn = func(string) ([]byte, error) {
		return nil, os.ErrPermission
	}
	t.Cleanup(func() { readFileFn = orig })

	_, _, err := ReadTaskContent("/nonexistent/task.md")
	if err == nil {
		t.Fatal("expected error for unreadable path, got nil")
	}
}
