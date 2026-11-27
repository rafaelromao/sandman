package prompt

import (
	"fmt"
	"strings"
)

// TaskHistoryHeading names the Sandman-owned section that points a compacted
// Task at the archive holding its earlier content.
const TaskHistoryHeading = "## Sandman Task History"

// nextStepHeading is the template section the agent rewrites with its current
// position. Its beginning is kept when it grows; see nextStepLimit.
const nextStepHeading = "## Next Step"

// nextStepLimit caps the Next Step group of a compacted Task.
const nextStepLimit = 4 * 1024

// movedHeadingsListed bounds the moved headings the history pointer names.
const movedHeadingsListed = 20

// runtimeHeadings are Sandman's own sections. They are composed at every
// launch, so they are kept, and never anchor the agent's sections: a fresh
// render already places some of them after the template.
var runtimeHeadings = map[string]bool{
	"## Sandman Runtime Context":      true,
	reviewEvidenceHeading:             true,
	continuationFreshnessGuardHeading: true,
	contextRecoveryGuardHeading:       true,
	ciOwnershipContextHeading:         true,
	managedHandoffContextHeading:      true,
	TaskHistoryHeading:                true,
}

// TaskCompaction configures CompactTask.
type TaskCompaction struct {
	// Threshold is the size in bytes above which the Task is compacted.
	Threshold int
	// Target is the size in bytes compaction aims for.
	Target int
	// Force compacts even when the Task is not above Threshold.
	Force bool
	// Anchors are the H2 heading lines of the Task's fresh render, one entry
	// per occurrence. Sections whose headings they name are the Task's
	// structure; see TaskHeadings and TemplateHeadings.
	Anchors []string
	// ArchivePath is the worktree-relative path the history pointer names.
	ArchivePath string
}

// TaskCompactionResult reports what CompactTask did.
type TaskCompactionResult struct {
	Content       string
	Compacted     bool
	Before, After int
	Moved         []string
	// ArchivePath is the archive the history pointer names, when compacted.
	ArchivePath string
	// Reason says why a Task that qualified for compaction was left alone.
	Reason string
}

type taskSection struct {
	heading string // trimmed heading line; empty for the preamble
	text    string
}

// splitTaskSections splits a Task at its H2 headings outside fenced code
// blocks. Text before the first heading is the preamble section.
func splitTaskSections(content string) []taskSection {
	lines := strings.SplitAfter(content, "\n")
	sections := []taskSection{{}}
	var fence string
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		if marker := fenceMarker(trimmed); marker != "" {
			switch {
			case fence == "":
				fence = marker
			case strings.HasPrefix(strings.TrimSpace(trimmed), fence):
				fence = ""
			}
		} else if fence == "" && strings.HasPrefix(trimmed, "## ") {
			sections = append(sections, taskSection{heading: strings.TrimSpace(trimmed)})
		}
		sections[len(sections)-1].text += line
	}
	return sections
}

func fenceMarker(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return ""
	}
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			return marker
		}
	}
	return ""
}

// TaskHeadings returns the H2 heading lines of a Task, one per occurrence,
// ignoring headings inside fenced code blocks.
func TaskHeadings(content string) []string {
	var headings []string
	for _, section := range splitTaskSections(content) {
		if section.heading != "" {
			headings = append(headings, section.heading)
		}
	}
	return headings
}

// TemplateHeadings returns the H2 heading lines of the template a Task would
// be rendered from, plus those of the built-in default template. It is the
// fallback structure for Tasks rendered before Sandman recorded their anchors.
func TemplateHeadings(cfg RenderConfig) []string {
	headings := TaskHeadings(defaultPrompt)
	cfg.PromptFlag = ""
	if template, err := loadTemplate(cfg); err == nil && template != defaultPrompt {
		headings = mergeHeadingCounts(headings, TaskHeadings(template))
	}
	return headings
}

// mergeHeadingCounts keeps, for each heading, the larger occurrence count.
func mergeHeadingCounts(a, b []string) []string {
	counts := map[string]int{}
	for _, h := range a {
		counts[h]++
	}
	seen := map[string]int{}
	merged := append([]string(nil), a...)
	for _, h := range b {
		seen[h]++
		if seen[h] > counts[h] {
			merged = append(merged, h)
		}
	}
	return merged
}

func isProtectedHeading(heading string) bool {
	if strings.HasPrefix(heading, "## Status:") {
		return true
	}
	return heading == "## Plan" || heading == "## Plan Review" ||
		strings.HasPrefix(heading, "## Plan ") || strings.HasPrefix(heading, "## Plan:")
}

type sectionRole uint8

const (
	roleKeep sectionRole = iota
	roleNextStep
	roleHistory
	rolePointer
)

