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
	History         string // preserved prior handoff content after ## History, if present
}

func ParseHandoff(content string) HandoffDoc {
	lines := strings.Split(content, "\n")
	var stage, sourcePrompt, lastSkill, lastSkillStatus string
	var bodyLines []string
	var historyLines []string
	inHeader := true
	inHistory := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inHeader {
			switch {
			case strings.HasPrefix(trimmed, "## Stage:"):
				stage = strings.TrimSpace(strings.TrimPrefix(trimmed, "## Stage:"))
				continue
			case strings.HasPrefix(trimmed, "## Source Prompt:"):
				sourcePrompt = strings.TrimSpace(strings.TrimPrefix(trimmed, "## Source Prompt:"))
				continue
			case strings.HasPrefix(trimmed, "## Last Skill:"):
				lastSkill = strings.TrimSpace(strings.TrimPrefix(trimmed, "## Last Skill:"))
				continue
			case strings.HasPrefix(trimmed, "## Last Skill Status:"):
				lastSkillStatus = strings.TrimSpace(strings.TrimPrefix(trimmed, "## Last Skill Status:"))
				continue
			case trimmed == "## History":
				inHeader = false
				inHistory = true
				continue
			case strings.HasPrefix(trimmed, "## "):
				inHeader = false
				inHistory = false
			case trimmed == "":
				continue
			default:
				inHeader = false
				inHistory = false
			}
		} else if trimmed == "## History" && !inHistory {
			inHistory = true
			continue
		}
		if inHistory {
			historyLines = append(historyLines, line)
			continue
		}
		bodyLines = append(bodyLines, line)
	}

	body := strings.TrimSpace(strings.Join(bodyLines, "\n"))
	history := strings.TrimSpace(strings.Join(historyLines, "\n"))

	if sourcePrompt == "" {
		sourcePrompt = ".sandman/rendered-prompt.md"
	}

	return HandoffDoc{
		Stage:           stage,
		SourcePrompt:    sourcePrompt,
		LastSkill:       lastSkill,
		LastSkillStatus: lastSkillStatus,
		Body:            body,
		History:         history,
	}
}

func BuildResumePrompt(doc HandoffDoc) string {
	var b strings.Builder

	if doc.Body != "" {
		b.WriteString("## Prior Context\n")
		b.WriteString(doc.Body)
		b.WriteString("\n\n")
	}

	sourcePrompt := doc.SourcePrompt
	if sourcePrompt == "" {
		sourcePrompt = ".sandman/rendered-prompt.md"
	}
	b.WriteString("## Source Prompt: ")
	b.WriteString(sourcePrompt)
	b.WriteString("\n\n")
	b.WriteString("Re-read `")
	b.WriteString(sourcePrompt)
	b.WriteString("` before continuing so the original workflow stays in view.\n\n")

	if doc.Stage != "" || doc.LastSkill != "" || doc.LastSkillStatus != "" {
		b.WriteString("## New Instruction\n")
		b.WriteString("## Stage: ")
		b.WriteString(doc.Stage)
		b.WriteString("\n")
		b.WriteString("## Last Skill: ")
		b.WriteString(doc.LastSkill)
		b.WriteString("\n")
		b.WriteString("## Last Skill Status: ")
		b.WriteString(doc.LastSkillStatus)
		b.WriteString("\n")
		b.WriteString("Next: ")
		b.WriteString(extractNextStep(doc.Body))
		b.WriteString("\n\n")
	}
	if doc.History != "" {
		b.WriteString("## Prior History\n")
		b.WriteString(doc.History)
		b.WriteString("\n\n")
	}

	b.WriteString("## Update Handoff Context\n")
	b.WriteString("Write `.sandman/handoff.md` on exit with the latest checkpoint at top and any previous handoff content under `## History`. Preserve its current content before replacing it so history is not lost.\n\n")
	if doc.Stage != "" || doc.LastSkill != "" || doc.LastSkillStatus != "" {
		b.WriteString("## Stage: ")
		b.WriteString(doc.Stage)
		b.WriteString("\n")
		b.WriteString("## Source Prompt: ")
		b.WriteString(sourcePrompt)
		b.WriteString("\n")
		b.WriteString("## Last Skill: ")
		b.WriteString(doc.LastSkill)
		b.WriteString("\n")
		if doc.LastSkillStatus != "" {
			b.WriteString("## Last Skill Status: ")
			b.WriteString(doc.LastSkillStatus)
			b.WriteString("\n")
		} else {
			b.WriteString("## Last Skill Status: <context>\n")
		}
	}
	b.WriteString("## Completed\n\n\n")
	b.WriteString("## Pending\n\n\n")
	b.WriteString("## Blockers\n\n\n")
	b.WriteString("## Key Decisions\n\n\n")
	b.WriteString("## Next Step\n")
	b.WriteString(extractNextStep(doc.Body))
	b.WriteString("\n")
	b.WriteString("\n## History\n")
	b.WriteString("(preserve previous handoff content here, if any)\n")

	return b.String()
}

func extractNextStep(body string) string {
	if body == "" {
		return "Continue the work."
	}
	lines := strings.Split(body, "\n")
	inNextStep := false
	var nextLines []string
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
			if trimmed != "" || len(nextLines) > 0 {
				nextLines = append(nextLines, line)
			}
		}
	}
	if len(nextLines) == 0 {
		return "Continue the work."
	}
	return strings.TrimSpace(strings.Join(nextLines, "\n"))
}
