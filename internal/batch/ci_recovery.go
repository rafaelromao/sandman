package batch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// CI recovery window (issue #3011, ADR-0064). A current-head CI failure the
// implementor diagnosed as transient, whose single authorized rerun failed the
// same way, leaves a CI repair nothing to do: the implementor has no source to
// repair, cannot request review while CI is red, and has spent its rerun. The
// Run waits in a bounded window and the runtime reruns the diagnosed failed job
// itself. The window is the reusable recovery window of recovery_window.go (the
// GitHub recovery window of issue #2938 is its first user): an attempt on the
// first observation, then hourly; each attempt gated by githubstatus.com's
// Actions health; 24 hours from the window's start end the Run with
// CI_RECOVERY_EXHAUSTED. The window lives in the run.await evidence, so restart
// rehydrates it from the event fold, while `sandman run --continue` (a new
// RunID) opens a fresh one.
const (
	gateCIRecovery            = "ci-recovery"
	ciRecoveryReason          = "CI_RECOVERY"
	ciRecoveryExhaustedReason = "CI_RECOVERY_EXHAUSTED"
	ciRecoveryKey             = "ci_recovery"
	ciRecoveryProtocol        = "ci-recovery/v1"
	ciRecoveryInterval        = time.Hour
	ciRecoveryBound           = 24 * time.Hour

	ciRecoveryExhaustedNextAction = "the diagnosed transient CI failure did not recover within 24 hours; once its cause is resolved, run `sandman run <issue> --continue`, which starts a fresh 24-hour recovery window"
)

// ciRecoverySpec binds the reusable recovery window to CI: an attempt every
// hour, a 24-hour bound, and health judged by GitHub Actions alone.
var ciRecoverySpec = recoverySpec{
	Key:        ciRecoveryKey,
	Protocol:   ciRecoveryProtocol,
	Gate:       gateCIRecovery,
	Operation:  "ci",
	Interval:   ciRecoveryInterval,
	Bound:      ciRecoveryBound,
	Components: github.CIRerunComponents(),
}

// ciRerunOutcome is the last rerun attempt of a window: the rerun client's
// states, or "failed" once a confirmed rerun is observed failing again.
type ciRerunOutcome struct {
	State           string
	Reason          string
	ObservedAttempt int
	At              time.Time
}

// ciRecovery is one CI recovery window: the reusable window (comparable, so an
// unchanged window is recognised without re-recording it), the diagnosed
// failure it reruns, and the last rerun outcome. Attempts counts the recovery
// attempts made, skipped hours excluded.
type ciRecovery struct {
	recoveryWindow
	Target ciRerunActionEvidence
	Rerun  ciRerunOutcome
}

func (r ciRecovery) bound() time.Time { return r.recoveryWindow.bound(ciRecoverySpec) }

func (r ciRecovery) payload() map[string]any {
	payload := ciRecoverySpec.payload(r.recoveryWindow)
	payload["attempts"] = r.LookupAttempts
	payload["target"] = r.Target.payload()
	if r.Rerun.State != "" {
		last := map[string]any{"state": r.Rerun.State, "reason": r.Rerun.Reason}
		if r.Rerun.ObservedAttempt > 0 {
			last["observed_attempt"] = r.Rerun.ObservedAttempt
		}
		if !r.Rerun.At.IsZero() {
			last["at_unix_seconds"] = r.Rerun.At.Unix()
		}
		payload["last_rerun"] = last
	}
	return payload
}

// ciRecoveryFromEvidence reads the window from a wait's evidence. Evidence
// without a readable window or without the diagnosed target is not a window.
func ciRecoveryFromEvidence(evidence map[string]any) (ciRecovery, bool) {
	window, ok := ciRecoverySpec.fromEvidence(evidence)
	if !ok {
		return ciRecovery{}, false
	}
	value, _ := evidence[ciRecoveryKey].(map[string]any)
	target, ok := ciRerunActionFromPayload(value["target"])
	if !ok {
		return ciRecovery{}, false
	}
	recovery := ciRecovery{recoveryWindow: window, Target: *target}
	if last, ok := value["last_rerun"].(map[string]any); ok {
		recovery.Rerun.State, _ = last["state"].(string)
		recovery.Rerun.Reason, _ = last["reason"].(string)
		recovery.Rerun.ObservedAttempt = intValue(last["observed_attempt"])
		if at, ok := unixSecondsValue(last["at_unix_seconds"]); ok {
			recovery.Rerun.At = time.Unix(at, 0).UTC()
		}
	}
	return recovery, true
}

