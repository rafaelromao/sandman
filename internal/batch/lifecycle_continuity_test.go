package batch

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/reviewpub"
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

func TestRepairRound_SameHeadRerequest(t *testing.T) {
	for _, tc := range []struct {
		gate      string
		sameRound bool
	}{
		{gate: gateActionableFeedback, sameRound: true},
		{gate: "ci-failure"},
		{gate: gateReadyToMerge},
		{gate: gateBlindReviewRequired},
	} {
		t.Run(tc.gate, func(t *testing.T) {
			round := func(trigger string) string {
				return repairRound(map[string]any{
					"gate":           tc.gate,
					"review_request": map[string]any{"trigger_id": trigger},
				}, "head-1")
			}
			first, second := round("trigger-1"), round("trigger-2")
			if (first == second) != tc.sameRound {
				t.Fatalf("repair rounds after re-requesting on the same head = %q, %q; want same round: %v", first, second, tc.sameRound)
			}
		})
	}
}

// A new head starts a fresh review-feedback allowance; an unchanged head
// keeps it.
func TestContinuityDecision_AllowanceIsPerRepairRound(t *testing.T) {
	extras := map[string]any{"gate": gateActionableFeedback, "reason": actionableFeedbackReason}
	s := &runSession{decisionPRHead: "head-1"}
	s.lastResumeSignature = resumeSignature(extras, "head-1")
	s.feedbackAllowance = &reviewFeedbackAllowance{usedByHead: map[string]int{"head-1": s.resumeCapFor()}}
	verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
	if verdict.Action != lifecycleFailure {
		t.Fatalf("exhausted formal changes on the same head = %v, want the terminal budget failure", verdict.Action)
	}
	s.decisionPRHead = "head-2"
	verdict = applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
	if verdict.Action != lifecycleResume {
		t.Fatalf("formal changes on a new head = %v, want a fresh repair round", verdict.Action)
	}
}

