package batch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

// lifecycleObservationRequest describes one runtime observation of an
// implementation pull request.
type lifecycleObservationRequest struct {
	WorkDir            string
	Branch             string
	RunID              string
	HostPathsReady     bool
	AttemptStatus      string
	AfterAgentAttempt  bool
	UsageLimitReached  bool
	CIRerunAction      *ciRerunActionEvidence
	OrdinaryRetry      *ordinaryRetryState
	CIRecoveryRequired bool
}

// lifecycleVerdict is the selected lifecycle action and the evidence used to
// apply it. lifecycleNone means the observation did not select an action.
type lifecycleVerdict struct {
	Action           lifecycleAction
	Evidence         map[string]any
	Kind             lifecycleVerdictKind
	ImplementorWork  bool
	ciRepairRequired bool
	ciExecutionID    string
}

type lifecycleVerdictKind uint8

const (
	lifecycleVerdictOrdinary lifecycleVerdictKind = iota
	lifecycleVerdictCompletion
	lifecycleVerdictClosedPR
	// lifecycleVerdictGitHubUnavailable is a wait for GitHub to recover from
	// a failed pull-request lookup or refresh (issue #2938).
	lifecycleVerdictGitHubUnavailable
	// lifecycleVerdictOrdinaryRetry returns a no-op recovery observation to the
	// ordinary retry loop for the non-success attempt being observed.
	lifecycleVerdictOrdinaryRetry
)

// lifecycleObservation is the single operation callers use to observe an
// implementation pull request. The implementation owns evidence gathering,
// lifecycle selection, and post-decision policy.
type lifecycleObservation interface {
	Observe(context.Context, lifecycleObservationRequest) lifecycleVerdict
}

type lifecycleObserver struct {
	*runSession
	outcomes     continuityTable
	ciRecoveryPR *github.PR
}

type managedCIFailureObservation struct {
	Repository          string
	Source              *github.ManagedCIExecution
	Replacement         *github.ManagedCIExecution
	IndependentFailure  bool
	Unknown             bool
	ReplacementFailures []github.ManagedCIFailureEvidence
	SourceFailures      []github.ManagedCIFailureEvidence
}

type managedCILifecycleObservation struct {
	PR                 *github.PR
	Failure            managedCIFailureObservation
	ReplacementFailure map[string]any
	SourceFailure      map[string]any
	Error              error
}

func (s *lifecycleObserver) observeManagedCIFailureForAttempt(
	ctx context.Context,
	pr *github.PR,
	headSHA, repository, runID, branch, worktreeHeadSHA string,
) managedCILifecycleObservation {
	result := managedCILifecycleObservation{
		PR: pr, Failure: s.observeManagedCIFailure(ctx, pr, headSHA, repository),
	}
	retainedExecutionRefreshed := false
	result.SourceFailure = managedCISourceFailureEvidence(
		result.Failure.Source, result.Failure.SourceFailures,
	)
	if result.PR != nil && strings.TrimSpace(result.PR.BaseRefOid) == "" &&
		result.Failure.Source != nil && strings.TrimSpace(result.Failure.Source.BaseSHA) != "" {
		resolvedPR := *result.PR
		resolvedPR.BaseRefOid = strings.TrimSpace(result.Failure.Source.BaseSHA)
		result.PR = &resolvedPR
	}
	authoritativeGreen := result.PR != nil &&
		strings.EqualFold(strings.TrimSpace(result.PR.StatusCheckRollup), "success") &&
		!ciActive(result.PR, headSHA)
	if !authoritativeGreen && !result.Failure.IndependentFailure && result.Failure.Replacement == nil {
		retained, established := s.priorOperationEvidence(runID, branch, worktreeHeadSHA)
		if established {
			if _, retainedReplacement := managedCIReplacementWait(retained); result.PR != nil &&
				strings.TrimSpace(result.PR.BaseRefOid) == "" && retainedReplacement {
				provider, ok := s.deps.githubClient.(github.ManagedCIPullRequestIdentityProvider)
				if !ok {
					result.Failure.Unknown = true
				} else if identity, identityErr := provider.FetchManagedCIPullRequestIdentity(
					ctx, result.PR.Number,
				); identityErr != nil ||
					identity.PullRequest != result.PR.Number ||
					identity.BaseRefName != result.PR.BaseRefName ||
					!strings.EqualFold(identity.HeadSHA, result.PR.HeadRefOid) ||
					result.Failure.Repository != "" &&
						!strings.EqualFold(identity.Repository, result.Failure.Repository) {
					result.Failure.Unknown = true
				} else {
					resolvedPR := *result.PR
					resolvedPR.BaseRefOid = identity.BaseSHA
					result.PR = &resolvedPR
				}
			}
			retainedExecution := managedCIReplacementExecutionForObservation(
				retained, result.PR, result.Failure.Repository,
			)
			if retainedExecution != nil {
				if result.Failure.Repository == "" {
					name, repoErr := s.deps.githubClient.RepoName(ctx)
					if repoErr != nil || strings.TrimSpace(name) == "" {
						result.Failure.Unknown = true
					} else {
						result.Failure.Repository = strings.TrimSpace(name)
					}
				}
				if strings.EqualFold(retainedExecution.Repository, result.Failure.Repository) {
					if checks := managedCIVisibleActiveChecksForRun(result.PR, retainedExecution); len(checks) > 0 {
						activeExecution, activeErr := s.observeManagedCIActiveExecution(
							ctx, *retainedExecution, checks,
						)
						if activeErr == nil {
							result.Failure.Repository = activeExecution.Repository
							result.Failure.Replacement = activeExecution
							retainedExecutionRefreshed = true
						} else if refresher, ok := s.deps.githubClient.(github.ManagedCIExecutionRefresher); ok {
							refreshed, refreshErr := refresher.RefreshManagedCIExecution(ctx, *retainedExecution)
							if refreshErr != nil ||
								!managedCIRefreshedExecutionMatches(*retainedExecution, refreshed) {
								result.Failure.Unknown = true
							} else {
								result.Failure.Repository = refreshed.Repository
								result.Failure.Replacement = &refreshed
								retainedExecutionRefreshed = true
								if strings.EqualFold(strings.TrimSpace(refreshed.Status), "completed") &&
									!strings.EqualFold(strings.TrimSpace(refreshed.Conclusion), "success") {
									if managedCIVisibleActiveRunDiffers(result.PR, retainedExecution) {
										result.Failure.Unknown = true
										retainedExecutionRefreshed = false
									} else {
										diagnosis := s.observeRetainedManagedCIExecutionFailure(ctx, refreshed)
										if diagnosis.Unknown {
											result.Failure.Unknown = true
											retainedExecutionRefreshed = false
										} else if diagnosis.Replacement != nil {
											result.Failure.Replacement = diagnosis.Replacement
											result.Failure.ReplacementFailures = diagnosis.ReplacementFailures
											result.Failure.IndependentFailure = diagnosis.IndependentFailure
											if diagnosis.IndependentFailure {
												result.PR, result.ReplacementFailure = managedCIReplacementFailurePR(
													result.PR, diagnosis.Replacement, diagnosis.ReplacementFailures,
												)
											}
										}
									}
								}
							}
						} else {
							result.Failure.Unknown = true
						}
					} else if refresher, ok := s.deps.githubClient.(github.ManagedCIExecutionRefresher); ok {
						refreshed, refreshErr := refresher.RefreshManagedCIExecution(ctx, *retainedExecution)
						if refreshErr != nil ||
							!managedCIRefreshedExecutionMatches(*retainedExecution, refreshed) {
							result.Failure.Unknown = true
						} else {
							result.Failure.Repository = refreshed.Repository
							result.Failure.Replacement = &refreshed
							retainedExecutionRefreshed = true
							if strings.EqualFold(strings.TrimSpace(refreshed.Status), "completed") &&
								!strings.EqualFold(strings.TrimSpace(refreshed.Conclusion), "success") {
								if managedCIVisibleActiveRunDiffers(result.PR, retainedExecution) {
									result.Failure.Unknown = true
									retainedExecutionRefreshed = false
								} else {
									diagnosis := s.observeRetainedManagedCIExecutionFailure(ctx, refreshed)
									if diagnosis.Unknown {
										result.Failure.Unknown = true
										retainedExecutionRefreshed = false
									} else if diagnosis.Replacement != nil {
										result.Failure.Replacement = diagnosis.Replacement
										result.Failure.ReplacementFailures = diagnosis.ReplacementFailures
										result.Failure.IndependentFailure = diagnosis.IndependentFailure
										if diagnosis.IndependentFailure {
											result.PR, result.ReplacementFailure = managedCIReplacementFailurePR(
												result.PR, diagnosis.Replacement, diagnosis.ReplacementFailures,
											)
										}
									}
								}
							}
						}
					} else if strings.EqualFold(strings.TrimSpace(retainedExecution.Status), "completed") &&
						!strings.EqualFold(strings.TrimSpace(retainedExecution.Conclusion), "success") {
						if managedCIVisibleActiveRunDiffers(result.PR, retainedExecution) {
							result.Failure.Unknown = true
						} else {
							diagnosis := s.observeRetainedManagedCIExecutionFailure(ctx, *retainedExecution)
							if diagnosis.Unknown {
								result.Failure.Unknown = true
							} else if diagnosis.Replacement != nil {
								result.Failure.Repository = diagnosis.Repository
								result.Failure.Replacement = diagnosis.Replacement
								result.Failure.ReplacementFailures = diagnosis.ReplacementFailures
								result.Failure.IndependentFailure = diagnosis.IndependentFailure
								if diagnosis.IndependentFailure {
									result.PR, result.ReplacementFailure = managedCIReplacementFailurePR(
										result.PR, diagnosis.Replacement, diagnosis.ReplacementFailures,
									)
								}
							}
						}
					} else {
						result.Failure.Unknown = true
					}
				} else if !result.Failure.Unknown {
					result.Failure.Unknown = true
				}
			}
		}
	}
	if result.Failure.Unknown && !result.Failure.IndependentFailure &&
		!retainedExecutionRefreshed && ctx.Err() == nil {
		retained, established := s.priorOperationEvidence(runID, branch, worktreeHeadSHA)
		if !established || !managedCIWaitIncludesExecution(retained, result.Failure.Source) {
			result.Error = errors.New("managed CI observation is inconclusive")
		}
	}
	return result
}

