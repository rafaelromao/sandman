package batch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/prompt"
)

func TestReadTaskContent_ThenParseTask(t *testing.T) {
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

	doc := prompt.ParseTask(raw)
	if doc.SourcePrompt != ".sandman/task.md" {
		t.Fatalf("expected default SourcePrompt, got %q", doc.SourcePrompt)
	}
	if doc.Stage != "plan-approved" {
		t.Fatalf("expected Stage=plan-approved, got %q", doc.Stage)
	}
	if doc.LastSkill != "sandman-tdd" {
		t.Fatalf("expected LastSkill=sandman-tdd, got %q", doc.LastSkill)
	}
	if doc.LastSkillStatus != "complete" {
		t.Fatalf("expected LastSkillStatus=complete, got %q", doc.LastSkillStatus)
	}
	if !strings.Contains(doc.Body, "Initial implementation done.") {
		t.Fatalf("expected Body to contain task content, got %q", doc.Body)
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

func TestBatchParseTask_AllFourStages(t *testing.T) {
	for _, stage := range []string{"plan-approved", "implementation-committed", "pr-created", "pr-review-finished"} {
		content := "## Stage: " + stage + "\n## Last Skill: s\n## Last Skill Status: c\n\n## Completed\ndone."
		doc := prompt.ParseTask(content)
		if doc.Stage != stage {
			t.Fatalf("for stage %q: expected %q, got %q", stage, stage, doc.Stage)
		}
	}
}

func TestBatchParseTask_MissingFieldFallback(t *testing.T) {
	content := "## Completed\nSome work.\n\n## Next Step\nContinue."
	doc := prompt.ParseTask(content)
	if doc.Stage != "" {
		t.Fatalf("expected empty Stage, got %q", doc.Stage)
	}
	if doc.LastSkill != "" {
		t.Fatalf("expected empty LastSkill, got %q", doc.LastSkill)
	}
	if doc.LastSkillStatus != "" {
		t.Fatalf("expected empty LastSkillStatus, got %q", doc.LastSkillStatus)
	}
	if doc.SourcePrompt != ".sandman/task.md" {
		t.Fatalf("expected default SourcePrompt, got %q", doc.SourcePrompt)
	}
}
