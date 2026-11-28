package prompt

import (
	"strings"
)

// TaskBounds locates the parts of a Task that compaction may change, as byte
// offsets into it. Everything before StructureEnd is template and issue
// content, kept except for the Next Step group. Everything after it is agent
// history and Sandman's own sections.
type TaskBounds struct {
	StructureEnd int
	// NextStepStart and NextStepEnd bound the template's `## Next Step` and
	// what the agent added to it; they are equal when it cannot be located.
	NextStepStart, NextStepEnd int
}

// Marker lines a fresh implementation render writes into its Task. They are
// HTML comments, so they do not show when rendered, and carry a per-render
// nonce recorded outside the Task, so issue text cannot forge them.
const (
	nextStepStartMarker = "sandman:next-step-start"
	// nextStepEndMarker follows the heading of the section after Next Step;
	// nextStepLastMarker closes a Next Step that ends the structure.
	nextStepEndMarker  = "sandman:next-step-end"
	nextStepLastMarker = "sandman:next-step-last"
	taskEndMarker      = "sandman:task-end"
)

// composedHeadings are the sections Sandman removes and writes again at
// launches, so a marker placed inside one would be lost with it.
var composedHeadings = map[string]bool{
	reviewEvidenceHeading:             true,
	continuationFreshnessGuardHeading: true,
	contextRecoveryGuardHeading:       true,
	ciOwnershipContextHeading:         true,
	managedHandoffContextHeading:      true,
}

func structureMarker(name, nonce string) string {
	return "<!-- " + name + " " + nonce + " -->"
}

// markTemplate writes the structure markers into a template before
// substitution, so they sit at the template's positions whatever the issue
// body holds. The structure ends after the template and every place the issue
// body is substituted into. The Next Step group is marked from its heading to
// the next template section, whose heading line the end marker follows so that
// rewriting Next Step keeps it. It is left unmarked if the issue body is
// substituted into it, or if that next heading holds a placeholder, which
// could expand to several lines.
func markTemplate(template, nonce string) string {
	sections := splitTaskSections(template)
	offsets := make([]int, len(sections)+1)
	for i, section := range sections {
		offsets[i+1] = offsets[i] + len(section.text)
	}
	// The structure ends before trailing sections Sandman composes again at
	// every launch, unless the template puts the issue body in one.
	last := len(sections) - 1
	for last > 0 && composedHeadings[sections[last].heading] && !strings.Contains(sections[last].text, issueBodyPlaceholder) {
		last--
	}
	end := offsets[last+1]
	step, next := -1, -1
	for i := 1; i <= last && step < 0; i++ {
		if sections[i].heading == nextStepHeading {
			step = i
		}
	}
	if step >= 0 {
		groupEnd := end
		for i := step + 1; i <= last && next < 0; i++ {
			if !composedHeadings[sections[i].heading] {
				next, groupEnd = i, offsets[i]
			}
		}
		heading := ""
		if next >= 0 {
			heading, _, _ = strings.Cut(sections[next].text, "\n")
		}
		if strings.Contains(template[offsets[step]:groupEnd], issueBodyPlaceholder) || strings.Contains(heading, "{{") {
			step, next = -1, -1
		}
	}

	var b strings.Builder
	written := 0
	insert := func(at int, line string) {
		b.WriteString(template[written:at])
		if at > 0 && template[at-1] != '\n' {
			b.WriteString("\n")
		}
		b.WriteString(line + "\n")
		written = at
	}
	if step >= 0 {
		insert(offsets[step], structureMarker(nextStepStartMarker, nonce))
	}
	if next >= 0 {
		headingEnd := offsets[next+1]
		if nl := strings.IndexByte(sections[next].text, '\n'); nl >= 0 {
			headingEnd = offsets[next] + nl + 1
		}
		insert(headingEnd, structureMarker(nextStepEndMarker, nonce))
	}
	closing := structureMarker(taskEndMarker, nonce)
	if step >= 0 && next < 0 {
		closing = structureMarker(nextStepLastMarker, nonce) + "\n" + closing
	}
	insert(end, closing)
	b.WriteString(template[written:])
	return b.String()
}

