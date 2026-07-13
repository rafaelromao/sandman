package batch

import (
	"regexp"
	"strings"
)

// acceptanceHeadingRe matches a `## Acceptance criteria` line, case
// insensitive, with optional trailing whitespace. It is intentionally
// narrow: only the h2 form counts, because that is the level the
// issue template uses for AC sections.
var acceptanceHeadingRe = regexp.MustCompile(`(?im)^\s*##\s+acceptance\s+criteria\s*$`)

// anyHeadingRe matches any `## ...` heading; used to bound the
// acceptance-criteria section so paragraphs after a sibling `##`
// heading are not included in the AC set.
var anyHeadingRe = regexp.MustCompile(`(?m)^\s*##\s+`)

// acBulletRe matches `- [ ]` or `- [x]` bullets (case-insensitive x).
// The capture group is the bullet body, trimmed of leading whitespace.
var acBulletRe = regexp.MustCompile(`(?m)^\s*-\s*\[[ xX]\]\s*(.+?)\s*$`)

// goTestRunRe matches a shell line that begins with `go test -run`.
// The T1 oracle uses the match as a fingerprint; the full line is
// passed through unchanged.
var goTestRunRe = regexp.MustCompile(`(?i)^\s*go\s+test\s+-run\b`)

// ParseAcceptanceCriteria extracts the `go test -run` lines from the
// `## Acceptance criteria` section of an issue body. Anything outside
// the section, or that doesn't look like a `go test -run` line, is
// ignored. An empty result is meaningful: it means the issue did not
// declare ACs and the T1 oracle should report `No signal` rather than
// attempting to run an empty test plan.
func ParseAcceptanceCriteria(body string) []string {
	headingIdx := acceptanceHeadingRe.FindStringIndex(body)
	if headingIdx == nil {
		return nil
	}
	after := body[headingIdx[1]:]
	nextIdx := anyHeadingRe.FindStringIndex(after)
	section := after
	if nextIdx != nil {
		section = after[:nextIdx[0]]
	}
	matches := acBulletRe.FindAllStringSubmatch(section, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		line := strings.TrimSpace(m[1])
		if !goTestRunRe.MatchString(line) {
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseSandmanEvidence extracts `ok: <cmd> -> <sentinel>` lines from
// a fenced ` ```sandman-evidence ` block in the issue body. It is the
// T3 transitional fallback's input format. The block must use the
// `sandman-evidence` info-string; any other info-string is ignored so
// general code blocks are not consumed.
func ParseSandmanEvidence(body string) []EvidenceLine {
	fenceRe := regexp.MustCompile("(?s)```sandman-evidence\\s*\\n(.*?)```")
	match := fenceRe.FindStringSubmatch(body)
	if match == nil {
		return nil
	}
	block := match[1]
	okRe := regexp.MustCompile(`(?m)^\s*ok:\s*(.+?)\s*->\s*(.+?)\s*$`)
	matches := okRe.FindAllStringSubmatch(block, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]EvidenceLine, 0, len(matches))
	for _, m := range matches {
		out = append(out, EvidenceLine{Command: strings.TrimSpace(m[1]), Sentinel: strings.TrimSpace(m[2])})
	}
	return out
}

// EvidenceLine is one `ok: <cmd> -> <sentinel>` line from a
// `sandman-evidence` block. The T3 oracle replays each Command inside
// a sandbox and asserts the Sentinel appears in the combined output.
type EvidenceLine struct {
	Command  string
	Sentinel string
}
