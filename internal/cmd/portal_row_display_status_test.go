package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

func TestPortalRowDisplayStatus_StartedAwaitIsWaitingNotQueuedLike(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "batches"), 0755); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	eventList := []events.Event{
		{
			Type: "run.started", RunID: "awaiting-run", Issue: 42, Timestamp: startedAt,
			Payload: map[string]any{"batch_id": "batch-awaiting"},
		},
		{
			Type: "run.await", RunID: "awaiting-run", Issue: 42, Timestamp: startedAt.Add(time.Minute),
			Payload: map[string]any{"reason": "review", "gate": "review"},
		},
	}

	runs, err := (&portalRunsView{}).computeFromEvents(repoRoot, eventList)
	if err != nil {
		t.Fatalf("compute portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("projected rows = %d, want 1: %#v", len(runs), runs)
	}
	row := runs[0]
	if row.Status != "waiting" || !row.Waiting {
		t.Fatalf("started await status/waiting = %q/%v, want waiting/true", row.Status, row.Waiting)
	}

	data, err := json.Marshal(portalSummaryRuns(runs))
	if err != nil {
		t.Fatalf("marshal portal summary: %v", err)
	}
	var payload []map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode portal summary: %v", err)
	}
	for _, flag := range []string{"waiting", "queuedLike", "canAbort", "canArchive"} {
		if _, ok := payload[0][flag]; !ok {
			t.Errorf("Portal JSON omits projected %q flag: %s", flag, data)
		}
	}
	if got := payload[0]["queuedLike"]; got != false {
		t.Errorf("queuedLike = %v, want false for a started suspension", got)
	}
	if got := payload[0]["canAbort"]; got != true {
		t.Errorf("canAbort = %v, want true for an active waiting row", got)
	}
	if got := payload[0]["canArchive"]; got != false {
		t.Errorf("canArchive = %v, want false for an active row", got)
	}
}

func TestPortalRowDisplayStatus_StartedSuspensionGatesStayWaiting(t *testing.T) {
	startedAt := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{name: "review", payload: map[string]any{"await_reason": "pending", "gate": "review"}},
		{name: "ci", payload: map[string]any{"await_reason": "pending", "ci_wait": map[string]any{"deadline_unix_seconds": startedAt.Add(time.Hour).Unix()}}},
		{name: "confirmed review request", payload: map[string]any{"await_reason": "pending"}},
		{name: "quota", payload: map[string]any{"await_reason": "usage-limit"}},
		{name: "ready to merge", payload: map[string]any{"await": true, "gate": "pending"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := events.ProjectRunStates([]events.Event{
				{Type: "run.started", RunID: "suspended-run", Issue: 42, Timestamp: startedAt},
				{Type: "run.await", RunID: "suspended-run", Issue: 42, Timestamp: startedAt.Add(time.Minute), Payload: tc.payload},
			})[0]
			row := (&portalRunsView{}).runFromState(t.TempDir(), state, nil, nil, nil, nil)
			if row.Status != "waiting" || !row.Waiting || row.QueuedLike {
				t.Fatalf("started %s suspension status/waiting/queuedLike = %q/%v/%v, want waiting/true/false", tc.name, row.Status, row.Waiting, row.QueuedLike)
			}
		})
	}
}

func TestPortalRowDisplayStatus_NodeConsumesGoProjectedFlags(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "batches"), 0755); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	eventList := []events.Event{
		{
			Type: "run.started", RunID: "awaiting-run", Issue: 42, Timestamp: startedAt,
			Payload: map[string]any{"batch_id": "batch-awaiting"},
		},
		{
			Type: "run.await", RunID: "awaiting-run", Issue: 42, Timestamp: startedAt.Add(time.Minute),
			Payload: map[string]any{"reason": "review", "gate": "review"},
		},
	}
	runs, err := (&portalRunsView{}).computeFromEvents(repoRoot, eventList)
	if err != nil {
		t.Fatalf("compute portal runs: %v", err)
	}
	data, err := json.Marshal(portalSummaryRuns(runs))
	if err != nil {
		t.Fatalf("marshal portal summary: %v", err)
	}

	runPortalHTMLScript(t, `
	const rows = `+string(data)+`;
const projected = rows[0];
if (!projected.waiting || projected.queuedLike) throw new Error('Go projection flags were not transported: ' + JSON.stringify(projected));
if (!projected.canAbort || projected.canArchive) throw new Error('Go action eligibility flags were not transported: ' + JSON.stringify(projected));
if (isWaitStateRun({ ...projected, status: 'queued' })) throw new Error('renderer re-derived queued-like from status instead of reading the Go flag');
if (!SandmanPortalDiff.subjectRunValue({ ...projected, status: 'queued' })) throw new Error('diff renderer re-derived queued-like from status instead of reading the Go flag');
if (isRunAbortable({ ...projected, canAbort: false }, new Set())) throw new Error('renderer re-derived abort availability instead of reading the Go flag');
if (isRunAbortable(projected, new Set([projected.key + ':' + projected.issueNumber]))) throw new Error('renderer ignored the client-side in-flight abort reservation');
if (isRunArchivable({ ...projected, kind: 'completed', canArchive: false, archived: false, unavailable: false, sourceExists: true })) throw new Error('renderer re-derived archive availability instead of reading the Go flag');
console.log('PASS');
`)
}

