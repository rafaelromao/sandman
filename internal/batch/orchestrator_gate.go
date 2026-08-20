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

// handleExternalGate adapts a clean AgentRun exit to the implementation-PR
// lifecycle decision. It is shared by fresh and continuation runs.
func (s *runSession) handleExternalGate(ctx context.Context, workDir, branch, logPath, runID string) (string, map[string]any, bool) {
	return s.handleExternalGateWithHostPaths(ctx, workDir, branch, logPath, runID, true)
}

func (s *runSession) handleExternalGateWithHostPaths(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	return s.handleLifecycleDecision(ctx, workDir, branch, logPath, runID, hostPathsReady)
}

func withExternalGateDiagnostics(result string, extras map[string]any, handled bool, diagnostics map[string]any) (string, map[string]any, bool) {
	if !handled || result == "aborted" || len(diagnostics) == 0 {
		return result, extras, handled
	}
	return result, mergeBlockerExtras(extras, diagnostics), handled
}

func (s *runSession) awaitExternalGateWithDiagnostics(ctx context.Context, reason string, diagnostics map[string]any) (string, map[string]any, bool) {
	result, extras, handled := s.awaitExternalGate(ctx, reason)
	return withExternalGateDiagnostics(result, extras, handled, diagnostics)
}

// resumeWorthyActionableFeedback decomposes a live failed gate into the
// await+actionable-feedback resume path when retained evidence proves the
// requested changes are actionable at the current head. It returns a zero
// result when no actionable evidence is available.
func (s *runSession) resumeWorthyActionableFeedback(ctx context.Context, workDir string, pr *github.PR, headSHA string, diagnostics map[string]any) (string, map[string]any, bool) {
	if pr == nil || retainedReviewPRGateFailed(pr) {
		return "", nil, false
	}
	extras := s.actionableFeedbackExtras(ctx, workDir, pr, headSHA)
	if extras == nil {
		return "", nil, false
	}
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	return withExternalGateDiagnostics("await", extras, true, diagnostics)
}

