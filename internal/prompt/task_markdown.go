package prompt

import (
	"bytes"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// A Task is read with a CommonMark parser, for the byte offsets of its
// headings and blocks only: nothing is rendered, and what compaction writes is
// sliced from the original bytes.

const (
	// maxContainerDepth bounds the block quotes and lists held one inside
	// another, past which their markers are text: every line costs the depth
	// it is read at, so a line of markers would take quadratic time.
	maxContainerDepth = 64
	// maxReferenceParagraph bounds the paragraphs link reference definitions
	// are read from, which costs the square of their lines.
	maxReferenceParagraph = 64
)

// markdownHeading is a heading that begins its line, indented by at most three
// spaces. Headings after a block quote or list marker are not recorded.
type markdownHeading struct {
	start, end int // its lines
	level      int
	underlined bool
	topLevel   bool     // not inside a block quote or list
	title      []string // the lines of an underlined title
}

// markdownBlock is a code or HTML block, the blocks whose lines are not
// Markdown.
type markdownBlock struct {
	start, end int    // its lines
	opening    int    // bytes from start through its opening delimiter
	closed     bool   // a line of its own, or none, ends it
	closer     string // the line that ends it, for a block cut short
	fence      fence  // zero for any other block
	info       bool   // a fenced block with an info string
	htmlKind   int    // an HTML block's type, 1 to 5, when it needs a closer
	indented   bool   // an indented code block
}

// endsBy reports whether prefix, which cuts the block's last line, already
// holds enough of that line to end the block.
func (b markdownBlock) endsBy(prefix string) bool {
	at := lineStart(prefix, len(prefix))
	line := prefix[at:]
	if b.fence.char == 0 {
		// An HTML block can end on the line that opens it, which a marker
		// further down does not.
		return (at <= b.start || !isSandmanMarker(line)) && holdsHTMLEnd(line, b.htmlKind)
	}
	if at <= b.start {
		return false
	}
	char, length, ok := closingFence(strings.TrimLeft(line, "> \t"))
	return ok && char == b.fence.char && length >= b.fence.length
}

// fence is the delimiter of a fenced code block: its character and length.
type fence struct {
	char   byte
	length int
}

type taskDocument struct {
	content  string
	headings []markdownHeading
	blocks   []markdownBlock
}

var markdownParser = parser.NewParser(
	parser.WithBlockParsers(
		util.Prioritized(parser.NewSetextHeadingParser(), 100),
		util.Prioritized(parser.NewThematicBreakParser(), 200),
		util.Prioritized(containerParser{parser.NewListParser()}, 300),
		util.Prioritized(parser.NewListItemParser(), 400),
		util.Prioritized(parser.NewCodeBlockParser(), 500),
		util.Prioritized(parser.NewATXHeadingParser(), 600),
		util.Prioritized(leafParser{parser.NewFencedCodeBlockParser()}, 700),
		util.Prioritized(containerParser{parser.NewBlockquoteParser()}, 800),
		util.Prioritized(leafParser{parser.NewHTMLBlockParser()}, 900),
		util.Prioritized(parser.NewParagraphParser(), 1000),
	),
	parser.WithParagraphTransformers(util.Prioritized(shortReferences{}, 100)),
)

// parseMarkdown is a variable so tests can make the parser fail.
var parseMarkdown = func(source []byte, context parser.Context) ast.Node {
	return markdownParser.Parse(text.NewReader(source), parser.WithContext(context))
}

// containerParser stops block quotes and lists nesting past maxContainerDepth.
type containerParser struct{ parser.BlockParser }

func (p containerParser) Open(parent ast.Node, reader text.Reader, context parser.Context) (ast.Node, parser.State) {
	depth := 0
	for node := parent; node != nil; node = node.Parent() {
		if kind := node.Kind(); kind == ast.KindList || kind == ast.KindBlockquote {
			if depth++; depth >= maxContainerDepth {
				return nil, parser.NoChildren
			}
		}
	}
	return p.BlockParser.Open(parent, reader, context)
}

// closedAt is the attribute holding the offset a fenced block's closing line
// ends at, which the parser does not keep.
const closedAt = "sandman-closed-at"

// leafParser records where fenced code blocks are closed and, when closers is
// set, reads a top-level fenced code or HTML block that nothing ends as text,
// so a truncated log cannot swallow the sections after it.
type leafParser struct{ parser.BlockParser }

var closersKey = parser.NewContextKey()

func (p leafParser) Open(parent ast.Node, reader text.Reader, context parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	number, position := reader.Position()
	node, state := p.BlockParser.Open(parent, reader, context)
	if node == nil || parent.Kind() != ast.KindDocument {
		return node, state
	}
	if closers, ok := context.Get(closersKey).(*blockClosers); ok && !closers.ends(node, line, position.Start) {
		reader.SetPosition(number, position)
		return nil, parser.NoChildren
	}
	return node, state
}

func (p leafParser) Continue(node ast.Node, reader text.Reader, context parser.Context) parser.State {
	line, segment := reader.PeekLine()
	if html, ok := node.(*ast.HTMLBlock); ok && context.Get(closersKey) != nil && passesMarker(html, string(line), reader.Source()) {
		node.Lines().Append(segment)
		reader.AdvanceToEOL()
		return parser.Continue | parser.NoChildren
	}
	state := p.BlockParser.Continue(node, reader, context)
	if state&parser.Continue == 0 && node.Kind() == ast.KindFencedCodeBlock {
		node.SetAttributeString(closedAt, segment.Stop)
	}
	return state
}

// passesMarker reports whether line is a Sandman marker line that would end a
// block it did not open, and so is part of it.
func passesMarker(node *ast.HTMLBlock, line string, source []byte) bool {
	kind := int(node.HTMLBlockType)
	if kind >= len(htmlEnds) || !isSandmanMarker(line) || !holdsHTMLEnd(line, kind) {
		return false
	}
	// A block its first line ends closes here, before this line is read.
	if node.Lines().Len() != 1 {
		return true
	}
	first := node.Lines().At(0)
	return !holdsHTMLEnd(string(first.Value(source)), kind)
}

type shortReferences struct{}

func (shortReferences) Transform(node *ast.Paragraph, reader text.Reader, context parser.Context) {
	if node.Lines().Len() <= maxReferenceParagraph {
		parser.LinkReferenceParagraphTransformer.Transform(node, reader, context)
	}
}

// blockClosers indexes the lines that could end a top-level block.
type blockClosers struct {
	content string
	// fences holds the bare fence lines of each character, each with the
	// longest such fence from it on.
	fences map[byte]*fenceLines
	// html holds, by HTML block type 1 to 5, the lines with the type's end
	// condition, but for Sandman's marker lines.
	html [6]lineList
}

type fenceLines struct {
	lineList
	longest []int
}

// lineList holds where some lines start, in order. The parser reads forward,
// so a cursor finds the next of them without searching.
type lineList struct {
	starts []int
	cursor int
}

// from returns the index of the first line starting at or after offset.
func (l *lineList) from(offset int) int {
	if l.cursor > 0 && l.starts[l.cursor-1] >= offset {
		l.cursor = 0
	}
	for l.cursor < len(l.starts) && l.starts[l.cursor] < offset {
		l.cursor++
	}
	return l.cursor
}

// closingFence returns the delimiter of a line that would close a top-level
// fenced code block, by the parser's own test: its blank space is ASCII's.
func closingFence(line string) (char byte, length int, ok bool) {
	width, at := util.IndentWidth([]byte(line[:min(len(line), 4)]), 0)
	if width > 3 || at >= len(line) || line[at] != '`' && line[at] != '~' {
		return 0, 0, false
	}
	for at+length < len(line) && line[at+length] == line[at] {
		length++
	}
	return line[at], length, length >= 3 && util.IsBlank([]byte(line[at+length:]))
}

// rawTagEnd is the parser's end condition of HTML block type 1.
var rawTagEnd = regexp.MustCompile(`(?i)^.*</(?:script|pre|style|textarea)>.*`)

// htmlEnds are the end conditions of HTML block types 2 to 5.
var htmlEnds = [6]string{2: "-->", 3: "?>", 4: ">", 5: "]]>"}

var rawTags = []string{"script", "pre", "style", "textarea"}

// htmlOpeners are the lengths of the openers of HTML block types 2 to 5.
var htmlOpeners = [6]int{2: len("<!--"), 3: len("<?"), 4: len("<!"), 5: len("<![CDATA[")}

func holdsHTMLEnd(line string, kind int) bool {
	if kind != 1 {
		return strings.Contains(line, htmlEnds[kind])
	}
	return strings.Contains(line, "</") && rawTagEnd.MatchString(strings.TrimRight(line, "\r\n"))
}

func findClosers(content string) *blockClosers {
	c := &blockClosers{content: content, fences: map[byte]*fenceLines{'`': {}, '~': {}}}
	for start := 0; start < len(content); start = lineEnd(content, start) {
		line := content[start:lineEnd(content, start)]
		if char, length, ok := closingFence(line); ok {
			lines := c.fences[char]
			lines.starts = append(lines.starts, start)
			lines.longest = append(lines.longest, length)
		}
		if isSandmanMarker(line) {
			continue
		}
		for kind := 1; kind < len(c.html); kind++ {
			if holdsHTMLEnd(line, kind) {
				c.html[kind].starts = append(c.html[kind].starts, start)
			}
		}
	}
	for _, lines := range c.fences {
		for i := len(lines.longest) - 2; i >= 0; i-- {
			lines.longest[i] = max(lines.longest[i], lines.longest[i+1])
		}
	}
	return c
}

// ends reports whether something ends the top-level block node, opened by line
// at offset. A Sandman marker line does not end a comment it did not open (see
// leafParser.Continue): what an agent or an issue left open is text.
func (c *blockClosers) ends(node ast.Node, line []byte, offset int) bool {
	start := lineStart(c.content, offset)
	switch node := node.(type) {
	case *ast.FencedCodeBlock:
		i := bytes.IndexAny(line, "`~")
		if i < 0 {
			return true
		}
		length := 0
		for i+length < len(line) && line[i+length] == line[i] {
			length++
		}
		lines := c.fences[line[i]]
		next := lines.from(start + 1)
		return next < len(lines.starts) && lines.longest[next] >= length
	case *ast.HTMLBlock:
		kind := int(node.HTMLBlockType)
		if kind >= len(c.html) {
			return true // types 6 and 7 end at a blank line
		}
		return holdsHTMLEnd(string(line), kind) || c.html[kind].from(start+1) < len(c.html[kind].starts)
	}
	return true
}

func isSandmanMarker(line string) bool {
	line = strings.TrimSpace(line)
	return line == taskHistoryEnd || strings.HasPrefix(line, "<!-- sandman:")
}

// isFenceLine recognises a line written as a code fence, indented by at most
// three spaces.
func isFenceLine(line string) bool {
	line = strings.TrimRight(line, "\r\n")
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || trimmed[0] != '`' && trimmed[0] != '~' {
		return false
	}
	length := len(trimmed) - len(strings.TrimLeft(trimmed, trimmed[:1]))
	return length >= 3 && (trimmed[0] != '`' || !strings.Contains(trimmed[length:], "`"))
}

// lineStart returns the offset of the line holding offset.
func lineStart(content string, offset int) int {
	return strings.LastIndexByte(content[:offset], '\n') + 1
}

// lineEnd returns the offset after the line holding offset.
func lineEnd(content string, offset int) int {
	if nl := strings.IndexByte(content[offset:], '\n'); nl >= 0 {
		return offset + nl + 1
	}
	return len(content)
}

// readTask reads a Task for compaction: a top-level block that nothing ends is
// text.
func readTask(content string) taskDocument {
	context := parser.NewContext()
	context.Set(closersKey, findClosers(content))
	return readMarkdown(content, context)
}

// readMarkdown reads content as CommonMark does, but for what context asks.
func readMarkdown(content string, context parser.Context) taskDocument {
	doc := taskDocument{content: content}
	root := parseMarkdown([]byte(content), context)
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.Heading:
			doc.addHeading(node)
		case *ast.CodeBlock:
			doc.addIndentedCode(node)
		case *ast.FencedCodeBlock:
			doc.addFence(node)
		case *ast.HTMLBlock:
			doc.addHTML(node)
		}
		return ast.WalkContinue, nil
	})
	return doc
}

