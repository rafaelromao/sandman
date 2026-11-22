package batch

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/reviewpub"
)

// REVIEW_FEEDBACK_STALLED (issue #2999): a feedback relaunch that leaves the
// head, the review request and its responses unchanged found nothing the
// implementor could do. The Run ends with a permanent reason instead of
// parking in a RESUME_STALLED wait that nothing resolves.

const stalledFeedbackBody = "Please make the retry loop in `worker.go:12` keep the request."

// stalledFeedbackExtras is the actionable-feedback evidence of one retained
// informal response with a locator, JSON-round-tripped as it is read back from
// run events.
func stalledFeedbackExtras(t *testing.T, body string) map[string]any {
	t.Helper()
	return stalledFeedbackExtrasFor(t, body, "request", "response")
}

// stalledFeedbackExtrasFor builds that evidence for one review request and the
// identities of the responses it received.
func stalledFeedbackExtrasFor(t *testing.T, body, trigger string, responseIDs ...string) map[string]any {
	t.Helper()
	feedback := make([]informalFeedbackEvidence, 0, len(responseIDs))
	for _, id := range responseIDs {
		feedback = append(feedback, informalFeedbackEvidence{
			Source: "inline_comments", ID: id, HeadStatus: "current",
			Locator: "internal/worker.go:12", Body: body,
		})
	}
	extras := map[string]any{
		"gate": gateActionableFeedback, "reason": informalFeedbackReason,
		"pull_request": 17, "head_sha": "head-1",
		"review_request": map[string]any{"trigger_id": trigger, "informal_feedback": feedback},
	}
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

// Tracer bullet: an informal-feedback relaunch that repeats the last resume
// signature ends the Run, and the terminal evidence names the stalled
// feedback and the stalled resume signature.
func TestReviewFeedbackStalled_UnchangedInformalRelaunchEndsTheRun(t *testing.T) {
	extras := stalledFeedbackExtras(t, stalledFeedbackBody)
	signature := resumeSignature(extras, "head-1")
	s := &runSession{decisionPRHead: "head-1", lastResumeSignature: signature}

	verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})

	if verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != "REVIEW_FEEDBACK_STALLED" {
		t.Fatalf("unchanged informal-feedback relaunch = %v/%v, want a terminal REVIEW_FEEDBACK_STALLED failure", verdict.Action, verdict.Evidence)
	}
	for _, marker := range []string{"await", "gate", "continuity_hold", "resume_stalled"} {
		if _, present := verdict.Evidence[marker]; present {
			t.Errorf("terminal evidence carries the wait marker %q: %v", marker, verdict.Evidence)
		}
	}
	if verdict.Evidence["external_gate"] != gateActionableFeedback {
		t.Errorf("external_gate = %v, want %s", verdict.Evidence["external_gate"], gateActionableFeedback)
	}
	if verdict.Evidence["pull_request"] != 17 && verdict.Evidence["pull_request"] != float64(17) {
		t.Errorf("pull_request = %v, want 17", verdict.Evidence["pull_request"])
	}
	if verdict.Evidence["head_sha"] != "head-1" {
		t.Errorf("head_sha = %v, want head-1", verdict.Evidence["head_sha"])
	}
	next, _ := verdict.Evidence["next_action"].(string)
	for _, want := range []string{"inspect", "address", "answer", "sandman run", "--continue"} {
		if !strings.Contains(next, want) {
			t.Errorf("next_action = %q, want it to mention %q", next, want)
		}
	}
	if verdict.Evidence["stalled_resume"] != signature {
		t.Errorf("stalled_resume = %v, want the stalled resume signature %s", verdict.Evidence["stalled_resume"], signature)
	}
	request, _ := verdict.Evidence["review_request"].(map[string]any)
	feedback, _ := json.Marshal(request["informal_feedback"])
	for _, want := range []string{`"source":"inline_comments"`, `"locator":"internal/worker.go:12"`, "retry loop"} {
		if !strings.Contains(string(feedback), want) {
			t.Errorf("terminal evidence feedback = %s, want it to carry %s", feedback, want)
		}
	}
}

// stalledLedgerLog is an event log whose Run waits on a stalled relaunch and
// records the given signature, as an earlier Sandman binary or an earlier
// observation of this one left it.
func stalledLedgerLog(signature string) *spyEventLog {
	return &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run", Payload: map[string]any{"branch": "b"}},
		{Type: "run.await", RunID: "run", Payload: map[string]any{
			"await": true, "resume_stalled": true, stalledResumesKey: []any{signature},
		}},
	}}
}

