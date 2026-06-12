package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPromptDocMatchesCanonicalPrompt(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "usage", "default-prompt.md")
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

func extractPromptBlock(doc string) (string, error) {
	lines := strings.Split(doc, "\n")
	start := -1
	end := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "<!-- default-prompt:start -->" {
			start = i + 1
		}
		if trimmed == "<!-- default-prompt:end -->" {
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
