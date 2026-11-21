package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPromptDocMatchesCanonicalPrompt(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "usage", "default-task-prompt.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read default prompt doc: %v", err)
	}

	got, err := extractPromptBlock(string(data))
	if err != nil {
		t.Fatal(err)
	}

	templatePath := filepath.Join("default-task-prompt.md")
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read default prompt template: %v", err)
	}

	want := strings.TrimSpace(string(template))
	if got != want {
		t.Fatalf("default prompt doc drifted\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestDefaultPromptRequiresMeaningfulImplementationPRDescription(t *testing.T) {
	data, err := os.ReadFile("default-task-prompt.md")
	if err != nil {
		t.Fatalf("read default task prompt: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"meaningful Summary and actual Verification evidence",
		"target repository's pull-request template",
		"preserve every required template section",
		"local checks, known failures or skips, and current-head CI",
		"Never invent a green result or present stale CI as current",
		"reference-and-footer-only body cannot pass",
		"creation, continuation, and body edits",
		"existing substantive description",
		"exactly one final linked Sandman footer",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("default task prompt is missing publication contract %q", required)
		}
	}
}

// The closing reference stays mandatory in the pull-request body, but Sandman
// never closes work items: it only links the implemented work item, and the
// repository decides whether the tracker closes it on merge. Closing keywords
// stay out of commit messages so that an issue does not close later when a
// feature branch lands on the default branch through a merge commit or rebase.
func TestDefaultPromptDescribesClosingReferenceAsALink(t *testing.T) {
	data, err := os.ReadFile("default-task-prompt.md")
	if err != nil {
		t.Fatalf("read default task prompt: %v", err)
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	for _, required := range []string{
		"the closing-reference body requirement (one of `Closes #<issue_number>`, `Fixes #<issue_number>`, `Resolves #<issue_number>`)",
		"The closing reference links the pull request to the work item it implements",
		"the repository decides whether the tracker closes it when the pull request merges",
		"Closing keywords (`Closes`, `Fixes`, `Resolves`) belong only in the pull-request body, never in commit messages",
		"through a merge commit or a rebase",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("default task prompt is missing closing-reference contract %q", required)
		}
	}
}

func extractPromptBlock(doc string) (string, error) {
	lines := strings.Split(doc, "\n")
	start := -1
	end := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "<!-- default-task-prompt:start -->" {
			start = i + 1
		}
		if trimmed == "<!-- default-task-prompt:end -->" {
			end = i
			break
		}
	}
	if start < 0 || end < 0 || end < start {
		return "", os.ErrInvalid
	}

	block := make([]string, 0, end-start)
	for _, line := range lines[start:end] {
		block = append(block, strings.TrimPrefix(line, "    "))
	}

	return strings.TrimSpace(strings.Join(block, "\n")), nil
}
