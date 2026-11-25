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
)

func daemonReviewRequest(prefix string) bool {
	end, _ := reviewtrigger.Match(prefix)
	return end >= 0
}

// The canonical envelope (including its original wall-clock deadline) is a
// request identity and response-validation boundary. Quota changes the managed
// wait deadline, not that envelope or standalone response validation.
func (s *runSession) managedReviewClock(request reviewRequestEnvelope, now time.Time) (time.Time, bool) {
	deadline := time.Unix(int64(request.DeadlineUnixSeconds), 0).UTC()
	if s.deps.layout.SandmanDir == "" || !daemonReviewRequest(request.TriggerPrefix) {
		return deadline, false
	}
	state, err := reviewquota.Read(s.deps.layout.ReviewsDir())
	if err != nil {
		return deadline, false
	}
	started := time.Unix(int64(request.StartedUnixSeconds), 0).UTC()
	return state.Clock(started, deadline, now)
}

func (s *runSession) applyManagedReviewClock(registration *reviewRequestRegistration, evidence retainedReviewEvidence) retainedReviewEvidence {
	now := s.runtimeNow()
	deadline, paused := s.managedReviewClock(registration.Request, now)
	original := time.Unix(int64(registration.Request.DeadlineUnixSeconds), 0)
	if !paused && deadline.Equal(original) {
		return evidence
	}
	// Overlay only the managed clock, while retaining the canonical request
	// identity even if the current response classification was empty.
	evidence.payload = mergeLifecycleDiagnostics(evidence.payload, map[string]any{
		"review_request": reviewRegistrationDiagnostic(registration)["review_request"],
	})
	request := evidence.payload["review_request"].(map[string]any)
	request[reviewEffectiveDeadlineKey] = deadline.Format(time.RFC3339Nano)
	if paused {
		request[reviewQuotaPausedKey] = true
	}
	if evidence.outcome == retainedReviewTimeout && (paused || now.Before(deadline)) {
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
	return ok && confirmedReviewExpiryEvidence(request) && daemonReviewRequest(stringEvidence(request, "trigger_prefix")) &&
		!payloadBoolValue(request, "operation_resolved")
}
