package prompt

import (
	"strings"
	"testing"
)

func TestParseTask_AllHeadings(t *testing.T) {
	content := `## Stage: plan-approved
## Last Skill: sandman-tdd
## Last Skill Status: complete
## Completed
Initial implementation done.

## Pending
Nothing.

## Blockers
None.

## Key Decisions
Use TDD.

## Next Step
Continue the work.`

	doc := ParseTask(content)
	if doc.Stage != "plan-approved" {
		t.Fatalf("expected Stage=plan-approved, got %q", doc.Stage)
	}
	if doc.LastSkill != "sandman-tdd" {
		t.Fatalf("expected LastSkill=sandman-tdd, got %q", doc.LastSkill)
	}
	if doc.LastSkillStatus != "complete" {
		t.Fatalf("expected LastSkillStatus=complete, got %q", doc.LastSkillStatus)
	}
	if !strings.Contains(doc.Body, "## Completed") {
		t.Fatalf("expected Body to contain Completed section, got %q", doc.Body)
	}
	if !strings.Contains(doc.Body, "Initial implementation done.") {
		t.Fatalf("expected Body to contain initial implementation, got %q", doc.Body)
	}
	if !strings.Contains(doc.Body, "## Next Step") {
		t.Fatalf("expected Body to contain Next Step, got %q", doc.Body)
	}
}

func TestParseTask_AllFourStages(t *testing.T) {
	for _, stage := range []string{"plan-approved", "implementation-committed", "pr-created", "pr-review-finished"} {
		content := "## Stage: " + stage + "\n## Last Skill: s\n## Last Skill Status: c\n\n## Completed\ndone."
		doc := ParseTask(content)
		if doc.Stage != stage {
			t.Fatalf("for stage %q: expected %q, got %q", stage, stage, doc.Stage)
		}
	}
}

func TestParseTask_MissingHeadings(t *testing.T) {
	content := `## Completed
Some work.

## Next Step
Continue.`

	doc := ParseTask(content)
	if doc.Stage != "" {
		t.Fatalf("expected empty Stage, got %q", doc.Stage)
	}
	if doc.LastSkill != "" {
		t.Fatalf("expected empty LastSkill, got %q", doc.LastSkill)
	}
	if doc.LastSkillStatus != "" {
		t.Fatalf("expected empty LastSkillStatus, got %q", doc.LastSkillStatus)
	}
	if !strings.Contains(doc.Body, "## Completed") {
		t.Fatalf("expected Body to contain Completed")
	}
}

func TestParseTask_EmptyContent(t *testing.T) {
	doc := ParseTask("")
	if doc.Stage != "" || doc.LastSkill != "" || doc.LastSkillStatus != "" || doc.Body != "" {
		t.Fatalf("expected all zero values, got %+v", doc)
	}
}

func TestParseTask_LastSkillStatusWithContext(t *testing.T) {
	content := `## Stage: plan-approved
## Last Skill: sandman-tdd
## Last Skill Status: incomplete — tests failed
## Completed
Partial.`

	doc := ParseTask(content)
	if doc.LastSkillStatus != "incomplete — tests failed" {
		t.Fatalf("expected LastSkillStatus with context, got %q", doc.LastSkillStatus)
	}
}

func TestParseTask_SourcePrompt(t *testing.T) {
	content := `## Stage: plan-approved
## Source Prompt: .sandman/custom-prompt.md
## Last Skill: sandman-tdd
## Last Skill Status: complete
## Completed
Done.`

	doc := ParseTask(content)
	if doc.SourcePrompt != ".sandman/custom-prompt.md" {
		t.Fatalf("expected SourcePrompt=.sandman/custom-prompt.md, got %q", doc.SourcePrompt)
	}
}

func TestParseTask_SourcePromptDefault(t *testing.T) {
	content := `## Stage: plan-approved
## Completed
Done.`

	doc := ParseTask(content)
	if doc.SourcePrompt != ".sandman/task.md" {
		t.Fatalf("expected default SourcePrompt=.sandman/task.md, got %q", doc.SourcePrompt)
	}
}

