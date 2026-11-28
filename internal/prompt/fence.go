package prompt

import "strings"

// fence is the delimiter of a fenced code block: its character and length,
// whether an info string follows it, and the column the content of the list
// item holding the block starts at, or zero outside a list.
type fence struct {
	char   byte
	length int
	info   bool
	column int
}

// parseFence recognises a fence line, indented by at most three spaces.
func parseFence(line string) (fence, bool) {
	line = strings.TrimRight(line, "\r\n")
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

// parseListFence recognises a fence opened on a list item's own line, such as
// "- ```text" or "1. ```", through any number of nested markers.
func parseListFence(line string) (fence, bool) {
	rest := expandTabs(strings.TrimRight(line, "\r\n"))
	column := 0
	for {
		trimmed := strings.TrimLeft(rest, " ")
		spaces := len(rest) - len(trimmed)
		marker := listMarker(trimmed)
		if marker == 0 || column == 0 && spaces > 3 {
			break
		}
		content := strings.TrimLeft(trimmed[marker:], " ")
		gap := len(trimmed) - marker - len(content)
		if gap == 0 || gap > 4 {
			break
		}
		column += spaces + marker + gap
		rest = content
	}
	if column == 0 {
		return fence{}, false
	}
	f, ok := parseFence(rest)
	f.column = column
	return f, ok
}

// listMarker returns the length of the bullet or number that starts text.
func listMarker(text string) int {
	if text != "" && strings.IndexByte("-*+", text[0]) >= 0 {
		return 1
	}
	digits := 0
	for digits < len(text) && digits < 9 && text[digits] >= '0' && text[digits] <= '9' {
		digits++
	}
	if digits > 0 && digits < len(text) && (text[digits] == '.' || text[digits] == ')') {
		return digits + 1
	}
	return 0
}

// expandTabs writes tabs as the spaces up to the next fourth column.
func expandTabs(line string) string {
	if !strings.Contains(line, "\t") {
		return line
	}
	var b strings.Builder
	for _, r := range line {
		if r != '\t' {
			b.WriteRune(r)
			continue
		}
		b.WriteString(strings.Repeat(" ", 4-b.Len()%4))
	}
	return b.String()
}

// closedBy reports whether line closes the block f opened: the same character,
// at least as long, and no info string, indented as the block's list item is.
func (f fence) closedBy(line string) bool {
	if f.column > 0 {
		line = expandTabs(line)
		if len(line) < f.column || strings.TrimLeft(line[:f.column], " ") != "" {
			return false
		}
		line = line[f.column:]
	}
	marker, ok := parseFence(line)
	return ok && marker.char == f.char && marker.length >= f.length && !marker.info
}

// leftBy reports whether line ends the list item holding the block, and so the
// block: text indented less than the item's content.
func (f fence) leftBy(line string) bool {
	if f.column == 0 {
		return false
	}
	line = expandTabs(strings.TrimRight(line, "\r\n"))
	trimmed := strings.TrimLeft(line, " ")
	return trimmed != "" && len(line)-len(trimmed) < f.column
}

// fenceScanner follows fenced code blocks line by line, including those a list
// item holds. Every part of compaction reads code blocks through it.
type fenceScanner struct{ open fence }

// next reads a line and reports whether it belongs to a code block, delimiters
// included. openable says whether a fence outside a list may open a block here.
func (s *fenceScanner) next(line string, openable bool) bool {
	if s.open.char != 0 {
		if !s.open.leftBy(line) {
			if s.open.closedBy(line) {
				s.open = fence{}
			}
			return true
		}
		s.open = fence{}
	}
	if f, ok := parseListFence(line); ok {
		s.open = f
		return true
	}
	if f, ok := parseFence(line); ok && openable {
		s.open = f
		return true
	}
	return false
}

// fencedLines reports which lines belong to a fenced code block, delimiters
// included. A fence outside a list with no closer (a truncated log, say) is
// read as text, so it cannot swallow the headings after it; one in a list ends
// with its item.
func fencedLines(lines []string) []bool {
	// closable[i] reports whether a later line closes a block opened at i.
	closable := make([]bool, len(lines))
	longest := map[byte]int{} // the longest bare fence further down
	for i := len(lines) - 1; i >= 0; i-- {
		if f, ok := parseFence(lines[i]); ok {
			closable[i] = longest[f.char] >= f.length
			if !f.info {
				longest[f.char] = max(longest[f.char], f.length)
			}
		}
	}
	fenced := make([]bool, len(lines))
	var scanner fenceScanner
	for i, line := range lines {
		fenced[i] = scanner.next(line, closable[i])
	}
	return fenced
}

// openFence returns the delimiter of the code block text leaves open, or the
// zero fence.
func openFence(text string) fence {
	var scanner fenceScanner
	for line := range strings.SplitSeq(text, "\n") {
		scanner.next(line, true)
	}
	return scanner.open
}
