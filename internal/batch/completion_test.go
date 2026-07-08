package batch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/prompt"
)

func TestReadTaskContent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestLogRetryMarker_FormatsByRetryIndexOverRetriesBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")

	if err := logRetryMarker(logPath, 1, 3); err != nil {
		t.Fatalf("logRetryMarker(1, 3): %v", err)
	}
	if err := logRetryMarker(logPath, 2, 3); err != nil {
		t.Fatalf("logRetryMarker(2, 3): %v", err)
	}
	if err := logRetryMarker(logPath, 3, 3); err != nil {
		t.Fatalf("logRetryMarker(3, 3): %v", err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	want := "--- retry 1/3 ---\n--- retry 2/3 ---\n--- retry 3/3 ---\n"
	if string(got) != want {
		t.Fatalf("log content = %q, want %q", string(got), want)
	}
}
