package prompt

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TaskHistoryHeading names the Sandman-owned section that points a compacted
// Task at the archive holding its earlier content.
const TaskHistoryHeading = "## Sandman Task History"

// taskHistoryEnd closes the history pointer, so text an agent writes after it
// stays its own and is never replaced with the pointer.
const taskHistoryEnd = "<!-- end of Sandman Task History -->"

// nextStepHeading is the template section the agent rewrites with its current
// position. Its beginning is kept when it grows; see nextStepLimit.
const nextStepHeading = "## Next Step"

// nextStepLimit caps the Next Step group of a compacted Task.
const nextStepLimit = 4 * 1024

// runtimeHeadings are the sections Sandman composes or rewrites at launches,
// including both Runtime Context headings it updates (see
// holdsRuntimeContext), so compaction always keeps them.
var runtimeHeadings = map[string]bool{
	runtimeContextHeading:             true,
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
	// ArchivePath is the worktree-relative path the history pointer names.
	ArchivePath string
	// Bounds locates the Task's structure (see MarkedTaskBounds and
	// BuiltInTaskBounds); nil leaves the Task alone.
	Bounds *TaskBounds
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
	heading string // "## title" for classification; empty for the preamble
	text    string
	// headingLength is the length of the heading's lines, two or more for an
	// underlined heading, whose title lines title holds.
	headingLength int
	underlined    bool
	// nested marks a heading inside a list item, which the list above gives
	// its meaning.
	nested bool
	title  []string
	// blocks are the section's code and HTML blocks, relative to text.
	blocks []markdownBlock
	// holdsNextTitle marks the section before an underlined title that is
	// kept: an underline is easily written by accident, as under a rule.
	holdsNextTitle bool
	// afterPointer marks text an agent wrote after a history pointer's end
	// marker, before the next heading.
	afterPointer     bool
	generatedPointer bool
}

// maxLabel bounds the name the history pointer gives an underlined section,
// whose title can be a whole paragraph.
const maxLabel = 120

// pointerSafe keeps a title the history pointer lists from being read as
// markup: it can neither open a block there nor end one left open above.
var pointerSafe = strings.NewReplacer("<", "&lt;", ">", "&gt;", "```", "\\`\\`\\`", "~~~", "\\~\\~\\~")

// indented reports whether the section's first line of text is indented, so a
// list above could take it in.
func (s taskSection) indented() bool {
	first := strings.TrimLeft(s.text, "\r\n")
	return strings.HasPrefix(first, " ") || strings.HasPrefix(first, "\t")
}

// mayJoin reports whether the section could continue whatever precedes it: an
// underlined title or a note without a heading continues a paragraph, list or
// quote with no blank line before it, and an indented one even with it.
func (s taskSection) mayJoin() bool {
	return s.underlined || s.afterPointer || s.indented()
}

// label names a section in the history pointer's list of moved sections.
func (s taskSection) label() string {
	if s.afterPointer {
		return "## Notes after the previous Sandman Task History"
	}
	name := s.heading
	if s.underlined && len(name) > maxLabel {
		name = runePrefix(name, maxLabel) + "..."
	}
	return pointerSafe.Replace(name)
}

// protectedTitle reports whether the section's underlined title names one
// compaction keeps.
func (s taskSection) protectedTitle() bool {
	return s.underlined && underlinesProtected(s.title)
}

// from returns what follows offset in the section, without a heading.
func (s taskSection) from(offset int) taskSection {
	rest := taskSection{text: s.text[offset:], holdsNextTitle: s.holdsNextTitle}
	for _, block := range s.blocks {
		if block.start >= offset {
			block.start, block.end = block.start-offset, block.end-offset
			rest.blocks = append(rest.blocks, block)
		}
	}
	return rest
}

// joinNested joins each section inside a list item to the one holding the
// list, so compaction never moves one without the other.
func joinNested(sections []taskSection) []taskSection {
	var out []taskSection
	var texts []string
	join := func() {
		if len(out) > 0 {
			out[len(out)-1].text = strings.Join(texts, "")
		}
	}
	length := 0
	for _, section := range sections {
		if !section.nested || len(out) == 0 {
			join()
			out = append(out, section)
			texts, length = []string{section.text}, len(section.text)
			continue
		}
		holder := &out[len(out)-1]
		for _, block := range section.blocks {
			block.start, block.end = block.start+length, block.end+length
			holder.blocks = append(holder.blocks, block)
		}
		holder.holdsNextTitle = section.holdsNextTitle
		texts, length = append(texts, section.text), length+len(section.text)
	}
	join()
	return out
}

