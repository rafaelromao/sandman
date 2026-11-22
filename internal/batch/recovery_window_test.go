package batch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// probeRecoverySpec is a recovery that is not GitHub's: it proves the window
// mechanics are reusable, with their own key, protocol, gate, cadence, bound
// and health components (issue #3009).
var probeRecoverySpec = recoverySpec{
	Key:        "probe_recovery",
	Protocol:   "probe-recovery/v1",
	Gate:       "probe-unavailable",
	Operation:  "probe",
	Interval:   15 * time.Minute,
	Bound:      6 * time.Hour,
	Components: []github.StatusComponent{github.ComponentActions},
}

func probeWindow(started time.Time) recoveryWindow {
	return recoveryWindow{WindowStarted: started, NextAttempt: started.Add(probeRecoverySpec.Interval)}
}

// probeWait is the persisted shape of a wait on the probe gate: the evidence the
// event log folds back, JSON numbers included.
func probeWait(t *testing.T, window recoveryWindow) map[string]any {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"gate":                  probeRecoverySpec.Gate,
		probeRecoverySpec.Key:   probeRecoverySpec.payload(window),
		"unrelated_wait_detail": "kept",
	})
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string]any
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	return evidence
}

func TestRecoveryWindow_EvidenceRoundTripsThroughTheEventFold(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	window := probeWindow(started)
	window.LastChecked = started.Add(time.Minute)
	window.LastStatus, window.LastStatusDetail = "outage", "Actions: degraded_performance"
	window.StatusChecks, window.LookupAttempts, window.LookupError = 3, 2, "boom"

	payload := probeRecoverySpec.payload(window)
	if payload["protocol"] != probeRecoverySpec.Protocol ||
		payload["bound_unix_seconds"] != started.Add(probeRecoverySpec.Bound).Unix() ||
		payload["window_started_unix_seconds"] != started.Unix() ||
		payload["next_attempt_unix_seconds"] != window.NextAttempt.Unix() {
		t.Fatalf("payload = %v, want the spec's protocol and its own bound", payload)
	}

	got, ok := probeRecoverySpec.fromEvidence(probeWait(t, window))
	if !ok || got != window {
		t.Fatalf("round trip = %+v (ok=%v), want %+v", got, ok, window)
	}

	// A window that was never status-checked omits the check fields.
	bare := probeRecoverySpec.payload(probeWindow(started))
	for _, key := range []string{"last_status_check_unix_seconds", "last_status", "last_status_detail"} {
		if _, ok := bare[key]; ok {
			t.Errorf("unchecked window payload carries %q: %v", key, bare)
		}
	}
}

func TestRecoveryWindow_OnlyTheSpecsOwnEvidenceIsItsWindow(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	wait := probeWait(t, probeWindow(started))

	other := probeRecoverySpec
	other.Key = "other_recovery"
	if _, ok := other.fromEvidence(wait); ok {
		t.Error("a window under another evidence key was read")
	}
	other = probeRecoverySpec
	other.Protocol = "probe-recovery/v2"
	if _, ok := other.fromEvidence(wait); ok {
		t.Error("a window with another protocol was read")
	}

	// The next attempt, operation and signature exist only for a wait on the
	// spec's own gate.
	wait["gate"] = "usage-limit"
	if _, ok := probeRecoverySpec.nextAttempt(wait); ok {
		t.Error("a wait on another gate reported a next attempt")
	}
	if _, _, ok := probeRecoverySpec.operation(wait); ok {
		t.Error("a wait on another gate reported an operation")
	}
	if _, ok := probeRecoverySpec.signature(wait); ok {
		t.Error("a wait on another gate reported a signature")
	}

	// Corrupt windows are not windows: a next attempt before the start is invalid.
	corrupt := probeWait(t, recoveryWindow{WindowStarted: started, NextAttempt: started.Add(-time.Minute)})
	if _, ok := probeRecoverySpec.fromEvidence(corrupt); ok {
		t.Error("a window whose next attempt precedes its start was read")
	}
}

