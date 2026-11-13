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

func TestStandaloneRunRestartTrustsNoManagedState(t *testing.T) {
	flat := strings.Join(strings.Fields(readSkillFile(t, "run/SKILL.md")), " ")
	for _, required := range []string{
		"A restart begins from the live pull request and starts a fresh standalone cycle",
		"it does not import or trust managed lifecycle artifacts",
		"must not write managed runtime state, review registrations, or managed terminal outcomes",
		"do not translate them into managed runtime events or terminal state",
		"does not require the prompt package",
	} {
		if !strings.Contains(flat, required) {
			t.Errorf("sandman-run restart contract missing %q", required)
		}
	}
}

func flatSkill(t *testing.T, path string) string {
	t.Helper()
	return strings.Join(strings.Fields(readSkillFile(t, path)), " ")
}

// The shared skills carry no repository values: entry points resolve them at
// run time, caller first, then the repository's configuration, then live
// state, then defaults.
func TestStandaloneEntryPointsResolveRunContextInOrder(t *testing.T) {
	run := flatSkill(t, "run/SKILL.md")
	for _, required := range []string{
		"| Review command | the caller → `review_command` in `.sandman/config.yaml` → `/sandman review` |",
		"| Base branch | the caller → the base of the open pull request for this branch → `git.base_branch` in `.sandman/config.yaml` → the repository's default branch |",
		"| Implementation branch | the caller → the checked-out branch when it is not the base branch → the branch `sandman-implement` creates |",
		"(`REVIEW_TIMEOUT`) | the caller → `review_timeout` in `.sandman/config.yaml` → 1800 seconds |",
		"pass them to every capability this workflow loads",
		"Give it the resolved base branch and implementation branch",
		"load `sandman-review-cycle` with the resolved review command and review timeout",
	} {
		if !strings.Contains(run, required) {
			t.Errorf("sandman-run run context missing %q", required)
		}
	}

	cycle := readSkillFile(t, "review-cycle/SKILL.md")
	resolve := strings.Index(cycle, "### Resolve the review context")
	loop := strings.Index(cycle, "### Iteration loop")
	if resolve < 0 || loop < 0 || resolve > loop {
		t.Fatal("sandman-review-cycle must resolve its review context before the iteration loop")
	}
	section := strings.Join(strings.Fields(cycle[resolve:loop]), " ")
	for _, required := range []string{
		"| Review command | the caller → `review_command` in `.sandman/config.yaml` → `/sandman review` |",
		"| Review timeout | the caller → `review_timeout` in `.sandman/config.yaml`, in seconds → 1800 seconds |",
		"| Base branch | the caller → the pull request's base → `git.base_branch` in `.sandman/config.yaml` → the repository's default branch |",
		"| Current branch | the caller → the checked-out branch, which is the pull request's branch |",
		"always pass `\"$review_command\"` as one argument",
		"back-merge merges `origin/$base_branch` into it",
		"If your shell does not keep variables between commands, run this block again",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("sandman-review-cycle review context missing %q", required)
		}
	}
	for _, line := range strings.Split(cycle, "\n") {
		if strings.Contains(line, "trigger-prefix") && strings.Contains(line, "--") && !strings.Contains(line, `"$review_command"`) {
			t.Errorf("sandman-review-cycle passes a trigger prefix other than \"$review_command\": %q", line)
		}
	}

	facade := flatSkill(t, "pr-review/SKILL.md")
	for _, required := range []string{
		"the review command, review timeout, base branch and current branch when the caller supplied them",
		"That cycle resolves any value the caller did not supply, from the repository's `.sandman/config.yaml` and then the defaults",
	} {
		if !strings.Contains(facade, required) {
			t.Errorf("sandman-pr-review hand-off missing %q", required)
		}
	}
	if request := flatSkill(t, "review-request/SKILL.md"); !strings.Contains(request, "in a Sandman-managed run, the Task's `Review command` is that value and always wins") {
		t.Error("sandman-review-request must post the Task's review command in managed runs")
	}
}

func TestImplementSkillUsesResolvedBaseBranch(t *testing.T) {
	text := readSkillFile(t, "implement/SKILL.md")
	flat := strings.Join(strings.Fields(text), " ")
	for _, required := range []string{
		"In a Sandman-managed run, the Task's `Base branch` is that value and always wins.",
		"2. The base of the open pull request for the current branch.",
		"3. `git.base_branch` in the repository's `.sandman/config.yaml`.",
		"4. The repository's default branch.",
		`git fetch origin "$base_branch"`,
		`git merge-base --is-ancestor HEAD "origin/$base_branch"`,
		`gh pr create --base "$base_branch"`,
		`: "${base_branch:?resolve the base branch in step 1 first}" git fetch origin "$base_branch"`,
		`: "${base_branch:?resolve the base branch in step 1 first}" gh pr create --base "$base_branch"`,
		"If the current branch is already a non-base branch",
		"stay on it",
		`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh"`,
	} {
		if !strings.Contains(flat, required) {
			t.Errorf("sandman-implement base-branch contract missing %q", required)
		}
	}
	for _, forbidden := range []string{"git fetch origin main", "HEAD origin/main", "`origin/main`", "Checkout `main`", "Branch created from latest main"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("sandman-implement still assumes the base branch is main: %q", forbidden)
		}
	}
}

func TestManagedImplementLoadsOnlyStatelessRequestDelivery(t *testing.T) {
	flat := strings.Join(strings.Fields(readSkillFile(t, "implement/SKILL.md")), " ")
	for _, required := range []string{
		"`sandman-review-request` is a separate stateless capability",
		"this capability does not own the iterative review loop",
	} {
		if !strings.Contains(strings.ToLower(flat), strings.ToLower(required)) {
			t.Errorf("managed implementation boundary missing %q", required)
		}
	}
}