// actionableFeedbackExtras returns the retained actionable-feedback gate
// payload when the live PR was rejected for requested changes and the
// retained review evidence is still actionable at the current head. It
// returns nil when the evidence is absent, stale, or unreadable — the caller
// keeps the hard failed gate in that case.
func (s *runSession) actionableFeedbackExtras(ctx context.Context, workDir string, pr *github.PR, currentHead string) map[string]any {
	if pr == nil || !reviewTimeoutArtifactsPresent(workDir) {
		return nil
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil {
		return nil
	}
	handoff, err := readReviewTimeoutHandoff(workDir, repository, pr, currentHead)
	if err != nil || handoff == nil || !handoff.hasActionableFeedback() {
		return nil
	}
	extras := handoff.payloadFor(gateActionableFeedback, actionableFeedbackReason, actionableFeedbackNextAction)
	extras["gate"] = gateActionableFeedback
	extras["await"] = true
	return extras
}

// informalActionableFeedback classifies the retained informal sources of the
// active review request into request-scoped evidence for the pending-gate
// resume path. It returns the evidence list alongside the actionable-feedback
// extras so the caller can re-inject the list after folding the retained
// review diagnostics over the payload. It returns nil when the retained
// evidence is absent, stale, unreadable, superseded, formal-precedence-bearing,
// or lacks concrete informal feedback — the caller keeps the plain pending
// await in that case. The classifier itself is pure; this helper only reads
// the retained artifacts and builds the gate payload.
func (s *runSession) informalActionableFeedback(ctx context.Context, workDir string, pr *github.PR, currentHead string) ([]informalFeedbackEvidence, map[string]any) {
	if pr == nil || s.deps.githubClient == nil || !reviewTimeoutArtifactsPresent(workDir) {
		return nil, nil
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil {
		return nil, nil
	}
	handoff, err := readReviewTimeoutHandoff(workDir, repository, pr, currentHead)
	if err != nil || handoff == nil || handoff.Classification == nil {
		return nil, nil
	}
	evidence := handoff.Classification.informalFeedbackEvidenceFor(handoff.Request, handoff.Classification.WindowEnd)
	if len(evidence) == 0 {
		return nil, nil
	}
	extras := handoff.payloadFor(gateActionableFeedback, informalFeedbackReason, informalFeedbackNextAction)
	extras["gate"] = gateActionableFeedback
	extras["await"] = true
	return evidence, extras
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
		return retainedReviewEvidence{
			present: true,
			payload: map[string]any{"review_request": diagnostic["review_request"]},
		}
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
	return strings.Contains(message, "head") || strings.Contains(message, "superseded") || strings.Contains(message, "includes a trigger") || strings.Contains(message, "does not match")
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

func (s *runSession) handleReviewTimeoutGate(ctx context.Context, workDir, branch, logPath, runID, currentHead string) (string, map[string]any, bool) {
	if !reviewTimeoutArtifactsPresent(workDir) {
		return "", nil, false
	}
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	pr, err := s.deps.githubClient.FindPRByBranch(ctx, branch)
	if err != nil {
		// B2.3: a transient lookup error awaits the recoverable pending gate.
		return s.awaitExternalGate(ctx, "pending")
	}
	if pr == nil {
		return "", nil, false
	}
	if !reviewTimeoutArtifactsPresentForPR(workDir, pr.Number) {
		return "", nil, false
	}
	if strings.EqualFold(pr.State, "open") && retainedReviewPRGateFailed(pr) {
		// B2.1/B2.2: CI-failure / dirty / conflicting on an open PR awaits.
		return s.awaitExternalGate(ctx, "failed")
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil {
		// B2.7: review-timeout-state-error is operator-repairable local state
		// and awaits instead of terminalizing as blocked.
		return s.awaitExternalGate(ctx, gateReviewTimeoutError)
	}
	canonicalPath := paths.NewLayout(nil, workDir).PRReviewRegistrationPath(pr.Number)
	if _, err := readReviewRegistrationWithStore(s.reviewRegistrationStoreForRead(), canonicalPath, repository, pr, currentHead); err == nil || !isReviewRegistrationNotExist(err) {
		// Canonical evidence is immutable from this compatibility path too.
		// Legacy sidecars must not regain authority when it is present.
		return "", nil, false
	}
	artifacts, err := readReviewTimeoutArtifacts(workDir, repository, pr, currentHead)
	if err != nil {
		// Split legacy evidence is only a diagnostic compatibility path. A
		// missing or incomplete proposal is not a committed registration.
		return "", nil, false
	}
	handoff, err := reviewTimeoutHandoffFromArtifacts(artifacts, currentHead)
	if err != nil {
		if !reviewClassificationPresent(artifacts.State.Evidence) && strings.EqualFold(strings.TrimSpace(pr.ReviewDecision), "CHANGES_REQUESTED") {
			// B2.2: requested changes without retained evidence awaits.
			return s.awaitExternalGate(ctx, "failed")
		}
		// B2.7: invalid retained local state awaits for operator repair.
		return s.awaitExternalGate(ctx, gateReviewTimeoutError)
	}
	if handoff == nil {
		if pr.Merged || strings.EqualFold(pr.State, "merged") {
			return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
		}
		if !strings.EqualFold(pr.State, "open") {
			// B2.4: a PR closed without a merge is a terminal policy failure.
			return "failure", nil, true
		}
		return "", nil, false
	}
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	if pr.Merged || strings.EqualFold(pr.State, "merged") {
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	}
	if !strings.EqualFold(pr.State, "open") {
		// B2.4: a PR closed without a merge is a terminal policy failure.
		return "failure", nil, true
	}
	reviewDecision := strings.ToUpper(strings.TrimSpace(pr.ReviewDecision))
	if reviewDecision == "CHANGES_REQUESTED" && !handoff.hasActionableFeedback() {
		// B2.2: requested changes without retained actionable evidence awaits.
		return s.awaitExternalGate(ctx, "failed")
	}
	if handoff.hasActionableFeedback() {
		extras := handoff.payloadFor(gateActionableFeedback, actionableFeedbackReason, actionableFeedbackNextAction)
		extras["gate"] = gateActionableFeedback
		extras["await"] = true
		return "await", extras, true
	}
	if classification := handoff.Classification; classification != nil {
		if evidence := classification.informalFeedbackEvidenceFor(handoff.Request, classification.WindowEnd); len(evidence) > 0 {
			extras := handoff.payloadFor(gateActionableFeedback, informalFeedbackReason, informalFeedbackNextAction)
			if request, ok := extras["review_request"].(map[string]any); ok {
				request["informal_feedback"] = evidence
			}
			extras["gate"] = gateActionableFeedback
			extras["await"] = true
			return "await", extras, true
		}
	}
	if handoff.Outcome == retainedReviewApproval {
		return s.handleRetainedReviewApproval(ctx, workDir, branch, logPath, runID, pr, currentHead, handoff)
	}
	if handoff.Outcome != retainedReviewTimeout {
		return s.handleRetainedReviewPending(ctx, workDir, branch, logPath, runID, pr, currentHead, handoff)
	}
	extras := handoff.payloadFor(gateReviewTimeout, reviewTimeoutReason, reviewTimeoutNextAction)
	extras["gate"] = gateReviewTimeout
	extras["await"] = true
	return "await", extras, true
}

func retainedReviewPRGateFailed(pr *github.PR) bool {
	if pr == nil || !strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") {
		return true
	}
	mergeStatus := strings.ToUpper(strings.TrimSpace(pr.MergeStateStatus))
	return mergeStatus == "DIRTY" || mergeStatus == "CONFLICTING"
}

func (s *runSession) handleRetainedReviewApproval(ctx context.Context, workDir, branch, logPath, runID string, pr *github.PR, currentHead string, handoff *reviewTimeoutHandoff) (string, map[string]any, bool) {
	gate := checkPRExternalGateForPR(pr, currentHead, true)
	switch gate {
	case gateReadyToMerge:
		nextAction := "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate"
		extras := handoff.payload()
		extras["gate"] = gateReadyToMerge
		extras["reason"] = "REVIEW_APPROVED"
		extras["next_action"] = nextAction
		extras["await"] = true
		if request, ok := extras["review_request"].(map[string]any); ok {
			request["outcome"] = "approved"
			request["reason"] = "REVIEW_APPROVED"
			request["next_action"] = nextAction
		}
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
		return "await", extras, true
	case "resolved":
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	case "failed":
		// B2.1/B2.2: a failed open PR gate awaits.
		return s.awaitExternalGate(ctx, "failed")
	case "unavailable":
		// B2.4: a PR closed without a merge is a terminal policy failure.
		return "failure", nil, true
	default:
		return s.awaitExternalGate(ctx, "pending")
	}
}

func (s *runSession) handleRetainedReviewPending(ctx context.Context, workDir, branch, logPath, runID string, pr *github.PR, currentHead string, handoff *reviewTimeoutHandoff) (string, map[string]any, bool) {
	gate := checkPRExternalGateForPR(pr, currentHead, true)
	if gate == gateReadyToMerge {
		gate = "pending"
	}
	if gate == "resolved" {
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	}
	if gate == "failed" {
		// B2.1/B2.2: a failed open PR gate awaits.
		return s.awaitExternalGate(ctx, "failed")
	}
	if gate == "unavailable" {
		// B2.4: a PR closed without a merge is a terminal policy failure.
		return "failure", nil, true
	}
	extras := handoff.payload()
	extras["gate"] = gate
	extras["await"] = true
	return "await", extras, true
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

func (s *runSession) confirmExternalGate(ctx context.Context, workDir, branch, logPath, runID string) (string, map[string]any, bool) {
	if checkPRMergedForIssue(ctx, s.deps.githubClient, branch, s.issueNumber) {
		return "success", nil, true
	}
	if mergedPRMissingClosingReference(ctx, s.deps.githubClient, branch, s.issueNumber) {
		return "failure", mergeCompletionFailureExtras(nil, s.issueNumber), true
	}
	// The merged-but-unverifiable defensive arm (B2.5): the PR resolved but
	// no closing reference can be verified, so the run ends in a terminal
	// policy failure with the completion diagnostic — never blocked/unverified.
	return "failure", mergeCompletionFailureExtras(nil, s.issueNumber), true
}

// awaitExternalGate returns a non-terminal await result for recoverable PR
// lifecycle states. It does not consume retries or hold execution capacity.
func (s *runSession) awaitExternalGate(ctx context.Context, reason string) (string, map[string]any, bool) {
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	return "await", map[string]any{"gate": reason, "await": true}, true
}
