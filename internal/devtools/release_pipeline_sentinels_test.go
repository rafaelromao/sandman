// Package devtools hosts repository-wide sentinel tests that guard the
// post-implementation steady state. These are not unit tests of any
// production code path; they scan committed source for forbidden
// wording and fail loudly if a future change reintroduces it.
package devtools

import (
	"os/exec"
	"strings"
	"testing"
)

func TestReleasePipeline_AutoModeDoesNotLeak(t *testing.T) {
	repoRoot := gitTopLevel(t)

	// The sentinel tolerates its own bookkeeping, the historical
	// ADR-0022 (immutable once accepted), the index row that names
	// the rollback ADR by title, the run_test.go sentinel that
	// asserts the flags' absence, and the release-pipeline review.
	allowedFiles := map[string]struct{}{
		"internal/devtools/release_pipeline_sentinels_test.go": {},
		"docs/adr/0022-rename-ralph-to-auto-mode.md":            {}, // historical ADR, immutable
		"docs/adr/0041-rollback-auto-mode.md":                  {},
		"docs/adr/0041-cancel-auto-mode.md":                    {}, // legacy filename; sentinel intentionally tolerates
		"docs/adr/README.md":                                  {}, // index row names rollback ADR by title
		"internal/cmd/run_test.go":                            {}, // permanent-rollback absence sentinel
		"CHANGELOG.md":                                        {}, // the "### Removed" entry that records the rollback
		"docs/development/release-pipeline-review.md":          {},
		"scripts/check-no-auto-rollback.sh":                   {}, // shell mirror of this sentinel
		".sandman/state/2378.head_sha":                        {},
	}

	// Patterns whose reappearance in the live surface would mean the
	// rollback drifted. AGENTS.md is excluded because the wording
	// matches the regex's negative-test path legitimately.
	patterns := []string{
		"--auto",
		"--count ",
		"auto_max_count",
	}

	for _, pattern := range patterns {
		hits := gitGrep(t, repoRoot, []string{
			"--fixed-strings",
			"--ignore-case",
			"--line-number",
			"--",
			pattern,
		}, []string{".git", "node_modules", "AGENTS.md", ".sandman"}, allowedFiles)
		if len(hits) > 0 {
			t.Errorf("forbidden %q leaked into the live surface; matches:\n%s",
				pattern, strings.Join(hits, "\n"))
		}
	}
}

func gitGrep(t *testing.T, dir string, before []string, pathsToSkip []string, allowedFiles map[string]struct{}) []string {
	t.Helper()
	args := append([]string{"grep"}, before...)
	for _, skip := range pathsToSkip {
		args = append(args, ":(exclude)"+skip)
	}
	for allowed := range allowedFiles {
		args = append(args, ":(exclude)"+allowed)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if exitErr, ok := err.(*exec.ExitError); ok {
		// git grep exits 1 when there are no matches; that's a pass.
		if exitErr.ExitCode() == 1 {
			return nil
		}
		t.Fatalf("git grep %v in %s: %v\n", before, dir, err)
	}
	if err != nil {
		t.Fatalf("git grep %v in %s: %v\n", before, dir, err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func gitTopLevel(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimRight(string(out), "\n")
}
