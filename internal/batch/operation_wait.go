package batch

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

func (s *runSession) runtimeNow() time.Time {
	return s.opts.runtimeNow()
}

func (opts runSessionOptions) runtimeNow() time.Time {
	if opts.now != nil {
		return opts.now().UTC()
	}
	return time.Now().UTC()
}

func quotaPollingEvidence(state events.RunState) map[string]any {
	if state.IsCapacityQueued() && state.CapacityQueuedEvent != nil && payloadBoolValue(state.CapacityQueuedEvent.Payload, "usage_limit_probe") {
		return state.CapacityQueuedEvent.Payload
	}
	if state.IsAwaiting() && state.AwaitEvent != nil && state.AwaitReason() == "usage-limit" {
		return state.AwaitEvent.Payload
	}
	return nil
}

func quotaPollingWaited(value any) (time.Duration, bool) {
	var seconds int64
	switch typed := value.(type) {
	case int:
		seconds = int64(typed)
	case int64:
		seconds = typed
	case float64:
		if typed < 0 || typed > float64(usageLimitRetryWindow/time.Second) || typed != float64(int64(typed)) {
			return 0, false
		}
		seconds = int64(typed)
	default:
		return 0, false
	}
	if seconds < 0 || seconds > int64(usageLimitRetryWindow/time.Second) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func (s *runSession) restoreQuotaAccounting() error {
	states, err := events.ReadRunStates(s.deps.eventLog)
	if err != nil {
		return err
	}
	state, ok := states[s.issueRunID()]
	evidence := quotaPollingEvidence(state)
	if !ok || evidence == nil {
		return fmt.Errorf("quota probe has no validated operation evidence")
	}
	waited, ok := quotaPollingWaited(evidence["usage_limit_waited_seconds"])
	if !ok {
		return fmt.Errorf("quota probe has no valid completed-poll accounting")
	}
	// A completed poll may already have advanced the in-memory row beyond
	// its last await. Reconstruction must never erase that accounted wait.
	s.usageLimitWaited = max(s.usageLimitWaited, waited)
	if seconds, ok := lifecycleDeadlineSeconds(evidence["usage_limit_deadline_unix_seconds"]); ok {
		s.usageLimitDeadline = time.Unix(seconds, 0)
	}
	return nil
}

// priorObservation permits transport re-observation only for a current,
// previously authorized operation. Historical awaits on ready/terminal rows,
// incomplete identities and absent deadlines authorize no synthetic wait.
func (s *runSession) priorObservation(runID, branch, head string) (string, map[string]any, bool) {
	states, err := events.ReadRunStates(s.deps.eventLog)
	if err != nil {
		return "", nil, false
	}
	state, ok := states[runID]
	if !ok || !state.IsAwaiting() || state.IsCapacityQueued() || state.AwaitEvent == nil || state.Branch() != branch || head == "" {
		return "", nil, false
	}
	extras := cloneLifecycleExtras(state.AwaitEvent.Payload)
	valid := false
	if ci, ok := extras["ci_wait"].(map[string]any); ok {
		data, _ := json.Marshal(ci)
		var registration ciWaitRegistration
		if json.Unmarshal(data, &registration) == nil && strings.EqualFold(registration.HeadSHA, head) {
			_, validationErr := ciWaitEvidenceFromRegistration(registration, registration.PullRequest)
			valid = validationErr == nil && registration.PullRequest > 0
		}
		if !valid {
			delete(extras, "ci_wait")
		}
	}
	if request, ok := extras["review_request"].(map[string]any); ok {
		data, _ := json.Marshal(request)
		var envelope reviewRequestEnvelope
		requestValid := json.Unmarshal(data, &envelope) == nil && envelope.Protocol == "review-wait/v1" &&
			strings.EqualFold(envelope.HeadSHA, head) && envelope.TriggerID != "" && envelope.Repository != "" && envelope.PullRequest > 0 &&
			envelope.TriggerCreatedAt != "" && envelope.ConfirmedAt != "" && envelope.TriggerPrefix != "" && len(envelope.PollPlan) > 0 &&
			envelope.EffectiveTimeout > 0 && envelope.StartedUnixSeconds >= 0 && envelope.DeadlineUnixSeconds == envelope.StartedUnixSeconds+envelope.EffectiveTimeout
		valid = valid || requestValid
		if !requestValid {
			delete(extras, "review_request")
		}
	}
	deadline, gate, bounded := lifecycleDeadline(extras)
	if !valid || !bounded {
		return "", nil, false
	}
	if !s.runtimeNow().Before(deadline) {
		extras["gate"] = gate
		extras["reason"] = lifecycleDeadlineReason(gate)
		extras["next_action"] = lifecycleDeadlineNextAction(gate)
		return "resume", extras, true
	}
	extras["observation_error"] = "transport-unavailable; retaining established operation deadline"
	return "await", extras, true
}
