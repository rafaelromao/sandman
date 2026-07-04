package review

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// extractBodiesFromLog returns, in chronological order, every body the
// bot posted via `gh pr comment <N> --body <body>` in the run log at
// runLogPath. It is the structural replacement for the LLM-driven
// `record_review_posted` bash wrapper removed in #1757. The daemon is
// the only entity that both sees the run log and can decide which
// bodies are self-authored, so this is the right ownership boundary
// (issue #1759).
func extractBodiesFromLog(runLogPath string) ([]string, error) {
	f, err := os.Open(runLogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open run log: %w", err)
	}
	defer f.Close()

	var bodies []string
	var current *bodyAccumulator

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()
		stripped := stripLogContinuationPrefix(raw)
		if current != nil {
			// We're inside an open body. The closing quote may be
			// on this continuation line. Try to parse a body
			// continuation; if a closing quote is found, the body
			// closes and the remainder of the line (if any) is
			// discarded.
			cont, closed, err := parseBodyContinuation(current.quote, stripped)
			if err != nil {
				return nil, err
			}
			current.body += cont
			if closed {
				bodies = append(bodies, current.body)
				current = nil
			}
			continue
		}
		body, quote, open, err := parseGhPrCommentBody(stripped, lineNo)
		if err != nil {
			return nil, err
		}
		if open {
			current = &bodyAccumulator{body: body, quote: quote}
		} else if body != "" {
			bodies = append(bodies, body)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan run log: %w", err)
	}
	// If a body is still open at EOF, the closing quote was never
	// observed. Per the issue contract, return an explicit error
	// (no partial body returned).
	if current != nil {
		return nil, fmt.Errorf("unclosed body at end of log (line %d)", lineNo)
	}
	return bodies, nil
}

// bodyAccumulator is the in-progress body of a `gh pr comment --body`
// invocation that may span multiple log lines. quote is the opening
// quote character ('\'' or '"') that bounds the body.
type bodyAccumulator struct {
	body  string
	quote byte
}

// logLinePrefix is the standard run-log line prefix: "[<runID>] HH:MM:SS "
// (or just "HH:MM:SS " for continuation lines after the $ prompt has
// dropped). stripLogContinuationPrefix removes the prefix from a line
// and returns the payload. The detector does not require the bracketed
// run-id segment — multiline bodies produce lines that begin with
// only the timestamp.
var logLinePrefix = regexp.MustCompile(`^(?:\[[^\]]*\]\s+)?\d{2}:\d{2}:\d{2}\s+`)

// stripLogContinuationPrefix removes the run-log line prefix from
// raw and returns the payload. Lines that don't match the prefix are
// returned as-is.
func stripLogContinuationPrefix(raw string) string {
	return logLinePrefix.ReplaceAllString(raw, "")
}

// parseGhPrCommentBody scans stripped for a `gh pr comment` invocation
// and extracts the body. Returns:
//
//   - (body, quote, true, nil) when an opening quote is found and the
//     body is open across lines (the caller must keep accumulating
//     continuation lines until a closing quote is observed).
//   - (body, quote, false, nil) when the body is fully closed on this
//     line.
//   - ("", 0, false, nil) when the line contains no `gh pr comment` body.
//   - ("", 0, false, err) on parse error (unclosed quote with lineNo).
func parseGhPrCommentBody(stripped string, lineNo int) (string, byte, bool, error) {
	idx := strings.Index(stripped, "gh pr comment")
	if idx < 0 {
		return "", 0, false, nil
	}
	// Find --body or --body-file after the gh pr comment invocation.
	bodyIdx, _, hasBody, hasBodyFile := indexOfBodyFlag(stripped, idx)
	if hasBodyFile {
		// --body-file is parsed by the caller via a separate helper;
		// for the first slice we only handle --body. (Slice 2 covers
		// --body-file.)
		return "", 0, false, nil
	}
	if !hasBody {
		return "", 0, false, nil
	}
	rest := stripped[bodyIdx+len("--body"):]
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" {
		return "", 0, false, fmt.Errorf("unclosed body flag on line %d", lineNo)
	}
	switch rest[0] {
	case '\'':
		body, open, err := parseSingleQuoted(rest)
		return body, '\'', open, err
	case '"':
		body, open, err := parseDoubleQuoted(rest)
		return body, '"', open, err
	default:
		return "", 0, false, fmt.Errorf("unclosed body flag on line %d", lineNo)
	}
}

