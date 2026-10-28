package batch

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type rowDriverExecutorFake struct {
	gotRun       RunSpec
	gotCursor    ContinuationCursor
	result       AgentRunResult
	started      bool
	results      []AgentRunResult
	observations []lifecycleVerdict
	events       *[]string
	observeCalls int
	executeCalls int
}

func (f *rowDriverExecutorFake) ExecuteRun(_ context.Context, run RunSpec, cursor ContinuationCursor) (AgentRunResult, bool) {
	f.gotRun, f.gotCursor = run, cursor
	f.executeCalls++
	f.record("execute")
	if len(f.results) > 0 {
		result := f.results[0]
		f.results = f.results[1:]
		return result, f.started
	}
	return f.result, f.started
}

func (f *rowDriverExecutorFake) ObserveRun(context.Context, RunSpec, ContinuationCursor) lifecycleVerdict {
	f.observeCalls++
	f.record("observe")
	if len(f.observations) == 0 {
		return lifecycleVerdict{}
	}
	verdict := f.observations[0]
	f.observations = f.observations[1:]
	return verdict
}

func (f *rowDriverExecutorFake) PersistAwait(context.Context, RunSpec, ContinuationCursor, map[string]any) error {
	f.record("persist-await")
	return nil
}

func (f *rowDriverExecutorFake) FinishObserved(_ context.Context, _ RunSpec, _ ContinuationCursor, verdict lifecycleVerdict) AgentRunResult {
	f.record("finish")
	return AgentRunResult{Status: verdict.status()}
}

func (f *rowDriverExecutorFake) record(event string) {
	if f.events != nil {
		*f.events = append(*f.events, event)
	}
}

type rowDriverGateFake struct {
	acquires    int
	releases    int
	paused      bool
	pauseChecks int
	waitErr     error
	clock       *rowDriverClockFake
	events      *[]string
}

func (f *rowDriverGateFake) Acquire(_ context.Context, _ bool, opportunity awaitOpportunity, observe func() (rowObservationOutcome, error), _ time.Duration) (awaitOpportunity, rowObservationOutcome, error) {
	f.acquires++
	f.record("acquire")
	if observe != nil {
		outcome, err := observe()
		if err != nil || outcome.Kind != rowObservationNone {
			return opportunity, outcome, err
		}
	}
	return opportunity, rowObservationOutcome{}, nil
}

func (f *rowDriverGateFake) Release(bool) { f.releases++; f.record("release") }

func (f *rowDriverGateFake) QuotaPaused() bool {
	f.pauseChecks++
	f.record("quota-check")
	return f.paused && f.pauseChecks == 1
}

func (f *rowDriverGateFake) WaitQuota(_ context.Context, observe func() (rowObservationOutcome, error), _ time.Duration) (rowObservationOutcome, error) {
	f.record("quota-wait")
	if observe != nil {
		outcome, err := observe()
		if err != nil || outcome.Kind != rowObservationNone {
			return outcome, err
		}
	}
	f.paused = false
	return rowObservationOutcome{}, nil
}

func (f *rowDriverGateFake) Wait(_ context.Context, interval time.Duration) error {
	f.record("wait")
	if f.clock != nil {
		f.clock.now = f.clock.now.Add(interval)
	}
	return f.waitErr
}

func (f *rowDriverGateFake) RecordQuotaPaused(RunSpec, ContinuationCursor, bool, bool, time.Time) error {
	f.record("record-quota-paused")
	return nil
}

func (f *rowDriverGateFake) RecordReady(RunSpec, ContinuationCursor, map[string]any, time.Time) error {
	f.record("record-ready")
	return nil
}

func (f *rowDriverGateFake) Checkpoint(_ RunSpec, _ ContinuationCursor, ready bool, _ time.Duration) error {
	if ready {
		f.record("checkpoint-ready")
	} else {
		f.record("checkpoint-await")
	}
	return nil
}

func (f *rowDriverGateFake) Report(AgentRunResult, bool) {}

func (f *rowDriverGateFake) record(event string) {
	if f.events != nil {
		*f.events = append(*f.events, event)
	}
}

type rowDriverClockFake struct{ now time.Time }

func (f *rowDriverClockFake) Now() time.Time { return f.now }

type rowDriverAbortFake struct {
	calls  int
	events *[]string
}

func (f *rowDriverAbortFake) Abort(_ context.Context, run RunSpec, _ ContinuationCursor, result AgentRunResult) AgentRunResult {
	f.calls++
	if f.events != nil {
		*f.events = append(*f.events, "abort")
	}
	result.IssueNumber, result.RunID, result.Branch, result.Status = run.IssueNumber, run.RunID, run.Branch, "aborted"
	return result
}

func TestRowDriverPassesRunIdentityAndContinuationCursor(t *testing.T) {
	ctx := context.Background()
	run := RunSpec{
		RunID: "run-42", IssueNumber: 42, BatchID: "batch-a",
		Branch: "42-fix-row-driver", BaseBranch: "main",
	}
	cursor := ContinuationCursor{
		Mode: ModeContinue, ReuseSession: true,
		PreviousRunID: "run-42", PreviousRunBatchID: "batch-before",
	}
	want := AgentRunResult{IssueNumber: 42, RunID: run.RunID, Branch: run.Branch, Status: "success"}
	executor := &rowDriverExecutorFake{result: want, started: true}
	gates := &rowDriverGateFake{}
	clock := &rowDriverClockFake{now: time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC)}

	got := newRowDriver(executor, gates, clock).Drive(ctx, run, cursor)
	if !reflect.DeepEqual(got.Result, want) || !got.Started {
		t.Fatalf("Drive = %+v, want result %+v and started", got, want)
	}
	if executor.gotRun != run {
		t.Fatalf("executor Run spec = %+v, want unchanged identity %+v", executor.gotRun, run)
	}
	if executor.gotCursor != cursor {
		t.Fatalf("executor cursor = %+v, want %+v", executor.gotCursor, cursor)
	}
	if gates.acquires != 1 || gates.releases != 1 {
		t.Fatalf("gate acquire/release = %d/%d, want one admitted execution", gates.acquires, gates.releases)
	}
}

