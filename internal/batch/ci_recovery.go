package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

const (
	ciRecoveryKey                    = "ci_recovery"
	ciRecoveryProtocol               = "ci-recovery/v1"
	ciRecoveryGate                   = "ci-recovery"
	ciRecoveryExhaustedReason        = "CI_RECOVERY_EXHAUSTED"
	ciRecoveryStateUnavailableReason = "CI_RECOVERY_STATE_UNAVAILABLE"
	ciRecoveryInterval               = time.Hour
	ciRecoveryBound                  = 24 * time.Hour
	ciRecoveryPrerequisiteKey        = "ci_recovery_prerequisite"
	ciRecoveryStatusEvent            = "ci.recovery.status_check"
	ciRecoveryIntentEvent            = "ci.recovery.rerun_intent"
	ciRecoveryOutcomeEvent           = "ci.recovery.rerun_outcome"
)

var ciRecoverySpec = recoverySpec{
	Key:        ciRecoveryKey,
	Protocol:   ciRecoveryProtocol,
	Gate:       ciRecoveryGate,
	Operation:  "ci-recovery",
	Interval:   ciRecoveryInterval,
	Bound:      ciRecoveryBound,
	Components: []github.StatusComponent{github.ComponentActions},
}

type ciRecoveryMutationIntent struct {
	ID                 string `json:"id"`
	WorkflowRunID      string `json:"workflow_run_id"`
	JobID              int64  `json:"job_id"`
	JobName            string `json:"job_name"`
	WorkflowAttempt    int    `json:"workflow_attempt"`
	StartedUnixSeconds int64  `json:"started_unix_seconds"`
}

type ciRecoveryState struct {
	Window              recoveryWindow
	Repository          string
	Issue               int
	Branch              string
	PullRequest         int
	HeadSHA             string
	WorkflowRunID       string
	JobName             string
	Diagnosis           string
	Receipt             ciRerunActionEvidence
	FailedChecks        []github.CIFailedCheck
	WorkflowCheckNames  []string
	RerunAttempts       int
	LastRerunOutcome    string
	LastRerunJobID      int64
	LastWorkflowAttempt int
	MutationIntent      *ciRecoveryMutationIntent
}

func (r ciRecoveryState) bound() time.Time { return r.Window.bound(ciRecoverySpec) }

func (r ciRecoveryState) payload() map[string]any {
	payload := ciRecoverySpec.payload(r.Window)
	payload["repository"] = r.Repository
	payload["issue"] = r.Issue
	payload["branch"] = r.Branch
	payload["pull_request"] = r.PullRequest
	payload["head_sha"] = r.HeadSHA
	payload["workflow_run_id"] = r.WorkflowRunID
	payload["job_name"] = r.JobName
	payload["diagnosis"] = r.Diagnosis
	payload["receipt"] = r.Receipt.payload()
	payload["failed_checks"] = r.FailedChecks
	payload["workflow_check_names"] = r.WorkflowCheckNames
	payload["rerun_attempts"] = r.RerunAttempts
	payload["last_rerun_outcome"] = r.LastRerunOutcome
	if r.LastRerunJobID > 0 {
		payload["last_rerun_job_id"] = r.LastRerunJobID
	}
	if r.LastWorkflowAttempt > 0 {
		payload["last_workflow_attempt"] = r.LastWorkflowAttempt
	}
	if r.MutationIntent != nil {
		payload["mutation_intent"] = r.MutationIntent
	}
	return payload
}

func ciRecoveryFromEvidence(evidence map[string]any) (ciRecoveryState, bool) {
	window, ok := ciRecoverySpec.fromEvidence(evidence)
	if !ok {
		return ciRecoveryState{}, false
	}
	value, ok := evidence[ciRecoveryKey].(map[string]any)
	if !ok {
		return ciRecoveryState{}, false
	}
	receipt, ok := ciRerunActionFromPayload(value["receipt"])
	if !ok || !receipt.confirmsRecoveryFor(ciRecoveryPrerequisite(value)) {
		return ciRecoveryState{}, false
	}
	prerequisite := ciRecoveryPrerequisite(value)
	failedChecks, checksOK := ciRerunFailedChecks(prerequisite["failed_checks"])
	checkNames, namesOK := ciRerunWorkflowCheckNames(prerequisite["workflow_check_names"])
	if !checksOK || !namesOK {
		return ciRecoveryState{}, false
	}
	issue := intValue(value["issue"])
	prNumber := intValue(value["pull_request"])
	repository, _ := value["repository"].(string)
	branch, _ := value["branch"].(string)
	head, _ := value["head_sha"].(string)
	workflowRunID, _ := value["workflow_run_id"].(string)
	jobName, _ := value["job_name"].(string)
	diagnosis, _ := value["diagnosis"].(string)
	if issue <= 0 || prNumber != receipt.PullRequest || strings.TrimSpace(branch) == "" ||
		strings.TrimSpace(repository) == "" || strings.TrimSpace(head) == "" ||
		workflowRunID != receipt.RunID || jobName != receipt.JobName || diagnosis != receipt.Diagnosis {
		return ciRecoveryState{}, false
	}
	state := ciRecoveryState{
		Window: window, Repository: repository, Issue: issue, Branch: branch, PullRequest: prNumber,
		HeadSHA: head, WorkflowRunID: workflowRunID, JobName: jobName, Diagnosis: diagnosis,
		Receipt: *receipt, FailedChecks: failedChecks, WorkflowCheckNames: checkNames,
		RerunAttempts:       intValue(value["rerun_attempts"]),
		LastRerunOutcome:    stringField(value["last_rerun_outcome"]),
		LastRerunJobID:      int64Value(value["last_rerun_job_id"]),
		LastWorkflowAttempt: intValue(value["last_workflow_attempt"]),
	}
	if rawIntent, ok := value["mutation_intent"]; ok {
		data, err := json.Marshal(rawIntent)
		if err != nil {
			return ciRecoveryState{}, false
		}
		var intent ciRecoveryMutationIntent
		if json.Unmarshal(data, &intent) != nil || intent.ID == "" || intent.JobID <= 0 ||
			intent.WorkflowAttempt <= 0 || intent.WorkflowRunID != state.WorkflowRunID ||
			intent.JobName != state.JobName {
			return ciRecoveryState{}, false
		}
		state.MutationIntent = &intent
	}
	return state, true
}

