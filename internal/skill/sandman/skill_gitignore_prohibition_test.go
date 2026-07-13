package sandman

import (
	"strings"
	"testing"
)

// TestSkillImplementSkill_ForbidsGitAddOnSandmanDir asserts that the
// user-facing sandman-implement skill contains an explicit instruction
// forbidding `git add` (with or without -f) on any path under `.sandman/`.
// This is acceptance criterion 3 of issue #2148: "The skill prompt for
// sandman-implement and friends explicitly says never to `git add` (even
// with `-f`) any path under `.sandman/`."
func TestSkillImplementSkill_ForbidsGitAddOnSandmanDir(t *testing.T) {
	files := readSkillMarkdown(t)
	const target = "implement/SKILL.md"
	text, ok := files[target]
	if !ok {
		t.Fatalf("expected %s to exist under skill tree", target)
	}

	lower := strings.ToLower(text)
	if !strings.Contains(lower, "git add") {
		t.Fatalf("%s must mention `git add` somewhere", target)
	}

	sandmanIdx := strings.Index(lower, ".sandman/")
	if sandmanIdx == -1 {
		t.Fatalf("%s must mention `.sandman/`", target)
	}

	window := lower[max(0, sandmanIdx-200):min(len(lower), sandmanIdx+200)]
	if !strings.Contains(window, "git add") && !strings.Contains(window, "staging") && !strings.Contains(window, "stage") {
		t.Fatalf("%s proximity window around `.sandman/` must mention `git add` or staging, got:\n---\n%s\n---", target, window)
	}

	if !strings.Contains(lower, "do not run `git add`") {
		t.Fatalf("%s must contain an explicit sentence forbidding `git add` on `.sandman/` paths", target)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
