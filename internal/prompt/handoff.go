package prompt

import (
	"strings"
)

type HandoffDoc struct {
	Stage           string // plan-approved, implementation-committed, pr-created, pr-review-finished
	SourcePrompt    string // always ".sandman/rendered-prompt.md"
	LastSkill       string // sandman sub-skill the previous run was on
	LastSkillStatus string // "complete" or "incomplete" with optional context after " — "
	Body            string // the remaining content (Completed, Pending, Blockers, Key Decisions, Next Step)
}

func ParseHandoff(content string) HandoffDoc {
	lines := strings.Split(content, "\n")
	var stage, lastSkill, lastSkillStatus string
	var bodyLines []string
	inMetadata := true

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inMetadata {
			if strings.HasPrefix(trimmed, "## Stage:") {
				stage = strings.TrimSpace(strings.TrimPrefix(trimmed, "## Stage:"))
				continue
			}
			if strings.HasPrefix(trimmed, "## Last Skill:") {
				lastSkill = strings.TrimSpace(strings.TrimPrefix(trimmed, "## Last Skill:"))
				continue
			}
			if strings.HasPrefix(trimmed, "## Last Skill Status:") {
				lastSkillStatus = strings.TrimSpace(strings.TrimPrefix(trimmed, "## Last Skill Status:"))
				continue
			}
			if strings.HasPrefix(trimmed, "##") {
				inMetadata = false
			}
		}
		if !inMetadata || !strings.HasPrefix(trimmed, "##") {
			bodyLines = append(bodyLines, line)
		}
	}

	body := strings.TrimSpace(strings.Join(bodyLines, "\n"))

	return HandoffDoc{
		Stage:           stage,
		SourcePrompt:    ".sandman/rendered-prompt.md",
		LastSkill:       lastSkill,
		LastSkillStatus: lastSkillStatus,
		Body:            body,
	}
}

func BuildResumePrompt(doc HandoffDoc) string {
	var b strings.Builder

	b.WriteString("## Prior Context\n")
	b.WriteString(doc.Body)
	b.WriteString("\n\n")

	b.WriteString("## Source Prompt\n")
	b.WriteString(".sandman/rendered-prompt.md\n\n")

	if doc.Stage != "" || doc.LastSkill != "" || doc.LastSkillStatus != "" {
		b.WriteString("## New Instruction\n")
		b.WriteString("Stage: ")
		b.WriteString(doc.Stage)
		b.WriteString(". Last skill: ")
		b.WriteString(doc.LastSkill)
		b.WriteString(" (")
		b.WriteString(doc.LastSkillStatus)
		b.WriteString("). Next: ")
		b.WriteString(extractNextStep(doc.Body))
		b.WriteString("\n\n")
	}

	b.WriteString("## Update Handoff Context\n")
	b.WriteString("Overwrite `.sandman/handoff.md` on exit with:\n\n")
	b.WriteString("## Stage: ")
	b.WriteString(doc.Stage)
	b.WriteString("\n")
	b.WriteString("## Last Skill: ")
	b.WriteString(doc.LastSkill)
	b.WriteString("\n")
	b.WriteString("## Last Skill Status: ")
	b.WriteString(doc.LastSkillStatus)
	b.WriteString(" — <context>\n")
	b.WriteString("## Completed\n\n\n")
	b.WriteString("## Pending\n\n\n")
	b.WriteString("## Blockers\n\n\n")
	b.WriteString("## Key Decisions\n\n\n")
	b.WriteString("## Next Step\n")
	b.WriteString(extractNextStep(doc.Body))
	b.WriteString("\n")

	return b.String()
}

func extractNextStep(body string) string {
	lines := strings.Split(body, "\n")
	inNextStep := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## Next Step") {
			inNextStep = true
			continue
		}
		if inNextStep {
			if strings.HasPrefix(trimmed, "## ") {
				break
			}
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return "Continue the work."
}
