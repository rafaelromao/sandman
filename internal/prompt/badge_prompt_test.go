package prompt

import (
	"strings"
	"testing"
)

func TestDefaultBadgePrompt_DirectsAgentToCreateControlFile(t *testing.T) {
	prompt := DefaultBadgePrompt()

	if !strings.Contains(prompt, ".sandman/.built_with_sandman") {
		t.Fatalf("expected badge prompt to mention the control file path .sandman/.built_with_sandman, got:\n%s", prompt)
	}

	if !strings.Contains(prompt, "temp-file") {
		t.Errorf("expected badge prompt to instruct the agent to use atomic temp-file write semantics, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "rename") {
		t.Errorf("expected badge prompt to instruct the agent to use rename for atomic replacement, got:\n%s", prompt)
	}

	prCreationIdx := strings.Index(prompt, "## PR creation")
	if prCreationIdx < 0 {
		t.Fatalf("expected badge prompt to contain '## PR creation' section, got:\n%s", prompt)
	}
	bodySection := prompt[prCreationIdx:]
	if !strings.Contains(bodySection, ".built_with_sandman") {
		t.Errorf("expected control-file instruction to be in or after '## PR creation' section, got section:\n%s", bodySection)
	}
}

func TestDefaultBadgePrompt_PreservesExistingSections(t *testing.T) {
	prompt := DefaultBadgePrompt()

	for _, section := range []string{"# Built with Sandman badge", "## Idempotency check", "## Context", "## Instructions", "## Branch and commit", "## PR creation"} {
		if !strings.Contains(prompt, section) {
			t.Errorf("expected badge prompt to preserve %q section, got:\n%s", section, prompt)
		}
	}

	if !strings.Contains(prompt, "<!-- sandman-badge-pr -->") {
		t.Errorf("expected badge prompt to preserve the marker comment, got:\n%s", prompt)
	}
}
