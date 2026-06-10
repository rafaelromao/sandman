package prompt

import (
	"strings"
	"testing"
)

func TestParseHandoff_AllHeadings(t *testing.T) {
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

	doc := ParseHandoff(content)
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

func TestParseHandoff_AllFourStages(t *testing.T) {
	for _, stage := range []string{"plan-approved", "implementation-committed", "pr-created", "pr-review-finished"} {
		content := "## Stage: " + stage + "\n## Last Skill: s\n## Last Skill Status: c\n\n## Completed\ndone."
		doc := ParseHandoff(content)
		if doc.Stage != stage {
			t.Fatalf("for stage %q: expected %q, got %q", stage, stage, doc.Stage)
		}
	}
}

func TestParseHandoff_MissingHeadings(t *testing.T) {
	content := `## Completed
Some work.

## Next Step
Continue.`

	doc := ParseHandoff(content)
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

func TestParseHandoff_EmptyContent(t *testing.T) {
	doc := ParseHandoff("")
	if doc.Stage != "" || doc.LastSkill != "" || doc.LastSkillStatus != "" || doc.Body != "" {
		t.Fatalf("expected all zero values, got %+v", doc)
	}
}

func TestParseHandoff_LastSkillStatusWithContext(t *testing.T) {
	content := `## Stage: plan-approved
## Last Skill: sandman-tdd
## Last Skill Status: incomplete — tests failed
## Completed
Partial.`

	doc := ParseHandoff(content)
	if doc.LastSkillStatus != "incomplete — tests failed" {
		t.Fatalf("expected LastSkillStatus with context, got %q", doc.LastSkillStatus)
	}
}

func TestParseHandoff_StageLineWithExtraSpaces(t *testing.T) {
	content := "## Stage:   implementation-committed   \n## Last Skill:  sandman-tdd\n## Last Skill Status: complete\n\n## Completed\ndone."
	doc := ParseHandoff(content)
	if doc.Stage != "implementation-committed" {
		t.Fatalf("expected Stage=implementation-committed, got %q", doc.Stage)
	}
}

func TestBuildResumePrompt_HasAllSections(t *testing.T) {
	doc := HandoffDoc{
		Stage:           "plan-approved",
		LastSkill:       "sandman-tdd",
		LastSkillStatus: "complete",
		Body:            "## Completed\nDone.\n\n## Next Step\nPush PR.",
	}

	result := BuildResumePrompt(doc)

	if !strings.Contains(result, "## Prior Context") {
		t.Fatalf("expected ## Prior Context section, got:\n%s", result)
	}
	if !strings.Contains(result, "## Source Prompt") {
		t.Fatalf("expected ## Source Prompt section, got:\n%s", result)
	}
	if !strings.Contains(result, "## New Instruction") {
		t.Fatalf("expected ## New Instruction section, got:\n%s", result)
	}
	if !strings.Contains(result, "## Update Handoff Context") {
		t.Fatalf("expected ## Update Handoff Context section, got:\n%s", result)
	}
}

func TestBuildResumePrompt_SourcePromptReferencesFile(t *testing.T) {
	doc := HandoffDoc{Body: "## Completed\nDone."}
	result := BuildResumePrompt(doc)

	if !strings.Contains(result, ".sandman/rendered-prompt.md") {
		t.Fatalf("expected Source Prompt to reference rendered-prompt.md, got:\n%s", result)
	}
}

func TestBuildResumePrompt_NewInstruction(t *testing.T) {
	doc := HandoffDoc{
		Stage:           "implementation-committed",
		LastSkill:       "sandman-tdd",
		LastSkillStatus: "complete",
		Body:            "## Completed\nDone.\n\n## Next Step\nload sandman-merge, then push and create PR.",
	}

	result := BuildResumePrompt(doc)

	if !strings.Contains(result, "Stage: implementation-committed") {
		t.Fatalf("expected Stage in New Instruction, got:\n%s", result)
	}
	if !strings.Contains(result, "Last skill: sandman-tdd (complete)") {
		t.Fatalf("expected Last skill in New Instruction, got:\n%s", result)
	}
	if !strings.Contains(result, "Next: load sandman-merge") {
		t.Fatalf("expected Next in New Instruction, got:\n%s", result)
	}
}

func TestBuildResumePrompt_NewInstructionEmptyMetadata(t *testing.T) {
	doc := HandoffDoc{Body: "## Completed\nDone."}
	result := BuildResumePrompt(doc)

	if strings.Contains(result, "## New Instruction") {
		t.Fatalf("expected no New Instruction when metadata empty, got:\n%s", result)
	}
}

func TestBuildResumePrompt_UpdateHandoffContext(t *testing.T) {
	doc := HandoffDoc{
		Stage:           "plan-approved",
		LastSkill:       "sandman-tdd",
		LastSkillStatus: "complete",
		Body:            "## Completed\nDone.",
	}

	result := BuildResumePrompt(doc)

	if !strings.Contains(result, "## Stage:") {
		t.Fatalf("expected ## Stage: in Update Handoff Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Last Skill:") {
		t.Fatalf("expected ## Last Skill: in Update Handoff Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Last Skill Status:") {
		t.Fatalf("expected ## Last Skill Status: in Update Handoff Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Completed") {
		t.Fatalf("expected ## Completed in Update Handoff Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Pending") {
		t.Fatalf("expected ## Pending in Update Handoff Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Blockers") {
		t.Fatalf("expected ## Blockers in Update Handoff Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Key Decisions") {
		t.Fatalf("expected ## Key Decisions in Update Handoff Context, got:\n%s", result)
	}
	if !strings.Contains(result, "## Next Step") {
		t.Fatalf("expected ## Next Step in Update Handoff Context, got:\n%s", result)
	}
}

func TestBuildResumePrompt_UpdateHandoffContextTailIncludesHandoffMd(t *testing.T) {
	doc := HandoffDoc{Body: "## Completed\nDone."}
	result := BuildResumePrompt(doc)

	if !strings.Contains(result, ".sandman/handoff.md") {
		t.Fatalf("expected Update Handoff Context to reference handoff.md, got:\n%s", result)
	}
}

func TestBuildResumePrompt_WithStageOnly(t *testing.T) {
	doc := HandoffDoc{
		Stage: "pr-created",
		Body:  "## Completed\nDone.\n\n## Next Step\nMerge PR.",
	}

	result := BuildResumePrompt(doc)

	if !strings.Contains(result, "## New Instruction") {
		t.Fatalf("expected New Instruction with stage, got:\n%s", result)
	}
	if !strings.Contains(result, "Stage: pr-created") {
		t.Fatalf("expected Stage in New Instruction, got:\n%s", result)
	}
}

func TestBuildResumePrompt_SourcePromptDoesNotInlineContent(t *testing.T) {
	doc := HandoffDoc{Body: "some inline content"}
	result := BuildResumePrompt(doc)

	// Should reference the file, not include rendered prompt content
	if strings.Contains(result, "Implement GitHub issue") {
		t.Fatalf("expected Source Prompt to not inline rendered prompt content, got:\n%s", result)
	}
}
