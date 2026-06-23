package prompt

import (
	"strings"
)

type TaskDoc struct {
	Stage           string // plan-approved, implementation-committed, pr-created, pr-review-finished
	SourcePrompt    string // always ".sandman/task.md"
	LastSkill       string // sandman sub-skill the previous run was on
	LastSkillStatus string // "complete" or "incomplete" with optional context after " — "
	Plan            string // body of the top-level ## Plan section when present, "" otherwise
	Body            string // the remaining content (Completed, Pending, Blockers, Key Decisions, Next Step)
}

func ParseTask(content string) TaskDoc {
	lines := strings.Split(content, "\n")
	var stage, sourcePrompt, lastSkill, lastSkillStatus string
	var bodyLines []string
	var planLines []string
	inHeader := true
	inPlan := false
	planExited := false
	planHasContent := false

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
			case strings.HasPrefix(trimmed, "## "):
				inHeader = false
			case trimmed == "":
				continue
			default:
				inHeader = false
			}
		}
		if !inHeader && !inPlan && !planExited && trimmed == "## Plan" {
			inPlan = true
			continue
		}
		if inPlan {
			if strings.HasPrefix(trimmed, "## ") {
				inPlan = false
				planExited = true
			} else {
				planLines = append(planLines, line)
				if trimmed != "" {
					planHasContent = true
				}
				continue
			}
		}
		bodyLines = append(bodyLines, line)
	}

	body := strings.TrimSpace(strings.Join(bodyLines, "\n"))

	plan := ""
	if planHasContent {
		plan = strings.TrimSpace(strings.Join(planLines, "\n"))
	}

	if sourcePrompt == "" {
		sourcePrompt = ".sandman/task.md"
	}

	return TaskDoc{
		Stage:           stage,
		SourcePrompt:    sourcePrompt,
		LastSkill:       lastSkill,
		LastSkillStatus: lastSkillStatus,
		Plan:            plan,
		Body:            body,
	}
}

func BuildTaskPrompt(doc TaskDoc) string {
	var b strings.Builder

	if doc.Body != "" {
		b.WriteString("## Prior Context\n")
		b.WriteString(doc.Body)
		b.WriteString("\n\n")
	}

	sourcePrompt := doc.SourcePrompt
	if sourcePrompt == "" {
		sourcePrompt = ".sandman/task.md"
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

	b.WriteString("## Update Task Context\n")
	b.WriteString("Overwrite `.sandman/task.md` on exit with:\n\n")
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
	if doc.Plan != "" {
		b.WriteString("## Plan\n")
		b.WriteString(doc.Plan)
		b.WriteString("\n\n")
	}
	b.WriteString("## Completed\n\n\n")
	b.WriteString("## Pending\n\n\n")
	b.WriteString("## Blockers\n\n\n")
	b.WriteString("## Key Decisions\n\n\n")
	b.WriteString("## Next Step\n")
	b.WriteString(extractNextStep(doc.Body))
	b.WriteString("\n")

	return b.String()
}

// ContinuationTaskPrompt returns the resume prompt for `sandman run --continue`
// from the verbatim content of `.sandman/task.md`. The previous
// implementation routed the file through ParseTask → BuildTaskPrompt, which
// rewrote the file into a different scaffold (## Completed / ## Pending /
// ## Blockers / ## Key Decisions / ## Next Step) and silently destroyed the
// original default-task-prompt.md layout (# Task, ## Issue Context, ##
// Runtime Context, ## Execution Checklist). The continuation contract from
// docs/usage/default-prompt.md is "pass the contents of the task file
// verbatim" — this function is the single seam that enforces it.
//
// To prevent the secondary bug surfaced by #1193 — the carried-forward
// `## Blockers` section containing a stale PR/issue pointer — the blockers
// section is stripped from the returned prompt. Blockers are freeform text
// the agent wrote on a prior run and were never revalidated against the
// live GitHub state; carrying them forward unchecked is what caused the
// agent to believe PR #1208 was still blocked after its blocker was
// already resolved. Any new blockers must be re-discovered by the agent
// against the current state of the world, not inherited from the file.
//
// When content is empty (e.g. the file existed but was blank), this falls
// back to DefaultPrompt() so the agent still gets a usable scaffold. The
// "no task file" path lives in batch.ReadTaskContent and returns its own
// EmptyTaskTemplate.
func ContinuationTaskPrompt(content string) string {
	if strings.TrimSpace(content) == "" {
		return DefaultPrompt()
	}
	return stripBlockersSection(content)
}

// stripBlockersSection removes the `## Blockers` H2 block from content.
// The block is matched as `## Blockers` at the start of a (trimmed) line
// through the next `## ` heading or end of content. Other H2 sections are
// preserved verbatim — the goal is to drop only the stale-freeform-blocker
// carry-forward, not to rewrite the file.
func stripBlockersSection(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	inBlockers := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			if strings.EqualFold(trimmed, "## Blockers") {
				inBlockers = true
				continue
			}
			if inBlockers {
				inBlockers = false
			}
		}
		if !inBlockers {
			out = append(out, line)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
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