// continuityFeedbackExtras is the actionable-feedback evidence of one retained
// informal response, JSON-round-tripped as it is read back from run events.
func continuityFeedbackExtras(t *testing.T, body string, override bool) map[string]any {
	t.Helper()
	request := map[string]any{"trigger_id": "request", "informal_feedback": []informalFeedbackEvidence{{Source: "top_level", ID: "response", Body: body}}}
	if override {
		request[approvalOverrideKey] = approvalOverrideStatement
	}
	extras := map[string]any{"gate": gateActionableFeedback, "reason": informalFeedbackReason, "head_sha": "head-1", "review_request": request}
	data, err := json.Marshal(extras)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// A contradicted daemon approval is genuine requested changes (#2998): it
// keeps the full per-head relaunch allowance instead of stalling on a repeat,
// and an unresolved head ends the Run with the override as terminal evidence.
// The decision is derived from the retained report, so it survives evidence
// that was read back from the event log.
func TestContinuityDecision_ContradictedApprovalIsGenuineRequestedChanges(t *testing.T) {
	report := reviewpub.Body("## Suggested next steps\nMerge only after the race is fixed.\n\n## Decision\n**APPROVED**\n", "request", "head-1")
	plain := "Please make the retry loop in `worker.go:12` keep the request."
	feedback := continuityFeedbackExtras
	session := func(extras map[string]any, used int) *runSession {
		s := &runSession{decisionPRHead: "head-1"}
		s.lastResumeSignature = resumeSignature(extras, "head-1")
		s.feedbackAllowance = &reviewFeedbackAllowance{usedByHead: map[string]int{"head-1": used}}
		return s
	}

	t.Run("an unchanged relaunch does not stall", func(t *testing.T) {
		extras := feedback(t, report, true)
		verdict := applyContinuityForTest(session(extras, 1), "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
		if verdict.Action != lifecycleResume {
			t.Fatalf("contradicted approval after one unchanged relaunch = %v/%v, want another relaunch", verdict.Action, verdict.Evidence)
		}
	})
	t.Run("an exhausted head ends the Run with the override", func(t *testing.T) {
		extras := feedback(t, report, true)
		s := session(extras, defaultAwaitResumeMax)
		verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
		if verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != "REMEDIATION_BUDGET_EXHAUSTED" {
			t.Fatalf("exhausted contradicted approval = %v/%v, want the terminal budget failure", verdict.Action, verdict.Evidence)
		}
		request, _ := verdict.Evidence["review_request"].(map[string]any)
		if request[approvalOverrideKey] != approvalOverrideStatement {
			t.Fatalf("terminal evidence = %v, want %s", verdict.Evidence, approvalOverrideKey)
		}
		s.decisionPRHead = "head-2"
		fresh := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
		if fresh.Action != lifecycleResume {
			t.Fatalf("contradicted approval on a new head = %v, want a fresh allowance", fresh.Action)
		}
	})
	t.Run("the decision does not depend on the flag", func(t *testing.T) {
		extras := feedback(t, report, false)
		verdict := applyContinuityForTest(session(extras, defaultAwaitResumeMax), "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
		if verdict.Action != lifecycleFailure {
			t.Fatalf("contradicted approval evidence = %v, want the terminal budget failure", verdict.Action)
		}
	})
	t.Run("ordinary informal feedback still stalls", func(t *testing.T) {
		extras := feedback(t, plain, false)
		verdict := applyContinuityForTest(session(extras, 1), "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
		if verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != reviewFeedbackStalledReason {
			t.Fatalf("informal feedback after an unchanged relaunch = %v/%v, want the terminal %s failure", verdict.Action, verdict.Evidence, reviewFeedbackStalledReason)
		}
	})
	t.Run("a flag without a contradicted report is not requested changes", func(t *testing.T) {
		extras := feedback(t, plain, true)
		verdict := applyContinuityForTest(session(extras, 1), "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
		if verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != reviewFeedbackStalledReason {
			t.Fatalf("flag-only evidence = %v/%v, want evidence derived from the report to decide: plain feedback stalls", verdict.Action, verdict.Evidence)
		}
	})
}

// The approval override stays on the retained review request through every
// wait that carries it (#2998): an unreadable event log fails closed into a
// stalled wait, and a restart that finds a recorded stalled signature keeps
// waiting on that evidence instead of relaunching on it again.
func TestContinuityDecision_ApprovalOverrideSurvivesLaterWaits(t *testing.T) {
	report := reviewpub.Body("## Suggested next steps\nMerge only after the race is fixed.\n\n## Decision\n**APPROVED**\n", "request", "head-1")
	overridden := func(t *testing.T, verdict lifecycleVerdict) {
		t.Helper()
		request, _ := verdict.Evidence["review_request"].(map[string]any)
		if verdict.Action != lifecycleAwait || request[approvalOverrideKey] != approvalOverrideStatement {
			t.Fatalf("wait = %v/%v, want an await that keeps the approval override", verdict.Action, verdict.Evidence)
		}
	}
	t.Run("unreadable event log", func(t *testing.T) {
		extras := continuityFeedbackExtras(t, report, true)
		s := &runSession{deps: runDeps{eventLog: unreadableEventLog{}}, decisionPRHead: "head-1"}
		overridden(t, applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras}))
	})
	t.Run("restart with a recorded stalled signature", func(t *testing.T) {
		extras := continuityFeedbackExtras(t, report, true)
		log := &spyEventLog{events: []events.Event{
			{Type: "run.started", RunID: "run", Payload: map[string]any{"branch": "b"}},
			{Type: "run.await", RunID: "run", Payload: map[string]any{
				"await": true, "resume_stalled": true, stalledResumesKey: []any{resumeSignature(extras, "head-1")},
			}},
		}}
		s := &runSession{deps: runDeps{eventLog: log}, decisionPRHead: "head-1"}
		overridden(t, applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras}))
	})
	t.Run("a pending wait for a new request carries the new request only", func(t *testing.T) {
		pending := map[string]any{"gate": "pending", "review_request": map[string]any{"trigger_id": "request-2"}}
		log := &spyEventLog{events: []events.Event{
			{Type: "run.started", RunID: "run", Payload: map[string]any{"branch": "b"}},
			{Type: "run.await", RunID: "run", Payload: map[string]any{"await": true, "resume_stalled": true, stalledResumesKey: []any{"earlier"}}},
		}}
		s := &runSession{deps: runDeps{eventLog: log}, decisionPRHead: "head-1"}
		verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleAwait, Evidence: pending})
		if request, _ := verdict.Evidence["review_request"].(map[string]any); verdict.Action != lifecycleAwait || request[approvalOverrideKey] != nil {
			t.Fatalf("wait for a re-request = %v/%v, want the new request without an override", verdict.Action, verdict.Evidence)
		}
	})
}