// splitAtPointerEnd separates text written after a history pointer's end
// marker from the pointer, so compaction replaces only the generated bytes.
func splitAtPointerEnd(sections []taskSection) []taskSection {
	var out []taskSection
	for _, section := range sections {
		if section.heading == TaskHistoryHeading {
			// An agent's editor may have rewritten the line endings.
			if _, end := findLine(section.text, taskHistoryEnd); end >= 0 &&
				strings.HasPrefix(strings.ReplaceAll(section.text, "\r\n", "\n"), TaskHistoryHeading+"\n\nSandman moved ") {
				section.generatedPointer = true
				if strings.TrimSpace(section.text[end:]) != "" {
					tail := section.from(end)
					tail.afterPointer = true
					section.text = section.text[:end]
					out = append(out, section, tail)
					continue
				}
			}
		}
		out = append(out, section)
	}
	return out
}

// splitTaskSections splits a Task at its H2 headings, as CommonMark reads
// them (see boundaries). Text before the first heading is the preamble
// section.
func splitTaskSections(content string) []taskSection {
	return readTask(content).sections(0, len(content))
}

// sections splits the content between two line starts, slicing the original
// bytes. The Task is read whole, so a block that crosses from stays in the
// preamble and its lines are never headings.
func (d taskDocument) sections(from, to int) []taskSection {
	headings := d.boundaries(from, to)
	sections := make([]taskSection, len(headings)+1)
	start := from
	block := sort.Search(len(d.blocks), func(n int) bool { return d.blocks[n].start >= from })
	for n := range sections {
		end := to
		if n < len(headings) {
			end = headings[n].start
		}
		section := &sections[n]
		if n > 0 {
			h := headings[n-1]
			section.headingLength = min(h.end, end) - start
			section.underlined, section.title = h.underlined, h.title
			section.nested = !h.underlined && !h.topLevel
			if h.underlined {
				section.heading, _ = h2Heading("## " + strings.Join(h.title, " "))
			} else {
				section.heading, _ = h2Heading(d.content[start:min(h.end, end)])
			}
		}
		section.text = d.content[start:end]
		for ; block < len(d.blocks) && d.blocks[block].start < end; block++ {
			b := d.blocks[block]
			b.start, b.end = b.start-start, b.end-start
			section.blocks = append(section.blocks, b)
		}
		start = end
	}
	for n := 0; n+1 < len(sections); n++ {
		sections[n].holdsNextTitle = sections[n+1].protectedTitle()
	}
	return sections
}

// h2Heading names a line written as an H2, indented by up to three spaces as
// Markdown allows, as "## title" for classification, whatever blank space or
// closing number signs it is written with. Whether a line is a heading at all
// is the parser's call.
func h2Heading(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	body := strings.TrimLeft(line, " ")
	rest, ok := strings.CutPrefix(body, "##")
	if !ok || len(line)-len(body) > 3 || rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", false
	}
	words := strings.Fields(rest)
	if n := len(words); n > 0 && strings.Trim(words[n-1], "#") == "" {
		words = words[:n-1]
	}
	return strings.TrimSpace("## " + strings.Join(words, " ")), true
}

// isProtectedHeading reports whether a section holds agent state that is
// never moved: the checklist, a plan or a status line, in any capitalisation.
func isProtectedHeading(heading string) bool {
	heading = strings.ToLower(heading)
	for _, title := range []string{"## execution checklist", "## plan"} {
		if rest, ok := strings.CutPrefix(heading, title); ok && (rest == "" || rest[0] == ' ' || rest[0] == ':') {
			return true
		}
	}
	return strings.HasPrefix(heading, "## status:")
}

// holdsRuntimeContext reports whether any line is one a continuation takes for
// a Runtime Context heading, in a code block or not: the review settings may be
// written below it.
func holdsRuntimeContext(text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		switch strings.TrimSpace(line) {
		case reviewTimeoutContextHeading, runtimeContextHeading:
			return true
		}
	}
	return false
}

