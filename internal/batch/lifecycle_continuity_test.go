package batch

import (
	"context"
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
	if status, extras := s.continuityDecision(context.Background(), "b", "run", "resume", owned); status != "await" || extras["continuity_hold"] != true {
		t.Fatalf("owned work with an unreadable log = %s/%v, want a continuity hold", status, extras)
	}
	feedback := map[string]any{"gate": gateActionableFeedback, "reason": informalFeedbackReason, stalledResumesKey: []any{"earlier"}}
	status, extras := s.continuityDecision(context.Background(), "b", "run", "resume", feedback)
	if status != "await" || extras["reason"] != resumeStalledReason {
		t.Fatalf("feedback with an unreadable log = %s/%v, want a stalled wait", status, extras)
	}
	if list := stalledResumes(extras); len(list) != 2 || list[0] != "earlier" {
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
		status, extras := s.continuityDecision(context.Background(), "b", "run", "resume", map[string]any{"gate": gateCIWaitTimeout, "reason": tc.reason})
		if status != tc.want {
			t.Fatalf("%s after the allowance = %s/%v, want %s", tc.reason, status, extras, tc.want)
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
	if status, _ := s.continuityDecision(context.Background(), "b", "run", "resume", extras); status != "failure" {
		t.Fatalf("exhausted formal changes on the same head = %s, want the terminal budget failure", status)
	}
	s.decisionPRHead = "head-2"
	s.resumeCount = s.resumeCapFor()
	if status, _ := s.continuityDecision(context.Background(), "b", "run", "resume", extras); status != "resume" || s.resumeCount != 0 {
		t.Fatalf("formal changes on a new head = %s (count %d), want a fresh repair round", status, s.resumeCount)
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