func ciRecoveryPrerequisite(payload map[string]any) map[string]any {
	return map[string]any{
		"protocol":             "ci-rerun-prerequisite/v1",
		"repository":           payload["repository"],
		"pull_request":         payload["pull_request"],
		"head_sha":             payload["head_sha"],
		"workflow_run_id":      payload["workflow_run_id"],
		"failed_checks":        payload["failed_checks"],
		"workflow_check_names": payload["workflow_check_names"],
	}
}

func ciRecoveryNextAttempt(evidence map[string]any) (time.Time, bool) {
	return ciRecoverySpec.nextAttempt(evidence)
}

func ciRecoveryOperation(evidence map[string]any) (string, time.Time, bool) {
	return ciRecoverySpec.operation(evidence)
}

func ciRecoverySignature(evidence map[string]any) (string, bool) {
	return ciRecoverySpec.signature(evidence)
}

func (o *lifecycleObserver) ciRecoveryWindow(runID string) (ciRecoveryState, map[string]any, bool, bool) {
	if runID == "" || o.deps.eventLog == nil {
		return ciRecoveryState{}, nil, false, false
	}
	states, err := events.ReadRunStates(o.deps.eventLog)
	if err != nil {
		return ciRecoveryState{}, nil, false, true
	}
	run := states[runID]
	if !run.IsAwaiting() || run.IsCapacityQueued() || run.AwaitEvent == nil ||
		stringEvidence(run.AwaitEvent.Payload, "gate") != ciRecoveryGate {
		return ciRecoveryState{}, nil, false, false
	}
	base := cloneLifecycleExtras(run.AwaitEvent.Payload)
	state, valid := ciRecoveryFromEvidence(base)
	if !valid || state.Issue != run.IssueNumber() || state.Branch != run.Branch() {
		return ciRecoveryState{}, base, false, true
	}
	if err := rehydrateCIRerunIntent(o.deps.eventLog, runID, &state); err != nil {
		return ciRecoveryState{}, base, false, true
	}
	return state, base, true, false
}