// isKeptSection reports whether compaction keeps a section byte for byte
// wherever it is: Sandman's sections, protected state, text a continuation may
// write the review settings into, and text whose code or HTML block swallowed
// one of those, including a note without a heading of its own.
func isKeptSection(section taskSection, topLevel map[string]bool) bool {
	return runtimeHeadings[section.heading] || isProtectedHeading(section.heading) ||
		section.protectedTitle() || section.holdsNextTitle ||
		holdsRuntimeContext(section.text) || swallowsProtected(section, topLevel)
}

// capStyle is how Next Step may be cut.
type capStyle int

const (
	// capInside cuts inside a line or block when a cut before it would waste
	// most of the limit.
	capInside capStyle = iota
	// capBefore cuts only between lines and blocks.
	capBefore
	// capNothing leaves Next Step whole.
	capNothing
)

// readsDifferently is why a result that would not read as it was put together
// is not applied.
const readsDifferently = "compacting would change how the Task reads"

// CompactTask moves the oldest agent-written history out of an oversized Task
// and trims its Next Step group to the first 4 KiB, using opts.Bounds. The
// rest of the structure, plans, status lines and Sandman's sections are kept
// byte for byte; history is kept from the end while the Task fits the target.
// A result that would not read as it was put together is not applied; more
// careful cuts of Next Step are tried first. The caller archives the original
// before using the result.
func CompactTask(content string, opts TaskCompaction) (result TaskCompactionResult) {
	result = TaskCompactionResult{Content: content, Before: len(content), After: len(content)}
	// A parser failure on odd input must not take the Run with it.
	defer func() {
		if failure := recover(); failure != nil {
			result = TaskCompactionResult{Content: content, Before: len(content), After: len(content),
				Reason: fmt.Sprintf("the Task could not be read: %v", failure)}
		}
	}()
	if opts.Target <= 0 || !opts.Force && (opts.Threshold <= 0 || len(content) <= opts.Threshold) {
		return result
	}
	if opts.Bounds == nil {
		result.Reason = "no recorded Task structure"
		return result
	}
	doc := readTask(content)
	for style := capInside; ; style++ {
		if result = compactRead(doc, opts, style); result.Reason != readsDifferently || style == capNothing {
			return result
		}
	}
}