func managedCIRefreshedExecutionMatches(
	retained github.ManagedCIExecution,
	refreshed github.ManagedCIExecution,
) bool {
	return strings.EqualFold(strings.TrimSpace(retained.Repository), strings.TrimSpace(refreshed.Repository)) &&
		retained.PullRequest == refreshed.PullRequest &&
		retained.BaseRefName == refreshed.BaseRefName &&
		strings.EqualFold(strings.TrimSpace(retained.BaseSHA), strings.TrimSpace(refreshed.BaseSHA)) &&
		strings.EqualFold(strings.TrimSpace(retained.HeadSHA), strings.TrimSpace(refreshed.HeadSHA)) &&
		retained.WorkflowID == refreshed.WorkflowID &&
		strings.EqualFold(strings.TrimSpace(retained.Event), strings.TrimSpace(refreshed.Event)) &&
		retained.RunID == refreshed.RunID && retained.RunNumber == refreshed.RunNumber &&
		refreshed.Attempt >= retained.Attempt &&
		github.ValidManagedCIExecutionStatus(refreshed.Status, refreshed.Conclusion)
}

func (s *lifecycleObserver) observeManagedCIActiveExecution(
	ctx context.Context,
	retained github.ManagedCIExecution,
	checks []github.CIWorkflowCheck,
) (*github.ManagedCIExecution, error) {
	observer, ok := s.deps.githubClient.(github.ManagedCIActiveExecutionObserver)
	if !ok {
		return nil, errors.New("managed CI active execution observer is unavailable")
	}
	execution, err := observer.ObserveManagedCIActiveExecution(ctx, github.ManagedCIActiveExecutionRequest{
		Repository: retained.Repository, PullRequest: retained.PullRequest,
		BaseRefName: retained.BaseRefName, BaseSHA: retained.BaseSHA, HeadSHA: retained.HeadSHA,
		WorkflowID: retained.WorkflowID, RunID: retained.RunID,
		Checks: append([]github.CIWorkflowCheck(nil), checks...),
	})
	if err != nil {
		return nil, err
	}
	sameAttempt := execution.RunID == retained.RunID && execution.RunNumber == retained.RunNumber &&
		execution.Attempt == retained.Attempt && managedCIExecutionIdentityMatches(retained, execution) &&
		managedCIExecutionIsActive(retained)
	if execution.RunID != retained.RunID || execution.RunNumber != retained.RunNumber ||
		!sameAttempt && !managedCIReplacementFollows(retained, execution) ||
		!managedCIExecutionIsActive(execution) {
		return nil, errors.New("managed CI active execution does not match the retained or a newer same-run attempt")
	}
	return &execution, nil
}

func managedCIExecutionIsActive(execution github.ManagedCIExecution) bool {
	if execution.Conclusion != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(execution.Status)) {
	case "queued", "in_progress", "waiting", "requested", "pending":
		return github.ValidManagedCIExecutionStatus(execution.Status, execution.Conclusion)
	default:
		return false
	}
}

// observeManagedCIFailure returns identity-checked source/replacement evidence
// from one read-only Actions observation. Only confirmed code/test failures
// from a verified replacement are promoted as current repair evidence.
func (s *runSession) observeManagedCIFailure(
	ctx context.Context,
	pr *github.PR,
	headSHA string,
	repository string,
) managedCIFailureObservation {
	if s == nil || s.deps.githubClient == nil || ctx.Err() != nil {
		return managedCIFailureObservation{Repository: repository}
	}
	request, eligible := managedCIRequestForPR(pr, headSHA, repository)
	if !eligible {
		return managedCIFailureObservation{Repository: repository}
	}
	observer, ok := s.deps.githubClient.(github.ManagedCIObserver)
	if !ok {
		return managedCIFailureObservation{Repository: repository, Unknown: true}
	}
	if strings.TrimSpace(repository) == "" {
		name, err := s.deps.githubClient.RepoName(ctx)
		if err != nil {
			return managedCIFailureObservation{Repository: repository, Unknown: ctx.Err() == nil}
		}
		repository = strings.TrimSpace(name)
	}
	if ctx.Err() != nil {
		return managedCIFailureObservation{Repository: repository}
	}
	if repository == "" {
		return managedCIFailureObservation{Repository: repository, Unknown: true}
	}
	request.Repository = repository
	observation, err := observer.ObserveManagedCI(ctx, request)
	if err != nil && ctx.Err() != nil {
		return managedCIFailureObservation{Repository: repository}
	}
	return classifyManagedCIFailureObservation(repository, request, observation, err)
}

func (s *runSession) observeRetainedManagedCIExecutionFailure(
	ctx context.Context,
	execution github.ManagedCIExecution,
) managedCIFailureObservation {
	if s == nil || s.deps.githubClient == nil || ctx.Err() != nil {
		return managedCIFailureObservation{Repository: execution.Repository}
	}
	observer, ok := s.deps.githubClient.(github.ManagedCIExecutionFailureObserver)
	if !ok {
		return managedCIFailureObservation{Repository: execution.Repository, Unknown: true}
	}
	failures, err := observer.DiagnoseManagedCIExecutionFailure(ctx, execution)
	if err != nil {
		return managedCIFailureObservation{Repository: execution.Repository, Unknown: ctx.Err() == nil}
	}
	diagnosis := github.ManagedCIObservation{
		Replacement: &execution, ReplacementFailures: failures,
	}
	confirmed, hasConfirmedFailure, valid := managedCIConfirmedReplacementFailures(diagnosis)
	if !valid || !hasConfirmedFailure {
		return managedCIFailureObservation{Repository: execution.Repository, Unknown: true}
	}
	return managedCIFailureObservation{
		Repository: execution.Repository, Replacement: &execution,
		IndependentFailure: hasConfirmedFailure, ReplacementFailures: confirmed,
	}
}

func managedCIExecutionIdentityMatches(left, right github.ManagedCIExecution) bool {
	return strings.EqualFold(strings.TrimSpace(left.Repository), strings.TrimSpace(right.Repository)) &&
		left.PullRequest == right.PullRequest &&
		strings.TrimSpace(left.BaseRefName) == strings.TrimSpace(right.BaseRefName) &&
		strings.EqualFold(strings.TrimSpace(left.BaseSHA), strings.TrimSpace(right.BaseSHA)) &&
		strings.EqualFold(strings.TrimSpace(left.HeadSHA), strings.TrimSpace(right.HeadSHA)) &&
		left.WorkflowID == right.WorkflowID &&
		strings.EqualFold(strings.TrimSpace(left.Event), strings.TrimSpace(right.Event)) &&
		left.RunID == right.RunID && left.RunNumber == right.RunNumber && left.Attempt == right.Attempt
}

func managedCIReplacementExecutionForObservation(
	evidence map[string]any,
	pr *github.PR,
	repository string,
) *github.ManagedCIExecution {
	if pr == nil || pr.Number <= 0 || strings.TrimSpace(pr.BaseRefName) == "" ||
		strings.TrimSpace(pr.BaseRefOid) == "" || strings.TrimSpace(pr.HeadRefOid) == "" {
		return nil
	}
	wait, ok := evidence["ci_wait"].(map[string]any)
	if !ok || wait["protocol"] != ciWaitProtocol ||
		!strings.EqualFold(stringEvidence(wait, "head_sha"), strings.TrimSpace(pr.HeadRefOid)) {
		return nil
	}
	actionsEvidence, ok := wait["actions_execution"].(map[string]any)
	if !ok {
		return nil
	}
	actions, ok := ciWaitActionsExecutionFromEvidence(actionsEvidence)
	if !ok || actions.PullRequest != pr.Number ||
		(strings.TrimSpace(repository) != "" &&
			!strings.EqualFold(actions.Repository, strings.TrimSpace(repository))) ||
		actions.BaseRefName != strings.TrimSpace(pr.BaseRefName) ||
		!strings.EqualFold(actions.BaseSHA, strings.TrimSpace(pr.BaseRefOid)) ||
		!strings.EqualFold(actions.HeadSHA, strings.TrimSpace(pr.HeadRefOid)) ||
		!strings.EqualFold(actions.Event, "pull_request") ||
		actions.WorkflowID <= 0 || actions.RunID <= 0 || actions.RunNumber <= 0 || actions.Attempt <= 0 ||
		stringEvidence(wait, "execution_id") != managedCIExecutionID(actions) ||
		!github.ValidManagedCIExecutionStatus(actions.Status, actions.Conclusion) {
		return nil
	}
	return &github.ManagedCIExecution{
		Repository: actions.Repository, PullRequest: actions.PullRequest,
		BaseRefName: actions.BaseRefName, BaseSHA: actions.BaseSHA, HeadSHA: actions.HeadSHA,
		WorkflowID: actions.WorkflowID, Event: actions.Event, RunID: actions.RunID,
		RunNumber: actions.RunNumber, Attempt: actions.Attempt,
		Status: actions.Status, Conclusion: actions.Conclusion,
	}
}

