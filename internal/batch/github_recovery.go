package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// GitHub recovery (issue #2938). A pull-request lookup or refresh failure is
// an external outage that usually recovers, so the Run waits instead of
// failing. Recovery is attempted hourly; before each attempt the injected
// status checker reads GitHub's health from githubstatus.com and a reported
// outage skips the attempt. Only 24 hours of continuous failed recovery end
// the Run with GATE_LOOKUP_FAILED. The window is per RunID and lives in the
// run.await evidence, so restart rehydrates it from the event fold while
// `sandman run --continue` (a new RunID) starts a fresh window.
const (
	gateGitHubUnavailable   = "github-unavailable"
	githubUnavailableReason = "GITHUB_UNAVAILABLE"
	githubRecoveryKey       = "github_recovery"
	githubRecoveryProtocol  = "github-recovery/v1"
	githubRecoveryInterval  = time.Hour
	githubRecoveryBound     = 24 * time.Hour
)

// githubRecovery is one recovery window. It is comparable so an unchanged
// window can be recognised without re-recording it.
type githubRecovery struct {
	WindowStarted    time.Time
	NextAttempt      time.Time
	LastChecked      time.Time
	LookupAttempts   int
	StatusChecks     int
	LastStatus       string
	LastStatusDetail string
	LookupError      string
}

func (r githubRecovery) bound() time.Time { return r.WindowStarted.Add(githubRecoveryBound) }

func (r githubRecovery) payload() map[string]any {
	payload := map[string]any{
		"protocol":                    githubRecoveryProtocol,
		"window_started_unix_seconds": r.WindowStarted.Unix(),
		"next_attempt_unix_seconds":   r.NextAttempt.Unix(),
		"bound_unix_seconds":          r.bound().Unix(),
		"lookup_attempts":             r.LookupAttempts,
		"status_checks":               r.StatusChecks,
		"lookup_error":                r.LookupError,
	}
	if !r.LastChecked.IsZero() {
		payload["last_status_check_unix_seconds"] = r.LastChecked.Unix()
		payload["last_status"] = r.LastStatus
		if r.LastStatusDetail != "" {
			payload["last_status_detail"] = r.LastStatusDetail
		}
	}
	return payload
}

func githubRecoveryFromEvidence(evidence map[string]any) (githubRecovery, bool) {
	value, ok := evidence[githubRecoveryKey].(map[string]any)
	if !ok || value["protocol"] != githubRecoveryProtocol {
		return githubRecovery{}, false
	}
	started, okStarted := unixSecondsValue(value["window_started_unix_seconds"])
	next, okNext := unixSecondsValue(value["next_attempt_unix_seconds"])
	if !okStarted || !okNext || next < started {
		return githubRecovery{}, false
	}
	recovery := githubRecovery{
		WindowStarted:  time.Unix(started, 0).UTC(),
		NextAttempt:    time.Unix(next, 0).UTC(),
		LookupAttempts: intValue(value["lookup_attempts"]),
		StatusChecks:   intValue(value["status_checks"]),
	}
	recovery.LookupError, _ = value["lookup_error"].(string)
	if checked, ok := unixSecondsValue(value["last_status_check_unix_seconds"]); ok {
		recovery.LastChecked = time.Unix(checked, 0).UTC()
		recovery.LastStatus, _ = value["last_status"].(string)
		recovery.LastStatusDetail, _ = value["last_status_detail"].(string)
	}
	return recovery, true
}

// githubRecoveryNextAttempt reports when a github-unavailable wait next
// attempts recovery, so pollers sleep until then instead of polling.
func githubRecoveryNextAttempt(evidence map[string]any) (time.Time, bool) {
	if gate, _ := evidence["gate"].(string); gate != gateGitHubUnavailable {
		return time.Time{}, false
	}
	recovery, ok := githubRecoveryFromEvidence(evidence)
	return recovery.NextAttempt, ok
}

// githubRecoveryOperation identifies a github-unavailable wait as a bounded
// scheduler operation: the window's 24-hour bound is its deadline. Retained
// review or CI deadlines inside the wait are not enforced while GitHub cannot
// be observed.
func githubRecoveryOperation(evidence map[string]any) (string, time.Time, bool) {
	if _, ok := githubRecoveryNextAttempt(evidence); !ok {
		return "", time.Time{}, false
	}
	recovery, _ := githubRecoveryFromEvidence(evidence)
	return fmt.Sprintf("github:%d", recovery.WindowStarted.Unix()), recovery.bound(), true
}

