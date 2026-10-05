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
