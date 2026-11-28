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
	// nextStepStartMarker precedes the Next Step heading; nextStepBodyMarker
	// follows it.
	nextStepStartMarker = "sandman:next-step-start"
	nextStepBodyMarker  = "sandman:next-step-body"
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

// markTemplate writes the structure markers into a template before any
// substitution, so they sit at the template's own headings whatever the
// substituted values and the issue body hold. The structure ends after the
// template and every place something is substituted into. The Next Step group
// is marked from the template's heading to the next template section, whose
// heading line the end marker follows so that rewriting Next Step keeps it. It
// is left unmarked if anything from the one to the other is a placeholder:
// what it expands to is unknown here.
func markTemplate(template, nonce string) (marked string) {
	// A template the parser fails on is left unmarked.
	defer func() {
		if recover() != nil {
			marked = template
		}
	}()
	doc := readTask(template)
	sections := doc.sections(0, len(template))
	offsets := make([]int, len(sections)+1)
	for i, section := range sections {
		offsets[i+1] = offsets[i] + len(section.text)
	}
	// The structure ends before trailing sections Sandman composes again at
	// every launch, unless the template substitutes something into one.
	last := len(sections) - 1
	for last > 0 && composedHeadings[sections[last].heading] && !keyPattern.MatchString(sections[last].text) {
		last--
	}
	end := offsets[last+1]
	step, next := -1, -1
	for i := 1; i <= last && step < 0; i++ {
		// Markers would break an underlined heading apart.
		if sections[i].heading == nextStepHeading && !sections[i].underlined {
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
		if next >= 0 {
			heading, _, _ := strings.Cut(sections[next].text, "\n")
			groupEnd += len(heading)
		}
		if keyPattern.MatchString(template[offsets[step]:groupEnd]) || doc.holdsOtherHeading(offsets[step], groupEnd) {
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
	// A marker goes above and below the heading: a launch rewriting the section
	// above takes only the first, an agent rewriting the step only the
	// second.
	if step >= 0 {
		insert(offsets[step], structureMarker(nextStepStartMarker, nonce))
		headingEnd := offsets[step+1]
		if nl := strings.IndexByte(sections[step].text, '\n'); nl >= 0 {
			headingEnd = offsets[step] + nl + 1
		}
		insert(headingEnd, structureMarker(nextStepBodyMarker, nonce))
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

// nextStepStart returns the offset of the marked Next Step heading before to,
// or -1. The marker below the heading names the one right above it; the marker
// above names the first one after it. Neither ever reaches back into earlier
// text.
func nextStepStart(doc taskDocument, to int, nonce string) int {
	content := doc.content[:to]
	if below, _ := findLine(content, structureMarker(nextStepBodyMarker, nonce)); below >= 0 {
		at := strings.LastIndex(strings.TrimRight(content[:below], "\r\n"), "\n") + 1
		if doc.atxSectionAt(at, nextStepHeading) {
			return at
		}
	}
	_, offset := findLine(content, structureMarker(nextStepStartMarker, nonce))
	if offset < 0 {
		return -1
	}
	for _, section := range doc.sections(offset, to) {
		if section.heading == nextStepHeading && !section.underlined {
			return offset
		}
		offset += len(section.text)
	}
	return -1
}

// MarkedTaskBounds locates the structure a fresh render marked with nonce. It
// reports false when the end marker is gone. When both copies of the Next Step
// marker or the end of the group are gone, or the heading is renamed, the group
// is left unlocated. Markers with another nonce are text.
func MarkedTaskBounds(content, nonce string) (bounds TaskBounds, located bool) {
	defer func() {
		if recover() != nil {
			bounds, located = TaskBounds{}, false
		}
	}()
	if nonce == "" {
		return TaskBounds{}, false
	}
	closing, structureEnd := findLine(content, structureMarker(taskEndMarker, nonce))
	if closing < 0 {
		return TaskBounds{}, false
	}
	bounds = TaskBounds{StructureEnd: structureEnd}
	doc := readTask(content)
	to, _ := findLine(content[:closing], structureMarker(nextStepEndMarker, nonce))
	if to >= 0 {
		// The group ends at the heading the marker follows.
		heading := strings.LastIndex(strings.TrimRight(content[:to], "\r\n"), "\n") + 1
		if !doc.atxSectionAt(heading, "") {
			return bounds, true
		}
		to = heading
	} else if to, _ = findLine(content[:closing], structureMarker(nextStepLastMarker, nonce)); to < 0 {
		return bounds, true
	}
	if from := nextStepStart(doc, to, nonce); from >= 0 {
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
// the sections after it were added by the agent and not by the template. It is
// editable only if the Task holds exactly one before the requirements, and
// neither it nor the template's holds or ends at an H1 or underlined heading,
// nor the template's holds a placeholder.
func BuiltInTaskBounds(cfg RenderConfig, content, issueBody string) (bounds TaskBounds, located bool) {
	defer func() {
		if recover() != nil {
			bounds, located = TaskBounds{}, false
		}
	}()
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
	if at < 0 {
		return TaskBounds{}, false
	}
	// The body may end mid-line, so the sections start at the heading after it.
	after := strings.TrimLeft(content[at+len(body):], "\r\n")
	start := len(content) - len(after)
	if !strings.HasPrefix(after, runtimeContextHeading) || content[start-1] != '\n' {
		return TaskBounds{}, false
	}

	doc := readTask(content)
	offset, steps := start, 0
	for _, section := range doc.sections(start, len(content)) {
		switch {
		case section.heading == nextStepHeading && !section.underlined:
			steps++
			bounds.NextStepStart, bounds.NextStepEnd = offset, offset+len(section.text)
		case section.heading == completionHeading && strings.HasPrefix(section.text+"\n", requirements+"\n"):
			bounds.StructureEnd = offset + min(len(requirements)+1, len(section.text))
			if steps != 1 || unverifiableNextStep(template) ||
				doc.holdsOtherHeading(bounds.NextStepStart, doc.nextATXSection(bounds.NextStepStart)) {
				bounds.NextStepStart, bounds.NextStepEnd = 0, 0
			}
			return bounds, true
		}
		offset += len(section.text)
	}
	return TaskBounds{}, false
}

// unverifiableNextStep reports whether a template's Next Step section holds a
// placeholder, or holds or ends at an H1 or underlined heading.
func unverifiableNextStep(template string) bool {
	doc := readTask(template)
	offset := 0
	for _, section := range doc.sections(0, len(template)) {
		if section.heading == nextStepHeading && !section.underlined {
			end := doc.nextATXSection(offset)
			if keyPattern.MatchString(template[offset:end]) || doc.holdsOtherHeading(offset, end) {
				return true
			}
		}
		offset += len(section.text)
	}
	return false
}
