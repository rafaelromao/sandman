package batch

import (
	"fmt"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/reviewquota"
	"github.com/rafaelromao/sandman/internal/reviewtrigger"
)

const (
	reviewQuotaPausedKey       = "review_quota_paused"
	reviewEffectiveDeadlineKey = "review_effective_deadline_at"
	expiredOperationKey        = "expired_operation_diagnostic"
	liveReviewResolverKey      = "live_review_resolver"
)

func daemonReviewRequest(prefix string) bool {
	end, _ := reviewtrigger.Match(prefix)
	return end >= 0
}

type managedReviewDeadline struct {
	observedAt, original, deadline time.Time
	paused, daemonPaused           bool
}

func (c managedReviewDeadline) active() bool { return c.paused || c.observedAt.Before(c.deadline) }

func (c managedReviewDeadline) adjusted() bool { return c.paused || !c.deadline.Equal(c.original) }

// The canonical envelope (including its original wall-clock deadline) is a
// request identity and response-validation boundary. Quota changes the managed
// wait deadline, not that envelope or standalone response validation.
func (s *runSession) managedReviewClock(request reviewRequestEnvelope, now time.Time) managedReviewDeadline {
	deadline := time.Unix(int64(request.DeadlineUnixSeconds), 0).UTC()
	clock := managedReviewDeadline{observedAt: now, original: deadline, deadline: deadline}
	if s.deps.layout.SandmanDir == "" || !daemonReviewRequest(request.TriggerPrefix) {
		return clock
	}
	state, err := reviewquota.Read(s.deps.layout.ReviewsDir())
	if err != nil {
		return clock
	}
	started := time.Unix(int64(request.StartedUnixSeconds), 0).UTC()
	clock.deadline, clock.paused = state.Clock(started, deadline, now)
	clock.daemonPaused = state.Paused
	return clock
}

func applyManagedReviewClock(registration *reviewRequestRegistration, evidence retainedReviewEvidence) retainedReviewEvidence {
	clock := evidence.clock
	if !clock.adjusted() {
		return evidence
	}
	// Overlay only the managed clock, while retaining the canonical request
	// identity even if the current response classification was empty.
	evidence.payload = mergeLifecycleDiagnostics(evidence.payload, map[string]any{
		"review_request": reviewRegistrationDiagnostic(registration)["review_request"],
	})
	request := evidence.payload["review_request"].(map[string]any)
	request[reviewEffectiveDeadlineKey] = clock.deadline.Format(time.RFC3339Nano)
	if clock.paused {
		request[reviewQuotaPausedKey] = true
	}
	if evidence.outcome == retainedReviewTimeout && clock.active() {
		evidence.outcome = retainedReviewPending
		delete(evidence.payload, "gate")
		delete(evidence.payload, "reason")
		delete(evidence.payload, "next_action")
	}
	return evidence
}

func reviewRequestOperationDeadline(request map[string]any) (time.Time, bool) {
	base, ok := lifecycleDeadlineSeconds(request["deadline_unix_seconds"])
	if !ok || payloadBoolValue(request, reviewQuotaPausedKey) {
		return time.Time{}, false
	}
	deadline := time.Unix(base, 0)
	if adjusted, err := time.Parse(time.RFC3339Nano, stringEvidence(request, reviewEffectiveDeadlineKey)); err == nil && adjusted.After(deadline) {
		return adjusted, true
	}
	return deadline, true
}

func (s *runSession) deadlineObservationTime(gate string, clock managedReviewDeadline) time.Time {
	if gate == gateReviewTimeout && !clock.observedAt.IsZero() {
		return clock.observedAt
	}
	return s.runtimeNow()
}

// A daemon-backed review can lose its wall-clock cutoff between observations.
// The ownership lease therefore uses fixed recovery grace, capped only by
// independent CI when present. The observer enforces the actual review budget.
func lifecycleOwnershipOperation(extras map[string]any, runID string) (string, time.Time, bool) {
	if unresolvedDaemonReviewRequest(extras) {
		if deadline, bounded := lifecycleCIWaitDeadline(extras); bounded {
			return fmt.Sprintf("%s:%d", gateCIWaitTimeout, deadline.Unix()), deadline, true
		}
		return daemon.DaemonReviewWaitID(runID), time.Time{}, true
	}
	if deadline, gate, bounded := lifecycleDeadline(extras); bounded {
		return fmt.Sprintf("%s:%d", gate, deadline.Unix()), deadline, true
	}
	return "", time.Time{}, false
}

func unresolvedDaemonReviewRequest(extras map[string]any) bool {
	request, ok := extras["review_request"].(map[string]any)
	if !ok {
		diagnostic, expired := deferredReviewDiagnostic(extras)
		if !expired || !payloadBoolValue(diagnostic, liveReviewResolverKey) {
			return false
		}
		request, _ = diagnostic["evidence"].(map[string]any)
	}
	return confirmedReviewExpiryEvidence(request) && daemonReviewRequest(stringEvidence(request, "trigger_prefix")) &&
		!payloadBoolValue(request, "operation_resolved")
}

func deferredReviewDiagnostic(extras map[string]any) (map[string]any, bool) {
	diagnostic, ok := extras[expiredOperationKey].(map[string]any)
	return diagnostic, ok && stringEvidence(diagnostic, "gate") == gateReviewTimeout
}