// indexOfBodyFlag finds the first --body or --body-file flag that
// follows position start in s. It returns the start index of the
// flag, a bool for each flag presence, and the index of the
// --body-file flag (or -1).
func indexOfBodyFlag(s string, start int) (int, int, bool, bool) {
	// Simple scan: walk the string and look for "--body" or
	// "--body-file" as standalone tokens (preceded by whitespace or
	// the start-of-string, followed by whitespace or end-of-string).
	bodyIdx := -1
	bodyFileIdx := -1
	for i := start; i < len(s); {
		if s[i] != '-' {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "--body-file") {
			prev := byte(' ')
			if i > 0 {
				prev = s[i-1]
			}
			next := byte(' ')
			if i+len("--body-file") < len(s) {
				next = s[i+len("--body-file")]
			}
			if isBoundary(prev) && isBoundary(next) && bodyFileIdx < 0 {
				bodyFileIdx = i
			}
			i += len("--body-file")
			continue
		}
		if strings.HasPrefix(s[i:], "--body") {
			prev := byte(' ')
			if i > 0 {
				prev = s[i-1]
			}
			next := byte(' ')
			if i+len("--body") < len(s) {
				next = s[i+len("--body")]
			}
			if isBoundary(prev) && isBoundary(next) && bodyIdx < 0 {
				bodyIdx = i
			}
			i += len("--body")
			continue
		}
		i++
	}
	return bodyIdx, bodyFileIdx, bodyIdx >= 0, bodyFileIdx >= 0
}

func isBoundary(b byte) bool {
	return b == ' ' || b == '\t' || b == 0
}

// parseSingleQuoted extracts the body from a `gh ... --body '...`
// invocation. Returns (body, open, err). The opening `'` is at rest[0].
// A closing `'` on the same line closes the body. A bare backslash
// before `'` does not escape — bash single quotes do not interpret
// escapes; we pass bytes through verbatim.
func parseSingleQuoted(rest string) (string, bool, error) {
	// rest[0] is the opening quote.
	for i := 1; i < len(rest); i++ {
		if rest[i] == '\'' {
			return rest[1:i], false, nil
		}
	}
	// No closing quote on this line — the body is open.
	return rest[1:], true, nil
}

// parseDoubleQuoted extracts the body from a `gh ... --body "..."`
// invocation. A `\"` is treated as a literal `"` (the daemon does not
// try to interpret the body — pass through verbatim).
func parseDoubleQuoted(rest string) (string, bool, error) {
	for i := 1; i < len(rest); i++ {
		switch rest[i] {
		case '\\':
			if i+1 < len(rest) && rest[i+1] == '"' {
				// Skip the escape.
				i++
				continue
			}
		case '"':
			return rest[1:i], false, nil
		}
	}
	return rest[1:], true, nil
}

// parseBodyContinuation processes one continuation line of an
// already-open body. The continuation may contain a closing quote
// (which closes the body) or be a fully-continuation line that
// appends raw text to the body. quote is the opening quote character
// observed when the body opened.
//
// Returns:
//   - (cont, true, nil) when a closing quote is observed; the body
//     closes with the bytes up to (but not including) the quote
//     prepended to cont (with a leading "\n" if cont is non-empty).
//   - (cont, false, nil) when no closing quote is observed; cont is
//     the whole trimmed line (a leading "\n" is prepended so the
//     caller can keep accumulating).
//   - ("", false, err) on parse error.
//
// The "\n" is the inter-line separator the log writer inserts between
// physical lines; the body bytes themselves are passed through
// verbatim (no escape interpretation, matching the spec).
func parseBodyContinuation(quote byte, stripped string) (string, bool, error) {
	trimmed := strings.TrimLeft(stripped, " \t")
	for i := 0; i < len(trimmed); i++ {
		switch quote {
		case '\'':
			if trimmed[i] == '\'' {
				return "\n" + trimmed[:i], true, nil
			}
		case '"':
			if trimmed[i] == '\\' && i+1 < len(trimmed) && trimmed[i+1] == '"' {
				i++
				continue
			}
			if trimmed[i] == '"' {
				return "\n" + trimmed[:i], true, nil
			}
		}
	}
	// No closing quote on this line; treat the whole line as body
	// continuation.
	return "\n" + trimmed, false, nil
}