func managedCIVisibleActiveRunDiffers(pr *github.PR, execution *github.ManagedCIExecution) bool {
	if pr == nil || execution == nil || !ciActive(pr, execution.HeadSHA) {
		return false
	}
	if runID, err := strconv.ParseInt(strings.TrimSpace(pr.CIActiveFailedWorkflowRunID), 10, 64); err == nil &&
		runID > 0 && runID != execution.RunID {
		return true
	}
	for _, check := range pr.CIActiveWorkflowChecks {
		if check.RunID > 0 && check.RunID != execution.RunID &&
			strings.EqualFold(strings.TrimSpace(check.Repository), strings.TrimSpace(execution.Repository)) {
			return true
		}
	}
	return false
}

func managedCIVisibleActiveChecksForRun(
	pr *github.PR,
	execution *github.ManagedCIExecution,
) []github.CIWorkflowCheck {
	if pr == nil || execution == nil || !ciActive(pr, execution.HeadSHA) {
		return nil
	}
	var checks []github.CIWorkflowCheck
	for _, check := range pr.CIActiveWorkflowChecks {
		if check.RunID == execution.RunID ||
			check.RunID <= 0 && managedCIFailedRunID(pr) == strconv.FormatInt(execution.RunID, 10) {
			checks = append(checks, check)
		}
	}
	return checks
}

func managedCIRequestForPR(pr *github.PR, headSHA, repository string) (github.ManagedCIRequest, bool) {
	if pr == nil ||
		!strings.EqualFold(strings.TrimSpace(pr.State), "open") ||
		!strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") ||
		strings.TrimSpace(headSHA) == "" ||
		!strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(headSHA)) ||
		strings.TrimSpace(pr.BaseRefName) == "" ||
		pr.CIFailedWorkflow == nil ||
		strings.TrimSpace(pr.CIFailedWorkflow.RunID) == "" || len(pr.CIFailedWorkflow.FailedChecks) == 0 {
		return github.ManagedCIRequest{}, false
	}
	return github.ManagedCIRequest{
		Repository: repository, PullRequest: pr.Number, BaseRefName: pr.BaseRefName, BaseSHA: pr.BaseRefOid,
		HeadSHA: headSHA, FailedWorkflowRunID: pr.CIFailedWorkflow.RunID,
		FailedChecks: append([]github.CIFailedCheck(nil), pr.CIFailedWorkflow.FailedChecks...),
	}, true
}

func classifyManagedCIFailureObservation(
	repository string,
	request github.ManagedCIRequest,
	observation github.ManagedCIObservation,
	err error,
) managedCIFailureObservation {
	if err != nil {
		return managedCIUncertainFailureObservation(repository, request, observation)
	}
	if !managedCIObservationMatchesRequest(observation, request) {
		return managedCIUncertainFailureObservation(repository, request, observation)
	}
	source := observation.Source
	confirmedSourceFailures := managedCIConfirmedIndependentSourceFailures(observation, request)
	independentFailure, verifiedShortage, dependentClassification :=
		managedCISourceFailureKinds(observation.Failures)
	if independentFailure && len(confirmedSourceFailures) == 0 {
		return managedCIUncertainFailureObservation(repository, request, observation)
	}
	if (verifiedShortage || dependentClassification) &&
		!managedCISourceFailureClaimsMatchJobs(observation.Failures, observation.SourceJobs) {
		return managedCIUncertainFailureObservation(repository, request, observation)
	}
	confirmedReplacementFailures, replacementHasConfirmedFailure, replacementEvidenceValid :=
		managedCIConfirmedReplacementFailures(observation)
	if !replacementEvidenceValid {
		return managedCIFailureObservation{
			Repository: repository, Source: &source, IndependentFailure: independentFailure, Unknown: true,
			SourceFailures: confirmedSourceFailures,
		}
	}
	replacementFollows := observation.Replacement != nil &&
		managedCIReplacementFollows(observation.Source, *observation.Replacement)
	sourceFailureEvidenceComplete := managedCISourceFailureEvidenceComplete(
		observation.Failures, observation.SourceJobs,
	)
	dependentFailuresMatchShortage := sourceFailureEvidenceComplete &&
		managedCIDependentFailuresMatchShortage(observation.Failures, observation.SourceJobs)
	// An unproven aggregate cause is not attributed to the shortage. It also
	// cannot veto following a separately verified same-head replacement.
	if dependentClassification && !dependentFailuresMatchShortage && !verifiedShortage {
		return managedCIUncertainFailureObservation(repository, request, observation)
	}
	if !verifiedShortage || !replacementFollows {
		return managedCIFailureObservation{
			Repository: repository, Source: &source, IndependentFailure: len(confirmedSourceFailures) > 0,
			SourceFailures: confirmedSourceFailures,
		}
	}
	currentSourceFailures := managedCIUnsupersededSourceFailures(
		confirmedSourceFailures, observation.Replacement, observation.ReplacementJobs,
	)
	replacementFailed := strings.EqualFold(strings.TrimSpace(observation.Replacement.Status), "completed") &&
		!strings.EqualFold(strings.TrimSpace(observation.Replacement.Conclusion), "success")
	sourceFailureIsCurrent := len(currentSourceFailures) > 0
	if replacementFailed && !replacementHasConfirmedFailure {
		// A terminal replacement with diagnosed non-code outcomes has a
		// verified execution state even though it does not authorize code
		// repair. Preserve that state so the wait cannot remain queued. Missing
		// diagnostics, by contrast, are an inconclusive lookup.
		if len(observation.ReplacementFailures) > 0 {
			replacement := *observation.Replacement
			return managedCIFailureObservation{
				Repository: repository, Source: &source, Replacement: &replacement,
				IndependentFailure: sourceFailureIsCurrent,
				SourceFailures:     currentSourceFailures,
			}
		}
		return managedCIFailureObservation{
			Repository: repository, Source: &source, IndependentFailure: sourceFailureIsCurrent,
			Unknown: true, SourceFailures: currentSourceFailures,
		}
	}
	replacement := *observation.Replacement
	return managedCIFailureObservation{
		Repository: repository, Source: &source, Replacement: &replacement,
		IndependentFailure:  sourceFailureIsCurrent,
		ReplacementFailures: confirmedReplacementFailures, SourceFailures: currentSourceFailures,
	}
}

func managedCIUncertainFailureObservation(
	repository string,
	request github.ManagedCIRequest,
	observation github.ManagedCIObservation,
) managedCIFailureObservation {
	var source *github.ManagedCIExecution
	if managedCISourceFailuresMatchRequest(observation, request) {
		copy := observation.Source
		source = &copy
	}
	return managedCIFailureObservation{
		Repository: repository, Source: source,
		IndependentFailure: managedCIConfirmedIndependentSourceFailure(observation, request), Unknown: true,
		SourceFailures: managedCIConfirmedIndependentSourceFailures(observation, request),
	}
}

func managedCISourceFailureKinds(
	failures []github.ManagedCIFailureEvidence,
) (independent, shortage, dependent bool) {
	for _, failure := range failures {
		switch failure.Classification {
		case github.ManagedCIRunnerShortage:
			shortage = true
		case github.ManagedCIConfirmedCodeTestFailure:
			independent = true
		case github.ManagedCIDependentFailure:
			dependent = true
		case github.ManagedCIUnexplainedFailure:
			// A complete, identity-valid read can have an unexplained
			// failure cause. That is not a lookup failure: it must not
			// block a separately verified replacement or replace ordinary
			// handling of the PR's failed rollup.
		default:
			// Unknown classifications fail closed through the validation
			// of failure claims and cannot establish a shortage.
		}
	}
	return independent, shortage, dependent
}

