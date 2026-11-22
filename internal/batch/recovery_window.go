package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// Recovery window (issue #3009, extracted from the GitHub recovery of #2938).
// A bounded external-recovery wait keeps one window per RunID in its run.await
// evidence: it starts at the first failure, attempts recovery on a fixed
// interval, is gated by a githubstatus.com health check before each attempt,
// and ends the Run only once the bound has passed without recovery. The bound is
// also the wait's scheduler operation deadline. Restart rehydrates the window
// from the event fold; a new RunID starts a fresh one.
//
// The window carries mechanics only. Each recovery supplies its own wait and
// failure verdicts, reasons and continuity rows.

// recoverySpec configures one kind of recovery window.
type recoverySpec struct {
	// Key is the wait evidence key holding the window.
	Key string
	// Protocol tags the window payload; evidence with another protocol is not
	// this recovery's window.
	Protocol string
	// Gate is the await gate of the wait. The window is open only while the
	// Run's latest event is a wait on this gate.
	Gate string
	// Operation prefixes the wait's scheduler operation identity.
	Operation string
	// Interval separates recovery attempts.
	Interval time.Duration
	// Bound is how long recovery may stay failed, measured from the window's
	// start (skipped attempts included), before the wait expires.
	Bound time.Duration
	// Components are the githubstatus.com components whose health gates an
	// attempt. A major or critical incident gates it for any set.
	Components []github.StatusComponent
}

// recoveryWindow is one recovery window. It is comparable so an unchanged
// window can be recognised without re-recording it. LookupAttempts and
// LookupError count the recovery attempts made and carry the last attempt's
// error; their names are the persisted evidence's.
type recoveryWindow struct {
	WindowStarted    time.Time
	NextAttempt      time.Time
	LastChecked      time.Time
	LookupAttempts   int
	StatusChecks     int
	LastStatus       string
	LastStatusDetail string
	LookupError      string
}

func (w recoveryWindow) bound(spec recoverySpec) time.Time { return w.WindowStarted.Add(spec.Bound) }

func (spec recoverySpec) payload(w recoveryWindow) map[string]any {
	payload := map[string]any{
		"protocol":                    spec.Protocol,
		"window_started_unix_seconds": w.WindowStarted.Unix(),
		"next_attempt_unix_seconds":   w.NextAttempt.Unix(),
		"bound_unix_seconds":          w.bound(spec).Unix(),
		"lookup_attempts":             w.LookupAttempts,
		"status_checks":               w.StatusChecks,
		"lookup_error":                w.LookupError,
	}
	if !w.LastChecked.IsZero() {
		payload["last_status_check_unix_seconds"] = w.LastChecked.Unix()
		payload["last_status"] = w.LastStatus
		if w.LastStatusDetail != "" {
			payload["last_status_detail"] = w.LastStatusDetail
		}
	}
	return payload
}

func (spec recoverySpec) fromEvidence(evidence map[string]any) (recoveryWindow, bool) {
	value, ok := evidence[spec.Key].(map[string]any)
	if !ok || value["protocol"] != spec.Protocol {
		return recoveryWindow{}, false
	}
	started, okStarted := unixSecondsValue(value["window_started_unix_seconds"])
	next, okNext := unixSecondsValue(value["next_attempt_unix_seconds"])
	if !okStarted || !okNext || next < started {
		return recoveryWindow{}, false
	}
	window := recoveryWindow{
		WindowStarted:  time.Unix(started, 0).UTC(),
		NextAttempt:    time.Unix(next, 0).UTC(),
		LookupAttempts: intValue(value["lookup_attempts"]),
		StatusChecks:   intValue(value["status_checks"]),
	}
	window.LookupError, _ = value["lookup_error"].(string)
	if checked, ok := unixSecondsValue(value["last_status_check_unix_seconds"]); ok {
		window.LastChecked = time.Unix(checked, 0).UTC()
		window.LastStatus, _ = value["last_status"].(string)
		window.LastStatusDetail, _ = value["last_status_detail"].(string)
	}
	return window, true
}

// nextAttempt reports when a wait on the spec's gate next attempts recovery, so
// pollers sleep until then instead of polling.
func (spec recoverySpec) nextAttempt(evidence map[string]any) (time.Time, bool) {
	if gate, _ := evidence["gate"].(string); gate != spec.Gate {
		return time.Time{}, false
	}
	window, ok := spec.fromEvidence(evidence)
	return window.NextAttempt, ok
}

// operation identifies a wait on the spec's gate as a bounded scheduler
// operation: the window's bound is its deadline.
func (spec recoverySpec) operation(evidence map[string]any) (string, time.Time, bool) {
	if _, ok := spec.nextAttempt(evidence); !ok {
		return "", time.Time{}, false
	}
	window, _ := spec.fromEvidence(evidence)
	return fmt.Sprintf("%s:%d", spec.Operation, window.WindowStarted.Unix()), window.bound(spec), true
}

// signature identifies the window's recorded state, so an observation that
// returns the identical window is not re-recorded.
func (spec recoverySpec) signature(evidence map[string]any) (string, bool) {
	if _, ok := spec.nextAttempt(evidence); !ok {
		return "", false
	}
	data, err := json.Marshal(evidence[spec.Key])
	return string(data), err == nil
}