func TestRecoveryWindow_WaitIsABoundedSchedulerOperation(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	window := probeWindow(started)
	wait := probeWait(t, window)

	next, ok := probeRecoverySpec.nextAttempt(wait)
	if !ok || !next.Equal(window.NextAttempt) {
		t.Fatalf("next attempt = %v (ok=%v), want %v", next, ok, window.NextAttempt)
	}
	id, deadline, ok := probeRecoverySpec.operation(wait)
	if !ok || id != "probe:1800000000" || !deadline.Equal(started.Add(6*time.Hour)) {
		t.Fatalf("operation = %q until %v (ok=%v), want probe:1800000000 until window start + bound", id, deadline, ok)
	}
	if !window.bound(probeRecoverySpec).Equal(deadline) {
		t.Fatalf("window bound = %v, want the operation deadline %v", window.bound(probeRecoverySpec), deadline)
	}

	first, ok := probeRecoverySpec.signature(wait)
	again, _ := probeRecoverySpec.signature(probeWait(t, window))
	if !ok || first != again {
		t.Fatalf("signature of an unchanged window changed: %q vs %q", first, again)
	}
	advanced := window
	advanced.NextAttempt = advanced.NextAttempt.Add(probeRecoverySpec.Interval)
	if moved, _ := probeRecoverySpec.signature(probeWait(t, advanced)); moved == first {
		t.Fatal("signature did not change when the next attempt moved")
	}
}

// Every consumer of a recovery wait (row driver, foreground wait, scheduler,
// persistence) asks the registry of recovery waits, not one recovery's own
// helper, so a wait of any registered window is bounded, scheduled and
// de-duplicated alike.
func TestRecoveryWaits_RegistryRecognisesEveryRegisteredWindow(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	window := probeWindow(started)
	wait := probeWait(t, window)

	if _, ok := recoveryNextAttempt(wait); ok {
		t.Fatal("an unregistered window was recognised as a recovery wait")
	}
	t.Cleanup(func() { recoveryWaitSpecs = recoveryWaitSpecs[:len(recoveryWaitSpecs)-1] })
	recoveryWaitSpecs = append(recoveryWaitSpecs, probeRecoverySpec)

	if next, ok := recoveryNextAttempt(wait); !ok || !next.Equal(window.NextAttempt) {
		t.Fatalf("next attempt = %v (ok=%v), want %v", next, ok, window.NextAttempt)
	}
	if id, deadline, ok := recoveryOperation(wait); !ok || id != "probe:1800000000" || !deadline.Equal(started.Add(probeRecoverySpec.Bound)) {
		t.Fatalf("operation = %q until %v (ok=%v), want the probe window's bounded operation", id, deadline, ok)
	}
	first, ok := recoverySignature(wait)
	if again, _ := recoverySignature(probeWait(t, window)); !ok || first != again {
		t.Fatalf("signature of an unchanged window changed: %q vs %q", first, again)
	}
	if awaitOperationChanged(wait, probeWait(t, window)) {
		t.Fatal("an unchanged recovery wait was re-recorded")
	}
	advanced := window
	advanced.NextAttempt = advanced.NextAttempt.Add(probeRecoverySpec.Interval)
	if !awaitOperationChanged(wait, probeWait(t, advanced)) {
		t.Fatal("a recovery wait whose next attempt moved was not re-recorded")
	}
	// The GitHub window is registered from the start.
	githubWait := map[string]any{"gate": gateGitHubUnavailable, githubRecoveryKey: githubRecoverySpec.payload(recoveryWindow{WindowStarted: started, NextAttempt: started})}
	if _, _, ok := recoveryOperation(githubWait); !ok {
		t.Fatal("the GitHub recovery wait is not a registered recovery wait")
	}
}

func probeObserver(log events.EventLog) *lifecycleObserver {
	return newLifecycleObserver(&runSession{deps: runDeps{eventLog: log, errorLog: io.Discard}})
}