// Genuine requested changes are not stalled feedback: a daemon
// changes_requested verdict and an approval override keep their relaunch
// allowance and, with a recorded stall, today's hold. Only plain informal
// feedback ends as REVIEW_FEEDBACK_STALLED.
func TestReviewFeedbackStalled_GenuineChangesKeepTheirStallHold(t *testing.T) {
	for name, body := range map[string]string{
		"daemon changes_requested verdict": reviewpub.Body("## Findings\n- Blocking: `worker.go:12` drops the request on retry.\n\n## Decision\n**CHANGES_REQUESTED**\n", "request", "head-1"),
		"approval override":                reviewpub.Body("## Suggested next steps\nMerge only after the race is fixed.\n\n## Decision\n**APPROVED**\n", "request", "head-1"),
	} {
		t.Run(name, func(t *testing.T) {
			extras := stalledFeedbackExtras(t, body)
			s := &runSession{deps: runDeps{eventLog: stalledLedgerLog(resumeSignature(extras, "head-1"))}, decisionPRHead: "head-1"}
			verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
			if verdict.Action != lifecycleAwait || verdict.Evidence["reason"] != resumeStalledReason {
				t.Fatalf("recorded stall of genuine requested changes = %v/%v, want the unchanged stalled wait", verdict.Action, verdict.Evidence)
			}
		})
	}
}

// The recorded stalled signature of the Run's own wait proves the stall: the
// same evidence ends the Run even when no relaunch was attempted in this
// session.
func TestReviewFeedbackStalled_RecordedStallEndsTheRun(t *testing.T) {
	extras := stalledFeedbackExtras(t, stalledFeedbackBody)
	signature := resumeSignature(extras, "head-1")
	s := &runSession{deps: runDeps{eventLog: stalledLedgerLog(signature)}, decisionPRHead: "head-1"}

	verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})

	if verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != reviewFeedbackStalledReason {
		t.Fatalf("recorded stall = %v/%v, want the terminal %s failure", verdict.Action, verdict.Evidence, reviewFeedbackStalledReason)
	}
	if verdict.Evidence["stalled_resume"] != signature {
		t.Errorf("stalled_resume = %v, want %s", verdict.Evidence["stalled_resume"], signature)
	}
	for _, marker := range []string{"await", "gate", "continuity_hold", "resume_stalled"} {
		if _, present := verdict.Evidence[marker]; present {
			t.Errorf("terminal evidence carries the wait marker %q: %v", marker, verdict.Evidence)
		}
	}
}

// A Run that an earlier binary parked on a stalled-feedback hold ends on its
// next observation instead of waiting for evidence that cannot arrive: its
// recorded signature proves the stall, and the implementor is not relaunched.
func TestReviewFeedbackStalled_RunParkedByEarlierBinaryEndsWithoutRelaunch(t *testing.T) {
	extras := stalledFeedbackExtras(t, stalledFeedbackBody)
	session := &runSession{
		deps:           runDeps{eventLog: stalledLedgerLog(resumeSignature(extras, "head-1"))},
		decisionPRHead: "head-1",
	}
	eventsSeen := []string{}
	executor := &continuityStallRowDriverExecutor{
		rowDriverExecutorFake: &rowDriverExecutorFake{events: &eventsSeen},
		observer:              newLifecycleObserver(session),
		branch:                continuityBranch,
		runID:                 "run",
		verdict:               lifecycleVerdict{Action: lifecycleResume, Evidence: extras},
	}

	outcome := newRowDriver(executor, &rowDriverGateFake{}, nil).Drive(
		context.Background(),
		RunSpec{RunID: "run", IssueNumber: 42, Branch: continuityBranch},
		ContinuationCursor{},
		rowDriverOptions{Awaiting: true},
	)

	if executor.observed.Evidence["reason"] != reviewFeedbackStalledReason {
		t.Fatalf("observation = %v/%v, want %s", executor.observed.Action, executor.observed.Evidence, reviewFeedbackStalledReason)
	}
	if executor.executeCalls != 0 || outcome.Result.Status != "failure" || !reflect.DeepEqual(eventsSeen, []string{"finish"}) {
		t.Fatalf("parked Run relaunched or kept waiting: execute calls=%d result=%+v events=%v", executor.executeCalls, outcome.Result, eventsSeen)
	}
}

