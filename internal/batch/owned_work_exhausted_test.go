package batch

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
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

// CI repair that uses up its relaunch allowance ends the Run, naming the
// failing check execution it could not fix.
func TestOwnedWorkExhausted_CIRepairUsedUpAllowanceEndsTheRun(t *testing.T) {
	extras := map[string]any{
		"gate": "ci-failure", "reason": "CI_FAILURE",
		"pull_request": 17, "head_sha": "head-1",
		"next_action": "inspect current-head CI with gh pr checks and repair the failing checks",
	}
	signatureEvidence := cloneLifecycleExtras(extras)
	signatureEvidence["ci_execution_id"] = "checks-1"
	signature := resumeSignature(signatureEvidence, "head-1")
	s := &runSession{decisionPRHead: "head-1"}
	s.resumeCount = s.resumeCapFor()

	verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{
		Action: lifecycleResume, Evidence: extras, ciRepairRequired: true, ciExecutionID: "checks-1",
	})

	requireOwnedWorkExhausted(t, verdict, "ci-failure", "CI_FAILURE", signature)
	if got, _ := verdict.Evidence["stalled_resume"].(string); !strings.Contains(got, `"ci_execution_id":"checks-1"`) {
		t.Errorf("stalled_resume = %s, want it to name the failing CI execution", got)
	}
}

// parkedHoldObservation is what the row driver observed for a Run an earlier
// binary parked in a continuity hold.
type parkedHoldObservation struct {
	verdict      lifecycleVerdict
	executeCalls int
	status       string
	eventsSeen   []string
}

// parkedHoldSession is a session whose Run waits on a hold an earlier binary
// parked with the given resume signature.
func parkedHoldSession(signature string) *runSession {
	return &runSession{
		deps:           runDeps{eventLog: stalledLedgerLog(signature)},
		decisionPRHead: "head-1",
	}
}

// observeParkedHold drives the row driver's awaiting observation for a Run
// whose latest wait an earlier binary recorded with the given resume signature
// (a RESUME_STALLED hold), as a restarted batch observes it.
func observeParkedHold(t *testing.T, verdict lifecycleVerdict, signature string) parkedHoldObservation {
	t.Helper()
	session := parkedHoldSession(signature)
	seen := []string{}
	executor := &continuityStallRowDriverExecutor{
		rowDriverExecutorFake: &rowDriverExecutorFake{events: &seen},
		observer:              newLifecycleObserver(session),
		branch:                continuityBranch,
		runID:                 "run",
		verdict:               verdict,
	}
	// A parked hold that is not ended would keep the row driver waiting.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outcome := newRowDriver(executor, &rowDriverGateFake{}, nil).Drive(
		ctx,
		RunSpec{RunID: "run", IssueNumber: 42, Branch: continuityBranch},
		ContinuationCursor{},
		rowDriverOptions{Awaiting: true},
	)
	return parkedHoldObservation{verdict: executor.observed, executeCalls: executor.executeCalls, status: outcome.Result.Status, eventsSeen: seen}
}

func requireParkedHoldEnded(t *testing.T, observed parkedHoldObservation, gate, lastReason, signature string) {
	t.Helper()
	requireOwnedWorkExhausted(t, observed.verdict, gate, lastReason, signature)
	if observed.executeCalls != 0 || observed.status != "failure" || !reflect.DeepEqual(observed.eventsSeen, []string{"finish"}) {
		t.Fatalf("parked %s hold relaunched or kept waiting: execute calls=%d status=%q events=%v", gate, observed.executeCalls, observed.status, observed.eventsSeen)
	}
}

// ciRepairVerdict is the relaunch the lifecycle selects for failing
// current-head CI, with the signature the Run's stalled ledger records for it.
func ciRepairVerdict(t *testing.T, pr github.PR) (lifecycleVerdict, string) {
	t.Helper()
	decision := decideRecoverableLifecycle(lifecycleGatePending, &pr, pr.HeadRefOid, "", retainedReviewEvidence{}, false, "success", true)
	if decision.action != lifecycleResume || decision.gate != "ci-failure" {
		t.Fatalf("failing CI decision = %v/%s, want a ci-failure relaunch", decision.action, decision.gate)
	}
	// The lifecycle observation stamps the selected gate on a relaunch's
	// evidence after the decision.
	extras := cloneLifecycleExtras(decision.extras)
	extras["gate"], extras["await"] = string(decision.gate), true
	signatureEvidence := cloneLifecycleExtras(extras)
	signatureEvidence["ci_execution_id"] = decision.ciExecutionID
	return lifecycleVerdict{
		Action: lifecycleResume, Evidence: extras, ciRepairRequired: true, ciExecutionID: decision.ciExecutionID,
	}, resumeSignature(signatureEvidence, pr.HeadRefOid)
}

// A Run an earlier binary parked on a stalled CI repair ends on its next
// observation, because its recorded signature proves the stall. New evidence
// is not a stall: a new merge conflict selects conflict repair first, and a
// changed CI execution is repaired within the allowance.
func TestOwnedWorkExhausted_ParkedCIHoldEndsOnNextObservation(t *testing.T) {
	failing := github.PR{
		Number: continuityPR, State: "open", HeadRefName: continuityBranch, HeadRefOid: "head-1",
		StatusCheckRollup: "failure", MergeStateStatus: "CLEAN", CIExecutionID: "checks-1",
	}
	verdict, signature := ciRepairVerdict(t, failing)
	requireParkedHoldEnded(t, observeParkedHold(t, verdict, signature), "ci-failure", "CI_FAILURE", signature)

	t.Run("a new merge conflict resumes conflict repair first", func(t *testing.T) {
		conflicted := failing
		conflicted.MergeStateStatus = "DIRTY"
		decision := decideRecoverableLifecycle(lifecycleGatePending, &conflicted, "head-1", "", retainedReviewEvidence{}, false, "success", true)
		extras := cloneLifecycleExtras(decision.extras)
		extras["gate"], extras["await"] = string(decision.gate), true
		verdict := applyContinuityForTest(parkedHoldSession(signature), continuityBranch, "run", lifecycleVerdict{Action: decision.action, Evidence: extras})
		if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "MERGE_CONFLICT" {
			t.Fatalf("new conflict over a parked CI hold = %v/%v, want conflict repair to resume", verdict.Action, verdict.Evidence)
		}
	})
	t.Run("a changed CI execution is new evidence", func(t *testing.T) {
		rerun := failing
		rerun.CIExecutionID = "checks-2"
		changed, _ := ciRepairVerdict(t, rerun)
		verdict := applyContinuityForTest(parkedHoldSession(signature), continuityBranch, "run", changed)
		if verdict.Action != lifecycleResume {
			t.Fatalf("changed CI execution over a parked CI hold = %v/%v, want another repair relaunch", verdict.Action, verdict.Evidence)
		}
	})
}
