package prompt

import (
	"strings"
	"testing"
)

// TestDefaultBadgePrompt_DocumentsControlFileSidecarAuthoritatively pins
// the new contract (issue #2195): the post-batch badge sidecar in
// internal/batch/badge_hook.go is the sole authority on the control
// file. The agent prompt documents this rather than asking the agent
// to write the file itself.
func TestDefaultBadgePrompt_DocumentsControlFileSidecarAuthoritatively(t *testing.T) {
	prompt := DefaultBadgePrompt()

	controlFileIdx := strings.Index(prompt, "## Control file")
	if controlFileIdx < 0 {
		t.Fatalf("expected badge prompt to contain '## Control file' section, got:\n%s", prompt)
	}

	section := prompt[controlFileIdx:]
	if !strings.Contains(section, ".sandman/state/.built_with_sandman") {
		t.Errorf("expected '## Control file' section to mention the control file path, got section:\n%s", section)
	}
	if !strings.Contains(section, "post-batch badge sidecar") {
		t.Errorf("expected '## Control file' section to attribute control-file authorship to the sidecar (not the agent), got section:\n%s", section)
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

func TestDefaultBadgePrompt_AppendsBadgeAfterExistingContent(t *testing.T) {
	prompt := DefaultBadgePrompt()
	instructionsStart := strings.Index(prompt, "## Instructions")
	instructionsEnd := strings.Index(prompt, "## Branch and commit")
	if instructionsStart < 0 || instructionsEnd <= instructionsStart {
		t.Fatalf("expected badge prompt to contain an Instructions section before Branch and commit, got:\n%s", prompt)
	}
	instructions := prompt[instructionsStart:instructionsEnd]

	for _, want := range []string{
		"Preserve all existing content.",
		"Append the badge HTML after the final existing content.",
		"Separate the existing content from the badge with one blank line.",
		"Ensure `README.md` ends with a newline.",
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("expected existing README instructions to contain %q, got:\n%s", want, instructions)
		}
	}

	for _, legacy := range []string{"directly under the H1", "very first line of the file"} {
		if strings.Contains(instructions, legacy) {
			t.Errorf("expected existing README instructions to omit legacy placement %q, got:\n%s", legacy, instructions)
		}
	}
}

func TestDefaultBadgePrompt_ScaffoldPlacesBadgeAfterAbout(t *testing.T) {
	prompt := DefaultBadgePrompt()
	scaffoldInstruction := strings.Index(prompt, "Create `README.md` containing:")
	if scaffoldInstruction < 0 {
		t.Fatalf("expected badge prompt to contain the README scaffold instruction, got:\n%s", prompt)
	}
	blockStart := strings.Index(prompt[scaffoldInstruction:], "```markdown\n")
	if blockStart < 0 {
		t.Fatalf("expected README scaffold to have an opening fence, got:\n%s", prompt[scaffoldInstruction:])
	}
	blockStart += scaffoldInstruction + len("```markdown\n")
	blockEnd := strings.Index(prompt[blockStart:], "```")
	if blockEnd < 0 {
		t.Fatalf("expected README scaffold to have a closing fence, got:\n%s", prompt[blockStart:])
	}
	scaffold := prompt[blockStart : blockStart+blockEnd]

	aboutHeading := strings.Index(scaffold, "## About")
	aboutCopy := strings.Index(scaffold, "A description of this project and what it does.")
	badge := strings.Index(scaffold, `<a href="https://github.com/rafaelromao/sandman">`)
	if aboutHeading < 0 || aboutCopy <= aboutHeading || badge <= aboutCopy {
		t.Fatalf("expected README scaffold to place the badge after the About section, got:\n%s", scaffold)
	}
	if !strings.HasSuffix(strings.TrimSpace(scaffold), "</a>") {
		t.Errorf("expected badge to be the final README scaffold content, got:\n%s", scaffold)
	}
	if !strings.HasSuffix(strings.TrimRight(scaffold, " \t"), "\n") {
		t.Errorf("expected README scaffold to end with a newline, got %q", scaffold)
	}
}

// TestDefaultBadgePrompt_IdempotencyCheckIsDefenseInDepth pins the
// shared contract documented in internal/batch/badge_hook.go: the
// in-agent idempotency check is defense-in-depth and the post-batch
// hook is the authoritative gate.
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

// TestDefaultBadgePrompt_IdempotencyCheckUsesPaginatedGhApi pins the
// new pagination contract (issue #2195): the in-agent check uses
// `gh api --paginate` so it walks every page just like the Go-side
// hook. The hand-rolled `gh pr list --json --limit 100` shape that
// silently stopped after page 1 is gone.
func TestDefaultBadgePrompt_IdempotencyCheckUsesPaginatedGhApi(t *testing.T) {
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

	if !strings.Contains(section, "gh api --paginate") {
		t.Errorf("expected idempotency section to use `gh api --paginate` so the check matches the Go-side hook, got section:\n%s", section)
	}
	if strings.Contains(section, "gh pr list --limit 100") {
		t.Errorf("expected idempotency section to NOT use the legacy single-shot `gh pr list --limit 100` (see issue #2195), got section:\n%s", section)
	}
}

// TestDefaultBadgePrompt_ControlFileOrderingMatchesPRCreation pins
// the rendering order so a future refactor cannot silently let the
// control-file discussion slip into the wrong place relative to the
// PR creation instructions.
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
