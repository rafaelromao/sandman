package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readInstalledSkill syncs the embedded skill tree into a temporary home and
// returns the named skill's SKILL.md with whitespace collapsed, so contract
// phrases survive line wrapping.
func readInstalledSkill(t *testing.T, name string) string {
	t.Helper()
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, name, "SKILL.md"))
	if err != nil {
		t.Fatalf("read %s skill: %v", name, err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

// Sandman never closes a work item. The closing reference stays mandatory in
// the pull-request body, but only as the link that identifies the implemented
// work item: whether the tracker closes it on merge is the repository's own
// setting.
func TestImplementSkillDescribesClosingReferenceAsALink(t *testing.T) {
	text := readInstalledSkill(t, "implement")

	for _, want := range []string{
		// The exact closing-reference form and its post-create verification stay.
		"the platform's exact closing-reference line for the selected work item",
		"The first non-empty line of the returned body MUST match the platform's closing-reference form",
		"Use the platform's accepted closing-reference form",
		"A reference buried in prose or using a non-closing keyword is not acceptable",
		// The reference is a link, and the repository owns closure.
		"The reference links the implemented work item to the change request",
		"whether the tracker closes the work item when the change request merges is the repository's own setting",
		"The tracker's merge rules, not Sandman, decide whether a merged change request also closes its work item",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("implement skill missing %q", want)
		}
	}

	for _, banned := range []string{
		"so the work item closes when the change request merges",
		"automatically close its work item",
		"does not auto-close its work item is not acceptable",
		"no need to search for a closing change request",
	} {
		if strings.Contains(text, banned) {
			t.Errorf("implement skill must not claim closure on merge, still contains %q", banned)
		}
	}
}

// A closing keyword in a commit message can close the work item later than
// intended, for example when a feature branch lands on the default branch
// through a merge commit or a rebase. Keywords therefore live only in the PR
// body.
func TestSkillsForbidClosingKeywordsInCommitMessages(t *testing.T) {
	implement := readInstalledSkill(t, "implement")
	for _, want := range []string{
		"Closing keywords belong only in the pull-request body",
		"Never put a closing keyword (`Closes`, `Fixes`, `Resolves`, or any variant) in a commit message",
		"when a feature branch lands on the default branch through a merge commit or a rebase",
		"Commit messages never contain closing keywords",
	} {
		if !strings.Contains(implement, want) {
			t.Errorf("implement skill missing the commit-message rule %q", want)
		}
	}

	run := readInstalledSkill(t, "run")
	for _, want := range []string{
		"Closing keywords (`Closes`, `Fixes`, `Resolves`) belong only in the pull-request body, never in commit messages",
		"`sandman-implement` owns the closing-reference rule",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("run skill missing the commit-message rule %q", want)
		}
	}
}

// The usage documentation tells operators that Sandman never closes work items
// and where closure is actually decided, so teams that keep issues open after
// merge know which setting to change.
func TestUsageDocsExplainSandmanNeverClosesWorkItems(t *testing.T) {
	for path, required := range map[string][]string{
		filepath.Join("..", "..", "docs", "usage", "workflows.md"): {
			"## Work item closure",
			"Sandman never closes work items",
			"has no way to close an issue",
			"links the pull request to the work item it implements",
			"Auto-close issues with merged linked pull requests",
			"Settings → General → Issues",
			"closing keywords only in the pull-request body, never in commit messages",
			"through a merge commit or a rebase",
		},
		filepath.Join("..", "..", "docs", "usage", "skills.md"): {
			"Sandman never closes work items",
			"never in commit messages",
			"workflows.md#work-item-closure",
		},
		filepath.Join("..", "..", "docs", "usage", "default-task-prompt.md"): {
			"Sandman never closes work items",
			"workflows.md#work-item-closure",
		},
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(data)), " ")
		for _, want := range required {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}
}