func githubRecoverySignature(evidence map[string]any) (string, bool) {
	if _, ok := githubRecoveryNextAttempt(evidence); !ok {
		return "", false
	}
	data, err := json.Marshal(evidence[githubRecoveryKey])
	return string(data), err == nil
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

// githubRecoveryWindow returns the Run's open recovery window: its latest
// event is a github-unavailable wait. A later resume, continuation, capacity
// admission or any other wait has superseded it.
func (o *lifecycleObserver) githubRecoveryWindow(runID string) (githubRecovery, map[string]any, bool) {
	if runID == "" || o.deps.eventLog == nil {
		return githubRecovery{}, nil, false
	}
	states, err := events.ReadRunStates(o.deps.eventLog)
	if err != nil {
		return githubRecovery{}, nil, false
	}
	state, ok := states[runID]
	if !ok || !state.IsAwaiting() || state.IsCapacityQueued() || state.AwaitEvent == nil {
		return githubRecovery{}, nil, false
	}
	if _, ok := githubRecoveryNextAttempt(state.AwaitEvent.Payload); !ok {
		return githubRecovery{}, nil, false
	}
	recovery, _ := githubRecoveryFromEvidence(state.AwaitEvent.Payload)
	return recovery, cloneLifecycleExtras(state.AwaitEvent.Payload), true
}

// checkGitHubStatus records one status check and reports whether a lookup
// should be attempted. An unreachable status page is unknown health, which
// attempts the lookup.
func (o *lifecycleObserver) checkGitHubStatus(ctx context.Context, recovery *githubRecovery, now time.Time) bool {
	recovery.StatusChecks++
	recovery.LastChecked = now
	recovery.LastStatusDetail = ""
	if o.deps.githubStatus == nil {
		recovery.LastStatus = "unreachable"
		return true
	}
	report, err := o.deps.githubStatus.GitHubStatus(ctx)
	if err != nil {
		recovery.LastStatus = "unreachable"
		recovery.LastStatusDetail = err.Error()
		return true
	}
	detail := report.Description
	if len(report.Components) > 0 {
		detail = strings.TrimSpace(detail + " (" + strings.Join(report.Components, ", ") + ")")
	}
	recovery.LastStatusDetail = detail
	if report.Outage {
		recovery.LastStatus = "outage"
		return false
	}
	recovery.LastStatus = "operational"
	return true
}

// githubRecoveryAwait returns the wait for the window. base carries retained
// operation evidence; its review and CI deadlines are kept, not enforced.
func githubRecoveryAwait(base map[string]any, branch string, recovery githubRecovery) lifecycleVerdict {
	evidence := cloneLifecycleExtras(base)
	evidence["gate"] = gateGitHubUnavailable
	evidence["await_reason"] = gateGitHubUnavailable
	evidence["reason"] = githubUnavailableReason
	evidence["next_action"] = fmt.Sprintf("wait for GitHub to recover; the next githubstatus.com-gated lookup is at %s and the Run fails with %s after %s without recovery",
		recovery.NextAttempt.UTC().Format(time.RFC3339), lookupGateReason, recovery.bound().UTC().Format(time.RFC3339))
	evidence["branch"] = branch
	evidence["lookup_error"] = recovery.LookupError
	evidence[githubRecoveryKey] = recovery.payload()
	return lifecycleVerdict{Action: lifecycleAwait, Evidence: evidence, Kind: lifecycleVerdictGitHubUnavailable}
}

func githubRecoveryFailure(branch string, recovery githubRecovery) lifecycleVerdict {
	return lifecycleVerdict{Action: lifecycleFailure, Evidence: map[string]any{
		"reason":          lookupGateReason,
		"next_action":     lookupGateNextAction,
		"branch":          branch,
		"lookup_error":    recovery.LookupError,
		githubRecoveryKey: recovery.payload(),
	}}
}

// observeGitHubRecovery gates observation of a Run whose latest event is a
// github-unavailable wait. It returns handled=false when a lookup should be
// attempted now.
func (o *lifecycleObserver) observeGitHubRecovery(ctx context.Context, request lifecycleObservationRequest, recovery *githubRecovery, base map[string]any) (lifecycleVerdict, bool) {
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	now := o.runtimeNow()
	if now.Before(recovery.NextAttempt) {
		// Not yet time: no lookup and no status check, and the evidence is
		// unchanged so the wait is not re-recorded.
		return githubRecoveryAwait(base, request.Branch, *recovery), true
	}
	attempt := o.checkGitHubStatus(ctx, recovery, now)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if attempt {
		return lifecycleVerdict{}, false
	}
	if !now.Before(recovery.bound()) {
		return githubRecoveryFailure(request.Branch, *recovery), true
	}
	recovery.NextAttempt = now.Add(githubRecoveryInterval)
	return githubRecoveryAwait(base, request.Branch, *recovery), true
}

// recordGitHubLookupFailure extends (or opens) the recovery window after a
// failed lookup and selects the wait or, past the bound, the failure.
func (o *lifecycleObserver) recordGitHubLookupFailure(request lifecycleObservationRequest, verdict lifecycleVerdict, recovery githubRecovery, open bool) lifecycleVerdict {
	now := o.runtimeNow()
	if !open {
		recovery = githubRecovery{WindowStarted: now}
	}
	recovery.LookupAttempts++
	recovery.LookupError, _ = verdict.Evidence["lookup_error"].(string)
	if !now.Before(recovery.bound()) {
		return githubRecoveryFailure(request.Branch, recovery)
	}
	recovery.NextAttempt = now.Add(githubRecoveryInterval)
	return githubRecoveryAwait(verdict.Evidence, request.Branch, recovery)
}