func (d *taskDocument) addHeading(node *ast.Heading) {
	at := node.Pos()
	if at < 0 || at >= len(d.content) {
		return
	}
	start := lineStart(d.content, at)
	line := d.content[start:lineEnd(d.content, at)]
	body := strings.TrimLeft(line, " ")
	indent := len(line) - len(body)
	if indent > 3 {
		return
	}
	heading := markdownHeading{start: start, end: lineEnd(d.content, at), level: node.Level, topLevel: node.Parent().Kind() == ast.KindDocument}
	// An underlined heading is placed where its title's first line is, an ATX
	// one before its title, at its number signs.
	heading.underlined = node.Lines().Len() > 0 && node.Lines().At(0).Start == at
	if !heading.underlined {
		// One behind a quote or list marker does not begin its line, and a
		// section needs a name h2Heading reads.
		if _, named := h2Heading(line); !strings.HasPrefix(body, "#") || node.Level == 2 && !named {
			return
		}
	} else {
		if at-start > indent {
			return // behind a quote or list marker
		}
		for i := range node.Lines().Len() {
			segment := node.Lines().At(i)
			heading.title = append(heading.title, strings.TrimSpace(d.content[segment.Start:segment.Stop]))
		}
		last := node.Lines().At(node.Lines().Len() - 1)
		heading.end = lineEnd(d.content, lineEnd(d.content, last.Start))
	}
	d.headings = append(d.headings, heading)
}

