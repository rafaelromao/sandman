package batch

import (
	"context"
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

func (s *runSession) restoreQuotaAccounting() error {
	states, err := events.ReadRunStates(s.deps.eventLog)
	if err != nil {
		s.quotaAllowance = s.quotaAllowance.withoutAccounting()
		return err
	}
	state, ok := states[s.issueRunID()]
	evidence := quotaPollingEvidence(state)
	if !ok || evidence == nil {
		s.quotaAllowance = s.quotaAllowance.withoutAccounting()
		return fmt.Errorf("quota probe has no validated operation evidence")
	}
	s.quotaAllowance, err = s.quotaAllowance.restore(evidence)
	return err
}

// priorObservation permits transport re-observation only for a current,
// previously authorized operation. Historical awaits on ready/terminal rows,
// incomplete identities and absent deadlines authorize no synthetic wait.
func (s *lifecycleObserver) priorObservation(runID, branch, head string) (lifecycleVerdict, bool) {
	states, err := events.ReadRunStates(s.deps.eventLog)
	if err != nil {
		return lifecycleVerdict{}, false
	}
	state, ok := states[runID]
	if !ok || !state.IsAwaiting() || state.IsCapacityQueued() || state.AwaitEvent == nil || state.Branch() != branch || head == "" {
		return lifecycleVerdict{}, false
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
		return lifecycleVerdict{}, false
	}
	if !s.runtimeNow().Before(deadline) {
		extras["gate"] = gate
		extras["reason"] = lifecycleDeadlineReason(gate)
		extras["next_action"] = lifecycleDeadlineNextAction(gate)
		return lifecycleVerdict{Action: lifecycleResume, Evidence: extras}, true
	}
	extras["observation_error"] = "transport-unavailable; retaining established operation deadline"
	return lifecycleVerdict{Action: lifecycleAwait, Evidence: extras}, true
}

func (s *lifecycleObserver) retainedCIRerunEvidence(ctx context.Context, runID, branch, head string, prNumber int, workflowRunID, repository string) map[string]any {
	if prNumber <= 0 || strings.TrimSpace(head) == "" {
		return nil
	}
	states, err := events.ReadRunStates(s.deps.eventLog)
	if err != nil {
		return nil
	}
	state, ok := states[runID]
	if !ok || !state.IsAwaiting() || state.IsCapacityQueued() || state.AwaitEvent == nil || state.Branch() != branch {
		return nil
	}
	ci, ok := state.AwaitEvent.Payload["ci_wait"].(map[string]any)
	if !ok {
		return nil
	}
	data, err := json.Marshal(ci)
	if err != nil {
		return nil
	}
	var registration ciWaitRegistration
	if json.Unmarshal(data, &registration) != nil || registration.HeadSHA != head || registration.PullRequest != prNumber {
		return nil
	}
	if _, err := ciWaitEvidenceFromRegistration(registration, prNumber); err != nil {
		return nil
	}
	action, ok := ciRerunActionFromPayload(state.AwaitEvent.Payload[ciRerunActionKey])
	if !ok {
		return nil
	}
	if strings.TrimSpace(repository) == "" {
		repository, err = s.deps.githubClient.RepoName(ctx)
		if err != nil {
			return nil
		}
		repository = strings.TrimSpace(repository)
	}
	if !action.matchesPullRequest(prNumber, repository, head) {
		return nil
	}
	evidence := map[string]any{ciRerunActionKey: action.payload()}
	if strings.TrimSpace(workflowRunID) == "" {
		// The helper receipt can be retained while replacement checks are
		// ordinary pending CI, before the rollup identifies the containing
		// workflow as a rerun prerequisite.
		return evidence
	}
	prerequisite := map[string]any{
		"protocol":        "ci-rerun-prerequisite/v1",
		"repository":      repository,
		"pull_request":    prNumber,
		"head_sha":        head,
		"workflow_run_id": strings.TrimSpace(workflowRunID),
	}
	if !action.matchesPrerequisite(prerequisite) {
		return nil
	}
	evidence[ciRerunPrerequisiteKey] = prerequisite
	return evidence
}