// logProbeWait records the Run's wait on the probe gate, with retained
// operation evidence alongside the window, as the runtime does.
func logProbeWait(t *testing.T, log *spyEventLog, runID string, at time.Time, window recoveryWindow) {
	t.Helper()
	payload := probeWait(t, window)
	payload["await"], payload["await_reason"] = true, probeRecoverySpec.Gate
	payload["ci_wait"] = map[string]any{"retained": "operation"}
	if err := log.Log(events.Event{Type: "run.await", RunID: runID, Issue: 7, Timestamp: at, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func startProbeRun(t *testing.T, log *spyEventLog, runID string, at time.Time) {
	t.Helper()
	if err := log.Log(events.Event{Type: "run.started", RunID: runID, Issue: 7, Timestamp: at, Payload: map[string]any{"branch": "7-probe"}}); err != nil {
		t.Fatal(err)
	}
}

// The Run's open window is rehydrated from the event fold: its latest event
// is a wait on the spec's gate. The retained wait evidence comes back as a
// top-level copy, so a restart resumes the same window while a new RunID starts
// fresh.
func TestRecoveryWindow_RehydratesTheRunsOpenWindowFromTheEventFold(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	window := probeWindow(started)
	log := &spyEventLog{}
	startProbeRun(t, log, "run-a", started.Add(-time.Hour))
	logProbeWait(t, log, "run-a", started, window)
	observer := probeObserver(log)

	got, base, open, unknown := observer.openRecoveryWindow(probeRecoverySpec, "run-a")
	if unknown || !open || got != window {
		t.Fatalf("window = %+v open=%t unknown=%t, want the persisted window %+v", got, open, unknown, window)
	}
	retained, _ := base["ci_wait"].(map[string]any)
	if retained["retained"] != "operation" {
		t.Fatalf("retained evidence = %v, want the wait's operation evidence", base)
	}
	// The retained evidence is a copy of the wait's top-level keys, so the
	// caller can extend it without rewriting the recorded event.
	base["gate"] = "mutated"
	if _, again, _, _ := observer.openRecoveryWindow(probeRecoverySpec, "run-a"); again["gate"] != probeRecoverySpec.Gate {
		t.Fatalf("retained evidence gate = %v, want the recorded %q: the copy aliases the event log", again["gate"], probeRecoverySpec.Gate)
	}

	// A new RunID (sandman run --continue) has no window: it starts a fresh one.
	if _, _, open, unknown := observer.openRecoveryWindow(probeRecoverySpec, "run-b"); open || unknown {
		t.Fatalf("a different RunID has open=%t unknown=%t, want no window", open, unknown)
	}
	// No Run identity or no event log is no window, not an unknown one.
	if _, _, open, unknown := observer.openRecoveryWindow(probeRecoverySpec, ""); open || unknown {
		t.Fatalf("an empty RunID has open=%t unknown=%t, want no window", open, unknown)
	}
	if _, _, open, unknown := probeObserver(nil).openRecoveryWindow(probeRecoverySpec, "run-a"); open || unknown {
		t.Fatalf("a nil event log has open=%t unknown=%t, want no window", open, unknown)
	}
	// A wait on another gate is not this recovery's window.
	other := probeRecoverySpec
	other.Gate = "other-unavailable"
	if _, _, open, _ := observer.openRecoveryWindow(other, "run-a"); open {
		t.Fatal("a wait on another gate opened the window")
	}
}

// Anything after the wait supersedes the window: a resume or a capacity
// admission means the Run is no longer waiting in it.
func TestRecoveryWindow_LaterEventsCloseTheWindow(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	for _, later := range []string{"run.resumed", "run.capacity_queued"} {
		t.Run(later, func(t *testing.T) {
			log := &spyEventLog{}
			startProbeRun(t, log, "run-a", started.Add(-time.Hour))
			logProbeWait(t, log, "run-a", started, probeWindow(started))
			if err := log.Log(events.Event{Type: later, RunID: "run-a", Issue: 7, Timestamp: started.Add(time.Minute), Payload: map[string]any{"branch": "7-probe"}}); err != nil {
				t.Fatal(err)
			}
			if _, _, open, unknown := probeObserver(log).openRecoveryWindow(probeRecoverySpec, "run-a"); open || unknown {
				t.Fatalf("after %s: open=%t unknown=%t, want the window closed", later, open, unknown)
			}
		})
	}
}

// An unreadable event log hides the window, which cannot be found or
// extended: callers fail closed instead of opening a new window.
func TestRecoveryWindow_UnreadableEventLogIsUnknown(t *testing.T) {
	if _, _, open, unknown := probeObserver(unreadableEventLog{}).openRecoveryWindow(probeRecoverySpec, "run-a"); open || !unknown {
		t.Fatalf("unreadable log: open=%t unknown=%t, want unknown", open, unknown)
	}
}

// scopedProbeStatus is an injected health check that can scope its judgement
// by components and records what it was asked.
type scopedProbeStatus struct {
	report     github.StatusReport
	err        error
	requested  [][]github.StatusComponent
	plainCalls int
	onCheck    func()
}

func (s *scopedProbeStatus) GitHubStatus(context.Context) (github.StatusReport, error) {
	s.plainCalls++
	return s.report, s.err
}

func (s *scopedProbeStatus) GitHubComponentStatus(_ context.Context, components []github.StatusComponent) (github.StatusReport, error) {
	s.requested = append(s.requested, components)
	if s.onCheck != nil {
		s.onCheck()
	}
	return s.report, s.err
}

func gatingObserver(now time.Time, status github.StatusChecker) *lifecycleObserver {
	return newLifecycleObserver(&runSession{
		deps: runDeps{errorLog: io.Discard, githubStatus: status},
		opts: runSessionOptions{now: func() time.Time { return now }},
	})
}

// Before the next attempt there is nothing to do: no status check, and the
// window is unchanged so the wait is not re-recorded. At the attempt, one
// status check judged by the spec's components decides: operational or
// unreachable health attempts recovery; an outage skips the attempt and
// schedules the next one an interval later; an outage past the bound expires
// the window. The window is updated in place for the caller's later failure.
func TestRecoveryWindow_GatesEachAttemptOnComponentScopedHealth(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	next := started.Add(probeRecoverySpec.Interval)
	bound := started.Add(probeRecoverySpec.Bound)

	for _, tc := range []struct {
		name     string
		now      time.Time
		status   *scopedProbeStatus
		nilCheck bool
		gate     recoveryGate
		cause    continuityCause
		checks   int
		nextWant time.Time
		last     string
		detail   string
	}{
		{name: "before the next attempt", now: next.Add(-time.Second), status: &scopedProbeStatus{},
			gate: recoveryWaiting, cause: causeResolving, checks: 0, nextWant: next},
		{name: "healthy attempts recovery", now: next, status: &scopedProbeStatus{report: github.StatusReport{Indicator: "none", Description: "All Systems Operational"}},
			gate: recoveryAttempt, checks: 1, nextWant: next, last: "operational", detail: "All Systems Operational"},
		{name: "unreadable status page attempts recovery", now: next, status: &scopedProbeStatus{err: errors.New("githubstatus.com: HTTP 503")},
			gate: recoveryAttempt, checks: 1, nextWant: next, last: "unreachable", detail: "githubstatus.com: HTTP 503"},
		{name: "no checker attempts recovery without a detail", now: next, nilCheck: true,
			gate: recoveryAttempt, checks: 1, nextWant: next, last: "unreachable"},
		{name: "an outage skips the attempt", now: next, status: &scopedProbeStatus{report: github.StatusReport{Outage: true, Description: "Minor Service Outage", Components: []string{"Actions: degraded_performance"}}},
			gate: recoveryWaiting, cause: causeResolving, checks: 1, nextWant: next.Add(probeRecoverySpec.Interval), last: "outage", detail: "Minor Service Outage (Actions: degraded_performance)"},
		{name: "an outage past the bound expires the window", now: bound, status: &scopedProbeStatus{report: github.StatusReport{Outage: true, Description: "Partial System Outage"}},
			gate: recoveryWaiting, cause: causeExpired, checks: 1, nextWant: next, last: "outage", detail: "Partial System Outage"},
		{name: "healthy status past the bound still attempts", now: bound.Add(time.Hour), status: &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}},
			gate: recoveryAttempt, checks: 1, nextWant: next, last: "operational"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var checker github.StatusChecker
			if !tc.nilCheck {
				checker = tc.status
			}
			window := probeWindow(started)
			gate, cause := gatingObserver(tc.now, checker).gateRecoveryAttempt(t.Context(), probeRecoverySpec, &window)
			if gate != tc.gate || cause != tc.cause {
				t.Fatalf("gate = %v/%q, want %v/%q", gate, cause, tc.gate, tc.cause)
			}
			if window.StatusChecks != tc.checks || !window.NextAttempt.Equal(tc.nextWant) || window.LastStatus != tc.last || window.LastStatusDetail != tc.detail {
				t.Fatalf("window = %+v, want %d checks, next attempt %v, status %q (%q)", window, tc.checks, tc.nextWant, tc.last, tc.detail)
			}
			if tc.checks > 0 && !window.LastChecked.Equal(tc.now) {
				t.Fatalf("last checked = %v, want %v", window.LastChecked, tc.now)
			}
			if !window.WindowStarted.Equal(started) {
				t.Fatalf("window start moved to %v: gating must not restart the bound", window.WindowStarted)
			}
			if tc.status != nil {
				if len(tc.status.requested) != tc.checks || tc.status.plainCalls != 0 {
					t.Fatalf("status requests = %d (plain %d), want %d scoped checks", len(tc.status.requested), tc.status.plainCalls, tc.checks)
				}
				for _, requested := range tc.status.requested {
					if !slices.Equal(requested, probeRecoverySpec.Components) {
						t.Fatalf("health judged by %v, want the spec's components %v", requested, probeRecoverySpec.Components)
					}
				}
			}
		})
	}
}

