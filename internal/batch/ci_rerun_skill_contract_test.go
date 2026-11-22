package batch

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCIRerunDocumentedInvocationProducesRecognizableReceipt(t *testing.T) {
	data, err := os.ReadFile("../skill/sandman/implement/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "sh \"$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh\"")
	if start < 0 {
		t.Fatal("documented helper invocation missing")
	}
	command := strings.SplitN(text[start:], "\n```", 2)[0]
	command = strings.NewReplacer(
		"<repository>", "owner/repo", "<pull-request-number>", "17",
		"<source-head-sha>", "current-sha", "<tested-sha>", "tested-sha",
		"<workflow-run-id>", "101", "<failed-job-database-id>", "987654321",
	).Replace(command)
	for _, state := range []string{"waiting", "confirmed"} {
		receipt, err := json.Marshal(ciRerunActionEvidence{
			Protocol: "ci-rerun/v1", State: state, Repository: "owner/repo", PullRequest: 17,
			SourceHeadSHA: "current-sha", TestedSHA: "tested-sha", RunID: "101", JobID: "987654321",
			Diagnosis: "transient-provider-timeout", ObservedAttempt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := ciRerunHelperToolEvidence(command, string(receipt)); got == nil {
			t.Fatalf("documented command does not authenticate %s receipt: %s", state, command)
		}
	}
}

// The implement skill tells a managed implementor what the CI recovery window
// expects of it: after its single diagnosed rerun fails again it returns and
// leaves later reruns to the runtime. The wording stays behavioral (no
// internals, no tracker coordinates) and scoped to managed runs.
func TestCIRerunSkillHandsLaterRerunsToTheRuntime(t *testing.T) {
	data, err := os.ReadFile("../skill/sandman/implement/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"In a Sandman-managed run, the rerun you spend through the helper is your only one for that failed execution.",
		"If it fails the same way and nothing else needs repair, return at once",
		"Do not invoke the helper again for that failure, do not look for another way to rerun it, and do not stop the Run:",
		"the runtime reruns the diagnosed job itself, periodically, while the Run waits",
		"A new head or a different failing check is a new failure",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("implement skill missing the hand-off wording %q", required)
		}
	}
	for _, forbidden := range []string{"CI recovery window", "ci-recovery", "CI_RECOVERY", "ADR-"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("implement skill names an internal %q", forbidden)
		}
	}
}
