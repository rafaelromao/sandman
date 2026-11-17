package sandman

import (
	"strings"
	"testing"
)

// The tracker closes a work item from its closing reference only when the
// change request targets the default branch. The merge skill therefore closes
// the implementor's own work item itself after a verified merge into the
// configured base, before deleting the remote branch.
func TestPRMergeSkill_ClosesOwnWorkItemAfterVerifiedMergeIntoConfiguredBase(t *testing.T) {
	text := readSkillFile(t, "pr-merge/SKILL.md")

	ordered := []string{
		"5. Verify the PR actually merged.",
		"6. Close the implementor's own work item",
		"7. After verifying, delete the remote branch",
		"git push origin --delete <branch>",
		"8. If approval is not achieved after 10 review cycles",
	}
	last := -1
	for _, phrase := range ordered {
		at := strings.Index(text, phrase)
		if at < 0 {
			t.Fatalf("pr-merge skill is missing %q", phrase)
		}
		if at <= last {
			t.Fatalf("pr-merge skill places %q out of order", phrase)
		}
		last = at
	}

	for _, phrase := range []string{
		// Base check uses the merged change request's own base.
		"gh pr view <pr-number> --json state,mergedAt,baseRefName",
		"configured base branch",
		"do not close the work item; record the mismatch",
		// Own work item only, and only while it is still open.
		"Close only the implementor's own work item",
		"never another work item the change request references",
		"gh issue view <work-item-number> --json state --jq .state",
		"only while it is still open",
		// Exact comment shared with the runtime fallback.
		"Closed by sandman — merged in #<pr-number> into <base>.",
		`gh issue close <work-item-number> --comment "Closed by sandman — merged in #<pr-number> into <base>."`,
		// Read-back, one retry, then a durable blocker; the merge stands.
		"Read the state back",
		"retry the close once",
		"record the blocker and the exact close command in `.sandman/task.md` and the run log",
		"The merge result stands",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("pr-merge skill is missing the close contract %q", phrase)
		}
	}

	prose := stripCodeFences(text)
	for _, command := range []string{"gh issue close", "gh issue view", "gh pr view"} {
		if strings.Contains(prose, command) {
			t.Errorf("pr-merge skill shows %q outside a fenced block", command)
		}
	}
}
