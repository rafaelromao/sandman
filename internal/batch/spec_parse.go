package batch

import (
	"regexp"
	"strconv"
	"strings"
)

var issueRefPattern = regexp.MustCompile(`(?:/issues/(\d+)(?:#[^\s)\]]*)?|#(\d+)\b)`)

// ExtractIssueReferences returns the unique issue numbers referenced in the
// given text via `#N` shorthand or full issue URLs, preserving the order of
// first occurrence.
func ExtractIssueReferences(text string) []int {
	matches := issueRefPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(matches))
	var out []int
	for _, match := range matches {
		numberText := match[1]
		if numberText == "" {
			numberText = match[2]
		}
		number, err := strconv.Atoi(numberText)
		if err != nil {
			continue
		}
		if _, ok := seen[number]; ok {
			continue
		}
		seen[number] = struct{}{}
		out = append(out, number)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// specSearchToken returns the path-component form of the parent issue
// reference that GitHub search reliably matches (verified empirically:
// the full URL with scheme is tokenized into separate tokens and
// `gh issue list --search "<url> in:body"` returns no results).
func specSearchToken(parent int) string {
	return "issues/" + strconv.Itoa(parent)
}

// issueURLPattern matches full GitHub issue URLs of the form
// `https://<host>/<owner>/<repo>/issues/<n>` (any host).
var issueURLPattern = regexp.MustCompile(`/issues/(\d+)(?:\b|#)`)

var parentHeadingPattern = regexp.MustCompile(`(?im)^##\s+Parent\s*$`)
var nextHeadingPattern = regexp.MustCompile(`(?m)^\s*##\s`)

// ExtractParentReference parses the `## Parent` section of an issue body
// and returns the parent issue number if the section cites exactly one
// issue. The reference may be a `#N` shorthand or a full GitHub issue URL.
// Returns (0, false) if there is no `## Parent` section, no recognizable
// reference, or the section cites multiple distinct issues.
func ExtractParentReference(body string) (int, bool) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	idx := parentHeadingPattern.FindStringIndex(body)
	if idx == nil {
		return 0, false
	}
	afterHeading := body[idx[1]:]
	nextIdx := nextHeadingPattern.FindStringIndex(afterHeading)
	var section string
	if nextIdx != nil {
		section = afterHeading[:nextIdx[0]]
	} else {
		section = afterHeading
	}
	refs := ExtractIssueReferences(section)
	if len(refs) == 1 {
		return refs[0], true
	}
	if len(refs) > 1 {
		return 0, false
	}
	if m := issueURLPattern.FindStringSubmatch(section); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n, true
		}
	}
	return 0, false
}

// StripParentSection returns body with the `## Parent` H2 section
// (and any content until the next H2) removed. Used by the
// Specification gate so the parent backlink does not count as a
// child declaration. Returns the input unchanged when no `## Parent`
// heading is present.
func StripParentSection(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	idx := parentHeadingPattern.FindStringIndex(body)
	if idx == nil {
		return body
	}
	before := body[:idx[0]]
	after := body[idx[1]:]
	nextIdx := nextHeadingPattern.FindStringIndex(after)
	if nextIdx == nil {
		return before
	}
	return before + after[nextIdx[0]:]
}
