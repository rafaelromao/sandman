package prompt

import (
	"bytes"
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

// markdownBlock is a fenced code or HTML block, the blocks whose lines are not
// Markdown.
type markdownBlock struct {
	start, end int    // its lines
	closed     bool   // nothing more is needed to end it
	closer     string // the line that ends it, for a block left open
	fence      fence  // zero for an HTML block
	info       bool   // a fenced block with an info string
}

// fence is the delimiter of a fenced code block: its character and length, and
// the column it starts at, past any list or block quote markers.
type fence struct {
	char   byte
	length int
	column int
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
	for node := parent; node != nil && node.Kind() != ast.KindDocument; node = node.Parent() {
		if depth++; depth >= maxContainerDepth {
			return nil, parser.NoChildren
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
	_, segment := reader.PeekLine()
	state := p.BlockParser.Continue(node, reader, context)
	if state&parser.Continue == 0 && node.Kind() == ast.KindFencedCodeBlock {
		node.SetAttributeString(closedAt, segment.Stop)
	}
	return state
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
	// fences holds the bare fence lines of each character in order, each with
	// the longest such fence from it on.
	fences map[byte][]fenceLine
	// html holds, by HTML block type 1 to 5, where the lines with the type's
	// end condition start.
	html [6][]int
}

type fenceLine struct{ start, longest int }

// htmlEnds are the end conditions of HTML block types 2 to 5.
var htmlEnds = [6]string{2: "-->", 3: "?>", 4: ">", 5: "]]>"}

var rawTags = []string{"script", "pre", "style", "textarea"}

func holdsHTMLEnd(line string, kind int) bool {
	if kind != 1 {
		return strings.Contains(line, htmlEnds[kind])
	}
	if !strings.Contains(line, "</") {
		return false
	}
	lower := strings.ToLower(line)
	for _, tag := range rawTags {
		if strings.Contains(lower, "</"+tag+">") {
			return true
		}
	}
	return false
}

func findClosers(content string) *blockClosers {
	c := &blockClosers{content: content, fences: map[byte][]fenceLine{}}
	for start := 0; start < len(content); start = lineEnd(content, start) {
		line := content[start:lineEnd(content, start)]
		if f, info, ok := parseFence(line); ok && !info {
			c.fences[f.char] = append(c.fences[f.char], fenceLine{start, f.length})
		}
		for kind := 1; kind < len(c.html); kind++ {
			if holdsHTMLEnd(line, kind) {
				c.html[kind] = append(c.html[kind], start)
			}
		}
	}
	for _, lines := range c.fences {
		for i := len(lines) - 2; i >= 0; i-- {
			lines[i].longest = max(lines[i].longest, lines[i+1].longest)
		}
	}
	return c
}

// ends reports whether something ends the top-level block node, opened by line
// at offset. A Sandman marker line does not end a comment it did not open:
// what an agent or an issue left open is text.
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
		next := sort.Search(len(lines), func(n int) bool { return lines[n].start > start })
		return next < len(lines) && lines[next].longest >= length
	case *ast.HTMLBlock:
		kind := int(node.HTMLBlockType)
		if kind >= len(c.html) {
			return true // types 6 and 7 end at a blank line
		}
		lines := c.html[kind]
		next := sort.SearchInts(lines, start)
		return next < len(lines) && (lines[next] == start || !isSandmanMarker(c.content[lines[next]:lineEnd(c.content, lines[next])]))
	}
	return true
}

func isSandmanMarker(line string) bool {
	line = strings.TrimSpace(line)
	return line == taskHistoryEnd || strings.HasPrefix(line, "<!-- sandman:")
}

// parseFence recognises a fence line, indented by at most three spaces, and
// reports whether an info string follows the delimiter.
func parseFence(line string) (f fence, info, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || trimmed[0] != '`' && trimmed[0] != '~' {
		return fence{}, false, false
	}
	char := trimmed[0]
	length := 0
	for length < len(trimmed) && trimmed[length] == char {
		length++
	}
	rest := trimmed[length:]
	if length < 3 || char == '`' && strings.Contains(rest, "`") {
		return fence{}, false, false
	}
	return fence{char: char, length: length}, strings.TrimSpace(rest) != "", true
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

// readMarkdown reads content as CommonMark does, subject to context.
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
	// A paragraph cannot begin as an ATX heading does, so only an underlined
	// title lacks the number signs.
	if rest := strings.TrimLeft(body, "#"); len(rest) == len(body) || strings.TrimLeft(rest, " \t\r\n") == rest && rest != "" {
		// An underlined title behind a quote or list marker does not begin its
		// line.
		if at-start > indent || node.Lines().Len() == 0 {
			return
		}
		heading.underlined = true
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
	// The closing line repeats what holds the block, with spaces for list
	// markers.
	prefix := strings.Map(func(r rune) rune {
		if r == '>' || r == ' ' || r == '\t' {
			return r
		}
		return ' '
	}, first[:column])
	block := markdownBlock{
		start:  start,
		end:    lineEnd(d.content, start),
		closer: prefix + strings.Repeat(first[column:column+1], length),
		fence:  fence{char: first[column], length: length, column: column},
		info:   node.Info != nil,
	}
	if n := node.Lines().Len(); n > 0 {
		block.end = lineEnd(d.content, node.Lines().At(n-1).Start)
	}
	if stop, ok := node.AttributeString(closedAt); ok {
		block.end, block.closed = stop.(int), true
	}
	d.blocks = append(d.blocks, block)
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
		block.closer = htmlEnds[kind]
		if kind == 1 {
			lower := strings.ToLower(first)
			for _, tag := range rawTags {
				if strings.Contains(lower, "<"+tag) {
					block.closer = "</" + tag + ">"
					break
				}
			}
		}
	}
	d.blocks = append(d.blocks, block)
}

// splitsBlock reports whether cutting at the line starting at offset would cut
// a code or HTML block in two.
func (d taskDocument) splitsBlock(offset int) bool {
	i := sort.Search(len(d.blocks), func(n int) bool { return d.blocks[n].end > offset })
	return i < len(d.blocks) && d.blocks[i].start < offset
}

// boundaries returns the headings that start sections: every H2 an ATX line
// writes, inside a list too, and every underlined one outside lists and block
// quotes.
func (d taskDocument) boundaries(from, to int) []markdownHeading {
	var out []markdownHeading
	i := sort.Search(len(d.headings), func(n int) bool { return d.headings[n].start >= from })
	for _, h := range d.headings[i:] {
		if h.start >= to {
			break
		}
		if h.level == 2 && (!h.underlined || h.topLevel) {
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
	for _, h := range d.boundaries(offset+1, len(d.content)) {
		if !h.underlined {
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

// openBlock returns the fenced code or HTML block that text, read as
// CommonMark does, leaves open.
func openBlock(content string) (markdownBlock, bool) {
	doc := readMarkdown(content, parser.NewContext())
	if n := len(doc.blocks); n > 0 && !doc.blocks[n-1].closed && doc.blocks[n-1].end == len(content) {
		return doc.blocks[n-1], true
	}
	return markdownBlock{}, false
}

// openFence returns the delimiter of the code block text leaves open, or the
// zero fence.
func openFence(content string) fence {
	block, _ := openBlock(content)
	return block.fence
}