func TestBuildTaskPrompt_SourcePromptFormat(t *testing.T) {
	doc := TaskDoc{
		SourcePrompt: ".sandman/my-prompt.md",
		Body:         "## Completed\nDone.",
	}

	result := BuildTaskPrompt(doc)

	if !strings.Contains(result, "## Source Prompt: .sandman/my-prompt.md") {
		t.Fatalf("expected Source Prompt line with colon and path, got:\n%s", result)
	}
	if !strings.Contains(result, "Re-read `.sandman/my-prompt.md` before continuing") {
		t.Fatalf("expected explicit re-read instruction, got:\n%s", result)
	}
}

func TestParseTask_StageLineWithExtraSpaces(t *testing.T) {
	content := "## Stage:   implementation-committed   \n## Last Skill:  sandman-tdd\n## Last Skill Status: complete\n\n## Completed\ndone."
	doc := ParseTask(content)
	if doc.Stage != "implementation-committed" {
		t.Fatalf("expected Stage=implementation-committed, got %q", doc.Stage)
	}
}

func TestBuildTaskPrompt_HasAllSections(t *testing.T) {
	doc := TaskDoc{
		Stage:           "plan-approved",
		LastSkill:       "sandman-tdd",
		LastSkillStatus: "complete",
		Body:            "## Completed\nDone.\n\n## Next Step\nPush PR.",
	}

	result := BuildTaskPrompt(doc)

	if !strings.Contains(result, "## Prior Context") {
		t.Fatalf("expected ## Prior Context section, got:\n%s", result)
	}
	if !strings.Contains(result, "## Source Prompt") {
		t.Fatalf("expected ## Source Prompt section, got:\n%s", result)
	}
	if !strings.Contains(result, "## New Instruction") {
		t.Fatalf("expected ## New Instruction section, got:\n%s", result)
	}
	if !strings.Contains(result, "## Update Task Context") {
		t.Fatalf("expected ## Update Task Context section, got:\n%s", result)
	}
}

func TestBuildTaskPrompt_SourcePromptReferencesFile(t *testing.T) {
	doc := TaskDoc{Body: "## Completed\nDone."}
	result := BuildTaskPrompt(doc)

	if !strings.Contains(result, ".sandman/task.md") {
		t.Fatalf("expected Source Prompt to reference task.md, got:\n%s", result)
	}
}

func TestBuildTaskPrompt_NewInstruction(t *testing.T) {
	doc := TaskDoc{
		Stage:           "implementation-committed",
		LastSkill:       "sandman-tdd",
		LastSkillStatus: "complete",
		Body:            "## Completed\nDone.\n\n## Next Step\nload sandman-merge, then push and create PR.",
	}

	result := BuildTaskPrompt(doc)

	if !strings.Contains(result, "## Stage: implementation-committed") {
		t.Fatalf("expected ## Stage in New Instruction, got:\n%s", result)
	}
	if !strings.Contains(result, "## Last Skill: sandman-tdd") {
		t.Fatalf("expected ## Last Skill in New Instruction, got:\n%s", result)
	}
	if !strings.Contains(result, "## Last Skill Status: complete") {
		t.Fatalf("expected ## Last Skill Status in New Instruction, got:\n%s", result)
	}
	if !strings.Contains(result, "Next: load sandman-merge") {
		t.Fatalf("expected Next in New Instruction, got:\n%s", result)
	}
}

func TestBuildTaskPrompt_NewInstructionEmptyMetadata(t *testing.T) {
	doc := TaskDoc{Body: "## Completed\nDone."}
	result := BuildTaskPrompt(doc)

	if strings.Contains(result, "## New Instruction") {
		t.Fatalf("expected no New Instruction when metadata empty, got:\n%s", result)
	}
}

