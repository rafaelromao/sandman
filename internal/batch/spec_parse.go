package batch

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
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

// parentHeadingPattern matches H2 sections whose heading text
// declares a parent relationship. The matcher widens the original
// `^##\s+Parent\s*$` literal (which only matched the canonical
// `## Parent` heading) to two cases:
//   - any H2 containing the word "parent" (case-insensitive
//     substring), so `## Parent`, `## parent`, `## Parent area`,
//     and `## Parent spec` all match;
//   - any H2 starting the words "part of" (case-insensitive
//     substring), so `## Part of #N` matches the threeterm leaf
//     style where a leaf issue declares its parent area-spec under
//     `## Part of <spec>` (issue #305 → #232).
//
// H3-or-deeper sections (`### Parent area`) do not match because
// the anchor is two `#` characters. The case-insensitive flag lets
// `## parent`, `## Parent`, and `## Parent area` all match
// uniformly. See ADR-0042 for the original widening; the "part of"
// branch is the symmetric counterpart of the children-heading
// widening in ADR-0045, closing the verifier gap so that a
// threeterm-style area-spec body whose leaf rows carry `## Part of
// <spec>` rather than `## Parent` is still expanded.
var parentHeadingPattern = regexp.MustCompile(`(?im)^##\s+[^\n]*(?:parent|part of)[^\n]*\s*$`)
var nextHeadingPattern = regexp.MustCompile(`(?m)^\s*##\s`)

// ExtractParentReference parses the first H2 section of an issue body
// whose heading text contains the word "parent" and returns the
// parent issue number if that section cites exactly one issue. The
// reference may be a `#N` shorthand or a full GitHub issue URL.
// Returns (0, false) if there is no parent section, no recognizable
// reference, or the section cites multiple distinct issues.
//
// The matcher widened from `^##\s+Parent\s*$` to a substring match
// so headings like `## Parent area` and `## Parent spec` are
// recognised as parent sections; the eight `TestExtractParentReference`
// sub-cases pin the single-section, single-ref contract for `## Parent`,
// and that contract is preserved by this implementation. For
// multi-section or multi-ref verifications, callers should use
// `HasParentSectionBacklinkTo` instead.
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

// parentSectionReferences returns the unique issue references
// harvested from every H2 section of the body whose heading text
// contains the word "parent" (case-insensitive substring). The list
// preserves first-occurrence order across all matching sections; a
// reference that appears in multiple parent sections is reported
// only once. References may be `#N` shorthand or full GitHub issue
// URLs. Used by `HasParentSectionBacklinkTo`; exported for callers
// that want to inspect the matching set directly.
func parentSectionReferences(body string) []int {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	matches := headerLinePattern.FindAllStringIndex(body, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[int]struct{})
	var out []int
	for i, m := range matches {
		hStart, hEnd := m[0], m[1]
		headingText := strings.TrimSpace(body[hStart:hEnd])
		if !parentHeadingPattern.MatchString(headingText) {
			continue
		}
		var sectionEnd int
		if i+1 < len(matches) {
			sectionEnd = matches[i+1][0]
		} else {
			sectionEnd = len(body)
		}
		section := body[hEnd:sectionEnd]
		for _, n := range ExtractIssueReferences(section) {
			if n == 0 {
				continue
			}
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
		if refsFromURL := refsFromIssueURLPattern(section); len(refsFromURL) > 0 {
			for _, n := range refsFromURL {
				if _, ok := seen[n]; ok {
					continue
				}
				seen[n] = struct{}{}
				out = append(out, n)
			}
		}
	}
	return out
}

// refsFromIssueURLPattern returns issue numbers found via a bare
// `/issues/N` URL pattern inside `section`, used as a tiebreaker
// after `ExtractIssueReferences` to mirror `ExtractParentReference`'s
// URL-only fallback for sections that carry the parent-id as a
// direct URL with no `#N` shorthand nearby.
func refsFromIssueURLPattern(section string) []int {
	matches := issueURLPattern.FindAllStringSubmatch(section, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[int]struct{})
	var out []int
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// HasParentSectionBacklinkTo reports whether the candidate body has
// at least one parent-section H2 (heading text contains "parent",
// case-insensitive) whose extracted issue references include
// `parent`. Multi-ref parent sections are accepted when `parent` is
// among the references; refs across multiple parent sections are
// unioned before the membership check. The companion verifier that
// the spec resolver uses in place of the single-ref `## Parent`
// probe; see ADR-0042.
func HasParentSectionBacklinkTo(body string, parent int) bool {
	for _, n := range parentSectionReferences(body) {
		if n == parent {
			return true
		}
	}
	return false
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

// isBlockerHeading reports whether `heading` carries the canonical
// H2 text of a blocker section. The blocker vocabulary is owned by
// `internal/github.IsBlockedByHeading` so the spec candidate
// harvest and the dependency parse share a single source of truth.
func isBlockerHeading(heading string) bool {
	return github.IsBlockedByHeading(heading)
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
		if isBlockerHeading(sec.heading) {
			continue
		}
		if parentHeadingPattern.MatchString(sec.heading) {
			// Parent-style sections point upward and must not become
			// child candidates. Preserve the overlap case where a
			// qualified child heading also contains "parent" by
			// delegating its structured rows to the shared child parser.
			childRefs := github.ParseChildrenFromBody(sec.heading + "\n" + sec.content)
			for _, n := range childRefs {
				if n == 0 {
					continue
				}
				if _, ok := seen[n]; ok {
					continue
				}
				seen[n] = struct{}{}
				out = append(out, n)
			}
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
