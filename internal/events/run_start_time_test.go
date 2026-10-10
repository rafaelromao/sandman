package events

import (
	"testing"
	"time"
)

func TestRunState_StartedAtPreservesExecutionIdentity(t *testing.T) {
	start := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)
	for _, entry := range []string{"run.started", "run.continued"} {
		t.Run(entry, func(t *testing.T) {
			history := []Event{
				{Type: "run.queued", RunID: "same", Timestamp: start.Add(-time.Hour), Payload: map[string]any{"initial_admission": true}},
				{Type: entry, RunID: "same", Timestamp: start, Payload: map[string]any{"batch_id": "old"}},
				{Type: "run.await", RunID: "same", Timestamp: start.Add(time.Minute)},
				{Type: "run.capacity_queued", RunID: "same", Timestamp: start.Add(2 * time.Minute)},
				{Type: "run.continued", RunID: "same", Timestamp: start.Add(time.Hour), Payload: map[string]any{"batch_id": "new"}},
				{Type: "run.await", RunID: "same", Timestamp: start.Add(time.Hour + time.Minute)},
				{Type: "run.started", RunID: "fresh", Timestamp: start.Add(2 * time.Hour)},
			}
			states := ProjectRunStates(history)
			if !states[0].StartedAt().Equal(start) || states[0].DurationAt(start.Add(3*time.Hour)) != 2*time.Minute {
				t.Fatalf("same RunID clock changed: started=%s duration=%s", states[0].StartedAt(), states[0].DurationAt(start.Add(3*time.Hour)))
			}
			if states[0].BatchID() != "new" || !states[0].Started.Timestamp.Equal(start.Add(time.Hour)) {
				t.Fatal("first execution time replaced the latest ownership/admission evidence")
			}
			if !states[1].StartedAt().Equal(start.Add(2*time.Hour)) || states[1].DurationAt(start.Add(2*time.Hour+time.Minute)) != time.Minute {
				t.Fatal("new RunID inherited the earlier run's clock")
			}
		})
	}
}