func (d *taskDocument) addFence(node *ast.FencedCodeBlock) {
	at := node.Pos()
	if at < 0 || at >= len(d.content) {
		return
	}
	start := lineStart(d.content, at)
	first := d.content[start:lineEnd(d.content, start)]
	column := strings.IndexAny(first, "`~")
	if column < 0 {
		return
	}
	length := 0
	for column+length < len(first) && first[column+length] == first[column] {
		length++
	}
	block := markdownBlock{
		start:   start,
		end:     lineEnd(d.content, start),
		opening: column + length,
		closer:  closerPrefix(first[:column]) + strings.Repeat(first[column:column+1], length),
		fence:   fence{char: first[column], length: length},
		info:    node.Info != nil,
	}
	if n := node.Lines().Len(); n > 0 {
		block.end = lineEnd(d.content, node.Lines().At(n-1).Start)
	}
	if stop, ok := node.AttributeString(closedAt); ok {
		block.end, block.closed = stop.(int), true
	}
	d.blocks = append(d.blocks, block)
}

// addIndentedCode records an indented code block, which needs no closing line.
func (d *taskDocument) addIndentedCode(node *ast.CodeBlock) {
	n := node.Lines().Len()
	if n == 0 {
		return
	}
	d.blocks = append(d.blocks, markdownBlock{
		start:    lineStart(d.content, node.Lines().At(0).Start),
		end:      lineEnd(d.content, node.Lines().At(n-1).Start),
		closed:   true,
		indented: true,
	})
}

