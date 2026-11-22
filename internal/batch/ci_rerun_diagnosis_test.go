package batch

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func ciRerunEvidenceForDiagnosis(state, diagnosis string) ciRerunActionEvidence {
	evidence := ciRerunActionEvidence{
		Protocol: "ci-rerun/v1", State: state, Repository: "owner/repo", PullRequest: 17,
		SourceHeadSHA: "current-sha", TestedSHA: "tested-merge-sha", Diagnosis: diagnosis,
		RunID: "101", JobID: "987654321", JobName: "opencode-canary",
	}
	switch state {
	case "waiting":
		evidence.Reason, evidence.ObservedAttempt = "workflow-still-running", 1
	case "confirmed":
		evidence.Reason, evidence.ObservedAttempt = "selected-job-and-quality-gate-rerun-observed", 2
	}
	return evidence
}

func TestCIRerunEvidenceIsAcceptedForEveryTransientDiagnosis(t *testing.T) {
	for _, diagnosis := range []string{"transient-provider-timeout", "transient-network-timeout", "transient-provider-quota"} {
		for _, state := range []string{"waiting", "confirmed"} {
			evidence := ciRerunEvidenceForDiagnosis(state, diagnosis)
			if !evidence.matchesPullRequest(17, "owner/repo", "current-sha") {
				t.Errorf("%s %s receipt was not accepted for lifecycle selection", diagnosis, state)
			}
		}
	}
}

func TestCIRerunEvidenceRejectsUnknownAndNonTransientDiagnoses(t *testing.T) {
	for _, diagnosis := range []string{"transient-quota", "transient-provider-quotas", "deterministic-failure", "source-failure", ""} {
		for _, state := range []string{"waiting", "confirmed"} {
			evidence := ciRerunEvidenceForDiagnosis(state, diagnosis)
			if evidence.matchesPullRequest(17, "owner/repo", "current-sha") {
				t.Errorf("%q %s receipt was accepted for lifecycle selection", diagnosis, state)
			}
		}
	}
}

// feedCompletedHelperInvocation writes one completed helper tool invocation in
// the structured stream format of the given agent preset.
func feedCompletedHelperInvocation(t *testing.T, preset string, parser outputParser, command, output string) {
	t.Helper()
	write := func(record map[string]any) {
		t.Helper()
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parser.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if preset == "claude" {
		write(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "rerun-1", "name": "Bash", "input": map[string]any{"command": command}},
		}}})
		write(map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "rerun-1", "content": output},
		}}})
		return
	}
	write(map[string]any{
		"type": "tool_use",
		"part": map[string]any{"tool": "bash", "state": map[string]any{
			"status": "completed", "input": map[string]any{"command": command}, "output": output,
		}},
	})
}

func capturedCIRerunEvidence(t *testing.T, preset, command, output string) *ciRerunActionEvidence {
	t.Helper()
	strategy := strategyForPreset(preset)
	stdout, stderr := strategy.NewOutputParsers(io.Discard)
	if stdout == nil {
		t.Fatalf("%s strategy has no structured output parser", preset)
	}
	stdout.setDestination(io.Discard)
	feedCompletedHelperInvocation(t, preset, stdout, command, output)
	return strategy.CIRerunAction(stdout, stderr)
}

// Every strategy that captures helper results must carry each transient
// diagnosis from the real installed helper through to lifecycle selection, and
// the helper must accept every diagnosis the runtime does.
func TestCIRerunEveryStrategyCapturesEveryTransientDiagnosisFromTheInstalledHelper(t *testing.T) {
	for _, preset := range []string{"opencode", "opencode-v2", "claude"} {
		for index, diagnosis := range ciRerunTransientDiagnoses {
			t.Run(preset+"/"+diagnosis, func(t *testing.T) {
				root := t.TempDir()
				gh := newRecordingCIRerunGH(t, root)
				helper := installCIRerunHelperForManagedTest(t, root)
				command := fmt.Sprintf(`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha head --tested-sha tested-merge-sha --run-id 12345 --job-id 987654321 --diagnosis %s`, diagnosis)
				output := runCIRerunHelperForManagedTest(t, gh, helper, command)
				if !strings.Contains(output, `"state":"confirmed"`) || !strings.Contains(output, `"diagnosis":"`+diagnosis+`"`) {
					t.Fatalf("installed helper did not confirm a %s rerun: %s", diagnosis, output)
				}
				if actions := gh.actions(t); len(actions) != 1 || actions[0] != "POST repos/owner/repo/actions/jobs/987654321/rerun" {
					t.Fatalf("helper actions = %v, want one rerun of the selected job", actions)
				}
				evidence := capturedCIRerunEvidence(t, preset, command, output)
				if evidence == nil || evidence.Diagnosis != diagnosis || !evidence.matchesPullRequest(17, "owner/repo", "head") {
					t.Fatalf("%s did not carry %s evidence into lifecycle selection: %+v", preset, diagnosis, evidence)
				}
				other := ciRerunTransientDiagnoses[(index+1)%len(ciRerunTransientDiagnoses)]
				forged := strings.Replace(output, `"diagnosis":"`+diagnosis+`"`, `"diagnosis":"`+other+`"`, 1)
				if got := capturedCIRerunEvidence(t, preset, command, forged); got != nil {
					t.Fatalf("%s accepted a receipt whose diagnosis differs from the invoked command: %+v", preset, got)
				}
			})
		}
	}
}
