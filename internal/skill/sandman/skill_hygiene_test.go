package sandman

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	internalPackagePathRe  = regexp.MustCompile(`internal/(review|cmd|batch|daemon|skill|prompt|runid)/`)
	internalGoIdentifierRe = regexp.MustCompile(`processPR|MarkSeen|SelfPostStore|ParseTrigger|promotePendingComment|launchReview|RunSession|PrepareReviewRun|runid\.|batch\.Request`)
	sandmanPathRe          = regexp.MustCompile(`\.sandman/`)
	issueTrackerJargonRe   = regexp.MustCompile(`issue #\d+|PR #\d+|GitHub issue|triage|kanban|ready-for-agent`)
	ghCliInProseRe         = regexp.MustCompile(`gh (issue|pr|api|repo) (create|view|list|edit|comment|close)`)
)

func readSkillMarkdown(t *testing.T) map[string]string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	files := map[string]string{}
	walkErr := filepath.WalkDir(wd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		rel, err := filepath.Rel(wd, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk skill tree: %v", walkErr)
	}
	if len(files) == 0 {
		t.Fatalf("expected at least one .md file under %s", wd)
	}
	return files
}

func TestSkills_NoInternalPackagePaths(t *testing.T) {
	files := readSkillMarkdown(t)
	for path, text := range files {
		if loc := internalPackagePathRe.FindStringIndex(text); loc != nil {
			t.Errorf("%s contains forbidden internal package path %q at offset %d", path, text[loc[0]:loc[1]], loc[0])
		}
	}
}

func TestSkills_NoInternalGoIdentifiers(t *testing.T) {
	files := readSkillMarkdown(t)
	for path, text := range files {
		if loc := internalGoIdentifierRe.FindStringIndex(text); loc != nil {
			t.Errorf("%s contains forbidden internal Go identifier at offset %d: %q", path, loc[0], text[loc[0]:loc[1]])
		}
	}
}

func TestSkills_PreserveUserFacingPaths(t *testing.T) {
	files := readSkillMarkdown(t)
	hits := 0
	for path, text := range files {
		if sandmanPathRe.MatchString(text) {
			hits++
			t.Logf("%s preserves user-facing .sandman/ path", path)
		}
	}
	if hits == 0 {
		t.Fatalf("expected at least one .md to reference a .sandman/ path, found none across %d files", len(files))
	}
}

func TestSkills_NoIssueTrackerReferences(t *testing.T) {
	files := readSkillMarkdown(t)
	for path, text := range files {
		if loc := issueTrackerJargonRe.FindStringIndex(text); loc != nil {
			t.Errorf("%s contains forbidden issue tracker jargon %q at offset %d", path, text[loc[0]:loc[1]], loc[0])
		}
	}
}

func stripCodeFences(text string) string {
	var b strings.Builder
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func TestSkills_NoGhCliReferencesInProse(t *testing.T) {
	files := readSkillMarkdown(t)
	for path, text := range files {
		prose := stripCodeFences(text)
		if loc := ghCliInProseRe.FindStringIndex(prose); loc != nil {
			t.Errorf("%s contains forbidden gh CLI reference in prose %q at offset %d", path, prose[loc[0]:loc[1]], loc[0])
		}
	}
}

func TestSkills_ImplementSkillStillReadable(t *testing.T) {
	files := readSkillMarkdown(t)
	const target = "implement/SKILL.md"
	text, ok := files[target]
	if !ok {
		t.Fatalf("expected %s to exist under skill tree, found %d files", target, len(files))
	}
	lines := strings.Split(text, "\n")
	var descLine string
	var h1Line string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if descLine == "" && strings.HasPrefix(trimmed, "description:") {
			descLine = trimmed
		}
		if h1Line == "" && strings.HasPrefix(trimmed, "# ") {
			h1Line = trimmed
		}
	}
	if descLine == "" {
		t.Errorf("%s has no non-empty frontmatter description line", target)
		return
	}
	if !strings.Contains(text, "End-to-end automation for implementing") {
		t.Errorf("%s missing the entry-point signal phrase %q", target, "End-to-end automation for implementing")
	}
	if h1Line == "" {
		t.Errorf("%s has no H1 heading", target)
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(h1Line), "# implement") {
		t.Errorf("%s H1 %q does not start with the literal entry-point heading %q", target, h1Line, "# implement")
	}
}