// ciRecoveryAwait is the wait for the window. Only the identity of the pull
// request and the stalled-resume ledger are kept from base: the review and CI
// deadlines of earlier operations are not enforced while the window recovers,
// and no hold marker may override the window's own operation identity.
func ciRecoveryAwait(base map[string]any, branch string, recovery ciRecovery) lifecycleVerdict {
	evidence := map[string]any{
		"await":        true,
		"gate":         gateCIRecovery,
		"await_reason": gateCIRecovery,
		"reason":       ciRecoveryReason,
		"next_action": fmt.Sprintf("wait for the runtime to rerun the diagnosed failed job; the next githubstatus.com-gated rerun is at %s and the Run fails with %s after %s without recovery",
			recovery.NextAttempt.UTC().Format(time.RFC3339), ciRecoveryExhaustedReason, recovery.bound().UTC().Format(time.RFC3339)),
		"branch":       branch,
		ciRecoveryKey:  recovery.payload(),
		"lookup_error": recovery.LookupError,
		"pull_request": recovery.Target.PullRequest,
		"head_sha":     recovery.Target.SourceHeadSHA,
	}
	if ledger := base[stalledResumesKey]; ledger != nil {
		evidence[stalledResumesKey] = ledger
	}
	return lifecycleVerdict{Action: lifecycleAwait, Evidence: evidence, Kind: lifecycleVerdictCIRecovery}
}

// ciRecoveryFailure ends the Run once the window has passed its bound. It
// carries the window and, like every terminal failure, no wait marker.
func ciRecoveryFailure(branch string, recovery ciRecovery) lifecycleVerdict {
	return lifecycleVerdict{Action: lifecycleFailure, Evidence: map[string]any{
		"reason":        ciRecoveryExhaustedReason,
		"next_action":   ciRecoveryExhaustedNextAction,
		"external_gate": gateCIRecovery,
		"branch":        branch,
		"pull_request":  recovery.Target.PullRequest,
		"head_sha":      recovery.Target.SourceHeadSHA,
		"lookup_error":  recovery.LookupError,
		ciRecoveryKey:   recovery.payload(),
	}}
}

func (o *lifecycleObserver) ciRecoveryDisposition(base map[string]any, branch string, recovery ciRecovery, cause continuityCause) lifecycleVerdict {
	wait := ciRecoveryAwait(base, branch, recovery)
	row := continuityRow{gate: gateCIRecovery, reason: ciRecoveryReason, cause: cause}
	terminal := ciRecoveryFailure(branch, recovery)
	if cause != causeExpired {
		terminal = lifecycleVerdict{}
	}
	return o.applyContinuityDisposition(row, wait, terminal)
}

// enterCIRecovery turns a CI repair that can no longer be relaunched (its
// allowance is used up, or a relaunch would repeat a stalled signature) into a
// CI recovery wait, when the failure qualifies: a typed transient rerun receipt
// recorded for the logical Run matches the live settled failure, and the client
// can rerun a workflow job. Every other stalled CI repair keeps the outcome the
// continuity table gives it. The window starts now with its first attempt due
// at once, so a fresh window (including the one `--continue` opens) attempts
// immediately.
func (o *lifecycleObserver) enterCIRecovery(branch, runID string, verdict lifecycleVerdict, stalled []string, signature string) (lifecycleVerdict, bool) {
	if !verdict.ciRepairRequired || stringEvidence(verdict.Evidence, "reason") != "CI_FAILURE" || verdict.ciPrerequisite == nil {
		return lifecycleVerdict{}, false
	}
	if _, ok := o.deps.githubClient.(github.WorkflowJobRerunner); !ok {
		return lifecycleVerdict{}, false
	}
	receipt, ok := o.matchingCIRerunReceipt(runID, verdict.ciPrerequisite)
	if !ok {
		return lifecycleVerdict{}, false
	}
	now := o.runtimeNow()
	recovery := ciRecovery{recoveryWindow: recoveryWindow{WindowStarted: now, NextAttempt: now}, Target: receipt}
	base := map[string]any{stalledResumesKey: appendStalledResume(stalled, signature)}
	return o.ciRecoveryDisposition(base, branch, recovery, causeResolving), true
}

