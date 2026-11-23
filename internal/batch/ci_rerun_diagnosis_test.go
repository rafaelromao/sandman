package batch

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
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

func TestCIRerunRecoveryRequiresConfirmedMatchingReceipt(t *testing.T) {
	prerequisite := map[string]any{
		"pull_request":         17,
		"repository":           "owner/repo",
		"head_sha":             "current-sha",
		"workflow_run_id":      "101",
		"failed_checks":        []any{map[string]any{"name": "opencode-canary", "details_url": "https://github.com/owner/repo/actions/runs/101/job/11"}, map[string]any{"name": "quality-gate", "details_url": "https://github.com/owner/repo/actions/runs/101/job/12"}},
		"workflow_check_names": []string{"opencode-canary", "quality-gate"},
	}
	valid := ciRerunEvidenceForDiagnosis("confirmed", "transient-provider-timeout")
	if !valid.confirmsRecoveryFor(prerequisite) {
		t.Fatal("matching confirmed transient rerun should qualify for runtime recovery")
	}

	for name, mutate := range map[string]func(*ciRerunActionEvidence){
		"waiting is not confirmation": func(e *ciRerunActionEvidence) {
			*e = ciRerunEvidenceForDiagnosis("waiting", "transient-provider-timeout")
		},
		"uncertain state": func(e *ciRerunActionEvidence) { e.State = "uncertain" },
		"refused state":   func(e *ciRerunActionEvidence) { e.State = "refused" },
		"wrong run":       func(e *ciRerunActionEvidence) { e.RunID = "102" },
		"wrong head":      func(e *ciRerunActionEvidence) { e.SourceHeadSHA = "old-head" },
		"non-transient":   func(e *ciRerunActionEvidence) { e.Diagnosis = "deterministic-source-failure" },
		"wrong job":       func(e *ciRerunActionEvidence) { e.JobName = "build-linux" },
	} {
		t.Run(name, func(t *testing.T) {
			evidence := valid
			mutate(&evidence)
			if evidence.confirmsRecoveryFor(prerequisite) {
				t.Fatalf("receipt incorrectly qualified: %+v", evidence)
			}
		})
	}

	extraFailure := cloneLifecycleExtras(prerequisite)
	extraFailure["failed_checks"] = append(extraFailure["failed_checks"].([]any), map[string]any{
		"name": "unrelated-check", "details_url": "https://github.com/owner/repo/actions/runs/101/job/13",
	})
	if valid.confirmsRecoveryFor(extraFailure) {
		t.Fatal("receipt qualified despite an independent failed check")
	}
}

// feedCompletedHelperInvocation writes one completed helper tool invocation in
// the structured stream format of Claude Code (claudeStream) or OpenCode.
func feedCompletedHelperInvocation(t *testing.T, claudeStream bool, parser outputParser, command, output string) {
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
	if claudeStream {
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

func capturedCIRerunEvidence(t *testing.T, preset string, claudeStream bool, command, output string) *ciRerunActionEvidence {
	t.Helper()
	strategy := strategyForPreset(preset)
	stdout, stderr := strategy.NewOutputParsers(io.Discard)
	if stdout == nil {
		t.Fatalf("%s strategy has no structured output parser", preset)
	}
	stdout.setDestination(io.Discard)
	feedCompletedHelperInvocation(t, claudeStream, stdout, command, output)
	return strategy.CIRerunAction(stdout, stderr)
}

// Every strategy that captures helper results must carry each transient
// diagnosis from the real installed helper through to lifecycle selection, and
// the helper must accept every diagnosis the runtime does.
func TestCIRerunEveryStrategyCapturesEveryTransientDiagnosisFromTheInstalledHelper(t *testing.T) {
	for _, agent := range []struct {
		preset       string
		claudeStream bool
	}{{"opencode", false}, {"opencode-v2", false}, {"claude", true}} {
		preset := agent.preset
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
				evidence := capturedCIRerunEvidence(t, preset, agent.claudeStream, command, output)
				if evidence == nil || evidence.Diagnosis != diagnosis || !evidence.matchesPullRequest(17, "owner/repo", "head") {
					t.Fatalf("%s did not carry %s evidence into lifecycle selection: %+v", preset, diagnosis, evidence)
				}
				other := ciRerunTransientDiagnoses[(index+1)%len(ciRerunTransientDiagnoses)]
				forged := strings.Replace(output, `"diagnosis":"`+diagnosis+`"`, `"diagnosis":"`+other+`"`, 1)
				if got := capturedCIRerunEvidence(t, preset, agent.claudeStream, command, forged); got != nil {
					t.Fatalf("%s accepted a receipt whose diagnosis differs from the invoked command: %+v", preset, got)
				}
			})
		}
	}
}

// The helper script cannot import the runtime's accept-list, so both its
// accepted set and its usage text must list exactly the diagnoses the runtime
// accepts. The helper-per-diagnosis test catches a runtime entry the script
// lacks; this one catches a script entry the runtime lacks.
func TestCIRerunHelperAcceptsExactlyTheRuntimeDiagnoses(t *testing.T) {
	data, err := os.ReadFile("../skill/sandman/implement/ci-rerun-v1.sh")
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(ciRerunTransientDiagnoses)
	slices.Sort(want)
	for name, pattern := range map[string]string{
		"accepted set": `case "\$diagnosis" in\s*\n\s*([a-z-]+(?:\|[a-z-]+)*)\) ;;`,
		"usage text":   `--diagnosis ([a-z-]+(?:\|[a-z-]+)*)\nEOF`,
	} {
		match := regexp.MustCompile(pattern).FindSubmatch(data)
		if match == nil {
			t.Fatalf("helper %s not found", name)
		}
		got := strings.Split(string(match[1]), "|")
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("helper %s = %v, want exactly the runtime's %v", name, got, want)
		}
	}
}
