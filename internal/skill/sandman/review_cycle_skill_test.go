package sandman

import (
	"os"
	"strings"
	"testing"
)

func TestReviewCycleRetainsRequestScopedReviewBehavior(t *testing.T) {
	data, err := os.ReadFile("review-cycle/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"review-trigger-guard-v1.sh",
		"review-wait-v1.sh",
		"review-observe-v1.sh",
		"formal requested-changes precedence",
		"classification.request_state == \"superseded\"",
		"current-head formal `APPROVED` records",
		"at least 240 s of cumulative sleep",
		"Do not filter it out because the author shares your GitHub login",
		"REVIEW_CONFLICT_UNRESOLVED",
		"CI_FAILURE_UNRESOLVED",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-cycle lost behavior %q", required)
		}
	}
	if strings.Contains(text, ".sandman/state/") || strings.Contains(text, "gh pr comment") {
		t.Fatal("review-cycle must not persist managed state or post review triggers directly")
	}
}

func TestReviewCycleUsesEphemeralStateAndFreshRestart(t *testing.T) {
	data, err := os.ReadFile("review-cycle/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"live pull request",
		"live head",
		"live in memory",
		"fresh standalone invocation",
		"Disposable process transport",
		"cycle_tmp_dir",
		"rm -rf \"$cycle_tmp_dir\"",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-cycle restart contract missing %q", required)
		}
	}
}

func TestReviewCycleDelegatesDeliveryAndKeepsManagedStateOut(t *testing.T) {
	data, err := os.ReadFile("review-cycle/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"require_review_trigger_delivery",
		"request_result=$(sh \"$skill_root/review-request/review-request-v1.sh\"",
		"review-request/v1",
		"The guard is read-only",
		"do not translate them into managed runtime events",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-cycle delivery boundary missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"gh pr comment",
		".sandman/state/",
		"sandman-pr-review",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("review-cycle contains competing managed behavior %q", forbidden)
		}
	}
}

func TestReviewCyclePreservesFormerReviewGates(t *testing.T) {
	text := string(readSkillFile(t, "review-cycle/SKILL.md"))
	for _, required := range []string{
		"#### Step 3: Check if SHA changed",
		"#### Step 5: Wait for this confirmed request",
		"#### Step 6: Read and classify feedback",
		"temporary cycle head record",
		"temporary addressed-comment record",
		"every prior approval timestamp is stale",
		"an implementor `{{REVIEW_COMMAND}}` trigger that has not yet received a response",
		"at least 240 s of cumulative sleep",
		"current-head formal `APPROVED` records",
		"Do not filter it out because the author shares your GitHub login",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-cycle lost extracted review gate %q", required)
		}
	}
}

func TestReviewCycleUsesVersionedRequestObservation(t *testing.T) {
	text := string(readSkillFile(t, "review-cycle/SKILL.md"))
	for _, required := range []string{
		"review-wait-v1.sh",
		"review-observe-v1.sh",
		"protocol:\"review-wait/v1\"",
		"--request-file",
		"trigger_created_at",
		"elapsed_seconds",
		"state:\"unavailable\"",
		"raw evidence is available for the existing Step 6 classifier",
		"not require a host `sandman` binary",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-cycle lost versioned observation contract %q", required)
		}
	}
}

func TestReviewCycleFailsClosedOnUntrustedRequestPair(t *testing.T) {
	text := string(readSkillFile(t, "review-cycle/SKILL.md"))
	for _, required := range []string{
		"The request envelope is the authoritative identity record",
		"persisted_head_sha=$(jq -er '.head_sha' \"$request_file\")",
		"recorded_head_sha=$(tr -d '\\r\\n' <\"$head_file\")",
		"If either artifact is missing, malformed, or mismatched, fail closed",
		"Do not\nsilently repair it by posting another trigger",
		"started_unix_seconds",
		"elapsed_seconds",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-cycle lost fail-closed request contract %q", required)
		}
	}
}

func TestReviewCycleRefreshesDirtyStateAfterBackMerge(t *testing.T) {
	text := string(readSkillFile(t, "review-cycle/SKILL.md"))
	for _, required := range []string{
		"git push || { echo REVIEW_CONFLICT_UNRESOLVED; exit 1; }",
		"mergeStateStatus=$(printf '%s' \"$pr_data\" | jq -r '.mergeStateStatus')",
		"head_sha=\"$headRefOid\"",
		"echo REVIEW_CONFLICT_UNRESOLVED",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("DIRTY recovery contract missing %q", required)
		}
	}
	if strings.Contains(text, "Back-merge failed or unresolved conflicts — CI still blocked. Continuing to poll.") {
		t.Fatal("DIRTY recovery must not continue with stale PR state after a failed back-merge")
	}
}