func TestContinuityRepeatedFormalChangesUsesFailureReasonNotExhaustion(t *testing.T) {
	evidence := map[string]any{
		"gate": gateBlindReviewRequired, "reason": actionableFeedbackReason,
		"next_action":  actionableFeedbackNextAction,
		"pull_request": continuityPR, "head_sha": continuityProbeHead,
	}
	signature := resumeSignature(evidence, continuityProbeHead)
	session := &runSession{decisionPRHead: continuityProbeHead, lastResumeSignature: signature}
	verdict := (&lifecycleObserver{runSession: session}).applyContinuity(
		continuityBranch, "run-formal-feedback", lifecycleVerdict{
			Action: lifecycleResume, Evidence: cloneLifecycleExtras(evidence),
		},
	)
	if verdict.Action != lifecycleFailure {
		t.Fatalf("repeated formal changes = %s, want terminal failure", verdict.status())
	}
	if got := stringEvidence(verdict.Evidence, "reason"); got != actionableFeedbackReason {
		t.Fatalf("terminal reason = %q, want %q", got, actionableFeedbackReason)
	}
	if got := stringEvidence(verdict.Evidence, "next_action"); got != actionableFeedbackNextAction {
		t.Fatalf("terminal next action = %q, want %q", got, actionableFeedbackNextAction)
	}
	if session.resumeCount >= session.resumeCapFor() {
		t.Fatalf("fixture exhausted allowance (%d) unexpectedly", session.resumeCount)
	}
}

func TestFinalOwnedWorkOutcomeTableIncludesIdleReviewEligibility(t *testing.T) {
	for _, tc := range []struct {
		name          string
		head          string
		reviewCommand string
		wantRowReason string
		wantAction    lifecycleAction
	}{
		{
			name: "unrequested review with a current head holds",
			head: "head-1", wantRowReason: idleGateReason, wantAction: lifecycleAwait,
		},
		{
			name: "configured review command is owned work and terminal",
			head: "head-1", reviewCommand: "/sandman review",
			wantRowReason: implementorActionRequiredReason, wantAction: lifecycleFailure,
		},
		{
			name:          "unknown head cannot authorize an idle hold",
			wantRowReason: implementorActionRequiredReason, wantAction: lifecycleFailure,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := map[string]any{
				"gate": gateReviewRequestRequired, "reason": idleGateReason,
				"expected_head_sha": tc.head, "pull_request": continuityPR,
			}
			row := finalOwnedWorkContinuityRow(evidence, tc.reviewCommand)
			if row.reason != tc.wantRowReason {
				t.Fatalf("final owned-work table reason = %q, want %q", row.reason, tc.wantRowReason)
			}
			session := &runSession{
				decisionPRHead:        tc.head,
				finalOwnedWorkAttempt: true,
				renderCfg:             prompt.RenderConfig{ReviewCommand: tc.reviewCommand},
			}
			verdict := (&lifecycleObserver{runSession: session}).applyContinuity(
				continuityBranch, "run-final-owned-work", lifecycleVerdict{
					Action: lifecycleResume, Evidence: evidence, ImplementorWork: true,
				},
			)
			if verdict.Action != tc.wantAction {
				t.Fatalf("final owned-work disposition = %s, want action %v", verdict.status(), tc.wantAction)
			}
			if tc.wantAction == lifecycleFailure &&
				(verdict.Evidence["reason"] != idleGateReason || verdict.Evidence["gate"] != nil) {
				t.Fatalf("terminal owned-work evidence = %v, want original failure reason without gate", verdict.Evidence)
			}
		})
	}
}

func TestContinuityDecision_AllowanceDoesNotResetAcrossGatesForSameRequest(t *testing.T) {
	request := map[string]any{"trigger_id": "request-1"}
	ci := map[string]any{"gate": "ci-failure", "reason": "CI_FAILURE", "review_request": request}
	merge := map[string]any{"gate": gateReadyToMerge, "reason": "READY_TO_MERGE", "review_request": request}
	for _, tc := range []struct {
		name, priorGate string
		prior, current  map[string]any
	}{
		{name: "CI to merge", priorGate: "ci-failure", prior: ci, current: merge},
		{name: "merge to CI", priorGate: gateReadyToMerge, prior: merge, current: ci},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &runSession{
				decisionPRHead:      "head-1",
				lastResumeSignature: resumeSignature(tc.prior, "head-1"),
				lastResumeRound:     repairRound(tc.prior, "head-1"),
				resumeCount:         defaultAwaitResumeMax,
			}
			verdict := applyContinuityForTest(session, "42-work", "run-gate-transition", lifecycleVerdict{
				Action: lifecycleResume, Evidence: cloneLifecycleExtras(tc.current),
			})
			if verdict.Action != lifecycleAwait || session.resumeCount != defaultAwaitResumeMax {
				t.Fatalf("same request/head transition reset the exhausted relaunch allowance: verdict=%+v count=%d", verdict, session.resumeCount)
			}
		})
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
