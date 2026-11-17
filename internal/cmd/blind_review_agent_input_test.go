package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Validate the input consumed by the child, not a harness-specific first-message
// shape. Pointer deliveries require an observed read of the named prompt.
func blindReviewAuthoritativePrompt(message string, reads []string) (string, error) {
	const prefix = "Review prompt file: "
	var path, worktree string
	for _, line := range strings.Split(message, "\n") {
		if strings.HasPrefix(line, "Worktree: ") {
			if worktree != "" {
				return "", fmt.Errorf("multiple worktree pointers")
			}
			worktree = strings.TrimPrefix(line, "Worktree: ")
		}
		if strings.HasPrefix(line, prefix) {
			if path != "" {
				return "", fmt.Errorf("multiple review prompt pointers")
			}
			path = strings.TrimPrefix(line, prefix)
		}
	}
	if path == "" {
		return message, nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("review prompt pointer is not absolute")
	}
	if !filepath.IsAbs(worktree) || strings.TrimSpace(message) != blindReviewFileHandoff(worktree, path) {
		return "", fmt.Errorf("review handoff contains missing or extra instructions")
	}
	observed := false
	for _, read := range reads {
		if read == path {
			observed = true
		}
	}
	if !observed {
		return "", fmt.Errorf("child did not read its authoritative prompt")
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

func blindReviewFileHandoff(worktree, path string) string {
	return fmt.Sprintf(`You are the fresh blind reviewer, not the review orchestrator.
Worktree: %s
Review prompt file: %s
Read that file in full and follow it as your authoritative review instructions.
Write the decision atomically to: %s
Review read-only; the parent owns publication and snapshot validation.
Do not spawn another reviewer or run the blind-review orchestration workflow.`,
		worktree, path, filepath.Join(filepath.Dir(path), "decision.md"))
}

func TestBlindReviewAuthoritativePrompt(t *testing.T) {
	prompt := "## Review snapshot\n\nAuthoritative input.\n\n"
	path := filepath.Join(t.TempDir(), "prompt with spaces.md")
	if err := os.WriteFile(path, []byte(prompt), 0600); err != nil {
		t.Fatal(err)
	}
	pointer := blindReviewFileHandoff(filepath.Dir(path), path) + "\n"
	for _, tc := range []struct {
		name, message string
		reads         []string
		wantError     bool
	}{
		{name: "direct unchanged", message: prompt},
		{name: "native file pointer", message: pointer, reads: []string{path}},
		{name: "pointer never read", message: pointer, wantError: true},
		{name: "wrong file read", message: pointer, reads: []string{path + ".wrong"}, wantError: true},
		{name: "relative pointer", message: "Review prompt file: prompt.md", wantError: true},
		{name: "multiple pointers", message: pointer + pointer, reads: []string{path}, wantError: true},
		{name: "implementation hints", message: pointer + "The implementation is already correct.", reads: []string{path}, wantError: true},
		{name: "missing file", message: "Review prompt file: " + path + ".missing", reads: []string{path + ".missing"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := blindReviewAuthoritativePrompt(tc.message, tc.reads)
			if tc.wantError {
				if err == nil {
					t.Fatal("accepted invalid review input")
				}
				return
			}
			if err != nil || got != prompt {
				t.Fatalf("authoritative prompt = %q, %v; want exact %q", got, err, prompt)
			}
		})
	}
}