func (d *taskDocument) addHTML(node *ast.HTMLBlock) {
	at := node.Pos()
	if at < 0 || at >= len(d.content) || node.Lines().Len() == 0 {
		return
	}
	start := lineStart(d.content, at)
	first := d.content[start:lineEnd(d.content, start)]
	block := markdownBlock{start: start, end: lineEnd(d.content, node.Lines().At(node.Lines().Len()-1).Start), closed: true}
	if kind := int(node.HTMLBlockType); kind < len(htmlEnds) {
		if node.HasClosure() {
			block.end = lineEnd(d.content, node.ClosureLine.Start)
		}
		block.closed = node.HasClosure() || holdsHTMLEnd(first, kind)
		at := max(strings.IndexByte(first, '<'), 0)
		end, opener := htmlEnds[kind], htmlOpeners[kind]
		if kind == 1 {
			lower := strings.ToLower(first)
			for _, tag := range rawTags {
				if strings.Contains(lower, "<"+tag) {
					end, opener = "</"+tag+">", len("<"+tag)
					break
				}
			}
		}
		block.htmlKind, block.opening = kind, at+opener
		block.closer = closerPrefix(first[:at]) + end
	}
	d.blocks = append(d.blocks, block)
}

// closerPrefix is what a block's closing line starts with, given what precedes
// its opening on the first line: the same, with spaces for list markers.
func closerPrefix(before string) string {
	return strings.Map(func(r rune) rune {
		if r == '>' || r == ' ' || r == '\t' {
			return r
		}
		return ' '
	}, before)
}

// splitsBlock reports whether cutting at the line starting at offset would cut
// a code or HTML block in two.
func (d taskDocument) splitsBlock(offset int) bool {
	i := sort.Search(len(d.blocks), func(n int) bool { return d.blocks[n].end > offset })
	return i < len(d.blocks) && d.blocks[i].start < offset
}

// startsSection reports whether a heading starts a section: every H2 an ATX
// line writes, inside a list too, and every underlined one outside lists and
// block quotes.
func (h markdownHeading) startsSection() bool {
	return h.level == 2 && (!h.underlined || h.topLevel)
}

// boundaries returns the headings that start sections.
func (d taskDocument) boundaries(from, to int) []markdownHeading {
	var out []markdownHeading
	i := sort.Search(len(d.headings), func(n int) bool { return d.headings[n].start >= from })
	for _, h := range d.headings[i:] {
		if h.start >= to {
			break
		}
		if h.startsSection() {
			out = append(out, h)
		}
	}
	return out
}

// holdsOtherHeading reports whether a heading in the range ends a section
// without being an ATX H2: an H1 or an underlined one.
func (d taskDocument) holdsOtherHeading(from, to int) bool {
	i := sort.Search(len(d.headings), func(n int) bool { return d.headings[n].start >= from })
	for _, h := range d.headings[i:] {
		if h.start >= to {
			break
		}
		if h.level == 1 || h.underlined {
			return true
		}
	}
	return false
}

// nextATXSection returns where the next section an ATX heading starts after
// offset, or the end of the content.
func (d taskDocument) nextATXSection(offset int) int {
	i := sort.Search(len(d.headings), func(n int) bool { return d.headings[n].start > offset })
	for _, h := range d.headings[i:] {
		if h.level == 2 && !h.underlined {
			return h.start
		}
	}
	return len(d.content)
}

// atxSectionAt reports whether the section starting at offset is the ATX one
// named heading, or any ATX one when heading is empty.
func (d taskDocument) atxSectionAt(offset int, heading string) bool {
	for _, h := range d.boundaries(offset, offset+1) {
		if !h.underlined {
			name, _ := h2Heading(d.content[h.start:h.end])
			return heading == "" || name == heading
		}
	}
	return false
}
