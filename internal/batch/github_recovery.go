package batch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// GitHub recovery (issue #2938). A pull-request lookup or refresh failure is
// an external outage that usually recovers, so the Run waits instead of
// failing. Recovery is attempted hourly; before each attempt the injected
// status checker reads GitHub's health from githubstatus.com and a reported
// outage skips the attempt. Only 24 hours of continuous failed recovery end
// the Run with GATE_LOOKUP_FAILED. The window is per RunID and lives in the
// run.await evidence, so restart rehydrates it from the event fold while
// `sandman run --continue` (a new RunID) starts a fresh window. The window
// mechanics are the reusable recovery window of recovery_window.go (issue
// #3009); this file binds them to GitHub and owns GitHub's wait and failure
// verdicts.
const (
	gateGitHubUnavailable   = "github-unavailable"
	githubUnavailableReason = "GITHUB_UNAVAILABLE"
	githubRecoveryKey       = "github_recovery"
	githubRecoveryProtocol  = "github-recovery/v1"
	ordinaryRetryKey        = "ordinary_retry"
	ordinaryRetryProtocol   = "ordinary-retry/v1"
	githubRecoveryInterval  = time.Hour
	githubRecoveryBound     = 24 * time.Hour
)

// ordinaryRetryState preserves the failed attempt and its configured bound
// across an external GitHub recovery wait. AttemptsCompleted is the 1-based
// count of agent attempts that have already run; the next retry is logged and
// launched only after GitHub recovers.
type ordinaryRetryState struct {
	PreviousStatus    string
	AttemptsCompleted int
	MaxAttempts       int
}

func (s ordinaryRetryState) payload() map[string]any {
	return map[string]any{
		"protocol":           ordinaryRetryProtocol,
		"previous_status":    s.PreviousStatus,
		"attempts_completed": s.AttemptsCompleted,
		"max_attempts":       s.MaxAttempts,
	}
}

func ordinaryRetryFromEvidence(evidence map[string]any) (ordinaryRetryState, bool) {
	value, ok := evidence[ordinaryRetryKey].(map[string]any)
	if !ok || value["protocol"] != ordinaryRetryProtocol {
		return ordinaryRetryState{}, false
	}
	status, _ := value["previous_status"].(string)
	state := ordinaryRetryState{
		PreviousStatus:    strings.TrimSpace(strings.ToLower(status)),
		AttemptsCompleted: intValue(value["attempts_completed"]),
		MaxAttempts:       intValue(value["max_attempts"]),
	}
	if !attemptNeedsRetry(state.PreviousStatus) || state.AttemptsCompleted < 1 || state.MaxAttempts < state.AttemptsCompleted {
		return ordinaryRetryState{}, false
	}
	return state, true
}

func (s ordinaryRetryState) hasRemainingRetry() bool {
	return s.AttemptsCompleted < s.MaxAttempts
}

// selectedOrdinaryRetry rehydrates only a scheduler-selected retry handoff.
// A later await or retry event consumes/replaces that handoff; the event log
// remains authoritative across process restart.
func selectedOrdinaryRetry(log events.EventLog, runID string) (ordinaryRetryState, bool) {
	if log == nil || strings.TrimSpace(runID) == "" {
		return ordinaryRetryState{}, false
	}
	history, err := log.Read()
	if err != nil {
		return ordinaryRetryState{}, false
	}
	var selected ordinaryRetryState
	pending := false
	for _, event := range history {
		if event.RunID != runID {
			continue
		}
		switch event.Type {
		case "run.capacity_queued":
			selected, pending = ordinaryRetryFromEvidence(event.Payload)
		case "run.retry", "run.await", "run.started", "run.finished", "run.aborted", "run.cancelled":
			pending = false
		}
	}
	return selected, pending
}

// githubRecoverySpec binds the reusable recovery window to GitHub: an hourly
// attempt, a 24-hour bound, and health judged by the components that make
// pull-request lookups fail.
var githubRecoverySpec = recoverySpec{
	Key:        githubRecoveryKey,
	Protocol:   githubRecoveryProtocol,
	Gate:       gateGitHubUnavailable,
	Operation:  "github",
	Interval:   githubRecoveryInterval,
	Bound:      githubRecoveryBound,
	Components: github.PullRequestLookupComponents,
}

// githubRecovery is one GitHub recovery window: the reusable window's data,
// bound to GitHub's spec.
type githubRecovery recoveryWindow