// compactRead compacts a Task already read, cutting Next Step as style allows.
func compactRead(doc taskDocument, opts TaskCompaction, style capStyle) TaskCompactionResult {
	content, bounds := doc.content, *opts.Bounds
	result := TaskCompactionResult{Content: content, Before: len(content), After: len(content)}
	topLevel := map[string]bool{}
	for _, section := range doc.sections(0, len(content)) {
		topLevel[section.heading] = true
	}
	// Sections the settings span runs into are kept whole too: composition
	// ends a section where it reads a heading, which compaction may not.
	settingsFrom, settingsTo := settingsSpan(content)
	keeps := func(offset int, section taskSection) bool {
		return isKeptSection(section, topLevel) || offset < settingsTo && offset+len(section.text) > settingsFrom
	}
	// A Next Step whose bounds fall inside a code or HTML block is not capped:
	// cutting there would split the block.
	from, to := bounds.NextStepStart, bounds.NextStepEnd
	if style == capNothing || doc.splitsBlock(from) || doc.splitsBlock(to) {
		from, to = 0, 0
	}
	// want follows how the result must read, as it is put together.
	var want outline
	head, moved, changed := capNextStep(doc, &want, bounds.StructureEnd, from, to, style, keeps)

	// What follows the structure is history, except what is always kept. The
	// text before the first heading stays with the structure, and the pointer
	// goes right after it, so text the agent appends later never becomes part
	// of the pointer.
	tail := splitAtPointerEnd(joinNested(doc.sections(bounds.StructureEnd, len(content))))
	offsets := make([]int, len(tail))
	for i, offset := 0, bounds.StructureEnd; i < len(tail); i++ {
		offsets[i], offset = offset, offset+len(tail[i].text)
	}
	rest := 0
	if len(tail) > 0 && tail[0].heading == "" && !tail[0].afterPointer {
		rest = 1
	}
	// Nor does the pointer go inside the settings span.
	for rest < len(tail) && settingsFrom < offsets[rest] && offsets[rest] < settingsTo {
		rest++
	}
	// What precedes the pointer is one piece of the Task.
	prefixEnd := len(content)
	if rest < len(tail) {
		prefixEnd = offsets[rest]
	}
	prefix := head + content[bounds.StructureEnd:prefixEnd]
	want.copied(doc, bounds.StructureEnd, prefixEnd, len(head))
	separator := blankLineAfter(prefix)
	// kept holds what is written of each section: everything but old pointers,
	// until history moves.
	kept := make([]string, len(tail))
	var history []int
	fixed := len(head)
	for i, section := range tail {
		switch {
		case section.generatedPointer:
			continue
		case section.heading == "" && !section.afterPointer, keeps(offsets[i], section):
			fixed += len(section.text)
		default:
			history = append(history, i)
		}
		kept[i] = section.text
	}
	gaps := newGapCount(tail, kept, rest)

	// Move the fewest oldest history sections for the Task, with the pointer
	// naming exactly what moves, to fit the target; if none fits, move all.
	keepFrom := len(history)
	keptSize := fixed + historyBytes(tail, history)
	listedBytes := 0
	for _, heading := range moved {
		listedBytes += len("- \n") + len(strings.TrimPrefix(heading, "## "))
	}
	basePointerSize := len(renderHistoryPointer(opts, 0, nil)) - 1 + len(separator)
	for n := 0; n <= len(history); n++ {
		if n > 0 {
			section := tail[history[n-1]]
			keptSize -= len(section.text)
			gaps.drop(history[n-1])
			listedBytes += len("- \n") + len(strings.TrimPrefix(section.label(), "## "))
		}
		if n == 0 && !changed {
			if len(content) <= opts.Target {
				keepFrom = 0
				break
			}
			continue
		}
		pointerSize := basePointerSize + len(strconv.Itoa(len(content)-keptSize))
		count := len(moved) + n
		if count > 0 {
			pointerSize += len(movedSectionsHeading) + listedBytes
		}
		if keptSize+gaps.total+pointerSize <= opts.Target {
			keepFrom = n
			break
		}
	}
	for _, i := range history[:keepFrom] {
		moved = append(moved, tail[i].label())
	}

	if keepFrom == 0 && !changed {
		if len(content) <= opts.Target {
			result.Reason = "already compact"
		} else {
			result.Reason = "protected content exceeds the target"
		}
		return result
	}

	var b strings.Builder
	b.WriteString(prefix + separator)
	pointer := renderHistoryPointer(opts, len(content)-keptSize, moved)
	// The pointer is a section, and the comment ending it a block.
	want.sections = append(want.sections, b.Len())
	endMarker := b.Len() + strings.LastIndex(pointer, taskHistoryEnd)
	want.blocks = append(want.blocks, blockSpan{endMarker, endMarker + len(taskHistoryEnd), true})
	b.WriteString(pointer)
	for i := rest; i < len(tail); i++ {
		if kept[i] == "" {
			continue
		}
		b.WriteString(gapBefore(b.String(), tail, kept, i))
		want.copied(doc, offsets[i], offsets[i]+len(kept[i]), b.Len())
		b.WriteString(kept[i])
	}
	compacted := b.String()
	if !readsAs(compacted, want) {
		result.Reason = readsDifferently
		return result
	}
	if activeSettings(compacted) != activeSettings(content) {
		result.Reason = "compacting would change the active review settings"
		return result
	}
	if len(compacted) >= len(content) {
		result.Reason = "protected content exceeds the target, and moving the rest would not shrink the Task"
		return result
	}
	result.Content, result.Compacted, result.After, result.Moved = compacted, true, len(compacted), moved
	result.ArchivePath = opts.ArchivePath
	return result
}

// gapBefore returns what is written between written and section i once what
// preceded it is gone (see separatorFor).
func gapBefore(written string, tail []taskSection, kept []string, i int) string {
	if i > 0 && kept[i-1] != "" {
		return ""
	}
	return separatorFor(written, tail[i])
}

