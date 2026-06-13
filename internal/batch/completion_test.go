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

func TestReadTaskContent_MissingFile_ThenBuildTaskPrompt(t *testing.T) {
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
		t.Fatalf("expected EmptyTaskTemplate, got %q", content)
	}

	doc := prompt.ParseTask(content)
	result := prompt.BuildTaskPrompt(doc)

	if !strings.Contains(result, "## Prior Context") {
		t.Fatalf("expected ## Prior Context in resume prompt, got:\n%s", result)
	}
	if !strings.Contains(result, "## Source Prompt") {
		t.Fatalf("expected ## Source Prompt in resume prompt, got:\n%s", result)
	}
	if !strings.Contains(result, "## Update Task Context") {
		t.Fatalf("expected ## Update Task Context in task prompt, got:\n%s", result)
	}
	if !strings.Contains(result, "Continue the work.") {
		t.Fatalf("expected 'Continue the work.' in resume prompt, got:\n%s", result)
	}
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
