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
	registration, err := readReviewRegistrationWithStore(s.reviewRegistrationStoreForRead(), paths.NewLayout(nil, workDir).PRReviewRegistrationPath(pr.Number), repository, pr, currentHead)
	if err == nil {
		return reviewRegistrationDiagnostic(registration)
	}
	if !isReviewRegistrationNotExist(err) {
		// A canonical record exists but is not valid. It wins over legacy
		// sidecars as evidence, while the live PR gate remains authoritative.
		return s.invalidRetainedReviewDiagnostic(branch, err)
	}
	artifacts, err := readReviewTimeoutArtifacts(workDir, repository, pr, currentHead)
	if err != nil {
		return s.invalidRetainedReviewDiagnostic(branch, err)
	}
	handoff, err := reviewTimeoutHandoffFromArtifacts(artifacts, currentHead)
	if err != nil {
		return s.invalidRetainedReviewDiagnostic(branch, err)
	}
	if handoff == nil {
		return map[string]any{
			"review_diagnostic": map[string]any{
				"status": "pending",
				"state":  artifacts.State.State,
			},
		}
	}
	payload := handoff.payload()
	diagnostics := map[string]any{
		"review_diagnostic": map[string]any{
			"status":  "valid",
			"outcome": string(handoff.Outcome),
		},
	}
	if request, ok := payload["review_request"]; ok {
		diagnostics["review_request"] = request
	}
	return diagnostics
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
	canonicalPath := paths.NewLayout(nil, workDir).PRReviewRegistrationPath(pr.Number)
	registration, err := readReviewRegistrationWithStore(s.reviewRegistrationStoreForRead(), canonicalPath, repository, pr, currentHead)
	if err == nil {
		diagnostic := reviewRegistrationDiagnostic(registration)
		evidence := retainedReviewEvidence{
			present: true,
			payload: map[string]any{"review_request": diagnostic["review_request"]},
		}
		artifacts, artifactErr := readReviewTimeoutArtifacts(workDir, repository, pr, currentHead)
		if artifactErr != nil || artifacts == nil || !reviewRequestIdentityMatches(registration.Request, artifacts.Request) {
			return evidence
		}
		handoff, handoffErr := reviewTimeoutHandoffFromArtifacts(artifacts, currentHead)
		if handoffErr != nil || handoff == nil {
			return evidence
		}
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
			} else {
				return evidence
			}
		case handoff.Outcome == retainedReviewApproval:
			// Canonical registration must not turn aggregate approval into a
			// feedback resume. The live ready-to-merge gate remains the only
			// approval path.
			return evidence
		default:
			return evidence
		}
		if evidence.payload != nil {
			evidence.payload["gate"] = gateActionableFeedback
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
	if injectedStore && !reviewTimeoutArtifactsPresentForPR(workDir, pr.Number) {
		return retainedReviewEvidence{}
	}
	artifacts, err := readReviewTimeoutArtifacts(workDir, repository, pr, currentHead)
	if err != nil {
		if retainedEvidenceIsStale(err) {
			return retainedReviewEvidence{present: true}
		}
		return retainedReviewEvidence{present: true, stateError: true}
	}
	handoff, err := reviewTimeoutHandoffFromArtifacts(artifacts, currentHead)
	if err != nil {
		if retainedEvidenceIsStale(err) {
			return retainedReviewEvidence{present: true}
		}
		return retainedReviewEvidence{present: true, stateError: true}
	}
	if handoff == nil {
		return retainedReviewEvidence{present: true, outcome: retainedReviewPending}
	}
	evidence := retainedReviewEvidence{
		present:    true,
		outcome:    handoff.Outcome,
		actionable: handoff.hasActionableFeedback(),
	}
	evidenceGate := gateReviewTimeout
	switch {
	case evidence.actionable:
		evidence.payload = handoff.payloadFor(gateActionableFeedback, actionableFeedbackReason, actionableFeedbackNextAction)
		evidenceGate = gateActionableFeedback
	case handoff.Classification != nil:
		evidence.informalFeedback = handoff.Classification.informalFeedbackEvidenceFor(handoff.Request, handoff.Classification.WindowEnd)
		if len(evidence.informalFeedback) > 0 {
			evidence.payload = handoff.payloadFor(gateActionableFeedback, informalFeedbackReason, informalFeedbackNextAction)
			evidenceGate = gateActionableFeedback
			if request, ok := evidence.payload["review_request"].(map[string]any); ok {
				request["informal_feedback"] = evidence.informalFeedback
			}
		}
	case handoff.Outcome == retainedReviewApproval:
		evidence.payload = handoff.payloadFor(gateReadyToMerge, "REVIEW_APPROVED", "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate")
		evidenceGate = gateReadyToMerge
		if request, ok := evidence.payload["review_request"].(map[string]any); ok {
			request["outcome"] = "approved"
			request["reason"] = "REVIEW_APPROVED"
			request["next_action"] = "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate"
		}
	default:
		evidence.payload = handoff.payload()
	}
	if evidence.payload != nil {
		evidence.payload["gate"] = evidenceGate
		evidence.payload["await"] = true
	}
	return evidence
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

func (s *runSession) currentGateHead(workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	resolver := s.opts.currentHead
	if resolver == nil {
		resolver = currentBranchHeadFn
	}
	headSHA, err := resolver(workDir)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(headSHA)
}