// joinRule ends any list or quote above it.
const joinRule = "***\n\n"

// separatorFor returns what keeps an underlined title or a note, written after
// written rather than what preceded it, from continuing the paragraph, list or
// quote now above it: a blank line, and for an indented one, which a list
// would take in all the same, a rule that ends any.
func separatorFor(written string, section taskSection) string {
	if !section.mayJoin() {
		return ""
	}
	gap := blankLineAfter(written)
	// The history pointer's closing comment already ends whatever is above;
	// anything added after it would read as a note.
	if section.indented() && !strings.HasSuffix(written, pointerEnd) {
		gap += joinRule
	}
	return gap
}

// gapCount follows the size of the blank lines gapBefore adds as history is
// dropped, oldest first.
type gapCount struct {
	tail  []taskSection
	kept  []string
	sizes []int
	total int
	// previous is what is written before the section being dropped, found
	// by looking at each section once.
	previous string
	scanned  int
}

// pointerEnd is how the history pointer, written before every section counted,
// ends.
const pointerEnd = taskHistoryEnd + "\n"

func newGapCount(tail []taskSection, kept []string, rest int) *gapCount {
	g := &gapCount{tail: tail, kept: kept, sizes: make([]int, len(tail)), previous: pointerEnd, scanned: rest}
	previous := pointerEnd
	for i := rest; i < len(tail); i++ {
		if kept[i] != "" {
			g.set(i, previous)
			previous = kept[i]
		}
	}
	return g
}

func (g *gapCount) set(i int, written string) {
	size := len(gapBefore(written, g.tail, g.kept, i))
	g.total += size - g.sizes[i]
	g.sizes[i] = size
}

// drop stops writing section i, which follows every section dropped before.
func (g *gapCount) drop(i int) {
	g.kept[i] = ""
	g.total -= g.sizes[i]
	g.sizes[i] = 0
	for ; g.scanned < i; g.scanned++ {
		if g.kept[g.scanned] != "" {
			g.previous = g.kept[g.scanned]
		}
	}
	for next := i + 1; next < len(g.tail); next++ {
		if g.kept[next] != "" {
			g.set(next, g.previous)
			return
		}
	}
}

// outline is how a Task reads where compaction cares: where its sections
// start, and where its code and HTML blocks start and end and whether each is
// closed.
type outline struct {
	sections []int
	blocks   []blockSpan
	// What is copied is written in the order it is read, so each of these
	// moves forward through the headings and blocks once.
	heading, block int
}

// blockSpan is a block's place; end leaves out its trailing blank space, which
// grows and shrinks with the blank lines around it.
type blockSpan struct {
	start, end int
	closed     bool
}

const blankSpace = " \t\r\n"

// copied adds how d's bytes from from to to read once written at offset at.
func (o *outline) copied(d taskDocument, from, to, at int) {
	o.cut(d, from, to, at, -1)
}

// cut adds the same for bytes cut short at to: a block that runs on has to be
// closed, ending at end. A negative end says nothing is cut.
func (o *outline) cut(d taskDocument, from, to, at, end int) {
	for ; o.heading < len(d.headings) && d.headings[o.heading].start < to; o.heading++ {
		if h := d.headings[o.heading]; h.start >= from && h.startsSection() {
			o.sections = append(o.sections, at+h.start-from)
		}
	}
	for ; o.block < len(d.blocks) && d.blocks[o.block].start < to; o.block++ {
		// An indented code block can hide no heading, and is one or not
		// with what the lines above it are.
		if b := d.blocks[o.block]; b.start >= from && !b.indented {
			span := blockSpan{at + b.start - from, at + b.start - from + len(strings.TrimRight(d.content[b.start:b.end], blankSpace)), b.closed}
			if end >= 0 && b.end > to {
				span.end, span.closed = end, true
			}
			o.blocks = append(o.blocks, span)
		}
	}
}

// readsAs reports whether content reads as want says: no section or block lost,
// none made out of what was text, and no block left open that was not.
func readsAs(content string, want outline) bool {
	var got outline
	got.copied(readTask(content), 0, len(content), 0)
	return slices.Equal(got.sections, want.sections) && slices.Equal(got.blocks, want.blocks)
}