func managedCIConfirmedReplacementFailures(
	observation github.ManagedCIObservation,
) ([]github.ManagedCIFailureEvidence, bool, bool) {
	if len(observation.ReplacementFailures) == 0 {
		return nil, false, true
	}
	if observation.Replacement == nil ||
		!strings.EqualFold(strings.TrimSpace(observation.Replacement.Status), "completed") ||
		strings.EqualFold(strings.TrimSpace(observation.Replacement.Conclusion), "success") {
		return nil, false, false
	}
	confirmed := make([]github.ManagedCIFailureEvidence, 0, len(observation.ReplacementFailures))
	for _, failure := range observation.ReplacementFailures {
		if failure.JobID <= 0 || failure.RunID != observation.Replacement.RunID ||
			failure.Attempt != observation.Replacement.Attempt ||
			!managedCIKnownFailureClassification(failure.Classification) {
			return nil, false, false
		}
		if failure.Classification == github.ManagedCIConfirmedCodeTestFailure {
			confirmed = append(confirmed, failure)
		}
	}
	return confirmed, len(confirmed) > 0, true
}

func managedCIKnownFailureClassification(classification github.ManagedCIFailureClassification) bool {
	switch classification {
	case github.ManagedCIRunnerShortage, github.ManagedCIConfirmedCodeTestFailure,
		github.ManagedCIDependentFailure, github.ManagedCIUnexplainedFailure:
		return true
	default:
		return false
	}
}

func managedCIDependentFailuresMatchShortage(
	failures []github.ManagedCIFailureEvidence,
	jobs []github.ManagedCIJobEvidence,
) bool {
	if !managedCISourceFailureEvidenceComplete(failures, jobs) {
		return false
	}
	jobsByID := make(map[int64]github.ManagedCIJobEvidence, len(jobs))
	jobsByName := make(map[string][]github.ManagedCIJobEvidence, len(jobs))
	for _, job := range jobs {
		if job.JobID <= 0 || job.RunID <= 0 || job.Attempt <= 0 || strings.TrimSpace(job.Name) == "" {
			return false
		}
		if _, exists := jobsByID[job.JobID]; exists {
			return false
		}
		jobsByID[job.JobID] = job
		name := managedCICheckName(job.Name)
		if name == "" {
			return false
		}
		jobsByName[name] = append(jobsByName[name], job)
	}
	shortageByJob := make(map[int64]int, len(failures))
	for _, failure := range failures {
		if failure.Classification != github.ManagedCIRunnerShortage &&
			failure.Classification != github.ManagedCIDependentFailure {
			return false
		}
		if failure.Classification == github.ManagedCIRunnerShortage {
			shortageByJob[failure.JobID]++
		}
	}
	for _, failure := range failures {
		if failure.Classification != github.ManagedCIDependentFailure {
			continue
		}
		dependentJob, exists := jobsByID[failure.JobID]
		if !exists || dependentJob.RunID != failure.RunID || dependentJob.Attempt != failure.Attempt ||
			!strings.EqualFold(dependentJob.Name, failure.CheckName) ||
			!strings.EqualFold(strings.TrimSpace(dependentJob.Status), "completed") ||
			!strings.EqualFold(strings.TrimSpace(dependentJob.Conclusion), "failure") ||
			len(failure.CausedByChecks) == 0 || len(failure.CausedByChecks) != len(failure.CausedByResults) {
			return false
		}
		for index, cause := range failure.CausedByChecks {
			name := managedCICheckName(cause)
			matches := jobsByName[name]
			if name == "" || len(matches) != 1 {
				return false
			}
			causeJob := matches[0]
			if causeJob.RunID != failure.RunID || causeJob.Attempt != failure.Attempt ||
				!strings.EqualFold(strings.TrimSpace(causeJob.Status), "completed") ||
				!managedCIPrerequisiteOutcomeCompatible(failure.CausedByResults[index], causeJob.Conclusion) ||
				shortageByJob[causeJob.JobID] != 1 {
				return false
			}
		}
	}
	return true
}

func managedCISourceFailureEvidenceComplete(
	failures []github.ManagedCIFailureEvidence,
	jobs []github.ManagedCIJobEvidence,
) bool {
	if len(jobs) == 0 {
		return false
	}
	jobsByID := make(map[int64]github.ManagedCIJobEvidence, len(jobs))
	for _, job := range jobs {
		if job.JobID <= 0 || job.RunID <= 0 || job.Attempt <= 0 || strings.TrimSpace(job.Name) == "" {
			return false
		}
		if _, exists := jobsByID[job.JobID]; exists {
			return false
		}
		jobsByID[job.JobID] = job
	}

	failureCountByJob := make(map[int64]int, len(failures))
	for _, failure := range failures {
		job, exists := jobsByID[failure.JobID]
		if !exists {
			return false
		}
		failedConclusion, validConclusion := managedCIJobConclusion(job.Conclusion)
		if failure.RunID != job.RunID || failure.Attempt != job.Attempt ||
			!managedCIKnownFailureClassification(failure.Classification) ||
			!strings.EqualFold(strings.TrimSpace(failure.CheckName), strings.TrimSpace(job.Name)) ||
			!strings.EqualFold(strings.TrimSpace(job.Status), "completed") ||
			!validConclusion || !failedConclusion {
			return false
		}
		failureCountByJob[failure.JobID]++
		if failureCountByJob[failure.JobID] != 1 {
			return false
		}
	}
	for _, job := range jobs {
		if !strings.EqualFold(strings.TrimSpace(job.Status), "completed") {
			return false
		}
		conclusion := strings.ToLower(strings.TrimSpace(job.Conclusion))
		if conclusion == "" {
			return false
		}
		failed, valid := managedCIJobConclusion(conclusion)
		if !valid || failed != (failureCountByJob[job.JobID] == 1) {
			return false
		}
	}
	return true
}

func managedCISourceFailureClaimsMatchJobs(
	failures []github.ManagedCIFailureEvidence,
	jobs []github.ManagedCIJobEvidence,
) bool {
	if len(failures) == 0 || len(jobs) == 0 {
		return false
	}
	jobsByID := make(map[int64]github.ManagedCIJobEvidence, len(jobs))
	for _, job := range jobs {
		if job.JobID <= 0 || job.RunID <= 0 || job.Attempt <= 0 || strings.TrimSpace(job.Name) == "" {
			return false
		}
		if _, exists := jobsByID[job.JobID]; exists {
			return false
		}
		jobsByID[job.JobID] = job
	}
	for _, failure := range failures {
		job, exists := jobsByID[failure.JobID]
		failedConclusion, validConclusion := managedCIJobConclusion(job.Conclusion)
		if !exists || failure.RunID != job.RunID || failure.Attempt != job.Attempt ||
			!managedCIKnownFailureClassification(failure.Classification) ||
			!strings.EqualFold(strings.TrimSpace(failure.CheckName), strings.TrimSpace(job.Name)) ||
			!strings.EqualFold(strings.TrimSpace(job.Status), "completed") ||
			!validConclusion || !failedConclusion {
			return false
		}
	}
	return true
}

func managedCIJobConclusion(conclusion string) (failed, valid bool) {
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "action_required", "cancelled", "failure", "stale", "startup_failure", "timed_out":
		return true, true
	case "neutral", "success", "skipped":
		return false, true
	default:
		return false, false
	}
}

func managedCIPrerequisiteOutcomeCompatible(annotationResult, jobConclusion string) bool {
	annotationResult = strings.ToLower(strings.TrimSpace(annotationResult))
	jobConclusion = strings.ToLower(strings.TrimSpace(jobConclusion))
	if annotationResult == "abandoned" {
		return jobConclusion == "cancelled" || jobConclusion == "skipped" || jobConclusion == "abandoned"
	}
	return annotationResult != "" && annotationResult == jobConclusion && annotationResult != "success"
}

func managedCICheckName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if separator := strings.LastIndex(name, "/"); separator >= 0 {
		name = strings.TrimSpace(name[separator+1:])
	}
	var normalized strings.Builder
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			normalized.WriteRune(char)
		}
	}
	return strings.TrimSuffix(normalized.String(), "latest")
}

func managedCIObservationMatchesRequest(observation github.ManagedCIObservation, request github.ManagedCIRequest) bool {
	source := observation.Source
	if !managedCISourceFailuresMatchRequest(observation, request) {
		return false
	}
	sourceJobIDs := make(map[int64]struct{}, len(observation.SourceJobs))
	for _, job := range observation.SourceJobs {
		if job.JobID <= 0 || job.RunID != source.RunID || job.Attempt != source.Attempt ||
			strings.TrimSpace(job.Name) == "" {
			return false
		}
		if _, duplicate := sourceJobIDs[job.JobID]; duplicate {
			return false
		}
		sourceJobIDs[job.JobID] = struct{}{}
	}
	if len(observation.Failures) > 0 &&
		!managedCISourceFailureClaimsMatchJobs(observation.Failures, observation.SourceJobs) {
		return false
	}
	if observation.Replacement != nil &&
		!managedCIReplacementFollows(source, *observation.Replacement) {
		return false
	}
	if len(observation.ReplacementFailures) > 0 {
		if observation.Replacement == nil ||
			!strings.EqualFold(strings.TrimSpace(observation.Replacement.Status), "completed") ||
			strings.EqualFold(strings.TrimSpace(observation.Replacement.Conclusion), "success") {
			return false
		}
		for _, failure := range observation.ReplacementFailures {
			if failure.JobID <= 0 || failure.RunID != observation.Replacement.RunID ||
				failure.Attempt != observation.Replacement.Attempt ||
				!managedCIKnownFailureClassification(failure.Classification) {
				return false
			}
		}
	}
	return true
}