func (r githubRecovery) bound() time.Time { return recoveryWindow(r).bound(githubRecoverySpec) }

func (r githubRecovery) payload() map[string]any {
	return githubRecoverySpec.payload(recoveryWindow(r))
}

func githubRecoveryFromEvidence(evidence map[string]any) (githubRecovery, bool) {
	window, ok := githubRecoverySpec.fromEvidence(evidence)
	return githubRecovery(window), ok
}

// githubRecoveryNextAttempt reports when a github-unavailable wait next
// attempts recovery, so pollers sleep until then instead of polling.
func githubRecoveryNextAttempt(evidence map[string]any) (time.Time, bool) {
	return githubRecoverySpec.nextAttempt(evidence)
}

// githubRecoveryOperation identifies a github-unavailable wait as a bounded
// scheduler operation: the window's 24-hour bound is its deadline. Retained
// review or CI deadlines inside the wait are not enforced while GitHub cannot
// be observed.
func githubRecoveryOperation(evidence map[string]any) (string, time.Time, bool) {
	return githubRecoverySpec.operation(evidence)
}

func githubRecoverySignature(evidence map[string]any) (string, bool) {
	return githubRecoverySpec.signature(evidence)
}

// githubRecoveryWindow returns the Run's open GitHub recovery window: its
// latest event is a github-unavailable wait. unknown reports an unreadable
// event log: whether a window is open cannot be established.
func (o *lifecycleObserver) githubRecoveryWindow(runID string) (recovery githubRecovery, base map[string]any, open, unknown bool) {
	window, base, open, unknown := o.openRecoveryWindow(githubRecoverySpec, runID)
	return githubRecovery(window), base, open, unknown
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
	gate, cause := o.gateRecoveryAttempt(ctx, githubRecoverySpec, (*recoveryWindow)(recovery))
	switch gate {
	case recoveryAborted:
		return lifecycleVerdict{Action: lifecycleAborted}, true
	case recoveryAttempt:
		return lifecycleVerdict{}, false
	}
	return o.githubRecoveryDisposition(base, request.Branch, *recovery, cause), true
}

func (o *lifecycleObserver) githubRecoveryDisposition(base map[string]any, branch string, recovery githubRecovery, cause continuityCause) lifecycleVerdict {
	wait := githubRecoveryAwait(base, branch, recovery)
	row := continuityRow{gate: gateGitHubUnavailable, reason: githubUnavailableReason, cause: cause}
	terminal := githubRecoveryFailure(branch, recovery)
	if cause != causeExpired {
		terminal = lifecycleVerdict{}
	}
	return o.applyContinuityDisposition(row, wait, terminal)
}

// recordGitHubLookupFailure extends (or opens) the recovery window after a
// failed lookup and selects the wait or, past the bound, the failure.
func (o *lifecycleObserver) recordGitHubLookupFailure(request lifecycleObservationRequest, verdict lifecycleVerdict, recovery githubRecovery, open, unknown bool) lifecycleVerdict {
	if request.OrdinaryRetry != nil && !request.UsageLimitReached && attemptNeedsRetry(request.OrdinaryRetry.PreviousStatus) {
		verdict.Evidence[ordinaryRetryKey] = request.OrdinaryRetry.payload()
	}
	if unknown {
		// The event log is unreadable, so the window cannot be found or
		// extended. Fail closed into a wait without opening a new window,
		// which would restart the 24-hour bound on every poll.
		evidence := cloneLifecycleExtras(verdict.Evidence)
		evidence["gate"] = gateGitHubUnavailable
		evidence["await_reason"] = gateGitHubUnavailable
		evidence["reason"] = githubUnavailableReason
		evidence["branch"] = request.Branch
		evidence["observation_error"] = "event log unreadable; GitHub recovery window unknown"
		wait := lifecycleVerdict{Action: lifecycleAwait, Evidence: evidence, Kind: lifecycleVerdictGitHubUnavailable}
		return o.applyContinuityDisposition(
			continuityRow{gate: gateGitHubUnavailable, reason: githubUnavailableReason, cause: causeResolving},
			wait, lifecycleVerdict{},
		)
	}
	lookupError, _ := verdict.Evidence["lookup_error"].(string)
	window, cause := o.extendRecoveryWindow(githubRecoverySpec, recoveryWindow(recovery), open, lookupError)
	return o.githubRecoveryDisposition(verdict.Evidence, request.Branch, githubRecovery(window), cause)
}