// settingsSpan returns the byte range of the section a continuation writes the
// review settings in, read as composition reads it, or -1, -1.
func settingsSpan(content string) (int, int) {
	lines := strings.Split(content, "\n")
	first, end := runtimeContextSection(lines)
	if first < 0 {
		return -1, -1
	}
	from := 0
	for _, line := range lines[:first] {
		from += len(line) + 1
	}
	to := from
	for _, line := range lines[first:end] {
		to += len(line) + 1
	}
	return from, min(to, len(content))
}

// activeSettings returns the text of the settings span, without the blank space
// that ends it.
func activeSettings(content string) string {
	from, to := settingsSpan(content)
	if from < 0 {
		return ""
	}
	return strings.TrimRight(content[from:to], " \t\r\n")
}

// blankLineAfter returns the newlines that put a blank line after text. It
// only adds: no byte of what precedes the pointer is ever removed.
func blankLineAfter(text string) string {
	switch {
	case strings.HasSuffix(text, "\n\n"), strings.HasSuffix(text, "\n\r\n"):
		return ""
	case strings.HasSuffix(text, "\n"):
		return "\n"
	}
	return "\n\n"
}

// capNextStep keeps the first 4 KiB of the Next Step group of a Task's
// structure, which ends at end: the bytes from from to to. Once cut, what
// follows in the group moves too: the group keeps a prefix, never fragments
// from beyond the limit. Sections keeps reports, given their offset, stay and
// do not count. It returns the structure, adding how it must read to want,
// what moved and whether anything did.
func capNextStep(doc taskDocument, want *outline, end, from, to int, style capStyle, keeps func(int, taskSection) bool) (string, []string, bool) {
	structure := doc.content[:end]
	var sections []taskSection
	var kept []bool
	size := 0
	if from < to {
		sections = doc.sections(from, to)
		kept = make([]bool, len(sections))
		for i, offset := 0, from; i < len(sections); i++ {
			if kept[i] = keeps(offset, sections[i]); !kept[i] {
				size += len(sections[i].text)
			}
			offset += len(sections[i].text)
		}
	}
	if size <= nextStepLimit {
		want.copied(doc, 0, end, 0)
		return structure, nil, false
	}
	want.copied(doc, 0, from, 0)
	var b strings.Builder
	var moved []string
	// The cut ends with a blank line, which counts against the limit, so that
	// another compaction finds the group within it. So can one separator
	// before a kept section: after the cut only kept sections are written, so
	// only the first follows text of the step's own.
	room := nextStepLimit - len("\n\n")
	for i, section := range sections {
		if kept[i] && section.mayJoin() {
			room -= len("\n\n" + joinRule)
			break
		}
	}
	cut, dropped := false, false
	// write adds text, which holds the first keep bytes of the section at
	// offset, and returns where it lands.
	write := func(section taskSection, offset, keep int, text string) int {
		if dropped {
			b.WriteString(separatorFor(b.String(), section))
		}
		at := from + b.Len()
		want.cut(doc, offset, offset+keep, at, at+len(strings.TrimRight(text, blankSpace)))
		b.WriteString(text)
		dropped = false
		return at
	}
	for i, offset := 0, from; i < len(sections); i++ {
		section, start := sections[i], offset
		offset += len(section.text)
		switch {
		case section.text == "":
		case kept[i]:
			write(section, start, len(section.text), section.text)
		case !cut && len(section.text) <= room:
			write(section, start, len(section.text), section.text)
			room -= len(section.text)
		// A section is only cut after all of its heading's lines.
		case !cut && room >= section.headingLength:
			keep, closer, opened := len(cutOutsideBlocks(section, room)), "", -1
			if style == capInside && nextStepLimit-room+keep < nextStepLimit/2 {
				// A long line or code block would waste most of the limit.
				keep, closer, opened = cutInside(section, room)
			}
			moved = append(moved, fmt.Sprintf("%s (beyond the first %d KiB)", section.label(), nextStepLimit/1024))
			at := write(section, start, keep, trimTrailingNewlines(section.text[:keep]+closer)+"\n\n")
			if opened >= 0 {
				// The line cut short is now a block of its own.
				text := strings.TrimRight(section.text[:keep]+closer, blankSpace)
				want.blocks = append(want.blocks, blockSpan{at + opened, at + len(text), true})
			}
			cut = true
		default:
			cut, dropped = true, true
			moved = append(moved, section.label())
		}
	}
	want.copied(doc, to, end, from+b.Len())
	return structure[:from] + b.String() + structure[to:], moved, true
}

