package batch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

const gateReadyToMerge = "ready-to-merge"

func checkPRExternalGate(ctx context.Context, client github.Client, branch string) (string, error) {
	return checkPRExternalGateWithHead(ctx, client, branch, "", false)
}

func checkPRExternalGateAtHead(ctx context.Context, client github.Client, branch, headSHA string) (string, error) {
	return checkPRExternalGateWithHead(ctx, client, branch, headSHA, true)
}

func checkPRExternalGateWithHead(ctx context.Context, client github.Client, branch, headSHA string, requireHead bool) (string, error) {
	_, gate, err := lookupPRExternalGateWithHead(ctx, client, branch, headSHA, requireHead)
	return gate, err
}

func lookupPRExternalGateWithHead(ctx context.Context, client github.Client, branch, headSHA string, requireHead bool) (*github.PR, string, error) {
	pr, err := lookupPRForExternalGate(ctx, client, branch)
	if err != nil {
		return nil, "unavailable", err
	}
	if pr == nil {
		return nil, "none", nil
	}
	return pr, checkPRExternalGateForPR(pr, headSHA, requireHead), nil
}

func lookupPRForExternalGate(ctx context.Context, client github.Client, branch string) (*github.PR, error) {
	if client == nil || strings.TrimSpace(branch) == "" {
		return nil, nil
	}
	pr, err := client.FindPRByBranch(ctx, branch)
	if err != nil {
		return nil, err
	}
	if pr != nil && strings.TrimSpace(pr.HeadRefName) == "" {
		copy := *pr
		copy.HeadRefName = branch
		return &copy, nil
	}
	return pr, nil
}

func checkPRExternalGateForPR(pr *github.PR, headSHA string, requireHead bool) string {
	if pr == nil {
		return "none"
	}
	if pr.Merged || strings.EqualFold(pr.State, "merged") {
		return "resolved"
	}
	if !strings.EqualFold(pr.State, "open") {
		return "unavailable"
	}

	checkRollup := strings.ToLower(strings.TrimSpace(pr.StatusCheckRollup))
	hasCIPending := checkRollup == "pending"
	hasCIFailure := checkRollup == "failure"
	review := strings.ToUpper(strings.TrimSpace(pr.ReviewDecision))
	mergeStatus := strings.ToUpper(strings.TrimSpace(pr.MergeStateStatus))

	if hasCIFailure {
		return "failed"
	}
	if review == "CHANGES_REQUESTED" {
		return "failed"
	}
	if mergeStatus == "DIRTY" || mergeStatus == "CONFLICTING" {
		return "failed"
	}

	if hasCIPending || review == "REVIEW_REQUIRED" || mergeStatus == "BLOCKED" {
		return "pending"
	}
	if requireHead {
		if strings.TrimSpace(headSHA) == "" || strings.TrimSpace(pr.HeadRefOid) == "" {
			return "pending"
		}
		if !strings.EqualFold(pr.HeadRefOid, headSHA) {
			return "pending"
		}
	}
	if (checkRollup == "" || checkRollup == "success") && (review == "" || review == "APPROVED") && mergeStatus == "CLEAN" {
		return gateReadyToMerge
	}

	return "pending"
}

