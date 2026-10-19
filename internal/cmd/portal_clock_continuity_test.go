package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
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
	statusAt := func(now time.Time) string {
		t.Helper()
		var output bytes.Buffer
		cmd := newStatusCmd(log, func() time.Time { return now })
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("status at %s: %v", now, err)
		}
		return output.String()
	}
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
	if got := statusAt(start.Add(17 * time.Minute)); !strings.Contains(got, "elapsed 7m0s") {
		t.Fatalf("status duration = %q, want the Portal active duration 7m0s", got)
	}
	if err := log.Log(events.Event{Type: "run.await", Timestamp: start.Add(17 * time.Minute), RunID: "clock", Issue: 42}); err != nil {
		t.Fatal(err)
	}
	waiting := assertRow(start.Add(time.Hour), "7m0s", nil)
	if !waiting.Waiting {
		t.Fatal("waiting Portal row did not preserve its event-derived waiting lifecycle")
	}
	if got := statusAt(start.Add(time.Hour)); !strings.Contains(got, "elapsed 7m0s") {
		t.Fatalf("waiting status duration = %q, want the frozen Portal duration 7m0s", got)
	}
	resumeAt := start.Add(2 * time.Hour)
	if err := log.Log(events.Event{Type: "run.continued", Timestamp: resumeAt, RunID: "clock", Issue: 42, Payload: map[string]any{"batch_id": "third", "previous_run_id": "clock"}}); err != nil {
		t.Fatal(err)
	}
	resumed := assertRow(resumeAt.Add(2*time.Minute), "9m0s", &resumeAt)
	if resumed.Waiting || resumed.ActiveDurationSeconds != 420 {
		t.Fatalf("resumed Portal clock = waiting %v baseline %d, want waiting false baseline 420", resumed.Waiting, resumed.ActiveDurationSeconds)
	}
	if got := statusAt(resumeAt.Add(2 * time.Minute)); !strings.Contains(got, "elapsed 9m0s") {
		t.Fatalf("resumed status duration = %q, want the Portal duration 9m0s", got)
	}
	finishedAt := resumeAt.Add(7 * time.Minute)
	if err := log.Log(events.Event{Type: "run.finished", Timestamp: finishedAt, RunID: "clock", Issue: 42, Payload: map[string]any{"status": "success"}}); err != nil {
		t.Fatal(err)
	}
	terminal := assertRow(finishedAt.Add(time.Hour), "14m0s", nil)
	if terminal.Waiting || terminal.FinishedAt == nil {
		t.Fatalf("terminal Portal row = waiting %v finished %v, want terminal and not waiting", terminal.Waiting, terminal.FinishedAt)
	}

	sortNow := start.Add(65 * time.Minute)
	sortRoot := t.TempDir()
	sortLog := &events.JSONLLogger{Path: filepath.Join(sortRoot, ".sandman", "events.jsonl")}
	writePortalLog(t, sortLog.Path, []events.Event{
		{Type: "run.queued", Timestamp: sortNow.Add(-65 * time.Minute), RunID: "waiting-sort", Issue: 43, Payload: map[string]any{"initial_admission": true}},
		{Type: "run.continued", Timestamp: sortNow.Add(-65 * time.Minute), RunID: "waiting-sort", Issue: 43, Payload: map[string]any{"batch_id": "waiting-batch"}},
		{Type: "run.await", Timestamp: sortNow.Add(-60 * time.Minute), RunID: "waiting-sort", Issue: 43},
		{Type: "run.queued", Timestamp: sortNow.Add(-20 * time.Minute), RunID: "active-sort", Issue: 44, Payload: map[string]any{"initial_admission": true}},
		{Type: "run.continued", Timestamp: sortNow.Add(-20 * time.Minute), RunID: "active-sort", Issue: 44, Payload: map[string]any{"batch_id": "active-batch"}},
		{Type: "run.queued", Timestamp: sortNow.Add(-3 * time.Hour), RunID: "terminal-sort", Issue: 45, Payload: map[string]any{"initial_admission": true}},
		{Type: "run.continued", Timestamp: sortNow.Add(-3 * time.Hour), RunID: "terminal-sort", Issue: 45, Payload: map[string]any{"batch_id": "terminal-batch"}},
		{Type: "run.await", Timestamp: sortNow.Add(-2*time.Hour - 45*time.Minute), RunID: "terminal-sort", Issue: 45},
		{Type: "run.finished", Timestamp: sortNow.Add(-time.Hour), RunID: "terminal-sort", Issue: 45, Payload: map[string]any{"status": "success"}},
	})
	sortRuns, err := (&portalRunsView{now: func() time.Time { return sortNow }}).compute(sortRoot, sortLog)
	if err != nil || len(sortRuns) != 3 {
		t.Fatalf("sort rows: runs=%+v err=%v", sortRuns, err)
	}

	activeJSON, _ := json.Marshal(active)
	waitingJSON, _ := json.Marshal(waiting)
	resumedJSON, _ := json.Marshal(resumed)
	terminalJSON, _ := json.Marshal(terminal)
	sortRunsJSON, _ := json.Marshal(sortRuns)
	runNodeScript(t, `
const body = makeMockBody();
const opts = { helpers, stopGroups: new Set(), expandedKey: null };
const active = `+string(activeJSON)+`;
const waiting = `+string(waitingJSON)+`;
const resumed = `+string(resumedJSON)+`;
const terminal = `+string(terminalJSON)+`;
Date.now = () => `+jsonNumber(start.Add(17*time.Minute).UnixMilli())+`;
SandmanPortalDiff.diffRuns(body, [active], opts);
SandmanPortalDiff.refreshLiveTimeCells(body, [active]);
const duration = () => body.children[0].querySelector('[data-cell="duration"]').querySelector('.duration-value').textContent;
if (duration() !== '7m') throw new Error('resumed duration reset: ' + duration());
SandmanPortalDiff.diffRuns(body, [waiting], opts);
Date.now = () => `+jsonNumber(start.Add(time.Hour).UnixMilli())+`;
SandmanPortalDiff.refreshLiveTimeCells(body, [waiting]);
if (!/^7m(?:0s)?$/.test(duration())) throw new Error('waiting clock advanced: ' + duration());
Date.now = () => `+jsonNumber(resumeAt.Add(2*time.Minute).UnixMilli())+`;
SandmanPortalDiff.diffRuns(body, [resumed], opts);
SandmanPortalDiff.refreshLiveTimeCells(body, [resumed]);
if (!/^9m(?:0s)?$/.test(duration())) throw new Error('resumed clock did not advance from frozen baseline: ' + duration());
Date.now = () => `+jsonNumber(finishedAt.Add(time.Hour).UnixMilli())+`;
SandmanPortalDiff.diffRuns(body, [terminal], opts);
SandmanPortalDiff.refreshLiveTimeCells(body, [terminal]);
if (!/^14m(?:0s)?$/.test(duration())) throw new Error('terminal duration changed after finish: ' + duration());

const durationTable = `+string(sortRunsJSON)+`;
Date.now = () => `+jsonNumber(sortNow.UnixMilli())+`;
const sortedDurationTable = durationTable
  .slice()
  .sort((a, b) => SandmanPortalDiff.durationSortValue(b) - SandmanPortalDiff.durationSortValue(a));
const durationOrder = sortedDurationTable.map((run) => run.key).join(',');
if (durationOrder !== 'active-sort,terminal-sort,waiting-sort') throw new Error('duration sort used wall time: ' + durationOrder);
const byKey = new Map(durationTable.map((run) => [run.key, run]));
if (SandmanPortalDiff.durationSortValue(byKey.get('waiting-sort')) !== 300000) throw new Error('waiting sort value advanced during its wall-time wait');
if (SandmanPortalDiff.durationSortValue(byKey.get('active-sort')) !== 1200000) throw new Error('active sort value omitted its current execution segment');
if (SandmanPortalDiff.durationSortValue(byKey.get('terminal-sort')) !== 900000) throw new Error('terminal sort value changed after finish');
`)
}

func jsonNumber(value int64) string {
	data, _ := json.Marshal(value)
	return string(data)
}
