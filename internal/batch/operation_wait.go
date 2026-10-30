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

// priorOperationEvidence returns the identity-valid review or CI operation of
// the Run's current wait, with its established deadline unchanged. Historical
// awaits on ready/terminal rows and incomplete identities contribute nothing.
// It always returns a usable map; established reports whether it holds one.
func (s *lifecycleObserver) priorOperationEvidence(runID, branch, head string) (map[string]any, bool) {
	evidence := map[string]any{}
	states, err := events.ReadRunStates(s.deps.eventLog)
	if err != nil {
		return evidence, false
	}
	state, ok := states[runID]
	if !ok || !state.IsAwaiting() || state.IsCapacityQueued() || state.AwaitEvent == nil || state.Branch() != branch || head == "" {
		return evidence, false
	}
	valid := false
	if ci, ok := state.AwaitEvent.Payload["ci_wait"].(map[string]any); ok {
		data, _ := json.Marshal(ci)
		var registration ciWaitRegistration
		if json.Unmarshal(data, &registration) == nil && strings.EqualFold(registration.HeadSHA, head) {
			if _, validationErr := ciWaitEvidenceFromRegistration(registration, registration.PullRequest); validationErr == nil && registration.PullRequest > 0 {
				evidence["ci_wait"] = cloneLifecycleExtras(ci)
				valid = true
			}
		}
	}
	if request, ok := state.AwaitEvent.Payload["review_request"].(map[string]any); ok {
		data, _ := json.Marshal(request)
		var envelope reviewRequestEnvelope
		if json.Unmarshal(data, &envelope) == nil && envelope.Protocol == "review-wait/v1" &&
			strings.EqualFold(envelope.HeadSHA, head) && envelope.TriggerID != "" && envelope.Repository != "" && envelope.PullRequest > 0 &&
			envelope.TriggerCreatedAt != "" && envelope.ConfirmedAt != "" && envelope.TriggerPrefix != "" && len(envelope.PollPlan) > 0 &&
			envelope.EffectiveTimeout > 0 && envelope.StartedUnixSeconds >= 0 && envelope.DeadlineUnixSeconds == envelope.StartedUnixSeconds+envelope.EffectiveTimeout {
			evidence["review_request"] = cloneLifecycleExtras(request)
			valid = true
		}
	}
	if _, _, bounded := lifecycleDeadline(evidence); !valid || !bounded {
		return map[string]any{}, false
	}
	return evidence, true
}

func (s *lifecycleObserver) retainedCIRerunEvidence(ctx context.Context, runID, branch, head string, prNumber int, workflowRunID, repository string, failedChecks []github.CIFailedCheck, workflowCheckNames []string) map[string]any {
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
	if len(failedChecks) == 0 || len(workflowCheckNames) == 0 {
		return nil
	}
	checkNames := make([]string, 0, len(failedChecks))
	for _, check := range failedChecks {
		checkNames = append(checkNames, check.Name)
	}
	prerequisite := map[string]any{
		"protocol":             "ci-rerun-prerequisite/v1",
		"repository":           repository,
		"pull_request":         prNumber,
		"head_sha":             head,
		"workflow_run_id":      strings.TrimSpace(workflowRunID),
		"failed_check_names":   checkNames,
		"failed_checks":        append([]github.CIFailedCheck(nil), failedChecks...),
		"workflow_check_names": append([]string(nil), workflowCheckNames...),
	}
	if !action.matchesPrerequisite(prerequisite) {
		return nil
	}
	evidence[ciRerunPrerequisiteKey] = prerequisite
	return evidence
}