func rehydrateCIRerunIntent(log events.EventLog, runID string, recovery *ciRecoveryState) error {
	if log == nil {
		return fmt.Errorf("CI recovery event log is unavailable")
	}
	history, err := log.Read()
	if err != nil {
		return err
	}
	var intent *ciRecoveryMutationIntent
	var lastMutation ciRecoveryState
	mutationAfterAwait := false
	for _, event := range history {
		if event.RunID != runID || event.Payload == nil {
			continue
		}
		window, ok := event.Payload[ciRecoveryKey].(map[string]any)
		if !ok || int64Value(window["window_started_unix_seconds"]) != recovery.Window.WindowStarted.Unix() {
			continue
		}
		switch event.Type {
		case "run.await":
			mutationAfterAwait = false
		case ciRecoveryStatusEvent:
			window, valid := ciRecoverySpec.fromEvidence(event.Payload)
			if !valid {
				return fmt.Errorf("CI recovery status evidence is invalid")
			}
			if window.StatusChecks > recovery.Window.StatusChecks {
				recovery.Window = window
			}
		case ciRecoveryIntentEvent:
			raw, ok := event.Payload["mutation_intent"]
			if !ok {
				return fmt.Errorf("CI recovery mutation intent is missing")
			}
			data, marshalErr := json.Marshal(raw)
			if marshalErr != nil {
				return marshalErr
			}
			var decoded ciRecoveryMutationIntent
			if json.Unmarshal(data, &decoded) != nil || decoded.ID == "" || decoded.JobID <= 0 ||
				decoded.WorkflowRunID != recovery.WorkflowRunID || decoded.JobName != recovery.JobName ||
				decoded.WorkflowAttempt <= 0 {
				return fmt.Errorf("CI recovery mutation intent is invalid")
			}
			latest, valid := ciRecoveryFromEvidence(event.Payload)
			if !valid || latest.MutationIntent == nil || latest.MutationIntent.ID != decoded.ID {
				return fmt.Errorf("CI recovery mutation intent evidence is invalid")
			}
			intent = &decoded
			lastMutation, mutationAfterAwait = latest, true
		case ciRecoveryOutcomeEvent:
			if intent == nil || stringField(event.Payload["intent_id"]) != intent.ID {
				return fmt.Errorf("CI recovery outcome does not match its mutation intent")
			}
			latest, valid := ciRecoveryFromEvidence(event.Payload)
			if !valid {
				return fmt.Errorf("CI recovery outcome evidence is invalid")
			}
			lastMutation, mutationAfterAwait = latest, true
			if strings.HasPrefix(latest.LastRerunOutcome, "confirmed") {
				intent = nil
			}
		}
	}
	if mutationAfterAwait {
		recovery.RerunAttempts = lastMutation.RerunAttempts
		recovery.LastRerunJobID = lastMutation.LastRerunJobID
		recovery.LastWorkflowAttempt = lastMutation.LastWorkflowAttempt
		recovery.LastRerunOutcome = lastMutation.LastRerunOutcome
	}
	recovery.MutationIntent = intent
	return nil
}

func ciRecoveryAwait(base map[string]any, branch string, recovery ciRecoveryState) lifecycleVerdict {
	evidence := cloneLifecycleExtras(base)
	if evidence == nil {
		evidence = map[string]any{}
	}
	evidence["gate"] = ciRecoveryGate
	evidence["await_reason"] = ciRecoveryGate
	evidence["reason"] = "CI_FAILURE"
	evidence["branch"] = branch
	evidence["pull_request"] = recovery.PullRequest
	evidence["head_sha"] = recovery.HeadSHA
	evidence[ciRecoveryKey] = recovery.payload()
	delete(evidence, "ci_wait")
	delete(evidence, "review_request")
	delete(evidence, "continuity_hold")
	delete(evidence, "resume_stalled")
	delete(evidence, stalledReasonKey)
	delete(evidence, stalledResumeKey)
	return lifecycleVerdict{Action: lifecycleAwait, Evidence: evidence}
}

func (o *lifecycleObserver) ciRecoveryFailure(recovery ciRecoveryState) lifecycleVerdict {
	evidence := map[string]any{
		"reason":         ciRecoveryExhaustedReason,
		"external_gate":  ciRecoveryGate,
		"next_action":    "inspect the diagnosed current-head CI failure and continue with `sandman run --continue`",
		"pull_request":   recovery.PullRequest,
		"head_sha":       recovery.HeadSHA,
		ciRecoveryKey:    recovery.payload(),
		ciRerunActionKey: recovery.Receipt.payload(),
	}
	return o.applyContinuityDisposition(
		continuityRow{gate: ciRecoveryGate, reason: ciRecoveryExhaustedReason, cause: causeFailure},
		ciRecoveryAwait(nil, recovery.Branch, recovery),
		lifecycleVerdict{Action: lifecycleFailure, Evidence: evidence},
	)
}

func (o *lifecycleObserver) ciRecoveryUnavailable(branch string) lifecycleVerdict {
	evidence := map[string]any{
		"gate": ciRecoveryGate, "await_reason": ciRecoveryGate, "branch": branch,
		"reason":            ciRecoveryStateUnavailableReason,
		"observation_error": "CI recovery history could not be safely rehydrated",
	}
	return o.applyContinuityDisposition(
		continuityRow{gate: ciRecoveryGate, reason: ciRecoveryStateUnavailableReason, cause: causeFailure},
		lifecycleVerdict{Action: lifecycleAwait, Evidence: evidence},
		lifecycleVerdict{Action: lifecycleFailure, Evidence: evidence},
	)
}

