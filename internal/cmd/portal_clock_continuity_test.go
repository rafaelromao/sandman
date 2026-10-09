package cmd

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// Exercise the actual saved-event -> Portal JSON -> browser clock path. Waiting
// and capacity admission must not restart a row's start time or execution clock.
func TestPortal_RunClockContinuity(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)
	log := &events.JSONLLogger{Path: filepath.Join(root, ".sandman", "events.jsonl")}
	history := []events.Event{
		{Type: "run.queued", Timestamp: start.Add(-time.Minute), RunID: "clock", Issue: 42, Payload: map[string]any{"initial_admission": true}},
		{Type: "run.continued", Timestamp: start, RunID: "clock", Issue: 42, Payload: map[string]any{"batch_id": "first", "previous_run_id": "older-run"}},
		{Type: "run.await", Timestamp: start.Add(5 * time.Minute), RunID: "clock", Issue: 42},
		{Type: "run.capacity_queued", Timestamp: start.Add(10 * time.Minute), RunID: "clock", Issue: 42},
		{Type: "run.continued", Timestamp: start.Add(15 * time.Minute), RunID: "clock", Issue: 42, Payload: map[string]any{"batch_id": "second", "previous_run_id": "clock"}},
	}
	writePortalLog(t, log.Path, history)
	assertRow := func(now time.Time, duration string, since *time.Time) portalRun {
		t.Helper()
		// A new view rehydrates the clock from saved events on every observation.
		runs, err := (&portalRunsView{now: func() time.Time { return now }}).compute(root, log)
		if err != nil || len(runs) != 1 {
			t.Fatalf("compute: runs=%+v err=%v", runs, err)
		}
		run := runs[0]
		if !run.StartedAt.Equal(start) || run.Duration != duration {
			t.Fatalf("same RunID clock reset: started=%s duration=%s; want %s %s", run.StartedAt, run.Duration, start, duration)
		}
		if (run.ExecutionSince == nil) != (since == nil) || (since != nil && !run.ExecutionSince.Equal(*since)) {
			t.Fatalf("execution segment=%v, want %v", run.ExecutionSince, since)
		}
		return run
	}
	segment := start.Add(15 * time.Minute)
	active := assertRow(start.Add(17*time.Minute), "7m0s", &segment)
	if active.ActiveDurationSeconds != 300 {
		t.Fatalf("resumed baseline=%d, want 300", active.ActiveDurationSeconds)
	}
	if err := log.Log(events.Event{Type: "run.await", Timestamp: start.Add(17 * time.Minute), RunID: "clock", Issue: 42}); err != nil {
		t.Fatal(err)
	}
	waiting := assertRow(start.Add(time.Hour), "7m0s", nil)
	activeJSON, _ := json.Marshal(active)
	waitingJSON, _ := json.Marshal(waiting)
	runNodeScript(t, `
const body = makeMockBody();
const opts = { helpers, stopGroups: new Set(), expandedKey: null };
const active = `+string(activeJSON)+`;
const waiting = `+string(waitingJSON)+`;
Date.now = () => `+jsonNumber(start.Add(17*time.Minute).UnixMilli())+`;
SandmanPortalDiff.diffRuns(body, [active], opts);
SandmanPortalDiff.refreshLiveTimeCells(body, [active]);
const duration = () => body.children[0].querySelector('[data-cell="duration"]').querySelector('.duration-value').textContent;
if (duration() !== '7m') throw new Error('resumed duration reset: ' + duration());
SandmanPortalDiff.diffRuns(body, [waiting], opts);
Date.now = () => `+jsonNumber(start.Add(time.Hour).UnixMilli())+`;
SandmanPortalDiff.refreshLiveTimeCells(body, [waiting]);
if (!/^7m(?:0s)?$/.test(duration())) throw new Error('waiting clock advanced: ' + duration());
`)
}

func jsonNumber(value int64) string {
	data, _ := json.Marshal(value)
	return string(data)
}