// matchingCIRerunReceipt returns the latest typed transient rerun receipt of
// the Run's logical chain (the Run and the Runs it continued) that matches the
// live settled failure. The receipt is evidence the implementor's helper
// produced and an earlier wait recorded; an unreadable event log proves
// nothing, so it matches nothing.
func (o *lifecycleObserver) matchingCIRerunReceipt(runID string, prerequisite map[string]any) (ciRerunActionEvidence, bool) {
	if o.deps.eventLog == nil || strings.TrimSpace(runID) == "" {
		return ciRerunActionEvidence{}, false
	}
	history, err := o.deps.eventLog.Read()
	if err != nil {
		return ciRerunActionEvidence{}, false
	}
	chain := logicalRunChain(history, runID)
	for index := len(history) - 1; index >= 0; index-- {
		event := history[index]
		if !chain[event.RunID] {
			continue
		}
		for _, candidate := range ciRerunReceiptsIn(event.Payload) {
			if candidate.matchesPrerequisite(prerequisite) {
				return candidate, true
			}
		}
	}
	return ciRerunActionEvidence{}, false
}

// ciRerunReceiptsIn lists the typed rerun receipts an event's payload carries:
// the helper's own receipt, and the target a CI recovery window was opened for.
func ciRerunReceiptsIn(payload map[string]any) []ciRerunActionEvidence {
	var receipts []ciRerunActionEvidence
	if receipt, ok := ciRerunActionFromPayload(payload[ciRerunActionKey]); ok {
		receipts = append(receipts, *receipt)
	}
	if window, ok := payload[ciRecoveryKey].(map[string]any); ok {
		if receipt, ok := ciRerunActionFromPayload(window["target"]); ok {
			receipts = append(receipts, *receipt)
		}
	}
	return receipts
}

// logicalRunChain is the set of RunIDs of one logical Run: runID and every Run
// it continued, followed through run.continued previous_run_id links.
func logicalRunChain(history []events.Event, runID string) map[string]bool {
	chain := map[string]bool{}
	for runID != "" && !chain[runID] {
		chain[runID] = true
		next := ""
		for _, event := range history {
			if event.RunID != runID || event.Type != "run.continued" {
				continue
			}
			previous, _ := event.Payload["previous_run_id"].(string)
			if previous = strings.TrimSpace(previous); previous != runID {
				next = previous
			}
			break
		}
		runID = next
	}
	return chain
}

// ciRecoveryWindow returns the Run's open CI recovery window: its latest event
// is a wait on the ci-recovery gate whose evidence names the diagnosed
// failure. An unreadable event log reports no window: whether one is open
// cannot be established, so observation falls through to the ordinary
// decision, whose stalled-signature ledger is unreadable too and fails closed
// into a hold. No attempt is made and no new window is opened.
func (o *lifecycleObserver) ciRecoveryWindow(runID string) (recovery ciRecovery, base map[string]any, open bool) {
	_, base, open, unknown := o.openRecoveryWindow(ciRecoverySpec, runID)
	if unknown || !open {
		return ciRecovery{}, nil, false
	}
	recovery, ok := ciRecoveryFromEvidence(base)
	if !ok {
		return ciRecovery{}, nil, false
	}
	return recovery, base, true
}

// observeCIRecoveryGate gates observation of a Run whose latest event is a
// ci-recovery wait. Before the next attempt it does nothing: no status check,
// no lookup, no rerun, and the window is returned unchanged so the wait is not
// re-recorded. It returns handled=false when an attempt is due and the status
// gate lets it run.
func (o *lifecycleObserver) observeCIRecoveryGate(ctx context.Context, request lifecycleObservationRequest, recovery *ciRecovery, base map[string]any) (lifecycleVerdict, bool) {
	gate, cause := o.gateRecoveryAttempt(ctx, ciRecoverySpec, &recovery.recoveryWindow)
	switch gate {
	case recoveryAborted:
		return lifecycleVerdict{Action: lifecycleAborted}, true
	case recoveryAttempt:
		return lifecycleVerdict{}, false
	}
	return o.ciRecoveryDisposition(base, request.Branch, *recovery, cause), true
}

