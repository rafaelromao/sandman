package batch

import (
	"errors"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

type unreadableEventLog struct{}

func (unreadableEventLog) Log(events.Event) error { return nil }
func (unreadableEventLog) Read() ([]events.Event, error) {
	return nil, errors.New("event log unreadable")
}

// Without the Run's stalled signatures a relaunch could repeat stalled
// evidence, so an unreadable event log fails closed into a wait. Owned work
// waits too, and a wait carries the signatures the decision already holds.
func TestContinuityDecision_UnreadableEventLogKeepsWaiting(t *testing.T) {
	s := &runSession{deps: runDeps{eventLog: unreadableEventLog{}}, decisionPRHead: "head-1"}
	owned := map[string]any{"gate": gateReviewRequestRequired, "reason": "REVIEW_REQUEST_REQUIRED"}
	ownedVerdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: owned, ImplementorWork: true})
	if ownedVerdict.Action != lifecycleAwait || ownedVerdict.Evidence["continuity_hold"] != true {
		t.Fatalf("owned work with an unreadable log = %v/%v, want a continuity hold", ownedVerdict.Action, ownedVerdict.Evidence)
	}
	feedback := map[string]any{"gate": gateActionableFeedback, "reason": informalFeedbackReason, stalledResumesKey: []any{"earlier"}}
	feedbackVerdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: feedback})
	if feedbackVerdict.Action != lifecycleAwait || feedbackVerdict.Evidence["reason"] != resumeStalledReason {
		t.Fatalf("feedback with an unreadable log = %v/%v, want a stalled wait", feedbackVerdict.Action, feedbackVerdict.Evidence)
	}
	if list := stalledResumes(feedbackVerdict.Evidence); len(list) != 2 || list[0] != "earlier" {
		t.Fatalf("stalled signatures = %v, want the earlier signature carried forward", list)
	}
}

// Exhausted remediation of unusable CI wait state has no external operation
// that could resolve it: it stays terminal, while an expired CI wait waits.
func TestContinuityDecision_CIWaitStateErrorStaysTerminal(t *testing.T) {
	for _, tc := range []struct {
		reason, want string
	}{
		{"CI_WAIT_STATE_ERROR", "failure"},
		{"CI_WAIT_TIMEOUT", "await"},
	} {
		s := &runSession{decisionPRHead: "head-1"}
		s.resumeCount = s.resumeCapFor()
		verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: map[string]any{"gate": gateCIWaitTimeout, "reason": tc.reason}})
		if verdict.status() != tc.want {
			t.Fatalf("%s after the allowance = %v/%v, want %s", tc.reason, verdict.Action, verdict.Evidence, tc.want)
		}
	}
}

// The relaunch allowance belongs to one repair round. A new head (or request)
// is progress and starts a fresh allowance; an unchanged round keeps it.
func TestContinuityDecision_AllowanceIsPerRepairRound(t *testing.T) {
	extras := map[string]any{"gate": gateActionableFeedback, "reason": actionableFeedbackReason}
	s := &runSession{decisionPRHead: "head-1"}
	s.lastResumeSignature = resumeSignature(extras, "head-1")
	s.resumeCount = s.resumeCapFor()
	verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
	if verdict.Action != lifecycleFailure {
		t.Fatalf("exhausted formal changes on the same head = %v, want the terminal budget failure", verdict.Action)
	}
	s.decisionPRHead = "head-2"
	s.resumeCount = s.resumeCapFor()
	verdict = applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
	if verdict.Action != lifecycleResume || s.resumeCount != 0 {
		t.Fatalf("formal changes on a new head = %v (count %d), want a fresh repair round", verdict.Action, s.resumeCount)
	}
}

