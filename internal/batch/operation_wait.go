package batch

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

func (s *runSession) runtimeNow() time.Time {
	if s.opts.now != nil {
		return s.opts.now().UTC()
	}
	return time.Now().UTC()
}

func (s *runSession) restoreQuotaDeadline() {
	states, err := events.ReadRunStates(s.deps.eventLog)
	if err != nil {
		return
	}
	state, ok := states[s.issueRunID()]
	if !ok || state.AwaitEvent == nil || state.AwaitReason() != "usage-limit" {
		return
	}
	if seconds, ok := lifecycleDeadlineSeconds(state.AwaitEvent.Payload["usage_limit_deadline_unix_seconds"]); ok {
		s.usageLimitDeadline = time.Unix(seconds, 0)
	}
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