// The finish records the stalled feedback, its locator, the stalled resume
// signature and the operator's next action, and projects as a terminal
// failure with no wait left behind.
func TestReviewFeedbackStalled_FinishRecordsFeedbackAndNextAction(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	log := &spyEventLog{}
	o := NewOrchestrator(nil, nil, nil, log, WithErrorLog(io.Discard))
	executor := o.newRunExecutor(context.Background(), BatchConfig{}, &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: root}}, nil)
	extras := stalledFeedbackExtras(t, stalledFeedbackBody)
	signature := resumeSignature(extras, "head-1")
	s := &runSession{decisionPRHead: "head-1", lastResumeSignature: signature}
	verdict := applyContinuityForTest(s, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})

	result := executor.finishObserved(context.Background(), RowSpec{IssueNumber: 42, RunID: "stalled-feedback", Branches: map[int]string{42: "42-work"}}, verdict)

	finished := findEvent(log.snapshot(), "run.finished")
	if result.Status != "failure" || finished == nil {
		t.Fatalf("stalled feedback was not finished: result=%+v events=%+v", result, log.snapshot())
	}
	if finished.Payload["reason"] != reviewFeedbackStalledReason || finished.Payload["stalled_resume"] != signature || finished.Payload["external_gate"] != gateActionableFeedback {
		t.Fatalf("finish evidence = %+v, want the reason, the stalled resume signature and the gate", finished.Payload)
	}
	if next, _ := finished.Payload["next_action"].(string); !strings.Contains(next, "`sandman run --continue`") {
		t.Errorf("next_action = %q, want it to name `sandman run --continue`", next)
	}
	data, _ := json.Marshal(finished.Payload["review_request"])
	for _, want := range []string{"inline_comments", "internal/worker.go:12", "retry loop"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("finish evidence lost the stalled feedback %q: %s", want, data)
		}
	}
	for _, key := range []string{"await", "await_reason", "gate", "resume_stalled", "continuity_hold"} {
		if _, present := finished.Payload[key]; present {
			t.Errorf("finish retained the active %s marker: %+v", key, finished.Payload)
		}
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) != 1 || !states[0].IsTerminal() || states[0].IsAwaiting() {
		t.Fatalf("projection = %+v, want one terminal Run that is not waiting", states)
	}
}

// An informal hold recorded for a used-up relaunch allowance (a row #3001
// owns) keeps its signature in the Run's ledger, like a ledger an earlier
// binary wrote. Its next observation therefore reaches the stall row and ends
// the Run: the two cannot be told apart.
func TestReviewFeedbackStalled_ExhaustedInformalHoldEndsOnReobservation(t *testing.T) {
	extras := stalledFeedbackExtras(t, stalledFeedbackBody)
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run", Payload: map[string]any{"branch": "b"}},
	}}
	session := &runSession{deps: runDeps{eventLog: log}, decisionPRHead: "head-1"}
	session.feedbackAllowance = &reviewFeedbackAllowance{usedByHead: map[string]int{"head-1": session.resumeCapFor()}}

	held := applyContinuityForTest(session, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
	if held.Action != lifecycleAwait || held.Evidence["reason"] != resumeStalledReason {
		t.Fatalf("used-up allowance = %v/%v, want the hold the exhausted row keeps", held.Action, held.Evidence)
	}
	if err := log.Log(events.Event{Type: "run.await", RunID: "run", Payload: held.Evidence}); err != nil {
		t.Fatal(err)
	}

	ended := applyContinuityForTest(&runSession{deps: runDeps{eventLog: log}, decisionPRHead: "head-1"}, "b", "run",
		lifecycleVerdict{Action: lifecycleResume, Evidence: extras})
	if ended.Action != lifecycleFailure || ended.Evidence["reason"] != reviewFeedbackStalledReason {
		t.Fatalf("re-observed exhausted hold = %v/%v, want the terminal %s failure", ended.Action, ended.Evidence, reviewFeedbackStalledReason)
	}
}

// A relaunch that obtains new evidence continues as before: a new head, a new
// review request or a new response is not a stall.
func TestReviewFeedbackStalled_NewEvidenceStillContinues(t *testing.T) {
	stalled := stalledFeedbackExtras(t, stalledFeedbackBody)
	signature := resumeSignature(stalled, "head-1")
	for name, tc := range map[string]struct {
		extras map[string]any
		head   string
	}{
		"new head":     {extras: stalled, head: "head-2"},
		"new request":  {extras: stalledFeedbackExtrasFor(t, stalledFeedbackBody, "request-2", "response"), head: "head-1"},
		"new response": {extras: stalledFeedbackExtrasFor(t, stalledFeedbackBody, "request", "response", "response-2"), head: "head-1"},
	} {
		t.Run(name, func(t *testing.T) {
			recorded := &runSession{deps: runDeps{eventLog: stalledLedgerLog(signature)}, decisionPRHead: tc.head}
			if verdict := applyContinuityForTest(recorded, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: tc.extras}); verdict.Action != lifecycleResume {
				t.Fatalf("recorded stall with %s = %v/%v, want the implementor resumed", name, verdict.Action, verdict.Evidence)
			}
			repeated := &runSession{decisionPRHead: tc.head, lastResumeSignature: signature}
			if verdict := applyContinuityForTest(repeated, "b", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: tc.extras}); verdict.Action != lifecycleResume {
				t.Fatalf("repeated relaunch with %s = %v/%v, want the implementor resumed", name, verdict.Action, verdict.Evidence)
			}
		})
	}
}