// confirmedCIRerunReceipt follows only explicit Run continuation links. A
// readable event log and a matching issue/branch on the receipt event are
// required; current agent output alone is not a durable authorization.
func confirmedCIRerunReceipt(log events.EventLog, runID string, issue int, branch string, prerequisite map[string]any) (receipt *ciRerunActionEvidence, continued bool, lastAttempt int, err error) {
	if log == nil || strings.TrimSpace(runID) == "" || issue <= 0 || strings.TrimSpace(branch) == "" {
		return nil, false, 0, fmt.Errorf("CI recovery receipt history is unavailable")
	}
	history, err := log.Read()
	if err != nil {
		return nil, false, 0, err
	}
	visited := make(map[string]bool)
	for current := runID; current != ""; {
		if visited[current] {
			return nil, continued, 0, fmt.Errorf("CI recovery receipt history has cyclic continuation lineage")
		}
		visited[current] = true
		previous := ""
		for _, event := range history {
			if event.RunID != current || !eventHasIssue(event, issue) ||
				stringField(event.Payload["branch"]) != branch {
				continue
			}
			if event.Type == "run.continued" {
				if current == runID && stringField(event.Payload["previous_run_id"]) != current {
					continued = true
				}
				candidate := stringField(event.Payload["previous_run_id"])
				if candidate != "" && candidate != current {
					if previous != "" && previous != candidate {
						return nil, continued, 0, fmt.Errorf("CI recovery receipt history has ambiguous continuation lineage")
					}
					previous = candidate
				}
			}
			action, ok := ciRerunActionFromPayload(event.Payload[ciRerunActionKey])
			if ok && action.confirmsRecoveryFor(prerequisite) {
				if receipt == nil {
					receipt = action
				}
				lastAttempt = max(lastAttempt, action.ObservedAttempt-1)
			}
			if current == runID && event.Type == "run.await" && stringEvidence(event.Payload, "gate") == ciRecoveryGate {
				state, valid := ciRecoveryFromEvidence(event.Payload)
				if !valid || state.Receipt.confirmsRecoveryFor(prerequisite) {
					// This Run has already spent its window for the target.
					return nil, continued, 0, nil
				}
			}
			if event.Type == ciRecoveryIntentEvent {
				state, valid := ciRecoveryFromEvidence(event.Payload)
				if valid && state.MutationIntent != nil && state.Receipt.confirmsRecoveryFor(prerequisite) {
					// A fresh window cannot repeat a predecessor's possibly
					// committed mutation. Only a newer live attempt can qualify.
					lastAttempt = max(lastAttempt, state.MutationIntent.WorkflowAttempt)
				}
			}
		}
		current = previous
	}
	return receipt, continued, lastAttempt, nil
}

func eventHasIssue(event events.Event, issue int) bool {
	if event.IssueRef != nil {
		return *event.IssueRef == issue
	}
	return event.Issue == issue
}

func (o *lifecycleObserver) beginCIRerunRecovery(branch, runID string, evidence map[string]any, continuationOnly bool) (lifecycleVerdict, bool) {
	if o.deps.githubClient == nil {
		return lifecycleVerdict{}, false
	}
	if _, ok := o.deps.githubClient.(github.CIRerunClient); !ok {
		return lifecycleVerdict{}, false
	}
	prerequisite, ok := evidence[ciRecoveryPrerequisiteKey].(map[string]any)
	if !ok {
		return lifecycleVerdict{}, false
	}
	issue := o.issueNumber
	if issue <= 0 {
		issue = intValue(evidence["issue"])
	}
	action, continued, lastAttempt, err := confirmedCIRerunReceipt(o.deps.eventLog, runID, issue, branch, prerequisite)
	if err != nil || action == nil || continuationOnly && !continued {
		return lifecycleVerdict{}, false
	}
	checks, checksOK := ciRerunFailedChecks(prerequisite["failed_checks"])
	names, namesOK := ciRerunWorkflowCheckNames(prerequisite["workflow_check_names"])
	if !checksOK || !namesOK {
		return lifecycleVerdict{}, false
	}
	prNumber, prOK := lifecycleDeadlineSeconds(prerequisite["pull_request"])
	workflowRunID, _ := prerequisite["workflow_run_id"].(string)
	repository, _ := prerequisite["repository"].(string)
	head, _ := prerequisite["head_sha"].(string)
	if !prOK || int(prNumber) != action.PullRequest || strings.TrimSpace(workflowRunID) != action.RunID ||
		strings.TrimSpace(repository) == "" || strings.TrimSpace(head) == "" {
		return lifecycleVerdict{}, false
	}
	now := o.runtimeNow()
	nextAttempt := now.Add(ciRecoveryInterval)
	if continued {
		// A manual continuation starts a fresh window and gets an immediate
		// recovery attempt. A same-RunID stall follows the ordinary hourly
		// recovery cadence.
		nextAttempt = now
	}
	recovery := ciRecoveryState{
		Window:     recoveryWindow{WindowStarted: now, NextAttempt: nextAttempt},
		Repository: strings.TrimSpace(repository), Issue: issue, Branch: strings.TrimSpace(branch),
		PullRequest: action.PullRequest, HeadSHA: strings.TrimSpace(head),
		WorkflowRunID: strings.TrimSpace(workflowRunID), JobName: action.JobName,
		Diagnosis: action.Diagnosis, Receipt: *action, FailedChecks: checks, WorkflowCheckNames: names,
		LastWorkflowAttempt: lastAttempt,
	}
	wait := lifecycleVerdict{Action: lifecycleAwait, Evidence: map[string]any{
		"gate": ciRecoveryGate, "await_reason": ciRecoveryGate, "reason": "CI_FAILURE",
		"branch": branch, "pull_request": recovery.PullRequest, "head_sha": recovery.HeadSHA,
		ciRecoveryKey: recovery.payload(), ciRerunActionKey: action.payload(),
		ciRecoveryPrerequisiteKey: cloneLifecycleExtras(prerequisite),
		"next_action":             "wait for the hourly diagnosed CI recovery attempt",
	}}
	delete(wait.Evidence, "ci_wait")
	return o.applyContinuityDisposition(
		continuityRow{gate: ciRecoveryGate, reason: "CI_FAILURE", cause: causeResolving},
		wait, lifecycleVerdict{},
	), true
}

