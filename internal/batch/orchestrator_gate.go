package batch

import (
	"context"
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

func (s *runSession) retainedReviewDiagnostics(ctx context.Context, workDir, branch string, pr *github.PR, currentHead string) map[string]any {
	injectedStore := s.reviewRegistrationStore != nil || s.opts.reviewRegistrationStore != nil
	if ctx.Err() != nil || pr == nil || (!reviewTimeoutArtifactsPresent(workDir) && !injectedStore) {
		return nil
	}
	if !reviewTimeoutArtifactsPresentForPR(workDir, pr.Number) && !injectedStore {
		return nil
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil {
		return s.invalidRetainedReviewDiagnostic(branch, err)
	}
	registration, err := s.loadCanonicalReviewRegistration(ctx, workDir, repository, pr, currentHead)
	if err == nil {
		handoff, handoffErr := canonicalReviewHandoff(registration, currentHead)
		if handoffErr != nil {
			return s.invalidRetainedReviewDiagnostic(branch, handoffErr)
		}
		if handoff == nil {
			return reviewRegistrationDiagnostic(registration)
		}
		diagnostics := map[string]any{
			"review_diagnostic": map[string]any{
				"status":  "valid",
				"outcome": string(handoff.Outcome),
			},
		}
		payload := handoff.payload()
		if request, ok := payload["review_request"]; ok {
			diagnostics["review_request"] = request
		}
		return diagnostics
	}
	if !isReviewRegistrationNotExist(err) {
		// A canonical record exists but is not valid. It wins over legacy
		// sidecars as evidence, while the live PR gate remains authoritative.
		return s.invalidRetainedReviewDiagnostic(branch, err)
	}
	return map[string]any{
		"review_diagnostic": map[string]any{
			"status": "pending",
		},
	}
}

// retainedLifecycleEvidence decodes the latest retained review record without
// deciding what the live pull request means. Retained records enrich a
// lifecycle decision; they never outrank the live merged or closed state.
func (s *runSession) retainedLifecycleEvidence(ctx context.Context, workDir string, pr *github.PR, currentHead string) retainedReviewEvidence {
	if pr == nil || s.deps.githubClient == nil {
		return retainedReviewEvidence{}
	}
	injectedStore := s.reviewRegistrationStore != nil || s.opts.reviewRegistrationStore != nil
	if !reviewTimeoutArtifactsPresentForPR(workDir, pr.Number) && !injectedStore {
		return retainedReviewEvidence{}
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil {
		return retainedReviewEvidence{present: true, stateError: true}
	}
	registration, err := s.loadCanonicalReviewRegistration(ctx, workDir, repository, pr, currentHead)
	if err == nil {
		evidence := retainedReviewEvidence{
			present: true,
		}
		handoff, handoffErr := canonicalReviewHandoff(registration, currentHead)
		if handoffErr != nil || handoff == nil {
			if handoffErr != nil {
				evidence.stateError = true
			} else if registration != nil && !registration.LegacyImported {
				diagnostic := reviewRegistrationDiagnostic(registration)
				evidence.payload = map[string]any{"review_request": diagnostic["review_request"]}
			}
			return evidence
		}
		evidence.outcome = handoff.Outcome
		if !reviewEvidenceWithinCanonicalDeadline(handoff, registration.Request) {
			return evidence
		}
		evidence.actionable = handoff.hasActionableFeedback()
		switch {
		case evidence.actionable:
			evidence.payload = handoff.payloadFor(gateActionableFeedback, actionableFeedbackReason, actionableFeedbackNextAction)
		case handoff.Classification != nil:
			evidence.informalFeedback = handoff.Classification.informalFeedbackEvidenceFor(handoff.Request, handoff.Classification.WindowEnd)
			if len(evidence.informalFeedback) > 0 {
				evidence.payload = handoff.payloadFor(gateActionableFeedback, informalFeedbackReason, informalFeedbackNextAction)
				if request, ok := evidence.payload["review_request"].(map[string]any); ok {
					request["informal_feedback"] = evidence.informalFeedback
				}
			} else if handoff.Outcome == retainedReviewApproval {
				evidence.outcome = retainedReviewApproval
				evidence.payload = handoff.payloadFor(gateReadyToMerge, "REVIEW_APPROVED", "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate")
				if request, ok := evidence.payload["review_request"].(map[string]any); ok {
					request["outcome"] = "approved"
					request["review_decision_approval"] = handoff.Classification.reviewDecisionApprovalEvidenceFor(handoff.Request, handoff.Classification.WindowEnd)
				}
			} else {
				return evidence
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
	if !isReviewRegistrationNotExist(err) {
		if retainedEvidenceIsStale(err) {
			return retainedReviewEvidence{present: true}
		}
		return retainedReviewEvidence{present: true, stateError: true}
	}
	return retainedReviewEvidence{}
}

// confirmedReviewRequestActive reports whether a confirmed delegated-review
// request is actively resolving for this pull request at the current head. It
// validates the canonical runtime-owned registration after importing one
// matching legacy generation when necessary. The canonical record must match
// repository, pull request, and head, remain in the pending wait state, and
// stay within its deadline. A
// requested review counts as an ongoing external operation even before the
// review run starts (issue #2743). Stale, mismatched, expired, corrupt, or
// otherwise unusable records never authorize waiting.
func (s *runSession) confirmedReviewRequestActive(ctx context.Context, workDir string, pr *github.PR, currentHead string) bool {
	if pr == nil || pr.Number <= 0 || s.deps.githubClient == nil {
		return false
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil || strings.TrimSpace(repository) == "" {
		return false
	}
	now := s.reviewNow().Unix()
	if registration, err := s.loadCanonicalReviewRegistration(ctx, workDir, repository, pr, currentHead); err == nil && registration != nil {
		if registration.State.State != "pending" || int64(registration.Request.DeadlineUnixSeconds) <= now {
			return false
		}
		if registration.State.ObservedState != "" {
			return false
		}
		return true
	} else if err != nil && !isReviewRegistrationNotExist(err) {
		// A canonical record exists but is not valid. It wins over legacy
		// sidecars as evidence: fail closed rather than resurrecting stale
		// authority from an older artifact.
		return false
	}
	return false
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
		if registration.State.ObservedState == "" {
			if refreshed, refreshErr := readReviewRegistrationWithStore(store, path, repository, pr, currentHead); refreshErr == nil {
				registration = refreshed
			}
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
	outcome := retainedReviewClassificationOutcome(classification, registration.Request)
	if classification == nil && registration.State.ObservedState == "timed_out" {
		outcome = retainedReviewTimeout
	}
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
	if s.deps.errorLog != nil {
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
