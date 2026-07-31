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

// blockerHeadingPattern matches H2 sections whose heading text equals
// `Blocked by`, `Depends on`, or `Blocked-by` (case-insensitive on the
// heading text). The vocabulary mirrors `parseBlockedByHeading`'s
// recognised names in `internal/github/cli_client.go`, so the spec
// candidate harvest and the blocker parse share a single source of
// truth for what a "blocker" heading looks like.
var blockerHeadingPattern = regexp.MustCompile(`(?im)^##\s+(?:blocked\s+by|depends\s+on|blocked-by)\s*$`)

// IsBlockerHeading reports whether `heading` is the canonical H2
// text of a blocker section. `heading` may include leading
// whitespace and a trailing newline; only the heading text itself is
// matched. The case-insensitive vocabulary is reused from
// `parseBlockedByHeading`.
func IsBlockerHeading(heading string) bool {
	return blockerHeadingPattern.MatchString(heading)
}

// headerLinePattern consumes an entire H2 heading line up to (but
// not including) the trailing newline. `nextHeadingPattern` matches
// only `## ` at the start of a line, which leaves the heading text
// unconsumed; the walker uses this pattern to slice the line out
// in one shot so `headingText` carries the full heading title.
var headerLinePattern = regexp.MustCompile(`(?m)^\s*##\s+[^\n]+`)

// bodyReferencesOutsideBlockerSections walks the H2 sections of the
// given body in order and returns the unique issue references
// harvested from every section whose heading is not a blocker
// heading. References inside `## Blocked by`, `## Depends on`, and
// `## Blocked-by` sections are skipped — those names declare
// dependency edges, not children. Comment refs, native sub-issues,
// and the search fallback remain callers' responsibility.
//
// The harvest inside each non-blocker section is the same set the
// existing resolver path produced: every prose `#N` and `/issues/N`
// match. The existing helper `ExtractIssueReferences` is reused per
// section to keep the parser surface in sync. Bullet refs
// (`- #10`) and link refs (`- [text](url)`) are subsumed by
// `ExtractIssueReferences` because both forms contain either a bare
// `#N` shorthand or a `/issues/N` URL segment.
func bodyReferencesOutsideBlockerSections(body string) []int {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	type section struct {
		heading string
		content string
	}
	var sections []section
	matches := headerLinePattern.FindAllStringIndex(body, -1)
	if len(matches) == 0 {
		return ExtractIssueReferences(body)
	}
	for i, m := range matches {
		hStart, hEnd := m[0], m[1]
		headingText := strings.TrimSpace(body[hStart:hEnd])
		var sectionEnd int
		if i+1 < len(matches) {
			sectionEnd = matches[i+1][0]
		} else {
			sectionEnd = len(body)
		}
		sections = append(sections, section{heading: headingText, content: body[hEnd:sectionEnd]})
	}
	seen := make(map[int]struct{})
	var out []int
	for _, sec := range sections {
		if IsBlockerHeading(sec.heading) {
			continue
		}
		for _, n := range ExtractIssueReferences(sec.content) {
			if n == 0 {
				continue
			}
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}