func (o *lifecycleObserver) beginCIRerunRecoveryIfQualified(branch, runID string, verdict lifecycleVerdict, row continuityRow) (lifecycleVerdict, bool) {
	if !verdict.ciRepairRequired || row.gate != "ci-failure" || row.reason != "CI_FAILURE" ||
		(row.cause != causeStall && row.cause != causeExhausted) {
		return lifecycleVerdict{}, false
	}
	return o.beginCIRerunRecovery(branch, runID, verdict.Evidence, false)
}

func stringField(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		return 0
	}
}

func (o *lifecycleObserver) observeCIRecovery(ctx context.Context, request lifecycleObservationRequest, recovery *ciRecoveryState, base map[string]any) (lifecycleVerdict, bool) {
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if request.Branch != recovery.Branch || o.issueNumber != recovery.Issue {
		return o.ciRecoveryUnavailable(request.Branch), true
	}
	now := o.runtimeNow()
	if now.Before(recovery.bound()) && now.Before(recovery.Window.NextAttempt) {
		return ciRecoveryAwait(base, request.Branch, *recovery), true
	}
	if !now.Before(recovery.bound()) {
		pr, err := o.lifecyclePRForBranch(ctx, request.Branch)
		if ctx.Err() != nil {
			return lifecycleVerdict{Action: lifecycleAborted}, true
		}
		if err == nil && ciRecoveryBoundaryLeavesWindow(*recovery, pr) {
			o.ciRecoveryPR = pr
			return lifecycleVerdict{}, false
		}
		return o.ciRecoveryFailure(*recovery), true
	}

	allowAttempt := o.checkRecoveryStatus(ctx, ciRecoverySpec, &recovery.Window, now)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	statusDecision := "attempt"
	if !allowAttempt {
		statusDecision = "skip"
	}
	recovery.Window.NextAttempt = now.Add(ciRecoveryInterval)
	if recovery.bound().Before(recovery.Window.NextAttempt) {
		recovery.Window.NextAttempt = recovery.bound()
	}
	if err := o.logCIRecoveryEvent(request, *recovery, ciRecoveryStatusEvent, map[string]any{
		"status_decision": statusDecision,
	}); err != nil {
		recovery.LastRerunOutcome = "status-evidence-write-failed"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	if !allowAttempt {
		recovery.LastRerunOutcome = "skipped-github-actions-outage"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}

	pr, err := o.lifecyclePRForBranch(ctx, request.Branch)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if err != nil || pr == nil {
		if err == nil {
			err = fmt.Errorf("pull request no longer resolves by branch")
		}
		recovery.Window.LookupAttempts++
		recovery.Window.LookupError = err.Error()
		recovery.LastRerunOutcome = "lookup-failed"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	if ciRecoveryLeavesWindow(*recovery, pr) {
		o.ciRecoveryPR = pr
		return lifecycleVerdict{}, false
	}

	rollup := strings.ToLower(strings.TrimSpace(pr.StatusCheckRollup))
	if ciRecoveryPending(rollup) {
		if recovery.MutationIntent != nil {
			return o.reconcileCIRerunIntent(ctx, request, *recovery, base)
		}
		recovery.LastRerunOutcome = "target-workflow-pending"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	if rollup != "failure" {
		o.ciRecoveryPR = pr
		return lifecycleVerdict{}, false
	}

	prerequisite := ciRecoveryPrerequisiteFor(pr, recovery.HeadSHA, recovery.Repository)
	if prerequisite == nil || !recovery.Receipt.confirmsRecoveryFor(prerequisite) {
		// A different, extra, ambiguous or no-longer-identifiable failure is
		// implementation-owned work, not authority to mutate another job.
		o.ciRecoveryPR = pr
		return lifecycleVerdict{}, false
	}
	client, ok := o.deps.githubClient.(github.CIRerunClient)
	if !ok {
		o.ciRecoveryPR = pr
		return lifecycleVerdict{}, false
	}
	repository, err := o.deps.githubClient.RepoName(ctx)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if err != nil || !strings.EqualFold(strings.TrimSpace(repository), recovery.Repository) {
		if err == nil {
			err = fmt.Errorf("repository identity changed from %q to %q", recovery.Repository, strings.TrimSpace(repository))
		}
		recovery.Window.LookupAttempts++
		recovery.Window.LookupError = err.Error()
		recovery.LastRerunOutcome = "repository-revalidation-failed"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}

	jobs, err := client.ListWorkflowJobs(ctx, recovery.WorkflowRunID)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if err != nil {
		recovery.Window.LookupAttempts++
		recovery.Window.LookupError = err.Error()
		recovery.LastRerunOutcome = "job-lookup-failed"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	_, _, ready, pending, valid := ciRecoveryFailedJob(jobs, recovery.WorkflowRunID, recovery.JobName)
	if !valid {
		o.ciRecoveryPR = pr
		return lifecycleVerdict{}, false
	}
	if recovery.MutationIntent != nil {
		return o.reconcileCIRerunIntentWithJobs(request, *recovery, base, jobs)
	}
	if pending {
		recovery.LastRerunOutcome = "target-job-pending"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	if !ready {
		o.ciRecoveryPR = pr
		return lifecycleVerdict{}, false
	}

	// Re-read the branch and the latest workflow attempt immediately before
	// creating a durable intent. A head or job execution may have changed
	// while the earlier reads were in flight.
	latestPR, err := o.lifecyclePRForBranch(ctx, request.Branch)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if err != nil || latestPR == nil {
		if err == nil {
			err = fmt.Errorf("pull request no longer resolves by branch")
		}
		recovery.Window.LookupAttempts++
		recovery.Window.LookupError = err.Error()
		recovery.LastRerunOutcome = "pre-mutation-pr-revalidation-failed"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	if ciRecoveryLeavesWindow(*recovery, latestPR) {
		o.ciRecoveryPR = latestPR
		return lifecycleVerdict{}, false
	}
	if ciRecoveryPending(latestPR.StatusCheckRollup) {
		recovery.LastRerunOutcome = "target-workflow-pending"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	latestPrerequisite := ciRecoveryPrerequisiteFor(latestPR, recovery.HeadSHA, recovery.Repository)
	if !strings.EqualFold(strings.TrimSpace(latestPR.StatusCheckRollup), "failure") ||
		latestPrerequisite == nil || !recovery.Receipt.confirmsRecoveryFor(latestPrerequisite) {
		o.ciRecoveryPR = latestPR
		return lifecycleVerdict{}, false
	}
	liveRepository, err := o.deps.githubClient.RepoName(ctx)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if err != nil || !strings.EqualFold(strings.TrimSpace(liveRepository), recovery.Repository) {
		if err == nil {
			err = fmt.Errorf("repository identity changed from %q to %q", recovery.Repository, strings.TrimSpace(liveRepository))
		}
		recovery.Window.LookupAttempts++
		recovery.Window.LookupError = err.Error()
		recovery.LastRerunOutcome = "pre-mutation-repository-revalidation-failed"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	jobs, err = client.ListWorkflowJobs(ctx, recovery.WorkflowRunID)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if err != nil {
		recovery.Window.LookupAttempts++
		recovery.Window.LookupError = err.Error()
		recovery.LastRerunOutcome = "pre-mutation-job-revalidation-failed"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	selected, attempt, ready, pending, valid := ciRecoveryFailedJob(jobs, recovery.WorkflowRunID, recovery.JobName)
	if !valid {
		o.ciRecoveryPR = latestPR
		return lifecycleVerdict{}, false
	}
	if pending {
		recovery.LastRerunOutcome = "target-job-pending"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	if !ready {
		o.ciRecoveryPR = latestPR
		return lifecycleVerdict{}, false
	}
	if attempt <= recovery.LastWorkflowAttempt {
		recovery.LastRerunOutcome = "attempt-already-rerun"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	if !o.runtimeNow().Before(recovery.bound()) {
		return o.ciRecoveryFailure(*recovery), true
	}
	intent := ciRecoveryMutationIntent{
		ID:            fmt.Sprintf("%s/%d/%d", recovery.WorkflowRunID, attempt, selected.ID),
		WorkflowRunID: recovery.WorkflowRunID, JobID: selected.ID, JobName: recovery.JobName,
		WorkflowAttempt: attempt, StartedUnixSeconds: now.Unix(),
	}
	intentState := *recovery
	intentState.MutationIntent = &intent
	intentState.RerunAttempts++
	intentState.LastRerunJobID = selected.ID
	intentState.LastWorkflowAttempt = attempt
	intentState.LastRerunOutcome = "mutation-intent-recorded"
	if err := o.logCIRecoveryEvent(request, intentState, ciRecoveryIntentEvent, map[string]any{
		"mutation_intent": intent,
	}); err != nil {
		recovery.LastRerunOutcome = "mutation-intent-write-failed"
		return o.scheduleCIRecovery(request, *recovery, base), true
	}
	*recovery = intentState
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if !o.runtimeNow().Before(recovery.bound()) {
		return o.ciRecoveryFailure(*recovery), true
	}
	postErr := client.RerunWorkflowJob(ctx, selected.ID)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	readback, readErr := client.ListWorkflowJobs(ctx, recovery.WorkflowRunID)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if readErr == nil {
		confirmed, _ := ciRecoveryRerunReadback(readback, *recovery, intent)
		if confirmed {
			recovery.MutationIntent = nil
			recovery.LastWorkflowAttempt = intent.WorkflowAttempt
			recovery.LastRerunOutcome = "confirmed"
			if postErr != nil {
				recovery.LastRerunOutcome = "confirmed-after-ambiguous-post"
			}
		} else {
			recovery.LastRerunOutcome = "uncertain"
			if postErr != nil {
				recovery.LastRerunOutcome = "post-error-unconfirmed"
			}
		}
	} else {
		recovery.Window.LookupError = readErr.Error()
		recovery.LastRerunOutcome = "readback-uncertain"
		if postErr != nil {
			recovery.LastRerunOutcome = "post-and-readback-uncertain"
		}
	}
	if err := o.logCIRecoveryEvent(request, *recovery, ciRecoveryOutcomeEvent, map[string]any{
		"intent_id": intent.ID, "rerun_outcome": recovery.LastRerunOutcome,
		"workflow_attempt": recovery.LastWorkflowAttempt, "source_attempt": intent.WorkflowAttempt,
	}); err != nil {
		recovery.MutationIntent = &intent
		recovery.LastRerunOutcome = "outcome-evidence-write-failed"
	}
	return o.scheduleCIRecovery(request, *recovery, base), true
}

func ciRecoveryLeavesWindow(recovery ciRecoveryState, pr *github.PR) bool {
	if pr == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(pr.State), "open") || ciRecoveryMergeConflict(pr) {
		return true
	}
	if pr.Number != recovery.PullRequest || !strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), recovery.HeadSHA) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(pr.StatusCheckRollup)) {
	case "success":
		return true
	case "failure":
		prerequisite := ciRecoveryPrerequisiteFor(pr, recovery.HeadSHA, recovery.Repository)
		return prerequisite == nil || !recovery.Receipt.confirmsRecoveryFor(prerequisite)
	default:
		return false
	}
}

// ciRecoveryBoundaryLeavesWindow is deliberately stricter than the in-window
// transition: at the bound, a same-head failure with missing or ambiguous
// identity is inconclusive and must exhaust rather than authorize repair.
func ciRecoveryBoundaryLeavesWindow(recovery ciRecoveryState, pr *github.PR) bool {
	if pr == nil {
		return false
	}
	if pr.Merged || strings.EqualFold(strings.TrimSpace(pr.State), "merged") ||
		strings.EqualFold(strings.TrimSpace(pr.State), "closed") || ciRecoveryMergeConflict(pr) {
		return true
	}
	if !strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		return false
	}
	if head := strings.TrimSpace(pr.HeadRefOid); head == "" {
		return false
	} else if pr.Number != recovery.PullRequest || !strings.EqualFold(head, recovery.HeadSHA) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(pr.StatusCheckRollup)) {
	case "success":
		return true
	case "failure":
		prerequisite := ciRecoveryPrerequisiteFor(pr, recovery.HeadSHA, recovery.Repository)
		return pr.CIMultipleFailedWorkflows || (prerequisite != nil && !recovery.Receipt.confirmsRecoveryFor(prerequisite))
	default:
		return false
	}
}

func ciRecoveryMergeConflict(pr *github.PR) bool {
	switch strings.ToUpper(strings.TrimSpace(pr.MergeStateStatus)) {
	case "DIRTY", "CONFLICTING":
		return true
	}
	return false
}

func ciRecoveryPending(rollup string) bool {
	switch strings.ToLower(strings.TrimSpace(rollup)) {
	case "pending", "in_progress", "queued":
		return true
	}
	return false
}

func ciRecoveryFailedJob(jobs []github.CIWorkflowJob, workflowRunID, jobName string) (github.CIWorkflowJob, int, bool, bool, bool) {
	runID, err := strconv.ParseInt(strings.TrimSpace(workflowRunID), 10, 64)
	if err != nil || runID <= 0 || len(jobs) == 0 {
		return github.CIWorkflowJob{}, 0, false, false, false
	}
	attempt := 0
	var selected github.CIWorkflowJob
	selectedCount, qualityCount := 0, 0
	pending := false
	for _, job := range jobs {
		if job.ID <= 0 || job.RunID != runID || job.RunAttempt <= 0 || strings.TrimSpace(job.Name) == "" {
			return github.CIWorkflowJob{}, 0, false, false, false
		}
		if attempt == 0 {
			attempt = job.RunAttempt
		} else if attempt != job.RunAttempt {
			return github.CIWorkflowJob{}, 0, false, false, false
		}
		if strings.EqualFold(strings.TrimSpace(job.Name), strings.TrimSpace(jobName)) {
			selected, selectedCount = job, selectedCount+1
		}
		if strings.EqualFold(strings.TrimSpace(job.Name), "quality-gate") {
			qualityCount++
		}
		pending = pending || !strings.EqualFold(strings.TrimSpace(job.Status), "completed")
	}
	if selectedCount != 1 || qualityCount != 1 {
		return github.CIWorkflowJob{}, attempt, false, false, false
	}
	for _, job := range jobs {
		if !strings.EqualFold(strings.TrimSpace(job.Name), strings.TrimSpace(jobName)) &&
			!strings.EqualFold(strings.TrimSpace(job.Name), "quality-gate") && ciWorkflowJobFailed(job) {
			return selected, attempt, false, false, false
		}
	}
	if pending {
		return selected, attempt, false, true, true
	}
	if !ciWorkflowJobFailed(selected) {
		return selected, attempt, false, false, true
	}
	return selected, attempt, true, false, true
}

func ciWorkflowJobFailed(job github.CIWorkflowJob) bool {
	status := strings.ToLower(strings.TrimSpace(job.Status))
	conclusion := strings.ToLower(strings.TrimSpace(job.Conclusion))
	return status == "completed" && conclusion != "" &&
		conclusion != "success" && conclusion != "skipped" && conclusion != "neutral"
}

func ciRecoveryRerunReadback(jobs []github.CIWorkflowJob, recovery ciRecoveryState, intent ciRecoveryMutationIntent) (bool, int) {
	_, attempt, _, _, valid := ciRecoveryFailedJob(jobs, recovery.WorkflowRunID, recovery.JobName)
	if !valid || attempt <= intent.WorkflowAttempt {
		return false, attempt
	}
	return true, attempt
}

func (o *lifecycleObserver) reconcileCIRerunIntent(ctx context.Context, request lifecycleObservationRequest, recovery ciRecoveryState, base map[string]any) (lifecycleVerdict, bool) {
	client, ok := o.deps.githubClient.(github.CIRerunClient)
	if !ok || recovery.MutationIntent == nil {
		return o.scheduleCIRecovery(request, recovery, base), true
	}
	jobs, err := client.ListWorkflowJobs(ctx, recovery.WorkflowRunID)
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}, true
	}
	if err != nil {
		recovery.Window.LookupError = err.Error()
		recovery.LastRerunOutcome = "intent-reconciliation-unavailable"
		return o.scheduleCIRecovery(request, recovery, base), true
	}
	return o.reconcileCIRerunIntentWithJobs(request, recovery, base, jobs)
}

func (o *lifecycleObserver) reconcileCIRerunIntentWithJobs(request lifecycleObservationRequest, recovery ciRecoveryState, base map[string]any, jobs []github.CIWorkflowJob) (lifecycleVerdict, bool) {
	intent := recovery.MutationIntent
	if intent == nil {
		return o.scheduleCIRecovery(request, recovery, base), true
	}
	confirmed, attempt := ciRecoveryRerunReadback(jobs, recovery, *intent)
	if confirmed {
		recovery.MutationIntent = nil
		recovery.LastWorkflowAttempt = intent.WorkflowAttempt
		recovery.LastRerunOutcome = "confirmed-after-restart"
		if err := o.logCIRecoveryEvent(request, recovery, ciRecoveryOutcomeEvent, map[string]any{
			"intent_id": intent.ID, "rerun_outcome": recovery.LastRerunOutcome,
			"workflow_attempt": attempt, "source_attempt": intent.WorkflowAttempt,
		}); err != nil {
			recovery.MutationIntent = intent
			recovery.LastRerunOutcome = "outcome-evidence-write-failed"
		}
	} else {
		recovery.LastRerunOutcome = "intent-unresolved-no-replay"
	}
	return o.scheduleCIRecovery(request, recovery, base), true
}

func (o *lifecycleObserver) scheduleCIRecovery(request lifecycleObservationRequest, recovery ciRecoveryState, base map[string]any) lifecycleVerdict {
	return ciRecoveryAwait(base, request.Branch, recovery)
}

func (o *lifecycleObserver) logCIRecoveryEvent(request lifecycleObservationRequest, recovery ciRecoveryState, eventType string, extras map[string]any) error {
	if o.deps.eventLog == nil {
		return fmt.Errorf("CI recovery event log is unavailable")
	}
	payload := map[string]any{
		"branch": request.Branch, "base_branch": o.baseBranch, "batch_id": o.batchID,
		ciRecoveryKey: recovery.payload(),
	}
	for key, value := range extras {
		payload[key] = value
	}
	event := events.Event{
		Type: eventType, Timestamp: o.runtimeNow(), RunID: request.RunID,
		Issue: recovery.Issue, IssueRef: issueRef(recovery.Issue), Payload: payload,
	}
	if eventType == ciRecoveryIntentEvent || eventType == ciRecoveryStatusEvent {
		if durable, ok := o.deps.eventLog.(interface{ LogDurable(events.Event) error }); ok {
			return durable.LogDurable(event)
		}
	}
	return o.deps.eventLog.Log(event)
}