func TestRowDriverAdvancesCursorWithoutChangingRunIdentity(t *testing.T) {
	run := RunSpec{RunID: "run-42", IssueNumber: 42, BatchID: "batch-a", Branch: "42-fix", BaseBranch: "main"}
	executor := &rowDriverExecutorFake{
		started: true,
		results: []AgentRunResult{
			{IssueNumber: 42, RunID: run.RunID, Branch: run.Branch, Status: "await"},
			{IssueNumber: 42, RunID: run.RunID, Branch: run.Branch, Status: "success"},
		},
		observations: []lifecycleVerdict{{Action: lifecycleResume}, {Action: lifecycleResume}},
	}
	gates := &rowDriverGateFake{}
	got := newRowDriver(executor, gates, &rowDriverClockFake{}).Drive(context.Background(), run, ContinuationCursor{
		Mode: ModeFresh,
	}, rowDriverOptions{
		ReleaseAwaitCapacity: true,
		PollInterval:         func(int) time.Duration { return time.Nanosecond },
	})

	if got.Result.Status != "success" || executor.executeCalls != 2 {
		t.Fatalf("Drive = %+v, executions = %d; want same Run resumed to success", got, executor.executeCalls)
	}
	if executor.gotRun != run {
		t.Fatalf("Run spec changed across re-entry: %+v, want %+v", executor.gotRun, run)
	}
	if executor.gotCursor.Mode != ModeContinue || executor.gotCursor.PreviousRunID != run.RunID || executor.gotCursor.PreviousRunBatchID != run.BatchID || !executor.gotCursor.ReuseSession {
		t.Fatalf("continuation cursor = %+v, want mode/reuse/prior identity advanced", executor.gotCursor)
	}
}

func TestRowDriverAcceptsAwaitResumeThenWaitsForQuotaAdmission(t *testing.T) {
	var events []string
	now := &rowDriverClockFake{now: time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC)}
	gates := &rowDriverGateFake{paused: true, clock: now, events: &events}
	executor := &rowDriverExecutorFake{
		started: true, events: &events,
		result: AgentRunResult{IssueNumber: 42, RunID: "run-42", Branch: "42-fix", Status: "success"},
		observations: []lifecycleVerdict{
			{Action: lifecycleAwait, Evidence: map[string]any{"gate": "review"}},
			{Action: lifecycleResume, Evidence: map[string]any{"gate": "review"}},
			{Action: lifecycleResume}, // quota wait observation
			{Action: lifecycleResume}, // admission observation
		},
	}
	run := RunSpec{RunID: "run-42", IssueNumber: 42, BatchID: "batch-a", Branch: "42-fix", BaseBranch: "main"}
	cursor := ContinuationCursor{Mode: ModeContinue}
	got := newRowDriver(executor, gates, now).Drive(context.Background(), run, cursor, rowDriverOptions{
		Awaiting: true, ReleaseAwaitCapacity: true,
		PollInterval: func(int) time.Duration { return time.Minute },
	})

	if got.Result.Status != "success" || !got.Started {
		t.Fatalf("Drive = %+v, want successful started result", got)
	}
	if got.Cursor.Mode != ModeContinue || got.Cursor.PreviousRunID != "" || got.Cursor.PreviousRunBatchID != "" {
		t.Fatalf("terminal cursor = %+v, want unchanged continuation identity", got.Cursor)
	}
	wantEvents := []string{
		"observe", "persist-await", "checkpoint-await", "wait",
		"observe", "record-ready", "checkpoint-ready",
		"quota-check", "record-quota-paused", "checkpoint-ready", "quota-wait", "observe",
		"quota-check", "acquire", "observe", "quota-check", "execute", "release",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("driver event order = %v, want %v", events, wantEvents)
	}
}

func TestRowDriverAbortFenceStopsAfterWaitFailure(t *testing.T) {
	var events []string
	gates := &rowDriverGateFake{waitErr: errors.New("wait interrupted"), events: &events}
	executor := &rowDriverExecutorFake{
		events:       &events,
		observations: []lifecycleVerdict{{Action: lifecycleAwait, Evidence: map[string]any{"gate": "review"}}},
	}
	abort := &rowDriverAbortFake{events: &events}
	run := RunSpec{RunID: "run-42", IssueNumber: 42, BatchID: "batch-a", Branch: "42-fix", BaseBranch: "main"}
	got := newRowDriver(executor, gates, &rowDriverClockFake{}, abort).Drive(context.Background(), run, ContinuationCursor{}, rowDriverOptions{
		Awaiting: true, ReleaseAwaitCapacity: true,
		PollInterval: func(int) time.Duration { return time.Minute },
	})

	if got.Result.Status != "aborted" || abort.calls != 1 {
		t.Fatalf("Drive = %+v, abort calls %d; want one terminal abort", got, abort.calls)
	}
	if executor.observeCalls != 1 || executor.executeCalls != 0 || gates.acquires != 0 {
		t.Fatalf("post-abort work: observations=%d executions=%d admissions=%d", executor.observeCalls, executor.executeCalls, gates.acquires)
	}
	if got := events[len(events)-1]; got != "abort" {
		t.Fatalf("last event = %q, want abort fence", got)
	}
}