// CompactTask moves the oldest agent-written history out of an oversized
// Task. Sections are anchored to the Task's rendered structure (opts.Anchors):
// a section without an anchor belongs to the anchor before it, and sections
// after the last anchor are history. Anchored sections, plans, status lines
// and Sandman's own sections are kept byte for byte; the Next Step group keeps
// its first 4 KiB; history is kept from the end while the Task fits the
// target. The caller archives the original before using the result.
func CompactTask(content string, opts TaskCompaction) TaskCompactionResult {
	result := TaskCompactionResult{Content: content, Before: len(content), After: len(content)}
	if opts.Target <= 0 || !opts.Force && (opts.Threshold <= 0 || len(content) <= opts.Threshold) {
		return result
	}
	sections := splitTaskSections(content)
	roles := classifySections(sections, opts.Anchors)
	if roles == nil {
		result.Reason = "no recognised Task structure"
		return result
	}

	kept := make([]string, len(sections))
	var moved []string
	fixed := 0
	nextStep := 0
	truncated := false
	for i, section := range sections {
		switch roles[i] {
		case roleKeep:
			kept[i] = section.text
		case roleNextStep:
			switch remaining := nextStepLimit - nextStep; {
			case len(section.text) <= remaining:
				kept[i] = section.text
			case remaining > len(section.heading):
				kept[i] = cutAtLine(section.text, remaining)
				truncated = true
				moved = append(moved, section.heading+" (beyond the first 4 KiB)")
			default:
				moved = append(moved, section.heading)
			}
			nextStep += len(kept[i])
		}
		fixed += len(kept[i])
	}

	var history []int
	for i := range sections {
		if roles[i] == roleHistory {
			history = append(history, i)
		}
	}
	reserve := len(historyPointer(opts, len(content), append(append([]string(nil), moved...), headingsOf(sections, history)...)))
	budget := opts.Target - fixed - reserve
	keepFrom := len(history)
	for keepFrom > 0 {
		size := len(sections[history[keepFrom-1]].text)
		if size > budget {
			break
		}
		budget -= size
		keepFrom--
	}
	for _, i := range history[keepFrom:] {
		kept[i] = sections[i].text
	}
	moved = append(moved, headingsOf(sections, history[:keepFrom])...)

	if keepFrom == 0 && !truncated {
		if len(content) > opts.Target {
			result.Reason = "protected content exceeds the target"
		} else {
			result.Reason = "already compact"
		}
		return result
	}

	var b strings.Builder
	for i := range sections {
		b.WriteString(kept[i])
	}
	compacted := trimTrailingNewlines(b.String())
	movedBytes := len(content) - len(compacted)
	compacted += "\n\n" + historyPointer(opts, movedBytes, moved)
	result.Content, result.Compacted, result.After, result.Moved = compacted, true, len(compacted), moved
	result.ArchivePath = opts.ArchivePath
	return result
}

// classifySections assigns each section a role, or returns nil when no
// section matches an anchor.
func classifySections(sections []taskSection, anchors []string) []sectionRole {
	remaining := map[string]int{}
	for _, heading := range anchors {
		if !runtimeHeadings[heading] {
			remaining[heading]++
		}
	}
	anchored := make([]bool, len(sections))
	last := -1
	for i, section := range sections {
		if section.heading != "" && remaining[section.heading] > 0 {
			remaining[section.heading]--
			anchored[i] = true
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	roles := make([]sectionRole, len(sections))
	group := roleKeep
	for i, section := range sections {
		switch {
		case section.heading == TaskHistoryHeading:
			roles[i] = rolePointer
		case anchored[i]:
			group = roleKeep
			if section.heading == nextStepHeading {
				group = roleNextStep
			}
			roles[i] = group
		case section.heading == "" || runtimeHeadings[section.heading] || isProtectedHeading(section.heading):
			roles[i] = roleKeep
		case i > last:
			roles[i] = roleHistory
		default:
			roles[i] = group
		}
	}
	return roles
}

func headingsOf(sections []taskSection, indexes []int) []string {
	headings := make([]string, 0, len(indexes))
	for _, i := range indexes {
		headings = append(headings, sections[i].heading)
	}
	return headings
}

// cutAtLine returns the longest prefix of text, ending at a line break, that
// is at most limit bytes long.
func cutAtLine(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if cut := strings.LastIndexByte(text[:limit], '\n'); cut >= 0 {
		return text[:cut+1]
	}
	return ""
}

func historyPointer(opts TaskCompaction, movedBytes int, moved []string) string {
	var b strings.Builder
	b.WriteString(TaskHistoryHeading + "\n\n")
	fmt.Fprintf(&b, "Sandman moved %d bytes of earlier Task content to `%s` to keep this Task near %d bytes. That file holds the complete Task as it was before. It is history: read it only when a detail you need is missing here and in the live repository, pull request and checks.\n", movedBytes, opts.ArchivePath, opts.Target)
	if len(moved) == 0 {
		return b.String()
	}
	b.WriteString("\nMoved sections:\n\n")
	for i, heading := range moved {
		if i == movedHeadingsListed {
			fmt.Fprintf(&b, "- and %d more\n", len(moved)-i)
			break
		}
		fmt.Fprintf(&b, "- %s\n", strings.TrimPrefix(heading, "## "))
	}
	return b.String()
}