// openRecoveryWindow returns the Run's open window: its latest event is a wait
// on the spec's gate. A later resume, continuation, capacity admission or any
// other wait has superseded it. base is a top-level copy of that wait's evidence,
// which carries the retained operation and retry evidence. unknown reports an
// unreadable event log: whether a window is open cannot be established, so the
// caller must fail closed rather than open a new window.
func (o *lifecycleObserver) openRecoveryWindow(spec recoverySpec, runID string) (window recoveryWindow, base map[string]any, open, unknown bool) {
	if runID == "" || o.deps.eventLog == nil {
		return recoveryWindow{}, nil, false, false
	}
	states, err := events.ReadRunStates(o.deps.eventLog)
	if err != nil {
		return recoveryWindow{}, nil, false, true
	}
	state, ok := states[runID]
	if !ok || !state.IsAwaiting() || state.IsCapacityQueued() || state.AwaitEvent == nil {
		return recoveryWindow{}, nil, false, false
	}
	if _, ok := spec.nextAttempt(state.AwaitEvent.Payload); !ok {
		return recoveryWindow{}, nil, false, false
	}
	window, _ = spec.fromEvidence(state.AwaitEvent.Payload)
	return window, cloneLifecycleExtras(state.AwaitEvent.Payload), true, false
}

// recoveryGate is what a window decides at an observation.
type recoveryGate uint8

const (
	// recoveryAttempt: attempt recovery now.
	recoveryAttempt recoveryGate = iota
	// recoveryWaiting: keep waiting. The cause says whether the window is still
	// resolving or has passed its bound.
	recoveryWaiting
	// recoveryAborted: the observation was cancelled; nothing was attempted.
	recoveryAborted
)

// gateRecoveryAttempt decides whether an observation attempts recovery. Before
// the next attempt it does nothing: no status check, and the window is left
// unchanged so the wait is not re-recorded. At the attempt it records one
// status check, judged by the spec's components: operational or unreadable
// health attempts, an outage skips the attempt and schedules the next one an
// interval later, and an outage on or after the bound expires the window.
// window is updated in place so the caller's later failure extends it.
func (o *lifecycleObserver) gateRecoveryAttempt(ctx context.Context, spec recoverySpec, window *recoveryWindow) (recoveryGate, continuityCause) {
	if ctx.Err() != nil {
		return recoveryAborted, ""
	}
	now := o.runtimeNow()
	if now.Before(window.NextAttempt) {
		return recoveryWaiting, causeResolving
	}
	attempt := o.checkRecoveryStatus(ctx, spec, window, now)
	if ctx.Err() != nil {
		return recoveryAborted, ""
	}
	if attempt {
		return recoveryAttempt, ""
	}
	if !now.Before(window.bound(spec)) {
		return recoveryWaiting, causeExpired
	}
	window.NextAttempt = now.Add(spec.Interval)
	return recoveryWaiting, causeResolving
}

// extendRecoveryWindow records a failed recovery attempt. A failure with no
// open window opens one starting now, so the bound runs from the first failure;
// an open window keeps its start. It counts the attempt, keeps its error,
// schedules the next attempt an interval later, and reports causeExpired once
// the bound has passed (leaving the schedule alone) or causeResolving.
func (o *lifecycleObserver) extendRecoveryWindow(spec recoverySpec, window recoveryWindow, open bool, attemptError string) (recoveryWindow, continuityCause) {
	now := o.runtimeNow()
	if !open {
		window = recoveryWindow{WindowStarted: now}
	}
	window.LookupAttempts++
	window.LookupError = attemptError
	if !now.Before(window.bound(spec)) {
		return window, causeExpired
	}
	window.NextAttempt = now.Add(spec.Interval)
	return window, causeResolving
}

// checkRecoveryStatus records one status check, judged by the spec's
// components, and reports whether recovery should be attempted. An unreachable
// status page is unknown health, which attempts recovery.
func (o *lifecycleObserver) checkRecoveryStatus(ctx context.Context, spec recoverySpec, window *recoveryWindow, now time.Time) bool {
	window.StatusChecks++
	window.LastChecked = now
	window.LastStatusDetail = ""
	if o.deps.githubStatus == nil {
		window.LastStatus = "unreachable"
		return true
	}
	report, err := github.CheckStatus(ctx, o.deps.githubStatus, spec.Components)
	if err != nil {
		window.LastStatus = "unreachable"
		window.LastStatusDetail = err.Error()
		return true
	}
	detail := report.Description
	if len(report.Components) > 0 {
		detail = strings.TrimSpace(detail + " (" + strings.Join(report.Components, ", ") + ")")
	}
	window.LastStatusDetail = detail
	if report.Outage {
		window.LastStatus = "outage"
		return false
	}
	window.LastStatus = "operational"
	return true
}

func unixSecondsValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), typed > 0
	case int64:
		return typed, typed > 0
	case float64:
		return int64(typed), typed > 0 && typed == float64(int64(typed))
	}
	return 0, false
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	}
	return 0
}