func managedCIConfirmedIndependentSourceFailure(observation github.ManagedCIObservation, request github.ManagedCIRequest) bool {
	return len(managedCIConfirmedIndependentSourceFailures(observation, request)) > 0
}

func managedCIConfirmedIndependentSourceFailures(
	observation github.ManagedCIObservation,
	request github.ManagedCIRequest,
) []github.ManagedCIFailureEvidence {
	if !managedCISourceFailuresMatchRequest(observation, request) ||
		!managedCIConfirmedIndependentSourceFailureClaimsMatchJobs(observation.Failures, observation.SourceJobs) {
		return nil
	}
	var confirmed []github.ManagedCIFailureEvidence
	for _, failure := range observation.Failures {
		if failure.Classification == github.ManagedCIConfirmedCodeTestFailure {
			confirmed = append(confirmed, failure)
		}
	}
	return confirmed
}

func managedCIUnsupersededSourceFailures(
	sourceFailures []github.ManagedCIFailureEvidence,
	replacement *github.ManagedCIExecution,
	replacementJobs []github.ManagedCIJobEvidence,
) []github.ManagedCIFailureEvidence {
	if len(sourceFailures) == 0 || replacement == nil ||
		replacement.RunID <= 0 || replacement.Attempt <= 0 ||
		!strings.EqualFold(strings.TrimSpace(replacement.Status), "completed") &&
			!managedCIExecutionIsActive(*replacement) {
		return sourceFailures
	}
	// A run-level replacement is not enough: only an exact logical check that
	// has started in the replacement attempt proves this prior failure has been
	// superseded. Queued checks have not run yet, and missing or ambiguous job
	// evidence keeps the old failure actionable.
	jobsByName := make(map[string][]github.ManagedCIJobEvidence, len(replacementJobs))
	jobsByID := make(map[int64]struct{}, len(replacementJobs))
	sourceFailuresByName := make(map[string]int, len(sourceFailures))
	for _, failure := range sourceFailures {
		sourceFailuresByName[strings.ToLower(strings.TrimSpace(failure.CheckName))]++
	}
	for _, job := range replacementJobs {
		if job.JobID <= 0 || job.RunID != replacement.RunID ||
			job.Attempt != replacement.Attempt || strings.TrimSpace(job.Name) == "" {
			return sourceFailures
		}
		if _, duplicate := jobsByID[job.JobID]; duplicate {
			return sourceFailures
		}
		jobsByID[job.JobID] = struct{}{}
		status := strings.ToLower(strings.TrimSpace(job.Status))
		switch status {
		case "queued":
			if strings.TrimSpace(job.Conclusion) != "" {
				return sourceFailures
			}
			continue
		case "in_progress":
			if strings.TrimSpace(job.Conclusion) != "" {
				return sourceFailures
			}
		case "completed":
			if _, valid := managedCIJobConclusion(job.Conclusion); !valid {
				return sourceFailures
			}
		default:
			return sourceFailures
		}
		name := strings.ToLower(strings.TrimSpace(job.Name))
		jobsByName[name] = append(jobsByName[name], job)
	}
	remaining := make([]github.ManagedCIFailureEvidence, 0, len(sourceFailures))
	for _, failure := range sourceFailures {
		name := strings.ToLower(strings.TrimSpace(failure.CheckName))
		matches := jobsByName[name]
		if sourceFailuresByName[name] != 1 || len(matches) != 1 {
			remaining = append(remaining, failure)
		}
	}
	return remaining
}

func managedCIConfirmedIndependentSourceFailureClaimsMatchJobs(
	failures []github.ManagedCIFailureEvidence,
	jobs []github.ManagedCIJobEvidence,
) bool {
	if len(failures) == 0 || len(jobs) == 0 {
		return false
	}
	jobsByID := make(map[int64]github.ManagedCIJobEvidence, len(jobs))
	for _, job := range jobs {
		if job.JobID <= 0 || job.RunID <= 0 || job.Attempt <= 0 || strings.TrimSpace(job.Name) == "" {
			return false
		}
		if _, exists := jobsByID[job.JobID]; exists {
			return false
		}
		jobsByID[job.JobID] = job
	}
	claimedJobs := make(map[int64]struct{})
	for _, failure := range failures {
		if failure.Classification != github.ManagedCIConfirmedCodeTestFailure {
			continue
		}
		job, exists := jobsByID[failure.JobID]
		failedConclusion, validConclusion := managedCIJobConclusion(job.Conclusion)
		if !exists || failure.RunID != job.RunID || failure.Attempt != job.Attempt ||
			!strings.EqualFold(strings.TrimSpace(failure.CheckName), strings.TrimSpace(job.Name)) ||
			!strings.EqualFold(strings.TrimSpace(job.Status), "completed") ||
			!validConclusion || !failedConclusion {
			return false
		}
		if _, duplicate := claimedJobs[failure.JobID]; duplicate {
			return false
		}
		claimedJobs[failure.JobID] = struct{}{}
	}
	return len(claimedJobs) > 0
}

func managedCISourceFailuresMatchRequest(observation github.ManagedCIObservation, request github.ManagedCIRequest) bool {
	source := observation.Source
	runID, err := strconv.ParseInt(strings.TrimSpace(request.FailedWorkflowRunID), 10, 64)
	if err != nil || source.RunID != runID || source.RunNumber <= 0 || source.Attempt <= 0 ||
		source.WorkflowID <= 0 || !strings.EqualFold(strings.TrimSpace(source.Event), "pull_request") ||
		!strings.EqualFold(strings.TrimSpace(source.Repository), strings.TrimSpace(request.Repository)) ||
		source.PullRequest != request.PullRequest ||
		strings.TrimSpace(source.BaseRefName) != strings.TrimSpace(request.BaseRefName) ||
		strings.TrimSpace(request.BaseSHA) != "" &&
			!strings.EqualFold(strings.TrimSpace(source.BaseSHA), strings.TrimSpace(request.BaseSHA)) ||
		!strings.EqualFold(strings.TrimSpace(source.HeadSHA), strings.TrimSpace(request.HeadSHA)) ||
		!github.ValidManagedCIExecutionStatus(source.Status, source.Conclusion) ||
		len(observation.Failures) != len(request.FailedChecks) {
		return false
	}
	pending := make(map[string]int, len(request.FailedChecks))
	for _, check := range request.FailedChecks {
		key := strings.TrimSpace(check.Name) + "\x00" + strings.TrimSpace(check.DetailsURL)
		pending[key]++
	}
	for _, failure := range observation.Failures {
		if failure.JobID <= 0 || failure.RunID != source.RunID || failure.Attempt != source.Attempt {
			return false
		}
		key := strings.TrimSpace(failure.CheckName) + "\x00" + strings.TrimSpace(failure.DetailsURL)
		if pending[key] == 0 {
			return false
		}
		pending[key]--
	}
	for _, count := range pending {
		if count != 0 {
			return false
		}
	}
	return true
}

func managedCIReplacementFailurePR(
	pr *github.PR,
	execution *github.ManagedCIExecution,
	failures []github.ManagedCIFailureEvidence,
) (*github.PR, map[string]any) {
	if pr == nil || execution == nil || len(failures) == 0 {
		return pr, nil
	}
	runID := strconv.FormatInt(execution.RunID, 10)
	executionID := fmt.Sprintf("actions:%d:%d:%d", execution.WorkflowID, execution.RunID, execution.Attempt)
	copy := *pr
	copy.StatusCheckRollup = "failure"
	copy.CIExecutionID = executionID
	copy.CIFailedWorkflow = &github.CIFailedWorkflow{
		RunID: runID, FailedChecks: make([]github.CIFailedCheck, 0, len(failures)),
		CheckNames: make([]string, 0, len(failures)),
	}
	failedJobs := make([]any, 0, len(failures))
	for _, failure := range failures {
		check := github.CIFailedCheck{Name: strings.TrimSpace(failure.CheckName), DetailsURL: strings.TrimSpace(failure.DetailsURL)}
		copy.CIFailedWorkflow.FailedChecks = append(copy.CIFailedWorkflow.FailedChecks, check)
		copy.CIFailedWorkflow.CheckNames = append(copy.CIFailedWorkflow.CheckNames, check.Name)
		failedJobs = append(failedJobs, map[string]any{
			"job_id": failure.JobID, "run_id": failure.RunID, "attempt": failure.Attempt,
			"check_name": check.Name, "details_url": check.DetailsURL,
			"classification": string(failure.Classification),
		})
	}
	return &copy, map[string]any{
		"repository": execution.Repository, "pull_request": execution.PullRequest,
		"base_ref_name": execution.BaseRefName, "base_sha": execution.BaseSHA, "head_sha": execution.HeadSHA,
		"workflow_id": execution.WorkflowID, "run_id": execution.RunID,
		"run_number": execution.RunNumber, "attempt": execution.Attempt,
		"status": execution.Status, "conclusion": execution.Conclusion,
		"execution_id": executionID, "failed_jobs": failedJobs,
	}
}