func TestBuildTaskPrompt_UpdateTaskContext(t *testing.T) {
	doc := TaskDoc{
		Stage:           "plan-approved",
		LastSkill:       "sandman-tdd",
		LastSkillStatus: "complete",
		Body:            "## Completed\nDone.",
	}

	result := BuildTaskPrompt(doc)

	if !strings.Contains(result, "## Stage:") {
		t.Fatalf("expected ## Stage: in Update Task Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Source Prompt:") {
		t.Fatalf("expected ## Source Prompt: in Update Task Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Last Skill:") {
		t.Fatalf("expected ## Last Skill: in Update Task Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Last Skill Status: complete") {
		t.Fatalf("expected ## Last Skill Status: complete (no em-dash) in Update Task Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Completed") {
		t.Fatalf("expected ## Completed in Update Task Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Pending") {
		t.Fatalf("expected ## Pending in Update Task Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Blockers") {
		t.Fatalf("expected ## Blockers in Update Task Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Key Decisions") {
		t.Fatalf("expected ## Key Decisions in Update Task Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Next Step") {
		t.Fatalf("expected ## Next Step in Update Task Context, got:\n%s", result)
	}
}

func TestBuildTaskPrompt_UpdateTaskContextTailIncludesTaskMd(t *testing.T) {
	doc := TaskDoc{Body: "## Completed\nDone."}
	result := BuildTaskPrompt(doc)

	if !strings.Contains(result, ".sandman/task.md") {
		t.Fatalf("expected Update Task Context to reference task.md, got:\n%s", result)
	}
}

func TestParseTask_StageAfterBody(t *testing.T) {
	content := `## Completed
Some work.

## Stage: plan-approved
## Next Step
Continue.`

	doc := ParseTask(content)
	if doc.Stage != "" {
		t.Fatalf("expected empty Stage (after first body heading), got %q", doc.Stage)
	}
}

func TestParseTask_SourcePromptLast(t *testing.T) {
	content := `## Completed
Done.

## Source Prompt: .sandman/custom.md`

	doc := ParseTask(content)
	if doc.SourcePrompt != ".sandman/task.md" {
		t.Fatalf("expected default SourcePrompt (after first body heading), got %q", doc.SourcePrompt)
	}
}

func TestExtractNextStep_MultiLine(t *testing.T) {
	body := "## Completed\nDone.\n\n## Next Step\nload sandman-merge\npush PR\ncreate release."
	next := extractNextStep(body)
	expected := "load sandman-merge\npush PR\ncreate release."
	if next != expected {
		t.Fatalf("expected exact multi-line next step:\n%q\n\ngot:\n%q", expected, next)
	}
}

func TestBuildTaskPrompt_WithStageOnly(t *testing.T) {
	doc := TaskDoc{
		Stage: "pr-created",
		Body:  "## Completed\nDone.\n\n## Next Step\nMerge PR.",
	}

	result := BuildTaskPrompt(doc)

	if !strings.Contains(result, "## New Instruction") {
		t.Fatalf("expected New Instruction with stage, got:\n%s", result)
	}
	if !strings.Contains(result, "## Stage: pr-created") {
		t.Fatalf("expected ## Stage in New Instruction, got:\n%s", result)
	}
}

func TestBuildTaskPrompt_EmptyTemplateSuppressesMetadataInUHC(t *testing.T) {
	doc := TaskDoc{Body: "## Completed\nDone.\n\n## Next Step\nContinue."}
	result := BuildTaskPrompt(doc)

	if strings.Contains(result, "## Stage:") {
		t.Fatalf("expected no ## Stage: in UHC for empty metadata, got:\n%s", result)
	}
	if strings.Contains(result, "## Last Skill:") {
		t.Fatalf("expected no ## Last Skill: in UHC for empty metadata, got:\n%s", result)
	}
	if strings.Contains(result, "## Last Skill Status:") {
		t.Fatalf("expected no ## Last Skill Status: in UHC for empty metadata, got:\n%s", result)
	}
	if !strings.Contains(result, "## Update Task Context") {
		t.Fatalf("expected Update Task Context section, got:\n%s", result)
	}
	if !strings.Contains(result, "## Completed") {
		t.Fatalf("expected ## Completed in UHC, got:\n%s", result)
	}
}

func TestBuildTaskPrompt_SourcePromptDoesNotInlineContent(t *testing.T) {
	doc := TaskDoc{Body: "some inline content"}
	result := BuildTaskPrompt(doc)

	if strings.Contains(result, "Implement GitHub issue") {
		t.Fatalf("expected Source Prompt to not inline rendered prompt content, got:\n%s", result)
	}
}

func TestParseTask_MissingStage(t *testing.T) {
	content := "## Last Skill: sandman-tdd\n## Last Skill Status: complete\n\n## Completed\ndone."
	doc := ParseTask(content)
	if doc.Stage != "" {
		t.Fatalf("expected empty Stage, got %q", doc.Stage)
	}
	if doc.LastSkill != "sandman-tdd" {
		t.Fatalf("expected LastSkill=sandman-tdd, got %q", doc.LastSkill)
	}
	if doc.LastSkillStatus != "complete" {
		t.Fatalf("expected LastSkillStatus=complete, got %q", doc.LastSkillStatus)
	}
}

func TestParseTask_MissingLastSkill(t *testing.T) {
	content := "## Stage: plan-approved\n## Last Skill Status: complete\n\n## Completed\ndone."
	doc := ParseTask(content)
	if doc.Stage != "plan-approved" {
		t.Fatalf("expected Stage=plan-approved, got %q", doc.Stage)
	}
	if doc.LastSkill != "" {
		t.Fatalf("expected empty LastSkill, got %q", doc.LastSkill)
	}
	if doc.LastSkillStatus != "complete" {
		t.Fatalf("expected LastSkillStatus=complete, got %q", doc.LastSkillStatus)
	}
}

func TestParseTask_MissingLastSkillStatus(t *testing.T) {
	content := "## Stage: plan-approved\n## Last Skill: sandman-tdd\n\n## Completed\ndone."
	doc := ParseTask(content)
	if doc.Stage != "plan-approved" {
		t.Fatalf("expected Stage=plan-approved, got %q", doc.Stage)
	}
	if doc.LastSkill != "sandman-tdd" {
		t.Fatalf("expected LastSkill=sandman-tdd, got %q", doc.LastSkill)
	}
	if doc.LastSkillStatus != "" {
		t.Fatalf("expected empty LastSkillStatus, got %q", doc.LastSkillStatus)
	}
}

func TestParseTask_LastSkillStatusHardBlocker(t *testing.T) {
	content := "## Stage: pr-review-finished\n## Last Skill: sandman-pr-review\n## Last Skill Status: incomplete — hard blocker from reviewer\n## Completed\nReview issues found."
	doc := ParseTask(content)
	if doc.Stage != "pr-review-finished" {
		t.Fatalf("expected Stage=pr-review-finished, got %q", doc.Stage)
	}
	if doc.LastSkill != "sandman-pr-review" {
		t.Fatalf("expected LastSkill=sandman-pr-review, got %q", doc.LastSkill)
	}
	if doc.LastSkillStatus != "incomplete — hard blocker from reviewer" {
		t.Fatalf("expected LastSkillStatus with hard blocker context, got %q", doc.LastSkillStatus)
	}
}

func TestParseTask_WhitespaceTrimmedAllFields(t *testing.T) {
	content := "## Stage:   plan-approved   \n## Last Skill:   sandman-tdd   \n## Last Skill Status:   complete   \n\n## Completed\ndone."
	doc := ParseTask(content)
	if doc.Stage != "plan-approved" {
		t.Fatalf("expected Stage=plan-approved, got %q", doc.Stage)
	}
	if doc.LastSkill != "sandman-tdd" {
		t.Fatalf("expected LastSkill=sandman-tdd, got %q", doc.LastSkill)
	}
	if doc.LastSkillStatus != "complete" {
		t.Fatalf("expected LastSkillStatus=complete, got %q", doc.LastSkillStatus)
	}
}

func TestBuildTaskPrompt_UpdateTaskContext_EmptyStatusRendersPlaceholder(t *testing.T) {
	doc := TaskDoc{
		Stage:     "plan-approved",
		LastSkill: "sandman-tdd",
		Body:      "## Completed\nDone.\n\n## Next Step\nContinue.",
	}
	result := BuildTaskPrompt(doc)

	if !strings.Contains(result, "## Last Skill Status: <context>") {
		t.Fatalf("expected ## Last Skill Status: <context> placeholder in UHC, got:\n%s", result)
	}
}