// attemptCIRecovery judges the live lifecycle verdict of a due attempt against
// the window and returns handled=false when the window ends: a green rerun, a
// new head, a different failing check or any other lifecycle outcome leaves the
// normal lifecycle to decide. While the diagnosed job keeps failing the
// attempt reruns it; while the rerun is still running the window waits.
func (o *lifecycleObserver) attemptCIRecovery(ctx context.Context, request lifecycleObservationRequest, recovery *ciRecovery, base map[string]any, verdict lifecycleVerdict) (lifecycleVerdict, bool) {
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	now := o.runtimeNow()
	switch {
	case verdict.Kind == lifecycleVerdictGitHubUnavailable:
		// The lookup failed: a failed attempt of this window, not another
		// wait that would supersede it and restart its bound.
		return o.recordCIAttempt(base, request.Branch, recovery, stringEvidence(verdict.Evidence, "lookup_error")), true
	case ciSameFailure(verdict, recovery.Target):
		if !now.Before(recovery.bound()) {
			return o.ciRecoveryDisposition(base, request.Branch, *recovery, causeExpired), true
		}
		recovery.Rerun = o.rerunDiagnosedJob(ctx, recovery.Target, now)
		if ctx.Err() != nil {
			return lifecycleVerdict{Action: lifecycleAborted}, true
		}
		return o.recordCIAttempt(base, request.Branch, recovery, recovery.Rerun.failure()), true
	case ciStillRunning(verdict, recovery.Target):
		recovery.Rerun = ciRerunOutcome{State: "waiting", Reason: "ci-still-running", At: now}
		return o.recordCIAttempt(base, request.Branch, recovery, ""), true
	}
	return lifecycleVerdict{}, false
}

// recordCIAttempt counts one attempt, keeps its failure and schedules the next
// one an hour away, or ends the window once its bound has passed.
func (o *lifecycleObserver) recordCIAttempt(base map[string]any, branch string, recovery *ciRecovery, failure string) lifecycleVerdict {
	window, cause := o.extendRecoveryWindow(ciRecoverySpec, recovery.recoveryWindow, true, failure)
	recovery.recoveryWindow = window
	return o.ciRecoveryDisposition(base, branch, *recovery, cause)
}

// failure is the rerun outcome's failed-attempt text: a rerun that was
// refused or could not be confirmed. A confirmed or waiting rerun has none.
func (r ciRerunOutcome) failure() string {
	switch r.State {
	case string(github.WorkflowRerunRefused), string(github.WorkflowRerunUncertain):
		return r.Reason
	}
	return ""
}

// rerunDiagnosedJob submits the one rerun of an attempt through the GitHub
// client seam. The job is addressed by name in the identified workflow run,
// because job database IDs change with every attempt of that run.
func (o *lifecycleObserver) rerunDiagnosedJob(ctx context.Context, target ciRerunActionEvidence, now time.Time) ciRerunOutcome {
	rerunner, ok := o.deps.githubClient.(github.WorkflowJobRerunner)
	if !ok {
		return ciRerunOutcome{State: string(github.WorkflowRerunRefused), Reason: "rerun-unsupported", At: now}
	}
	result, err := rerunner.RerunWorkflowJob(ctx, github.WorkflowRerunRequest{
		Repository: target.Repository, PullRequest: target.PullRequest,
		HeadSHA: target.SourceHeadSHA, TestedSHA: target.TestedSHA,
		RunID: target.RunID, JobName: target.JobName,
	})
	if err != nil {
		return ciRerunOutcome{State: string(github.WorkflowRerunRefused), Reason: "rerun-error: " + err.Error(), At: now}
	}
	return ciRerunOutcome{State: string(result.State), Reason: result.Reason, ObservedAttempt: result.ObservedAttempt, At: now}
}

// ciSameFailure reports that the live CI repair is the failure the window was
// opened for: the same head, workflow run and diagnosed job, with no other
// failing check. A new head or a different failing check is new work for the
// implementor.
func ciSameFailure(verdict lifecycleVerdict, target ciRerunActionEvidence) bool {
	return verdict.Action == lifecycleResume && verdict.ciRepairRequired &&
		stringEvidence(verdict.Evidence, "reason") == "CI_FAILURE" &&
		verdict.ciPrerequisite != nil && target.matchesPrerequisite(verdict.ciPrerequisite)
}

// ciStillRunning reports that the head's CI is still resolving: the rerun the
// window submitted has not finished, so there is nothing to rerun yet.
func ciStillRunning(verdict lifecycleVerdict, target ciRerunActionEvidence) bool {
	if verdict.Action != lifecycleAwait {
		return false
	}
	wait, ok := verdict.Evidence["ci_wait"].(map[string]any)
	if !ok {
		return false
	}
	head, _ := wait["head_sha"].(string)
	return strings.EqualFold(strings.TrimSpace(head), strings.TrimSpace(target.SourceHeadSHA))
}