func TestPortalRowDisplayStatus_CapacityContinuationStaysWaitingAndInitialAdmissionStaysQueued(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "batches"), 0755); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	eventList := []events.Event{
		{
			Type: "run.started", RunID: "capacity-run", Issue: 42, Timestamp: startedAt,
			Payload: map[string]any{"batch_id": "batch-capacity"},
		},
		{Type: "run.capacity_queued", RunID: "capacity-run", Issue: 42, Timestamp: startedAt.Add(time.Minute)},
		{
			Type: "run.queued", RunID: "initial-queue", Issue: 43, Timestamp: startedAt,
			Payload: map[string]any{"batch_id": "batch-initial", "initial_admission": true},
		},
	}

	runs, err := (&portalRunsView{}).computeFromEvents(repoRoot, eventList)
	if err != nil {
		t.Fatalf("compute portal runs: %v", err)
	}
	capacity := findPortalRun(t, runs, "capacity-run")
	if capacity.Status != "waiting" || !capacity.Waiting || capacity.QueuedLike {
		t.Errorf("capacity continuation status/waiting/queuedLike = %q/%v/%v, want waiting/true/false", capacity.Status, capacity.Waiting, capacity.QueuedLike)
	}
	if capacity.ActiveDurationSeconds != 60 || capacity.Duration != "1m0s" {
		t.Errorf("capacity continuation clock = %d seconds / %q, want unchanged 60 seconds / 1m0s", capacity.ActiveDurationSeconds, capacity.Duration)
	}

	initial := findPortalRun(t, runs, "initial-queue")
	if initial.Status != "queued" || initial.Waiting || !initial.QueuedLike {
		t.Errorf("initial admission status/waiting/queuedLike = %q/%v/%v, want queued/false/true", initial.Status, initial.Waiting, initial.QueuedLike)
	}
}

func TestPortalRowDisplayStatus_LiveLinkedReviewPromotesOnlyNonTerminalParent(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "batches"), 0755); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	eventList := []events.Event{
		{
			Type: "run.started", RunID: "waiting-parent", Issue: 42, Timestamp: startedAt,
			Payload: map[string]any{"batch_id": "batch-parent-42"},
		},
		{
			Type: "run.await", RunID: "waiting-parent", Issue: 42, Timestamp: startedAt.Add(time.Minute),
			Payload: map[string]any{"reason": "review", "gate": "review"},
		},
		{
			Type: "run.finished", RunID: "terminal-parent", Issue: 43, Timestamp: startedAt.Add(time.Minute),
			Payload: map[string]any{"batch_id": "batch-parent-43", "status": "success"},
		},
		{
			Type: "run.started", RunID: "live-review-42", Issue: 42, Timestamp: startedAt.Add(2 * time.Minute),
			Payload: map[string]any{"batch_id": "batch-review-42", "review": true, "pr_number": 420},
		},
		{
			Type: "run.started", RunID: "live-review-43", Issue: 43, Timestamp: startedAt.Add(2 * time.Minute),
			Payload: map[string]any{"batch_id": "batch-review-43", "review": true, "pr_number": 430},
		},
	}

	runs, err := (&portalRunsView{}).computeFromEvents(repoRoot, eventList)
	if err != nil {
		t.Fatalf("compute portal runs: %v", err)
	}
	waitingParent := findPortalRun(t, runs, "waiting-parent")
	if waitingParent.Status != "reviewing" || !waitingParent.Waiting || waitingParent.QueuedLike {
		t.Errorf("live-review parent status/waiting/queuedLike = %q/%v/%v, want reviewing/true/false", waitingParent.Status, waitingParent.Waiting, waitingParent.QueuedLike)
	}
	terminalParent := findPortalRun(t, runs, "terminal-parent")
	if terminalParent.Status != "success" || terminalParent.Waiting {
		t.Errorf("terminal parent status/waiting = %q/%v, want success/false", terminalParent.Status, terminalParent.Waiting)
	}
}