// retainedLifecycleEvidence decodes the latest retained review record without
// deciding what the live pull request means. Retained records enrich a
// lifecycle decision; they never outrank the live merged or closed state.
func (s *runSession) retainedLifecycleEvidence(ctx context.Context, workDir string, pr *github.PR, currentHead string) retainedReviewEvidence {
	if s.blindReviewMode() {
		// Blind evidence is fetched once by the lifecycle adapter so lookup
		// errors enter GitHub recovery rather than masquerading as no review.
		return retainedReviewEvidence{}
	}
	if pr == nil || pr.Number <= 0 || s.deps.githubClient == nil {
		return retainedReviewEvidence{}
	}
	injectedStore := s.reviewRegistrationStore != nil || s.opts.reviewRegistrationStore != nil
	if !reviewLifecycleArtifactsPresentForPR(workDir, pr.Number) && !injectedStore {
		return retainedReviewEvidence{}
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil || strings.TrimSpace(repository) == "" {
		return retainedReviewEvidence{present: true, stateError: true, diagnosticErr: err}
	}
	registration, err := s.loadCanonicalReviewRegistration(ctx, workDir, repository, pr, currentHead)
	if isReviewRegistrationNotExist(err) {
		return retainedReviewEvidence{diagnostics: map[string]any{"review_diagnostic": map[string]any{"status": "pending"}}}
	}
	if err != nil {
		return retainedReviewEvidence{present: true, stateError: !retainedEvidenceIsStale(err), diagnosticErr: err}
	}
	handoff, err := canonicalReviewHandoff(registration, currentHead)
	if err != nil {
		return retainedReviewEvidence{present: true, stateError: true, diagnosticErr: err}
	}
	evidence := retainedEvidenceFromHandoff(registration, handoff)
	evidence.diagnostics = reviewRegistrationDiagnostic(registration)
	if handoff != nil {
		evidence.diagnostics = handoff.payload()
		delete(evidence.diagnostics, "reason")
		delete(evidence.diagnostics, "next_action")
		evidence.diagnostics["review_diagnostic"] = map[string]any{"status": "valid", "outcome": string(handoff.Outcome)}
	}
	if registration == nil {
		return evidence
	}
	evidence.clock = s.managedReviewClock(registration.Request, s.runtimeNow())
	evidence.requestActive = confirmedReviewRequestActive(registration, handoff, evidence.clock)
	return applyManagedReviewClock(registration, evidence)
}

func retainedEvidenceFromHandoff(registration *reviewRequestRegistration, handoff *reviewTimeoutHandoff) retainedReviewEvidence {
	evidence := retainedReviewEvidence{present: true}
	if handoff == nil {
		if registration != nil && !registration.LegacyImported {
			evidence.payload = map[string]any{"review_request": reviewRegistrationDiagnostic(registration)["review_request"]}
		}
		return evidence
	}
	evidence.outcome = handoff.Outcome
	if !reviewEvidenceWithinCanonicalDeadline(handoff, registration.Request) {
		if handoff.Outcome == retainedReviewTimeout {
			// Timeout evidence still carries the canonical request identity
			// needed to decide whether a different live resolver is keeping
			// this expired request actionable.
			evidence.payload = handoff.payload()
		}
		return evidence
	}
	// Confirmed approval settles this request before feedback heuristics.
	// Scope tables, informational comments and earlier feedback cannot turn
	// an approved current-head request into another repair launch.
	switch {
	case handoff.Outcome == retainedReviewApproval:
		evidence.payload = handoff.payloadFor(gateReadyToMerge, "REVIEW_APPROVED", "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate")
		if request, ok := evidence.payload["review_request"].(map[string]any); ok {
			request["outcome"] = "approved"
			if handoff.Classification != nil {
				request["review_decision_approval"] = handoff.Classification.reviewDecisionApprovalEvidenceFor(handoff.Request, handoff.Classification.WindowEnd)
			}
		}
	case handoff.hasActionableFeedback():
		evidence.actionable = true
		evidence.payload = handoff.payloadFor(gateActionableFeedback, actionableFeedbackReason, actionableFeedbackNextAction)
	case handoff.Classification != nil:
		evidence.informalFeedback = handoff.Classification.informalFeedbackEvidenceFor(handoff.Request, handoff.Classification.WindowEnd)
		if len(evidence.informalFeedback) > 0 {
			evidence.payload = handoff.payloadFor(gateActionableFeedback, informalFeedbackReason, informalFeedbackNextAction)
			if request, ok := evidence.payload["review_request"].(map[string]any); ok {
				request["informal_feedback"] = evidence.informalFeedback
				if approvalOverridden(evidence.informalFeedback) {
					request[approvalOverrideKey] = approvalOverrideStatement
				}
			}
		} else {
			if handoff.Outcome == retainedReviewTimeout {
				evidence.payload = handoff.payload()
			} else {
				return evidence
			}
		}
	default:
		if handoff.Outcome == retainedReviewTimeout {
			evidence.payload = handoff.payload()
		} else {
			return evidence
		}
	}
	if evidence.payload != nil {
		if evidence.outcome == retainedReviewApproval {
			evidence.payload["gate"] = gateReadyToMerge
		} else {
			evidence.payload["gate"] = gateActionableFeedback
		}
		evidence.payload["await"] = true
	}
	return evidence
}

// A validated pending request authorizes waiting before a reviewer starts.
// A stale timeout observation cannot shorten the effective deadline.
func confirmedReviewRequestActive(registration *reviewRequestRegistration, handoff *reviewTimeoutHandoff, clock managedReviewDeadline) bool {
	if registration.State.State != "pending" || !clock.active() {
		return false
	}
	switch registration.State.ObservedState {
	case "", "timed_out":
		return true
	}
	if handoff == nil || handoff.hasActionableFeedback() ||
		(handoff.Outcome != retainedReviewPending && !(clock.adjusted() && handoff.Outcome == retainedReviewTimeout)) {
		return false
	}
	return handoff.Classification == nil || len(handoff.Classification.informalFeedbackEvidenceFor(handoff.Request, handoff.Classification.WindowEnd)) == 0
}

// loadCanonicalReviewRegistration is the only lifecycle boundary that may
// read legacy review artifacts. A valid legacy generation is imported once;
// after that, lifecycle decisions never fall back to mutable sidecars.
func (s *runSession) loadCanonicalReviewRegistration(ctx context.Context, workDir, repository string, pr *github.PR, currentHead string) (*reviewRequestRegistration, error) {
	path := paths.NewLayout(nil, workDir).PRReviewRegistrationPath(pr.Number)
	store := s.reviewRegistrationStoreForRead()
	registration, err := readReviewRegistrationWithStore(store, path, repository, pr, currentHead)
	if err == nil {
		if importErr := s.importLegacyReviewEvidence(ctx, workDir, repository, pr, currentHead, path, registration); importErr != nil {
			return nil, importErr
		}
		if refreshed, refreshErr := readReviewRegistrationWithStore(store, path, repository, pr, currentHead); refreshErr == nil {
			registration = refreshed
		} else {
			return nil, refreshErr
		}
		return registration, nil
	}
	if !isReviewRegistrationNotExist(err) {
		return nil, err
	}
	if !reviewTimeoutArtifactsPresentForPR(workDir, pr.Number) {
		return nil, err
	}
	legacy, present, valid, inspectErr := inspectLegacyReviewRegistration(workDir, repository, pr, currentHead)
	if inspectErr != nil {
		return nil, inspectErr
	}
	if !present {
		return nil, err
	}
	if !valid || legacy == nil {
		return nil, fmt.Errorf("legacy review artifacts are invalid")
	}
	if writeErr := writeReviewRegistration(store, path, *legacy, func() error {
		return s.verifyCurrentReviewHead(ctx, pr, currentHead)
	}); writeErr != nil {
		return nil, fmt.Errorf("migrate legacy review registration: %w", writeErr)
	}
	return readReviewRegistrationWithStore(store, path, repository, pr, currentHead)
}

func canonicalReviewHandoff(registration *reviewRequestRegistration, currentHead string) (*reviewTimeoutHandoff, error) {
	if registration == nil || registration.State.Evidence == nil {
		return nil, nil
	}
	if err := validateCanonicalReviewObservation(registration.Request, registration.State); err != nil {
		return nil, err
	}
	classification, err := decodeReviewClassification(registration.State.Evidence, registration.Request, currentHead)
	if err != nil {
		return nil, err
	}
	counts, err := responseCountsFromState(registration.State, true)
	if err != nil {
		return nil, err
	}
	outcome := retainedReviewObservationOutcome(classification, registration.Request, registration.State)
	return &reviewTimeoutHandoff{
		Request:        registration.Request,
		State:          registration.State,
		ResponseCounts: counts,
		Classification: classification,
		Outcome:        outcome,
	}, nil
}

func retainedEvidenceIsStale(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "observed head") {
		return false
	}
	return strings.Contains(message, "head") || strings.Contains(message, "superseded") || strings.Contains(message, "includes a trigger") || strings.Contains(message, "state does not match")
}

func (s *runSession) invalidRetainedReviewDiagnostic(branch string, err error) map[string]any {
	// A request bound to an earlier head is expected after a push until the
	// current head is requested. The diagnostic below reports it on every wait;
	// a warning per poll would only drown the run log (issue #3008).
	if s.deps.errorLog != nil && !errors.Is(err, errReviewRegistrationStaleHead) {
		fmt.Fprintf(s.deps.errorLog, "warning: retained review artifacts ignored for live gate on branch %q: %v\n", branch, err)
	}
	return map[string]any{
		"review_diagnostic": map[string]any{
			"status": "invalid",
			"reason": gateReviewTimeoutError,
			"error":  err.Error(),
		},
	}
}

func (s *runSession) currentGateHeadSnapshot(workDir string) (string, error) {
	if strings.TrimSpace(workDir) == "" && s.opts.currentHead == nil {
		return "", nil
	}
	resolver := s.opts.currentHead
	if resolver == nil {
		resolver = currentBranchHeadFn
	}
	headSHA, err := resolver(workDir)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(headSHA), nil
}
