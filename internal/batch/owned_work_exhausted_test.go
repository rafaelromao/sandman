package batch

import (
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/events"
)

// REMEDIATION_BUDGET_EXHAUSTED for owned work (issue #3001): work the
// implementor owns and cannot resolve (merge work, CI repair, review-request
// delivery, a blind-review round, informal feedback bounded per head) ends the
// Run when it stalls or uses up its lifecycle relaunch allowance, instead of
// parking it in a continuity hold with no operation deadline.

// requireOwnedWorkExhausted asserts the terminal outcome of owned work that
// could not be resolved: the exhausted-budget reason, the gate it names, the
// gate's last reason and signature, and an operator next action, with no wait
// marker left behind.
func requireOwnedWorkExhausted(t *testing.T, verdict lifecycleVerdict, gate, lastReason, signature string) {
	t.Helper()
	if verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != "REMEDIATION_BUDGET_EXHAUSTED" {
		t.Fatalf("%s relaunch = %v/%v, want a terminal REMEDIATION_BUDGET_EXHAUSTED failure", gate, verdict.Action, verdict.Evidence)
	}
	for _, marker := range []string{"await", "gate", "continuity_hold", "resume_stalled"} {
		if _, present := verdict.Evidence[marker]; present {
			t.Errorf("terminal evidence carries the wait marker %q: %v", marker, verdict.Evidence)
		}
	}
	if verdict.ImplementorWork {
		t.Errorf("terminal %s verdict still asks for an implementor relaunch", gate)
	}
	if verdict.Evidence["external_gate"] != gate {
		t.Errorf("external_gate = %v, want %s", verdict.Evidence["external_gate"], gate)
	}
	if verdict.Evidence["stalled_reason"] != lastReason {
		t.Errorf("stalled_reason = %v, want the gate's last reason %s", verdict.Evidence["stalled_reason"], lastReason)
	}
	if verdict.Evidence["stalled_resume"] != signature {
		t.Errorf("stalled_resume = %v, want the used-up resume signature %s", verdict.Evidence["stalled_resume"], signature)
	}
	next, _ := verdict.Evidence["next_action"].(string)
	if !strings.Contains(next, "sandman run --continue") {
		t.Errorf("next_action = %q, want it to end with `sandman run --continue`", next)
	}
}

// Tracer bullet: merge work for an approved current-head pull request whose
// relaunch allowance is used up ends the Run, naming the gate and its last
// evidence.
func TestOwnedWorkExhausted_MergeWorkUsedUpAllowanceEndsTheRun(t *testing.T) {
	extras := map[string]any{
		"gate": gateReadyToMerge, "reason": "REVIEW_APPROVED",
		"pull_request": 17, "head_sha": "head-1",
		"review_request": map[string]any{"trigger_id": "request"},
		"blind_review":   map[string]any{"comment_id": "blind-1", "verdict": "approved"},
		"next_action":    "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate",
	}
	signature := resumeSignature(extras, "head-1")
	s := &runSession{decisionPRHead: "head-1"}
	s.resumeCount = s.resumeCapFor()

	verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})

	requireOwnedWorkExhausted(t, verdict, gateReadyToMerge, "REVIEW_APPROVED", signature)
	if verdict.Evidence["pull_request"] != 17 || verdict.Evidence["head_sha"] != "head-1" {
		t.Errorf("terminal evidence lost the pull request or head: %v", verdict.Evidence)
	}
	if _, kept := verdict.Evidence["review_request"]; !kept {
		t.Errorf("terminal evidence lost the retained review request: %v", verdict.Evidence)
	}
	if _, kept := verdict.Evidence[blindReviewKey]; !kept {
		t.Errorf("terminal evidence lost the blind publication the merge relied on: %v", verdict.Evidence)
	}
}

// requireExhaustedFinish asserts that a Run's event log ends in a terminal
// REMEDIATION_BUDGET_EXHAUSTED finish naming the gate, with no wait recorded
// and none left behind on the finish.
func requireExhaustedFinish(t *testing.T, logs []events.Event, gate string) {
	t.Helper()
	finished := lastEvent(logs, "run.finished")
	if finished == nil || finished.Payload["reason"] != "REMEDIATION_BUDGET_EXHAUSTED" || finished.Payload["external_gate"] != gate {
		t.Fatalf("run.finished = %+v, want a REMEDIATION_BUDGET_EXHAUSTED failure naming gate %s", finished, gate)
	}
	for _, marker := range []string{"await", "gate", "continuity_hold", "resume_stalled"} {
		if _, present := finished.Payload[marker]; present {
			t.Errorf("run.finished carries the wait marker %q: %+v", marker, finished.Payload)
		}
	}
	if next, _ := finished.Payload["next_action"].(string); !strings.Contains(next, "sandman run --continue") {
		t.Errorf("run.finished next_action = %q, want it to end with `sandman run --continue`", next)
	}
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Errorf("run.await events = %d, want none: the exhausted work ends the Run instead of parking it", got)
	}
	states := events.ProjectRunStates(logs)
	for _, state := range states {
		if state.IsAwaiting() || !state.IsTerminal() {
			t.Errorf("projected state %+v, want a terminal Run with no wait", state)
		}
	}
}