func managedCISourceFailureEvidence(
	execution *github.ManagedCIExecution,
	failures []github.ManagedCIFailureEvidence,
) map[string]any {
	if execution == nil || len(failures) == 0 {
		return nil
	}
	failedJobs := make([]any, 0, len(failures))
	for _, failure := range failures {
		failedJobs = append(failedJobs, map[string]any{
			"job_id": failure.JobID, "run_id": failure.RunID, "attempt": failure.Attempt,
			"check_name":     strings.TrimSpace(failure.CheckName),
			"details_url":    strings.TrimSpace(failure.DetailsURL),
			"classification": string(failure.Classification),
		})
	}
	return map[string]any{
		"repository": execution.Repository, "pull_request": execution.PullRequest,
		"base_ref_name": execution.BaseRefName, "base_sha": execution.BaseSHA, "head_sha": execution.HeadSHA,
		"workflow_id": execution.WorkflowID, "run_id": execution.RunID,
		"run_number": execution.RunNumber, "attempt": execution.Attempt,
		"status": execution.Status, "conclusion": execution.Conclusion,
		"failed_jobs": failedJobs,
	}
}

func managedCIReplacementFollows(source, replacement github.ManagedCIExecution) bool {
	if source.Repository == "" || replacement.Repository == "" ||
		!strings.EqualFold(strings.TrimSpace(source.Repository), strings.TrimSpace(replacement.Repository)) ||
		source.PullRequest <= 0 || source.PullRequest != replacement.PullRequest ||
		strings.TrimSpace(source.BaseRefName) != strings.TrimSpace(replacement.BaseRefName) ||
		!strings.EqualFold(strings.TrimSpace(source.BaseSHA), strings.TrimSpace(replacement.BaseSHA)) ||
		!strings.EqualFold(strings.TrimSpace(source.HeadSHA), strings.TrimSpace(replacement.HeadSHA)) ||
		source.WorkflowID <= 0 || source.WorkflowID != replacement.WorkflowID ||
		!strings.EqualFold(strings.TrimSpace(source.Event), "pull_request") ||
		!strings.EqualFold(strings.TrimSpace(replacement.Event), "pull_request") ||
		source.RunID <= 0 || source.RunNumber <= 0 || source.Attempt <= 0 ||
		replacement.RunID <= 0 || replacement.RunNumber <= 0 || replacement.Attempt <= 0 ||
		!github.ValidManagedCIExecutionStatus(source.Status, source.Conclusion) ||
		!github.ValidManagedCIExecutionStatus(replacement.Status, replacement.Conclusion) {
		return false
	}
	if replacement.RunID == source.RunID {
		return replacement.RunNumber == source.RunNumber && replacement.Attempt > source.Attempt
	}
	return replacement.RunNumber > source.RunNumber
}

func newLifecycleObserver(session *runSession) *lifecycleObserver {
	return &lifecycleObserver{runSession: session}
}

func (o *lifecycleObserver) Observe(ctx context.Context, request lifecycleObservationRequest) lifecycleVerdict {
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}
	}
	ciRecovery, ciBase, ciRecovering, ciUnknown := o.ciRecoveryWindow(request.RunID)
	if ciUnknown && (request.CIRecoveryRequired || ciBase != nil) {
		return o.ciRecoveryUnavailable(request.Branch)
	}
	if ciRecovering {
		if verdict, handled := o.observeCIRecovery(ctx, request, &ciRecovery, ciBase); handled {
			return verdict
		}
	}
	if request.OrdinaryRetry == nil {
		// Capacity admission observes the still-unconsumed retry handoff before
		// a new executor starts. Restore it here so an admission lookup outage
		// can carry the same failed attempt and bound into its replacement wait.
		if retry, selected := selectedOrdinaryRetry(o.deps.eventLog, request.RunID); selected {
			request.OrdinaryRetry = &retry
		}
	}
	recovery, recoveryBase, recovering, recoveryUnknown := o.githubRecoveryWindow(request.RunID)
	if recovering {
		if retry, ok := ordinaryRetryFromEvidence(recoveryBase); ok {
			request.OrdinaryRetry = &retry
			request.AttemptStatus = retry.PreviousStatus
		}
		if verdict, handled := o.observeGitHubRecovery(ctx, request, &recovery, recoveryBase); handled {
			return verdict
		}
	}
	verdict := o.decideLifecycleForAttempt(
		ctx,
		request.WorkDir,
		request.Branch,
		request.RunID,
		request.HostPathsReady,
		request.AttemptStatus,
		request.UsageLimitReached,
		request.AfterAgentAttempt,
		request.CIRerunAction,
	)
	if recovering && request.OrdinaryRetry != nil && !request.UsageLimitReached && ordinaryRetryNoOp(verdict) {
		return ordinaryRetryAfterGitHubRecovery(*request.OrdinaryRetry, request.Branch)
	}
	if verdict.Kind == lifecycleVerdictGitHubUnavailable && ctx.Err() == nil {
		// The recovery wait keeps the Run's stalled-resume ledger, so recovery
		// on unchanged evidence cannot relaunch the implementor.
		verdict = o.recordGitHubLookupFailure(request, verdict, recovery, recovering, recoveryUnknown)
		return o.applyContinuity(request.Branch, request.RunID, verdict)
	}
	if !verdict.handled() || ctx.Err() != nil {
		return verdict
	}
	if !ciRecovering && !ciUnknown && !request.AfterAgentAttempt && verdict.ciRepairRequired &&
		verdict.Action == lifecycleResume {
		if wait, ok := o.beginCIRerunRecovery(request.Branch, request.RunID, verdict.Evidence, true); ok {
			return wait
		}
	}
	verdict.ImplementorWork = isImplementorOwnedGateFailure(verdict.Evidence)
	return o.applyContinuity(request.Branch, request.RunID, verdict)
}

func ordinaryRetryNoOp(verdict lifecycleVerdict) bool {
	if verdict.Kind == lifecycleVerdictGitHubUnavailable || verdict.Action == lifecycleSuccess || verdict.Action == lifecycleAborted {
		return false
	}
	if _, ok := verdict.Evidence["ci_wait"]; ok {
		return false
	}
	if request, ok := verdict.Evidence["review_request"].(map[string]any); ok {
		resolved, _ := request["operation_resolved"].(bool)
		if !resolved {
			return false
		}
	}
	reason := stringEvidence(verdict.Evidence, "reason")
	if reason == "" {
		reason = stringEvidence(verdict.Evidence, stalledReasonKey)
	}
	if verdict.Action == lifecycleNone {
		return true
	}
	return reason == idleGateReason || reason == "REVIEW_REQUEST_REQUIRED" || reason == blindReviewRequiredReason
}

func ordinaryRetryAfterGitHubRecovery(state ordinaryRetryState, branch string) lifecycleVerdict {
	evidence := map[string]any{
		"branch":          branch,
		"previous_status": state.PreviousStatus,
		ordinaryRetryKey:  state.payload(),
	}
	if !state.hasRemainingRetry() {
		evidence["reason"] = "AGENT_FAILED"
		evidence["next_action"] = "GitHub recovered, but the configured ordinary retry budget is exhausted"
		return lifecycleVerdict{Action: lifecycleFailure, Evidence: evidence, Kind: lifecycleVerdictOrdinaryRetry}
	}
	evidence["gate"] = "agent-failed-retry"
	evidence["reason"] = "agent-failed"
	evidence["next_action"] = "resume the configured ordinary retry after GitHub recovery"
	return lifecycleVerdict{Action: lifecycleResume, Evidence: evidence, Kind: lifecycleVerdictOrdinaryRetry}
}