func TestContinuityDecision_StalledCIFailureTracksExecutionNotReviewResponse(t *testing.T) {
	prior := map[string]any{
		"gate": "ci-failure", "reason": "CI_FAILURE", "ci_execution_id": "checks-1",
		"review_request": map[string]any{
			"trigger_id":     "trigger-1",
			"formal_reviews": []any{map[string]any{"id": "review-1"}},
		},
	}
	priorSignature := resumeSignature(prior, "head-1")
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-ci", Payload: map[string]any{"branch": "42-work"}},
		{Type: "run.await", RunID: "run-ci", Payload: map[string]any{
			"await": true, "resume_stalled": true, stalledResumesKey: []any{priorSignature},
		}},
	}}
	current := map[string]any{
		"gate": "ci-failure", "reason": "CI_FAILURE",
		"review_request": map[string]any{
			"trigger_id":        "trigger-1",
			"informal_feedback": []any{map[string]any{"id": "approval-1"}},
		},
	}
	session := &runSession{
		deps:                runDeps{eventLog: log},
		decisionPRHead:      "head-1",
		lastResumeSignature: priorSignature,
	}
	unchangedExecution := applyContinuityForTest(session, "42-work", "run-ci", lifecycleVerdict{
		Action: lifecycleResume, Evidence: current, ciRepairRequired: true, ciExecutionID: "checks-1",
	})
	if unchangedExecution.Action != lifecycleAwait {
		t.Fatalf("approval appearing on the same failed CI execution = %s, want await", unchangedExecution.status())
	}

	newExecutionSession := &runSession{
		deps:                runDeps{eventLog: log},
		decisionPRHead:      "head-1",
		lastResumeSignature: priorSignature,
	}
	changedExecution := applyContinuityForTest(newExecutionSession, "42-work", "run-ci", lifecycleVerdict{
		Action: lifecycleResume, Evidence: cloneLifecycleExtras(current), ciRepairRequired: true, ciExecutionID: "checks-2",
	})
	if changedExecution.Action != lifecycleResume {
		t.Fatalf("new failed CI execution with allowance available = %s, want resume", changedExecution.status())
	}

	exhaustedSession := &runSession{
		deps:                runDeps{eventLog: log},
		decisionPRHead:      "head-1",
		lastResumeSignature: priorSignature,
		resumeCount:         defaultAwaitResumeMax,
	}
	exhaustedExecution := applyContinuityForTest(exhaustedSession, "42-work", "run-ci", lifecycleVerdict{
		Action: lifecycleResume, Evidence: cloneLifecycleExtras(current), ciRepairRequired: true, ciExecutionID: "checks-2",
	})
	if exhaustedExecution.Action != lifecycleAwait || exhaustedSession.resumeCount != defaultAwaitResumeMax {
		t.Fatalf("new failed CI execution after allowance exhaustion = %s (resume count %d), want await without resetting the request/head allowance", exhaustedExecution.status(), exhaustedSession.resumeCount)
	}
}

// A continuity wait has no operation deadline. Its schedule checkpoint still
// names a recovery operation, and restart discovery re-enters it within the
// ordinary ownerless grace instead of dropping it for a missing deadline.
func TestContinuityHold_RecoversThroughReadyContinuations(t *testing.T) {
	for _, payload := range []map[string]any{
		{"gate": gateActionableFeedback, "reason": resumeStalledReason, "resume_stalled": true, stalledResumesKey: []any{"sig"}},
		{"gate": string(lifecycleGatePending), "reason": idleGateReason, "continuity_hold": true},
	} {
		root := t.TempDir()
		layout := paths.NewLayout(nil, root)
		now := time.Now().UTC().Truncate(time.Second)
		await := cloneLifecycleExtras(payload)
		for key, value := range map[string]any{"await": true, "branch": "42-fix", "base_branch": "main", "batch_id": "batch"} {
			await[key] = value
		}
		log := &spyEventLog{events: []events.Event{
			{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"branch": "42-fix", "batch_id": "batch"}},
			{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Second), Payload: await},
		}}
		row := RowSpec{RunID: "row", IssueNumber: 42, BatchID: "batch", BaseBranch: "main", Branches: map[int]string{42: "42-fix"}}
		claim, err := daemon.ClaimRun(layout.SandmanDir, "row")
		if err != nil {
			t.Fatal(err)
		}
		owner, err := newWaitOwner(layout, layout.LocateBatch("batch", ""), daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: "42-fix", BaseBranch: "main", OperationID: "admission"}, func() time.Time { return now }, log)
		if err != nil {
			t.Fatal(err)
		}
		if err := owner.checkpoint(row, false, time.Minute); err != nil {
			t.Fatal(err)
		}
		owner.close()
		if err := claim.Close(); err != nil {
			t.Fatal(err)
		}
		ready := FindReadyContinuations(log.snapshot(), layout)
		if len(ready) != 1 || ready[0].Wait == nil || ready[0].Wait.Ready || ready[0].Wait.OperationID != continuityHoldOperation("row") || !ready[0].Wait.OperationDeadline.IsZero() {
			t.Fatalf("restart discovery of %v = %+v, want the continuity hold re-entered without a deadline", payload, ready)
		}
	}
}
