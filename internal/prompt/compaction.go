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
	reviewTimeoutContextHeading:       true,
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
// blocks. Text before the first heading is the preamble section. A fence left
// open at the end of the Task (a truncated log, say) is treated as text, so it
// cannot swallow the sections after it.
func splitTaskSections(content string) []taskSection {
	lines := strings.SplitAfter(content, "\n")
	ignored := map[int]bool{}
	for {
		sections, unclosed := splitTaskLines(lines, ignored)
		if unclosed < 0 {
			return sections
		}
		ignored[unclosed] = true
	}
}

// splitTaskLines splits lines into sections and returns the index of a fence
// opener still open at the end, or -1.
func splitTaskLines(lines []string, ignored map[int]bool) ([]taskSection, int) {
	headings := []string{""}
	starts := []int{0}
	var open fence
	openedAt := -1
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		if f, ok := parseFence(trimmed); ok && !ignored[i] {
			switch {
			case open.char == 0:
				open, openedAt = f, i
			case f.closes(open):
				open, openedAt = fence{}, -1
			}
			continue
		}
		if open.char == 0 && strings.HasPrefix(trimmed, "## ") {
			headings = append(headings, strings.TrimSpace(trimmed))
			starts = append(starts, i)
		}
	}
	sections := make([]taskSection, len(starts))
	for n, start := range starts {
		end := len(lines)
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		sections[n] = taskSection{heading: headings[n], text: strings.Join(lines[start:end], "")}
	}
	return sections, openedAt
}

// fence is a fenced code block delimiter: its character and length, and
// whether an info string follows it.
type fence struct {
	char   byte
	length int
	info   bool
}

// parseFence recognises a fence line, indented by at most three spaces.
func parseFence(line string) (fence, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || trimmed[0] != '`' && trimmed[0] != '~' {
		return fence{}, false
	}
	char := trimmed[0]
	length := 0
	for length < len(trimmed) && trimmed[length] == char {
		length++
	}
	rest := trimmed[length:]
	if length < 3 || char == '`' && strings.Contains(rest, "`") {
		return fence{}, false
	}
	return fence{char: char, length: length, info: strings.TrimSpace(rest) != ""}, true
}

// closes reports whether f ends the block open started: the same character,
// at least as long, and no info string.
func (f fence) closes(open fence) bool {
	return f.char == open.char && f.length >= open.length && !f.info
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
	return heading == "## Plan" || strings.HasPrefix(heading, "## Plan ") || strings.HasPrefix(heading, "## Plan:")
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
	roles, last := classifySections(sections, opts.Anchors)
	if roles == nil {
		result.Reason = "no recognised Task structure"
		return result
	}

	kept := make([]string, len(sections))
	var moved []string
	fixed := 0
	nextStep := 0
	changed := false
	for i, section := range sections {
		switch roles[i] {
		case roleKeep:
			kept[i] = section.text
		case roleNextStep:
			switch remaining := nextStepLimit - nextStep; {
			case len(section.text) <= remaining:
				kept[i] = section.text
			case remaining > len(section.heading):
				kept[i] = cutOutsideFence(section.text, remaining)
				changed = true
				moved = append(moved, fmt.Sprintf("%s (beyond the first %d KiB)", section.heading, nextStepLimit/1024))
			default:
				changed = true
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
	// The pointer and its blank-line separator; it lists at most everything
	// that could move.
	reserve := len(historyPointer(opts, len(content), append(append([]string(nil), moved...), headingsOf(sections, history)...))) + 2
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

	if keepFrom == 0 && !changed {
		if len(content) > opts.Target {
			result.Reason = "protected content exceeds the target"
		} else {
			result.Reason = "already compact"
		}
		return result
	}

	keptBytes := 0
	insertAt := len(sections)
	for i := range sections {
		keptBytes += len(kept[i])
		if i > last && kept[i] != "" && insertAt == len(sections) {
			insertAt = i
		}
	}
	// The pointer goes after the Task's structure, not at the end, so text
	// the agent appends later never becomes part of it.
	pointer := historyPointer(opts, len(content)-keptBytes, moved)
	var b strings.Builder
	for i := range sections {
		if i == insertAt {
			b.WriteString(pointer + "\n")
		}
		b.WriteString(kept[i])
	}
	compacted := trimTrailingNewlines(b.String())
	if insertAt == len(sections) {
		compacted += "\n\n" + trimTrailingNewlines(pointer)
	}
	compacted += "\n"
	result.Content, result.Compacted, result.After, result.Moved = compacted, true, len(compacted), moved
	result.ArchivePath = opts.ArchivePath
	return result
}

// classifySections assigns each section a role and returns the index of the
// last anchored section, or nil when no section matches an anchor.
func classifySections(sections []taskSection, anchors []string) ([]sectionRole, int) {
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
		return nil, -1
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
	return roles, last
}

func headingsOf(sections []taskSection, indexes []int) []string {
	headings := make([]string, 0, len(indexes))
	for _, i := range indexes {
		headings = append(headings, sections[i].heading)
	}
	return headings
}

// cutOutsideFence returns the longest prefix of text, ending at a line break
// outside any fenced code block, that is at most limit bytes long. The text
// starts outside a fence, as every section does.
func cutOutsideFence(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	safe, offset := 0, 0
	var open fence
	for _, line := range strings.SplitAfter(text, "\n") {
		if offset+len(line) > limit || !strings.HasSuffix(line, "\n") {
			break
		}
		if f, ok := parseFence(strings.TrimRight(line, "\r\n")); ok {
			switch {
			case open.char == 0:
				open = f
			case f.closes(open):
				open = fence{}
			}
		}
		offset += len(line)
		if open.char == 0 {
			safe = offset
		}
	}
	return text[:safe]
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