func historyBytes(sections []taskSection, indexes []int) int {
	n := 0
	for _, i := range indexes {
		n += len(sections[i].text)
	}
	return n
}

// swallowsProtected reports whether the section's text below its heading
// holds, in a code or HTML block or anywhere it is not read as a heading, a
// title compaction would keep, typically because an agent left a block open
// and something later ended it. Such a section is kept whole. Only a clearly
// bounded example is not: a language-tagged code block whose first line is the
// heading, which also heads a real section, closed with no fence inside.
func swallowsProtected(section taskSection, topLevel map[string]bool) bool {
	var fences []markdownBlock
	for _, block := range section.blocks {
		if block.fence.char != 0 {
			fences = append(fences, block)
		}
	}
	example, atStart := false, false
	var paragraph []string
	for start := section.headingLength; start < len(section.text); start = lineEnd(section.text, start) {
		end := lineEnd(section.text, start)
		line := strings.TrimRight(section.text[start:end], "\r\n")
		// A paragraph underlined with dashes names a title too.
		switch text := strings.TrimSpace(line); {
		case text != "" && strings.Trim(text, "-") == "" && len(paragraph) > 0:
			if underlinesProtected(paragraph) {
				return true
			}
			paragraph = paragraph[:0]
		case text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, "```") || strings.HasPrefix(text, "~~~"):
			paragraph = paragraph[:0]
		default:
			paragraph = append(paragraph, text)
		}
		for len(fences) > 0 && fences[0].end <= start {
			fences = fences[1:]
		}
		var block *markdownBlock
		if len(fences) > 0 && fences[0].start < end {
			block = &fences[0]
		}
		// The closing line ends an example; one left behind any other way was
		// never closed.
		if example && (block == nil || block.start >= start) {
			return true
		}
		switch {
		case block == nil:
		case block.start >= start:
			atStart = true
			continue
		case block.closed && block.end == end:
			example = false
			continue
		default:
			if isFenceLine(line) {
				if example {
					return true
				}
				continue
			}
		}
		if heading, ok := h2Heading(strings.TrimSpace(line)); ok && (runtimeHeadings[heading] || isProtectedHeading(heading)) {
			if block == nil || !block.info || !atStart || !topLevel[heading] {
				return true
			}
			example = true
		}
		if strings.TrimSpace(line) != "" {
			atStart = false
		}
	}
	return example
}

// cutInside returns how many bytes of the section to keep, at most limit with
// what closes the block the cut leaves open, ending between characters. It
// returns that closing text too, and where a line cut short became a fence of
// its own, or -1.
func cutInside(section taskSection, limit int) (int, string, int) {
	prefix := section.cutLine(limit)
	closer, delimited, _ := section.closerAfter(prefix)
	if closer == "" {
		return len(prefix), "", -1
	}
	short := section.cutLine(limit - len("\n"+closer))
	if len(short) < delimited {
		// The block's delimiter and its closer do not both fit.
		return len(cutOutsideBlocks(section, limit)), "", -1
	}
	again, _, opened := section.closerAfter(short)
	if again == "" {
		return len(short), "", -1
	}
	// The closer is the very next line: a blank one would end a quote holding
	// the block, and the closer would open another.
	if !strings.HasSuffix(short, "\n") {
		again = "\n" + again
	}
	return len(short), again, opened
}

// cutLine returns at most limit bytes of the section, without a last line cut
// short inside its quote or list markers, which would end an inner one, or one
// that cut short would close the block holding it.
func (s taskSection) cutLine(limit int) string {
	prefix := runePrefix(s.text, limit)
	start := lineStart(prefix, len(prefix))
	short, whole := prefix[start:], strings.TrimRight(s.text[start:lineEnd(s.text, start)], "\r\n")
	if short != whole && strings.Trim(short, "> \t") == "" {
		return prefix[:start]
	}
	_, _, closes := closingFence(strings.TrimLeft(prefix[start:], " \t>"))
	_, _, did := closingFence(strings.TrimLeft(s.text[start:lineEnd(s.text, start)], " \t>"))
	if closes && !did {
		return prefix[:start]
	}
	return prefix
}

