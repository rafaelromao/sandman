package sandman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readSkillFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestStandaloneRunComposesCapabilitiesAndOwnsItsTaskContract(t *testing.T) {
	text := readSkillFile(t, "run/SKILL.md")
	for _, required := range []string{
		"name: sandman-run",
		"## Execution Checklist",
		"## Next Step",
		"## Continuation Freshness Guard",
		"## AFK Rule",
		"sandman-implement",
		"sandman-review-cycle",
		"sandman-pr-merge",
		"PR-created checkpoint",
		"REVIEW_TIMEOUT",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("sandman-run contract missing %q", required)
		}
	}
	if strings.Index(text, "sandman-implement") >= strings.Index(text, "sandman-review-cycle") ||
		strings.Index(text, "sandman-review-cycle") >= strings.Index(text, "sandman-pr-merge") {
		t.Fatal("sandman-run must compose implementation, review cycle, then merge in order")
	}
	for _, forbidden := range []string{
		"internal/prompt",
		".sandman/events.jsonl",
		"canonical review-registration",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("sandman-run must not depend on managed implementation detail %q", forbidden)
		}
	}
}

func TestRouterDispatchesStandaloneRunAndCompatibilityReview(t *testing.T) {
	text := readSkillFile(t, "SKILL.md")
	for _, required := range []string{
		"`run` -> `sandman-run`",
		"`pr-review` -> `sandman-pr-review`",
		"## Capabilities",
		"sandman-review-request",
		"sandman-review-cycle",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("router missing dispatch %q", required)
		}
	}
	if strings.Contains(text, "`review-cycle` ->") || strings.Contains(text, "`review-request` ->") {
		t.Fatal("focused review skills must be documented as capabilities, not lifecycle modes")
	}
}

func TestReviewRequestIsOneShotAndStateless(t *testing.T) {
	text := readSkillFile(t, "review-request/SKILL.md")
	for _, required := range []string{
		"name: sandman-review-request",
		"current pull request",
		"live head",
		"read-only trigger-delivery guard",
		"exactly one",
		"structured confirmed-request envelope",
		"server timestamp",
		"delivery refusal",
		"Do not poll",
		"Do not create, edit, or delete review state",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-request contract missing %q", required)
		}
	}
	for _, forbidden := range []string{"sandman-review-cycle", "review-wait-v1.sh"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Errorf("review-request must not own standalone review behavior %q", forbidden)
		}
	}
}

func TestReviewCycleOwnsStandaloneLoopWithoutManagedState(t *testing.T) {
	text := readSkillFile(t, "review-cycle/SKILL.md")
	for _, required := range []string{
		"name: sandman-review-cycle",
		"sandman-review-request",
		"CI_TIMEOUT",
		"CI_FAILURE_UNRESOLVED",
		"REVIEW_TIMEOUT",
		"REVIEW_CONFLICT_UNRESOLVED",
		"formal requested-changes precedence",
		"current-head matching",
		"pending-trigger protection",
		"minimum poll window",
		"in memory",
		"restart",
		"AFK",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-cycle contract missing %q", required)
		}
	}
	for _, forbidden := range []string{
		".sandman/state/",
		"sandman-pr-review",
		"gh pr comment",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("review-cycle must not own competing request/state behavior %q", forbidden)
		}
	}
}

func TestPRReviewIsOnlyACompatibilityFacade(t *testing.T) {
	text := readSkillFile(t, "pr-review/SKILL.md")
	for _, required := range []string{
		"name: sandman-pr-review",
		"compatibility",
		"sandman-review-cycle",
		"delegates",
	} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(required)) {
			t.Errorf("pr-review facade missing %q", required)
		}
	}
	if strings.Contains(text, "#### Step 5") || strings.Contains(text, ".sandman/state/") {
		t.Fatal("pr-review facade must not contain a second review-cycle contract")
	}
}

func TestImplementSkillStopsAtPullRequestPublication(t *testing.T) {
	text := readSkillFile(t, "implement/SKILL.md")
	if strings.Contains(text, "sandman-pr-review") {
		t.Fatal("implement skill must not require the compatibility review loop")
	}
	for _, required := range []string{
		"PR-created checkpoint",
		"pull-request creation",
		"sandman-review-request",
		"does not own the iterative review loop",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("implementation capability missing boundary %q", required)
		}
	}
}
