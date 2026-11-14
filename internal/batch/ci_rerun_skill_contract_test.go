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