// A checker written against the plain StatusChecker still gates attempts.
func TestRecoveryWindow_PlainCheckerStillGatesAttempts(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	window := probeWindow(started)
	plain := &plainProbeStatus{report: github.StatusReport{Outage: true}}
	gate, cause := gatingObserver(window.NextAttempt, plain).gateRecoveryAttempt(t.Context(), probeRecoverySpec, &window)
	if gate != recoveryWaiting || cause != causeResolving || plain.calls != 1 || window.LastStatus != "outage" {
		t.Fatalf("gate = %v/%q after %d plain calls, status %q; want a skipped attempt", gate, cause, plain.calls, window.LastStatus)
	}
}

type plainProbeStatus struct {
	report github.StatusReport
	calls  int
}

func (s *plainProbeStatus) GitHubStatus(context.Context) (github.StatusReport, error) {
	s.calls++
	return s.report, nil
}

// An abort wins over everything: before the status check, and when it lands
// during one. Neither records a skipped or attempted recovery.
func TestRecoveryWindow_AbortEndsGatingWithoutAnAttempt(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	status := &scopedProbeStatus{}
	window := probeWindow(started)
	if gate, _ := gatingObserver(window.NextAttempt, status).gateRecoveryAttempt(cancelled, probeRecoverySpec, &window); gate != recoveryAborted || len(status.requested) != 0 {
		t.Fatalf("pre-cancelled gate = %v after %d checks, want aborted without a check", gate, len(status.requested))
	}

	during, cancelDuring := context.WithCancel(t.Context())
	status = &scopedProbeStatus{onCheck: cancelDuring}
	window = probeWindow(started)
	if gate, _ := gatingObserver(window.NextAttempt, status).gateRecoveryAttempt(during, probeRecoverySpec, &window); gate != recoveryAborted {
		t.Fatalf("gate cancelled during the status check = %v, want aborted", gate)
	}
}

