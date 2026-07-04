package sandman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readAGENTSMD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "..", "AGENTS.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func skillContentConstraintsSection(t *testing.T, text string) string {
	t.Helper()
	const heading = "## Skill content constraints"
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatalf("AGENTS.md must contain heading %q (issue #1761); full text:\n%s", heading, text)
	}
	rest := text[start+len(heading):]
	const nextHeading = "\n## "
	if end := strings.Index(rest, nextHeading); end >= 0 {
		return rest[:end]
	}
	return rest
}

func TestAGENTS_SkillContentConstraintsSectionExists(t *testing.T) {
	text := readAGENTSMD(t)
	if !strings.Contains(text, "## Skill content constraints") {
		t.Fatalf("AGENTS.md must contain the top-level heading %q (issue #1761); full text:\n%s", "## Skill content constraints", text)
	}
}

func TestAGENTS_SkillContentConstraintsIsTopLevel(t *testing.T) {
	text := readAGENTSMD(t)
	const heading = "## Skill content constraints"
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatalf("AGENTS.md must contain heading %q (issue #1761); full text:\n%s", heading, text)
	}

	prefix := text[:start]
	if idx := strings.LastIndex(prefix, "\n"); idx >= 0 {
		prevLine := strings.TrimSpace(prefix[idx+1:])
		if strings.HasPrefix(prevLine, "- ") || strings.HasPrefix(prevLine, "* ") {
			t.Errorf("Skill content constraints heading must not be buried inside a list; preceding line is a list item: %q", prevLine)
		}
		if strings.HasPrefix(prevLine, "  - ") || strings.HasPrefix(prevLine, "  * ") {
			t.Errorf("Skill content constraints heading must not be buried inside a nested list; preceding line is a nested list item: %q", prevLine)
		}
	}

	if start >= 0 {
		lineStart := start
		for lineStart > 0 && text[lineStart-1] != '\n' {
			lineStart--
		}
		lineEnd := strings.Index(text[lineStart:], "\n")
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += lineStart
		}
		line := text[lineStart:lineEnd]
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			t.Errorf("Skill content constraints heading line must not be indented; got: %q", line)
		}
	}
}

func TestAGENTS_SkillContentConstraintsSectionHasExpectedClauses(t *testing.T) {
	text := readAGENTSMD(t)
	section := skillContentConstraintsSection(t, text)

	required := []struct {
		name    string
		substr  string
		meaning string
	}{
		{
			name:    "internals prohibition names internal Go package paths",
			substr:  "internal/",
			meaning: "section forbids referencing Sandman internals by Go package path",
		},
		{
			name:    "internals prohibition names internal Go identifiers",
			substr:  "processPR",
			meaning: "section names example internal Go identifiers (e.g., processPR) to make the prohibition unambiguous",
		},
		{
			name:    "internals prohibition names SelfPostStore",
			substr:  "SelfPostStore",
			meaning: "section names SelfPostStore as an example internal Go identifier",
		},
		{
			name:    "tracker prohibition names issue numbers",
			substr:  "issue numbers",
			meaning: "section forbids referring to GitHub issue numbers in skill prose",
		},
		{
			name:    "tracker prohibition names triage vocabulary",
			substr:  "triage vocabulary",
			meaning: "section forbids kanban/triage jargon in skill prose",
		},
		{
			name:    "catch site names sandman-self-review",
			substr:  "sandman-self-review",
			meaning: "section names sandman-self-review as a catch site",
		},
		{
			name:    "catch site names sandman-pr-review",
			substr:  "sandman-pr-review",
			meaning: "section names sandman-pr-review as a catch site",
		},
		{
			name:    "regression net references the docguard tests",
			substr:  "internal/skill/sandman/skill_hygiene_test.go",
			meaning: "section points at the docguard tests added in the upstream scrub slices",
		},
	}
	for _, r := range required {
		if !strings.Contains(section, r.substr) {
			t.Errorf("%s: missing %q in Skill content constraints section; full section:\n%s", r.meaning, r.substr, section)
		}
	}
}