// findLine returns the offset of the first line equal to line and the offset
// after it, or -1, -1.
func findLine(content, line string) (int, int) {
	for start := 0; start < len(content); {
		next := len(content)
		if nl := strings.IndexByte(content[start:], '\n'); nl >= 0 {
			next = start + nl + 1
		}
		if strings.TrimRight(content[start:next], "\r\n") == line {
			return start, next
		}
		start = next
	}
	return -1, -1
}

// MarkedTaskBounds locates the structure a fresh render marked with nonce. It
// reports false when the end marker is gone. A Next Step marker that is gone
// or moved leaves the group unlocated.
func MarkedTaskBounds(content, nonce string) (TaskBounds, bool) {
	if nonce == "" {
		return TaskBounds{}, false
	}
	closing, structureEnd := findLine(content, structureMarker(taskEndMarker, nonce))
	if closing < 0 {
		return TaskBounds{}, false
	}
	bounds := TaskBounds{StructureEnd: structureEnd}
	_, from := findLine(content[:closing], structureMarker(nextStepStartMarker, nonce))
	if from < 0 {
		return bounds, true
	}
	to, _ := findLine(content[:closing], structureMarker(nextStepEndMarker, nonce))
	if to >= 0 {
		// The group ends at the heading the marker follows.
		heading := strings.LastIndex(strings.TrimRight(content[:to], "\r\n"), "\n") + 1
		if !strings.HasPrefix(content[heading:], "## ") {
			return bounds, true
		}
		to = heading
	} else if to, _ = findLine(content[:closing], structureMarker(nextStepLastMarker, nonce)); to < 0 {
		return bounds, true
	}
	if from < to {
		bounds.NextStepStart, bounds.NextStepEnd = from, to
	}
	return bounds, true
}

const (
	issueContextHeading    = "## Issue Context"
	runtimeContextHeading  = "## Runtime Context"
	completionHeading      = "## Completion Requirements"
	issueBodyPlaceholder   = "{{ISSUE_BODY}}"
	builtInIssueBlockStart = issueContextHeading + "\n\n"
)

// lastTemplateSection returns the template's last section that is not one of
// Sandman's own, without trailing newlines.
func lastTemplateSection(template string) string {
	sections := splitTaskSections(template)
	for i := len(sections) - 1; i >= 0; i-- {
		if !runtimeHeadings[sections[i].heading] {
			return trimTrailingNewlines(sections[i].text)
		}
	}
	return ""
}

// BuiltInTaskBounds locates the structure of a Task that carries no markers,
// such as one rendered before Sandman wrote them. Only the built-in layout can
// be verified: the template puts the issue body alone under `## Issue
// Context`, before `## Runtime Context`, and ends with the built-in `##
// Completion Requirements`. The Task must hold the current issue body there and
// those requirements verbatim; otherwise nothing is located. Only the `##
// Next Step` section itself is editable: without markers nothing proves that
// the sections after it were added by the agent and not by the template.
func BuiltInTaskBounds(cfg RenderConfig, content, issueBody string) (TaskBounds, bool) {
	template, err := loadTemplate(cfg)
	if err != nil {
		return TaskBounds{}, false
	}
	issueBlock := builtInIssueBlockStart + issueBodyPlaceholder + "\n\n" + runtimeContextHeading + "\n"
	requirements := lastTemplateSection(defaultPrompt)
	if strings.Count(template, issueBodyPlaceholder) != 1 || !strings.Contains(template, issueBlock) ||
		lastTemplateSection(template) != requirements {
		return TaskBounds{}, false
	}

	body := builtInIssueBlockStart + NeutraliseBodyPlaceholders(issueBody)
	at := strings.Index(content, body)
	if at < 0 || !strings.HasPrefix(strings.TrimLeft(content[at+len(body):], "\r\n"), runtimeContextHeading) {
		return TaskBounds{}, false
	}
	bodyEnd := at + len(body)

	var bounds TaskBounds
	offset := bodyEnd
	for _, section := range splitTaskSections(content[bodyEnd:]) {
		switch {
		case section.heading == nextStepHeading && bounds.NextStepEnd == 0:
			bounds.NextStepStart, bounds.NextStepEnd = offset, offset+len(section.text)
		case section.heading == completionHeading && strings.HasPrefix(section.text+"\n", requirements+"\n"):
			bounds.StructureEnd = offset + min(len(requirements)+1, len(section.text))
			return bounds, true
		}
		offset += len(section.text)
	}
	return TaskBounds{}, false
}
