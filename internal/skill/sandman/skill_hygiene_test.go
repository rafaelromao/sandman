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