// closerAfter returns the line a prefix of the section needs after it to leave
// no block open, how long a prefix must be to hold the delimiter it closes,
// and where a line cut short opened that block, or -1.
func (s taskSection) closerAfter(prefix string) (closer string, delimited, opened int) {
	if block, open := s.leftOpen(len(prefix)); open {
		return block.closer, block.start + block.opening, -1
	}
	// A line cut short can open a fence the whole line, with a backtick after
	// its delimiter, does not.
	start := lineStart(prefix, len(prefix))
	line := prefix[start:]
	if !isFenceLine(line) || isFenceLine(s.text[start:lineEnd(s.text, start)]) {
		return "", 0, -1
	}
	trimmed := strings.TrimLeft(line, " ")
	indent := len(line) - len(trimmed)
	delimiter := trimmed[:len(trimmed)-len(strings.TrimLeft(trimmed, trimmed[:1]))]
	return line[:indent] + delimiter, start + indent + len(delimiter), start
}

// leftOpen returns the block a cut at offset at leaves needing a closing line.
func (s taskSection) leftOpen(at int) (markdownBlock, bool) {
	for _, block := range s.blocks {
		if block.start >= at {
			break
		}
		if at < block.end {
			// Only the block's own last line can already close it: one like it
			// above is part of the block.
			closed := block.closed && lineStart(s.text, at) == lineStart(s.text, block.end-1) && block.endsBy(s.text[:at])
			return block, block.closer != "" && !closed
		}
	}
	return markdownBlock{}, false
}

// runePrefix returns the longest prefix of text of at most limit bytes that
// does not split a character.
func runePrefix(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:max(limit, 0)]
}

// underlinesProtected reports whether an underlined paragraph names a section
// compaction keeps: by its last line, or by its text from the first line that
// can begin such a title, however it wraps.
func underlinesProtected(paragraph []string) bool {
	titles := []string{paragraph[len(paragraph)-1]}
	for i, line := range paragraph {
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "execution") || strings.HasPrefix(lower, "plan") || strings.HasPrefix(lower, "status:") {
			titles = append(titles, strings.Join(paragraph[i:], " "))
			break
		}
	}
	for _, title := range titles {
		if heading, _ := h2Heading("## " + title); runtimeHeadings[heading] || isProtectedHeading(heading) {
			return true
		}
	}
	return false
}

// cutOutsideBlocks returns the longest prefix of the section, ending at a line
// break outside any of its code and HTML blocks, that is at most limit bytes
// long. The blocks are those the whole Task was read with.
func cutOutsideBlocks(section taskSection, limit int) string {
	text := section.text
	if len(text) <= limit {
		return text
	}
	safe, blocks := 0, section.blocks
	for end := lineEnd(text, 0); end <= limit && text[end-1] == '\n'; end = lineEnd(text, end) {
		for len(blocks) > 0 && blocks[0].end <= end {
			blocks = blocks[1:]
		}
		if len(blocks) == 0 || blocks[0].start >= end {
			safe = end
		}
	}
	return text[:safe]
}

const movedSectionsHeading = "\nMoved sections:\n\n"

func renderHistoryPointer(opts TaskCompaction, movedBytes int, moved []string) string {
	var b strings.Builder
	b.WriteString(TaskHistoryHeading + "\n\n")
	fmt.Fprintf(&b, "Sandman moved %d bytes of earlier Task content to `%s` to keep this Task near %d bytes. That file holds the complete Task as it was before. It is history: read it only when a detail you need is missing here and in the live repository, pull request and checks.\n", movedBytes, opts.ArchivePath, opts.Target)
	if len(moved) > 0 {
		b.WriteString(movedSectionsHeading)
		for _, heading := range moved {
			fmt.Fprintf(&b, "- %s\n", strings.TrimPrefix(heading, "## "))
		}
	}
	b.WriteString("\n" + taskHistoryEnd + "\n")
	return b.String()
}