// waitForTransition keeps a recoverable verdict in the foreground. Deadline
// expiry is a typed resume decision and goes through the same continuity
// override as every other observed action.
func (o *lifecycleObserver) waitForTransition(ctx context.Context, request lifecycleObservationRequest, result AgentRunResult, initial lifecycleVerdict) lifecycleVerdict {
	verdict := initial
	if stringEvidence(initial.Evidence, "gate") == ciRecoveryGate {
		request.CIRecoveryRequired = true
	}
	plan := o.lifecyclePollIntervals(verdict.Evidence)
	freshlyObserved := false
	for index := 0; ; index++ {
		_, githubWait := githubRecoveryNextAttempt(verdict.Evidence)
		_, ciRecoveryWait := ciRecoveryNextAttempt(verdict.Evidence)
		if deadline, gate, ok := lifecycleDeadline(verdict.Evidence); ok && !githubWait && !ciRecoveryWait && !o.runtimeNow().Before(deadline) {
			if (gate == gateReviewTimeout || gate == gateCIWaitTimeout) && o.deps.githubClient != nil {
				// Operation expiry cannot override another live resolver.
				// Reobserve at the foreground boundary, then continue under
				// that resolver's original deadline when it remains active.
				recovering := false
				if !freshlyObserved {
					verdict = o.Observe(ctx, request)
					freshlyObserved = true
					if !verdict.handled() {
						if ctx.Err() != nil {
							return lifecycleVerdict{Action: lifecycleAborted}
						}
						if attemptNeedsRetry(request.AttemptStatus) && !request.UsageLimitReached {
							return lifecycleVerdict{Action: lifecycleFailure, Kind: lifecycleVerdictOrdinaryRetry}
						}
						verdict = failureForUnhandledObservation(verdict)
					}
					if verdict.Action == lifecycleAwait {
						_, recovering = githubRecoveryNextAttempt(verdict.Evidence)
					}
					if recovering {
						// A GitHub recovery wait selected here has not been
						// persisted: record its window before the hourly
						// sleep, so a restart cannot reopen the 24-hour
						// bound. Persist it as observed, with the retained
						// operation and its original deadline. Every other
						// verdict is persisted by the poll below, once.
						o.emitAwait(ctx, request.RunID, result, verdict.Evidence)
					}
				}
				if verdict.Action != lifecycleAwait {
					return verdict
				}
				if !recovering {
					o.dropExpiredOperationBounds(verdict.Evidence, gate)
				}
			} else {
				evidence := cloneLifecycleExtras(verdict.Evidence)
				evidence["gate"] = gate
				evidence["reason"] = lifecycleDeadlineReason(gate)
				evidence["next_action"] = lifecycleDeadlineNextAction(gate)
				deadlineVerdict := lifecycleVerdict{Action: lifecycleResume, Evidence: evidence}
				deadlineVerdict.ImplementorWork = isImplementorOwnedGateFailure(evidence)
				if gate == gateReviewTimeout {
					deadlineVerdict = o.applyExpiredReviewOutcome(request.Branch, request.RunID, deadlineVerdict)
				} else {
					deadlineVerdict = o.applyContinuity(request.Branch, request.RunID, deadlineVerdict)
				}
				if deadlineVerdict.Action != lifecycleAwait {
					return deadlineVerdict
				}
				// A stalled or exhausted remediation keeps observing the gate
				// without the expired deadline.
				verdict = deadlineVerdict
				delete(verdict.Evidence, "review_request")
				delete(verdict.Evidence, "ci_wait")
			}
		}
		interval := plan[len(plan)-1]
		if index < len(plan) {
			interval = plan[index]
		}
		if next, ok := githubRecoveryNextAttempt(verdict.Evidence); ok {
			// Sleep until the next hourly recovery attempt.
			interval = max(next.Sub(o.runtimeNow()), 0)
		} else if next, ok := ciRecoveryNextAttempt(verdict.Evidence); ok {
			interval = max(next.Sub(o.runtimeNow()), 0)
		}
		if err := o.waitForLifecyclePoll(ctx, interval); err != nil {
			if errors.Is(err, errLifecycleObservationTestStop) {
				verdict.Action = lifecycleAwait
				return verdict
			}
			return lifecycleVerdict{Action: lifecycleAborted}
		}
		previousEvidence := verdict.Evidence
		verdict = o.Observe(ctx, request)
		freshlyObserved = true
		if !verdict.handled() {
			if ctx.Err() != nil {
				return lifecycleVerdict{Action: lifecycleAborted}
			}
			if attemptNeedsRetry(request.AttemptStatus) && !request.UsageLimitReached {
				return lifecycleVerdict{Action: lifecycleFailure, Kind: lifecycleVerdictOrdinaryRetry}
			}
			verdict = failureForUnhandledObservation(verdict)
		}
		if verdict.Action == lifecycleAwait {
			request.CIRecoveryRequired = stringEvidence(verdict.Evidence, "gate") == ciRecoveryGate
			if stringEvidence(verdict.Evidence, "reason") != ciRecoveryStateUnavailableReason &&
				(!ciRecoveryWait || awaitOperationChanged(previousEvidence, verdict.Evidence)) {
				o.emitAwait(ctx, request.RunID, result, verdict.Evidence)
			}
			continue
		}
		return verdict
	}
}

// dropExpiredOperationBounds removes the expired operation's bounds from a
// continuity hold's evidence so the hold can keep observing without a busy
// loop. An expired review leaves current-head CI that is still within its own
// deadline untouched. A GitHub recovery wait never passes through here: it
// retains its operation and original deadline until GitHub recovers.
func (o *lifecycleObserver) dropExpiredOperationBounds(evidence map[string]any, gate string) {
	delete(evidence, "review_request")
	if gate == gateReviewTimeout && hasUnexpiredCIWait(evidence, o.runtimeNow()) {
		return
	}
	delete(evidence, "ci_wait")
}

func hasUnexpiredCIWait(evidence map[string]any, now time.Time) bool {
	deadline, ok := lifecycleCIWaitDeadline(evidence)
	return ok && now.Before(deadline)
}

func isImplementorOwnedGateFailure(evidence map[string]any) bool {
	reason, _ := evidence["reason"].(string)
	// Blind rounds use lifecycle relaunch allowances, not the ordinary
	// publication/request-delivery retry path represented by this predicate.
	return reason == idleGateReason || reason == missingPRReason || reason == "REVIEW_REQUEST_REQUIRED"
}

func (o *lifecycleObserver) applyContinuity(branch, runID string, verdict lifecycleVerdict) lifecycleVerdict {
	if verdict.Action != lifecycleResume && verdict.Action != lifecycleAwait {
		return verdict
	}
	outcomes := o.continuityTable()
	stalled, readErr := latestStalledResumes(o.deps.eventLog, runID)
	if verdict.Action == lifecycleAwait {
		if len(stalled) == 0 {
			return verdict
		}
		held := cloneLifecycleExtras(verdict.Evidence)
		held[stalledResumesKey] = appendStalledResume(stalled[:len(stalled)-1], stalled[len(stalled)-1])
		verdict.Evidence = held
		return verdict
	}

	signatureEvidence := verdict.Evidence
	if verdict.ImplementorWork {
		signatureEvidence = cloneLifecycleExtras(verdict.Evidence)
		signatureEvidence["gate"] = gateOwnedWorkRequired
	} else if verdict.ciRepairRequired && stringEvidence(verdict.Evidence, "reason") == "CI_FAILURE" {
		signatureEvidence = cloneLifecycleExtras(verdict.Evidence)
		signatureEvidence["gate"] = "ci-failure"
		if verdict.ciExecutionID != "" {
			signatureEvidence["ci_execution_id"] = verdict.ciExecutionID
		} else {
			delete(signatureEvidence, "ci_execution_id")
		}
	}
	signature := resumeSignature(signatureEvidence, o.decisionPRHead)
	allowanceRound := repairRound(verdict.Evidence, o.decisionPRHead)
	o.pendingResumeSignature = signature
	o.pendingResumeRound = allowanceRound
	if readErr != nil {
		prior := stalledResumes(verdict.Evidence)
		row := continuityRow{cause: causeUnreadable}
		if verdict.ImplementorWork {
			row.gate = gateOwnedWorkRequired
		}
		if outcomes.outcome(row) == outcomeHold && verdict.ImplementorWork {
			verdict.Action = lifecycleAwait
			verdict.Evidence = o.ownedWorkHold(verdict.Evidence, signature, prior)
		} else if outcomes.outcome(row) == outcomeHold {
			verdict.Action = lifecycleAwait
			verdict.Evidence = stalledAwaitExtras(verdict.Evidence, signature, prior)
		}
		return verdict
	}
	if verdict.ImplementorWork {
		gate := gateOwnedWorkRequired
		if containsSignature(stalled, signature) {
			row := continuityRow{
				gate: gate, reason: continuityOutcomeReason(verdict.Evidence, causeStall), cause: causeStall,
			}
			if outcomes.outcome(row) == outcomeHold {
				verdict.Action = lifecycleAwait
				verdict.Evidence = o.ownedWorkHold(verdict.Evidence, signature, stalled)
			} else if outcomes.outcome(row) == outcomeTerminal {
				verdict.Action = lifecycleFailure
				verdict.Evidence = stalledTerminalEvidence(row, verdict.Evidence, signature)
				verdict.ImplementorWork = false
			}
			return verdict
		}
		if o.finalOwnedWorkAttempt {
			row := finalOwnedWorkContinuityRow(verdict.Evidence, o.renderCfg.ReviewCommand)
			outcome := outcomes.outcome(row)
			if outcome == outcomeHold {
				verdict.Action = lifecycleAwait
				verdict.Evidence = o.ownedWorkHold(verdict.Evidence, signature, stalled)
			} else if outcome == outcomeTerminal {
				verdict.Action = lifecycleFailure
				terminalEvidence := cloneLifecycleExtras(verdict.Evidence)
				delete(terminalEvidence, "gate")
				delete(terminalEvidence, "await")
				delete(terminalEvidence, "continuity_hold")
				verdict.Evidence = terminalEvidence
				verdict.ImplementorWork = false
			}
		}
		return verdict
	}
	if containsSignature(stalled, signature) {
		row := continuityRow{
			gate:   stringEvidence(verdict.Evidence, "gate"),
			reason: continuityOutcomeReason(verdict.Evidence, causeStall),
			cause:  causeStall,
		}
		if recovery, started := o.beginCIRerunRecoveryIfQualified(branch, runID, verdict, row); started {
			return recovery
		}
		switch outcomes.outcome(row) {
		case outcomeHold:
			verdict.Action = lifecycleAwait
			verdict.Evidence = stalledAwaitExtras(verdict.Evidence, signature, stalled)
		case outcomeTerminal:
			verdict.Action = lifecycleFailure
			verdict.Evidence = stalledTerminalEvidence(row, verdict.Evidence, signature)
		}
		return verdict
	}
	gate := stringEvidence(verdict.Evidence, "gate")
	used := o.resumeCount
	if gate == gateActionableFeedback {
		used = o.feedbackAllowance.usedFor(allowanceRound)
	} else if o.lastResumeRound != "" && allowanceRound != o.lastResumeRound {
		o.resumeCount = 0
		used = 0
	}
	repeated := signature == o.lastResumeSignature && stallsOnRepeat(verdict.Evidence)
	if !repeated && used < o.resumeCapFor() {
		return verdict
	}
	cause := causeExhausted
	reason := stringEvidence(verdict.Evidence, "reason")
	permanentEvidence := formalChangesRequested(verdict.Evidence) || reason == "CI_WAIT_STATE_ERROR"
	if repeated && used < o.resumeCapFor() {
		if permanentEvidence {
			// The blocker itself is terminal; the relaunch allowance has not
			// been used up.
			cause = causeFailure
		} else {
			cause = causeStall
		}
	}
	row := continuityRow{
		gate:   gate,
		reason: continuityOutcomeReason(verdict.Evidence, cause),
		cause:  cause,
	}
	if recovery, started := o.beginCIRerunRecoveryIfQualified(branch, runID, verdict, row); started {
		return recovery
	}
	switch outcomes.outcome(row) {
	case outcomeTerminal:
		verdict.Action = lifecycleFailure
		if row.cause == causeFailure {
			nextAction := stringEvidence(verdict.Evidence, "next_action")
			if nextAction == "" {
				nextAction = "inspect the current pull-request evidence and continue in a fresh session"
			}
			verdict.Evidence = classifiedLifecycleFailureEvidence(gate, verdict.Evidence, row.reason, nextAction)
		} else {
			verdict.Evidence = stalledTerminalEvidence(row, verdict.Evidence, signature)
		}
		verdict.ImplementorWork = false
	case outcomeHold:
		verdict.Action = lifecycleAwait
		verdict.Evidence = stalledAwaitExtras(verdict.Evidence, signature, stalled)
	}
	return verdict
}

