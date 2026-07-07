package prompt

import (
	"strings"
	"testing"
)

func TestDefaultBadgePrompt_DirectsAgentToCreateControlFile(t *testing.T) {
	prompt := DefaultBadgePrompt()

	if !strings.Contains(prompt, ".sandman/state/.built_with_sandman") {
		t.Fatalf("expected badge prompt to mention the control file path .sandman/state/.built_with_sandman, got:\n%s", prompt)
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
	if !strings.Contains(bodySection, ".sandman/state/.built_with_sandman") {
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

func TestDefaultBadgePrompt_IdempotencyCheckIsDefenseInDepth(t *testing.T) {
	prompt := DefaultBadgePrompt()

	idempotencyIdx := strings.Index(prompt, "## Idempotency check")
	if idempotencyIdx < 0 {
		t.Fatalf("expected badge prompt to contain '## Idempotency check' section, got:\n%s", prompt)
	}
	nextSectionIdx := strings.Index(prompt[idempotencyIdx+1:], "\n## ")
	var sectionEnd int
	if nextSectionIdx < 0 {
		sectionEnd = len(prompt)
	} else {
		sectionEnd = idempotencyIdx + 1 + nextSectionIdx
	}
	section := prompt[idempotencyIdx:sectionEnd]

	lowered := strings.ToLower(section)
	if !strings.Contains(lowered, "defense-in-depth") && !strings.Contains(lowered, "defense in depth") {
		t.Errorf("expected idempotency section to frame the in-agent check as defense-in-depth, got section:\n%s", section)
	}

	authoritativeHookMarkers := []string{"badge_hook.go", "MaybeSuggestBadge", "authoritative gate", "post-batch hook", "Go-side hook"}
	found := false
	for _, m := range authoritativeHookMarkers {
		if strings.Contains(section, m) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected idempotency section to reference the authoritative Go-side hook (one of %v), got section:\n%s", authoritativeHookMarkers, section)
	}
}

func TestDefaultBadgePrompt_ControlFileOrderingMatchesPRCreation(t *testing.T) {
	prompt := DefaultBadgePrompt()

	prCreationIdx := strings.Index(prompt, "## PR creation")
	controlFileIdx := strings.Index(prompt, "## Control file")

	if prCreationIdx < 0 {
		t.Fatalf("expected badge prompt to contain '## PR creation' section, got:\n%s", prompt)
	}
	if controlFileIdx < 0 {
		t.Fatalf("expected badge prompt to contain '## Control file' section, got:\n%s", prompt)
	}
	if controlFileIdx <= prCreationIdx {
		t.Fatalf("expected '## Control file' section to appear after '## PR creation', got prCreation=%d controlFile=%d", prCreationIdx, controlFileIdx)
	}
}
