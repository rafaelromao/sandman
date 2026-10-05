package events

import (
	"testing"
	"time"
)

func TestWaitingOwnerSurvivesSlotFreeTerminalObservation(t *testing.T) {
	now := time.Now().UTC()
	log := []Event{
		{Type: "run.started", RunID: "row", Timestamp: now, Payload: map[string]any{"batch_id": "old"}},
		{Type: "run.await", RunID: "row", Timestamp: now.Add(time.Minute)},
		{Type: "run.capacity_queued", RunID: "row", Timestamp: now.Add(2 * time.Minute), Payload: map[string]any{"batch_id": "new"}},
		{Type: "run.finished", RunID: "row", Timestamp: now.Add(10 * time.Minute), Payload: map[string]any{"status": "success"}},
		{Type: "run.capacity_queued", RunID: "row", Timestamp: now.Add(11 * time.Minute), Payload: map[string]any{"batch_id": "stale"}},
	}
	state := ProjectRunStates(log)[0]
	if state.BatchID() != "new" || state.Status() != "success" || state.Duration() != time.Minute {
		t.Fatalf("slot-free terminal observation changed ownership or duration: %+v duration=%s", state, state.Duration())
	}
}

func TestTerminalRunRejectsStaleExecutionAdmission(t *testing.T) {
	now := time.Now().UTC()
	for _, terminal := range []string{"success", "failure", "blocked", "aborted", "cancelled"} {
		for _, admission := range []string{"run.started", "run.continued", "run.finished", "run.aborted", "run.cancelled", "run.blocked"} {
			t.Run(terminal+"/"+admission, func(t *testing.T) {
				kind := "run.finished"
				if terminal == "blocked" || terminal == "aborted" || terminal == "cancelled" {
					kind = "run." + terminal
				}
				log := []Event{{Type: "run.started", RunID: "row", Timestamp: now, Payload: map[string]any{"batch_id": "owner"}}, {Type: kind, RunID: "row", Timestamp: now.Add(time.Minute), Payload: map[string]any{"status": terminal}}}
				before := ProjectRunStates(log)[0]
				log = append(log, Event{Type: admission, RunID: "row", Timestamp: now.Add(2 * time.Minute), Payload: map[string]any{"batch_id": "stale", "status": "failure"}})
				after := ProjectRunStates(log)[0]
				if !after.IsTerminal() || after.Status() != before.Status() || after.BatchID() != before.BatchID() || after.Duration() != before.Duration() || !after.Finished.Timestamp.Equal(before.Finished.Timestamp) {
					t.Fatalf("stale lifecycle event changed terminal row: before=%+v after=%+v", before, after)
				}
			})
		}
	}
}
