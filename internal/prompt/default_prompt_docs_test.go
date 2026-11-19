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
