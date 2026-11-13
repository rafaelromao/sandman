package sandman

import (
	"strings"
	"testing"
)

func TestPortableSkillExamplesUseResolvedInputsAndRepositoryPolicy(t *testing.T) {
	checks := map[string][]string{
		"plan/SKILL.md":           {`--body-file "$plan_file"`},
		"implement/SKILL.md":      {"COMMIT_SUBJECT='<one-line imperative description of the change>'", "--jq .body", `--repository "$repo"`, `--job-id "$failed_job_database_id"`, "repository's documented commit and title conventions"},
		"review-request/SKILL.md": {`sh "$skill_root/review-request/review-request-v1.sh"`, `--trigger-prefix "$review_command"`},
		"code-review/SKILL.md":    {"`git.base_branch`", "`origin/<resolved-base>` when available"},
		"pr-merge/SKILL.md":       {"hosting service's allowed methods", "no intersection", "gh pr merge --merge"},
		"review-cycle/SKILL.md":   {"record()", `exit 1; }`, `gh run view "$run_id" --repo "$repo" --log-failed`, `"$pr_number"`, `"$repo"`, "`$cycle_addressed_file`"},
	}
	for path, required := range checks {
		text := readSkillFile(t, path)
		for _, phrase := range required {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s missing portable example %q", path, phrase)
			}
		}
	}
	cycle := readSkillFile(t, "review-cycle/SKILL.md")
	for _, obsolete := range []string{"record REVIEW_TIMEOUT_STATE_ERROR and stop", "if sandman-back-merge;", ".workflow_runs[0].jobs", "<N>", "repos/<owner>/<repo>", "landed on main", "temporary addressed-comment record"} {
		if strings.Contains(cycle, obsolete) {
			t.Errorf("review cycle retains unsafe or inconsistent example %q", obsolete)
		}
	}
	run := readSkillFile(t, "run/SKILL.md")
	for _, obsolete := range []string{"orchestrator", "CI / semantic-pull-request", "Release Please"} {
		if strings.Contains(run, obsolete) {
			t.Errorf("standalone run assumes host infrastructure %q", obsolete)
		}
	}
}