// A failed recovery attempt opens the window at the first failure, counts the
// attempt, records its error and schedules the next attempt an interval later.
// An open window keeps its start and its status checks; once the bound passes
// the extension reports expiry and leaves the schedule alone.
func TestRecoveryWindow_FailedAttemptOpensAndExtendsTheWindow(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	now := started.Add(40 * time.Minute)
	observer := gatingObserver(now, nil)

	opened, cause := observer.extendRecoveryWindow(probeRecoverySpec, recoveryWindow{StatusChecks: 9}, false, "lookup-failed")
	want := recoveryWindow{WindowStarted: now, NextAttempt: now.Add(probeRecoverySpec.Interval), LookupAttempts: 1, LookupError: "lookup-failed"}
	if cause != causeResolving || opened != want {
		t.Fatalf("first failure = %+v/%q, want a window opened now: %+v", opened, cause, want)
	}

	open := recoveryWindow{
		WindowStarted: started, NextAttempt: started.Add(probeRecoverySpec.Interval), LookupAttempts: 2,
		StatusChecks: 3, LastChecked: started.Add(time.Minute), LastStatus: "operational", LookupError: "old",
	}
	extended, cause := observer.extendRecoveryWindow(probeRecoverySpec, open, true, "refresh-failed")
	open.LookupAttempts, open.LookupError, open.NextAttempt = 3, "refresh-failed", now.Add(probeRecoverySpec.Interval)
	if cause != causeResolving || extended != open {
		t.Fatalf("extended = %+v/%q, want the same window one attempt later: %+v", extended, cause, open)
	}

	late := gatingObserver(started.Add(probeRecoverySpec.Bound), nil)
	expiring := recoveryWindow{WindowStarted: started, NextAttempt: started.Add(time.Hour), LookupAttempts: 5}
	expired, cause := late.extendRecoveryWindow(probeRecoverySpec, expiring, true, "lookup-failed")
	if cause != causeExpired || expired.LookupAttempts != 6 || !expired.NextAttempt.Equal(expiring.NextAttempt) {
		t.Fatalf("at the bound = %+v/%q, want expiry with the schedule untouched", expired, cause)
	}
}