func finalOwnedWorkContinuityRow(evidence map[string]any, reviewCommand string) continuityRow {
	reason := continuityOutcomeReason(evidence, causeExhausted)
	if reason == idleGateReason &&
		(strings.TrimSpace(reviewCommand) != "" || !idleGateAwaitsReview(evidence)) {
		reason = implementorActionRequiredReason
	}
	return continuityRow{gate: gateOwnedWorkRequired, reason: reason, cause: causeExhausted}
}

func continuityOutcomeReason(evidence map[string]any, cause continuityCause) string {
	gate := stringEvidence(evidence, "gate")
	reason := stringEvidence(evidence, "reason")
	ownedFormalChanges := gate == gateOwnedWorkRequired && reason == idleGateReason &&
		strings.EqualFold(strings.TrimSpace(stringEvidence(evidence, "review_decision")), "CHANGES_REQUESTED")
	if (cause == causeExhausted || cause == causeFailure) && (formalChangesRequested(evidence) || ownedFormalChanges) {
		return actionableFeedbackReason
	}
	// Genuine requested changes carried as informal feedback (a daemon
	// changes_requested verdict, an approval override) are not stalled
	// feedback: their stall keeps the requested-changes row.
	if cause == causeStall && reason == informalFeedbackReason && formalChangesRequested(evidence) {
		return actionableFeedbackReason
	}
	return reason
}

func (o *lifecycleObserver) continuityTable() continuityTable {
	if o != nil && o.outcomes != nil {
		return o.outcomes
	}
	return continuityOutcomes
}

func (o *lifecycleObserver) applyContinuityDisposition(row continuityRow, wait, terminal lifecycleVerdict) lifecycleVerdict {
	switch o.continuityTable().outcome(row) {
	case outcomeWait:
		return wait
	case outcomeHold:
		wait.Action = lifecycleAwait
		wait.Evidence = continuityHold(wait.Evidence, row.gate)
		return wait
	default:
		if terminal.Action == lifecycleFailure {
			return terminal
		}
		return lifecycleVerdict{
			Action: lifecycleFailure,
			Evidence: remediationBudgetFailureEvidence(row.gate, wait.Evidence,
				"inspect the current pull-request evidence; the external operation is no longer authorized to keep this Run waiting"),
		}
	}
}

// applyExpiredReviewOutcome uses the expiry row to decide whether the
// previously authorized, bounded recovery attempt remains available. A
// terminal row ends it before another implementation attempt can launch.
func (o *lifecycleObserver) applyExpiredReviewOutcome(branch, runID string, verdict lifecycleVerdict) lifecycleVerdict {
	if terminal, ended := o.expiredReviewTerminal(verdict); ended {
		return terminal
	}
	return o.applyContinuity(branch, runID, verdict)
}

// expiredReviewTerminal consults the expiry row for an expired review request.
// A hold leaves continuity to the caller: Observe applies it once to the
// verdict the decision returns, so a decision site that observes through it
// must not apply it first. A second application takes the await branch and
// overwrites the stalled signature the first one recorded.
func (o *lifecycleObserver) expiredReviewTerminal(verdict lifecycleVerdict) (lifecycleVerdict, bool) {
	request, ok := verdict.Evidence["review_request"].(map[string]any)
	if !ok || !confirmedReviewExpiryEvidence(request) {
		return lifecycleVerdict{}, false
	}
	row := continuityRow{
		gate:   gateReviewTimeout,
		reason: stringEvidence(verdict.Evidence, "reason"),
		cause:  causeExpired,
	}
	if row.reason == "" {
		row.reason = reviewTimeoutReason
	}
	if o.continuityTable().outcome(row) == outcomeHold {
		return lifecycleVerdict{}, false
	}
	evidence := cloneLifecycleExtras(verdict.Evidence)
	if evidence == nil {
		evidence = map[string]any{}
	}
	delete(evidence, "gate")
	delete(evidence, "await")
	delete(evidence, "continuity_hold")
	evidence["pull_request"] = request["pull_request"]
	evidence["head_sha"] = request["head_sha"]
	evidence["reason"] = reviewRequestExpiredReason
	evidence["external_gate"] = row.gate
	evidence["next_action"] = reviewRequestExpiredNextAction
	if _, observed := evidence["review_run_observed"]; !observed {
		evidence["review_run_observed"] = false
	}
	return lifecycleVerdict{Action: lifecycleFailure, Evidence: evidence}, true
}

func stringEvidence(evidence map[string]any, key string) string {
	value, _ := evidence[key].(string)
	return value
}

func (o *lifecycleObserver) resumeUnavailable(ctx context.Context, verdict lifecycleVerdict, nextAction string) lifecycleVerdict {
	if ctx.Err() != nil {
		return lifecycleVerdict{Action: lifecycleAborted}
	}
	if verdict.Action != lifecycleResume {
		return verdict
	}
	return lifecycleVerdict{
		Action: lifecycleFailure,
		Evidence: remediationBudgetFailureEvidence(
			stringEvidence(verdict.Evidence, "gate"),
			verdict.Evidence,
			nextAction,
		),
	}
}

func (o *lifecycleObserver) failUnperformedResume(verdict lifecycleVerdict, reason, nextAction string) lifecycleVerdict {
	if verdict.Action != lifecycleResume {
		return verdict
	}
	return lifecycleVerdict{
		Action:   lifecycleFailure,
		Evidence: lifecycleGateFailureEvidence(reason, nextAction, lifecycleGateNone, nil, ""),
	}
}

func (v lifecycleVerdict) status() string {
	switch v.Action {
	case lifecycleSuccess:
		return "success"
	case lifecycleFailure:
		return "failure"
	case lifecycleAwait:
		return "await"
	case lifecycleResume:
		return "resume"
	case lifecycleAborted:
		return "aborted"
	default:
		return ""
	}
}

func (v lifecycleVerdict) handled() bool { return v.Action != lifecycleNone }

func (v lifecycleVerdict) terminal() bool {
	switch v.Action {
	case lifecycleSuccess, lifecycleFailure, lifecycleAborted:
		return true
	default:
		return false
	}
}

func (v lifecycleVerdict) supersedesQuotaProbe() bool {
	return v.Action == lifecycleSuccess || v.Kind == lifecycleVerdictCompletion || v.Kind == lifecycleVerdictClosedPR
}

func failureForUnhandledObservation(verdict lifecycleVerdict) lifecycleVerdict {
	if verdict.Action != lifecycleNone {
		return verdict
	}
	return lifecycleVerdict{
		Action: lifecycleFailure,
		Evidence: lifecycleGateFailureEvidence(
			idleGateReason,
			idleGateNextAction,
			lifecycleGateNone,
			nil,
			"",
		),
	}
}
