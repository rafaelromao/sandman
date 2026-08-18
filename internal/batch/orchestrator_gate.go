package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

type gateResult int

const (
	gateResolved gateResult = iota
	gateFailed
	gatePollBudgetExhausted
	gatePollUnavailable
	gatePollPRMissing
	gatePollReadyToMerge
)

const gateReadyToMerge = "ready-to-merge"

var (
	defaultGatePollInitial  = time.Millisecond
	defaultGatePollMaxSleep = time.Millisecond
	defaultGatePollBudget   = 5 * time.Millisecond
)

func pollPRGate(ctx context.Context, client github.Client, branch string, opts runSessionOptions) gateResult {
	return pollPRGateWithHead(ctx, client, branch, "", false, opts)
}

func pollPRGateAtHead(ctx context.Context, client github.Client, branch, headSHA string, opts runSessionOptions) gateResult {
	return pollPRGateWithHead(ctx, client, branch, headSHA, true, opts)
}

func pollPRGateWithHead(ctx context.Context, client github.Client, branch, headSHA string, requireHead bool, opts runSessionOptions) gateResult {
	initial := opts.gatePollInitial
	if initial <= 0 {
		initial = defaultGatePollInitial
	}
	maxSleep := opts.gatePollMaxSleep
	if maxSleep <= 0 {
		maxSleep = defaultGatePollMaxSleep
	}
	budget := opts.gatePollBudget
	if budget <= 0 {
		budget = defaultGatePollBudget
	}

	pollCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	deadline := time.Now().Add(budget)
	delay := initial
	lastLookupUnavailable := false

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastLookupUnavailable {
				return gatePollUnavailable
			}
			return gatePollBudgetExhausted
		}
		if delay > remaining {
			delay = remaining
		}
		select {
		case <-pollCtx.Done():
			if lastLookupUnavailable {
				return gatePollUnavailable
			}
			return gatePollBudgetExhausted
		case <-time.After(delay):
		}

		gate, err := checkPRExternalGateWithHead(pollCtx, client, branch, headSHA, requireHead)
		switch gate {
		case "resolved":
			return gateResolved
		case "failed":
			return gateFailed
		case "unavailable":
			if err == nil {
				return gatePollUnavailable
			}
			lastLookupUnavailable = true
		case "none":
			return gatePollPRMissing
		case gateReadyToMerge:
			return gatePollReadyToMerge
		default:
			lastLookupUnavailable = false
			delay = delay * 2
			if delay > maxSleep {
				delay = maxSleep
			}
		}
	}
}

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

// handleExternalGate turns an AgentRun exit into a terminal external-gate
// result when the branch has a PR waiting on CI or review. It is shared by
// fresh and continuation runs so an explicit intervention cannot re-enter the
// old failure/retry path while the same PR gate is still unresolved.
func (s *runSession) handleExternalGate(ctx context.Context, workDir, branch, logPath, runID string) (string, map[string]any, bool) {
	return s.handleExternalGateWithHostPaths(ctx, workDir, branch, logPath, runID, true)
}

func (s *runSession) handleExternalGateWithHostPaths(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	if s.deps.githubClient == nil {
		return "", nil, false
	}
	// The live pull request is authoritative. Local review records can enrich
	// the result, but they must not decide whether the run is terminal.
	headSHA := s.currentGateHead(workDir)
	if !hostPathsReady {
		headSHA = ""
	}
	pr, err := lookupPRForExternalGate(ctx, s.deps.githubClient, branch)
	initialUnavailable := err != nil
	refreshUnavailable := false
	gate := "none"
	if err != nil && s.deps.errorLog != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: external gate lookup for branch %q: %v\n", branch, err)
		gate = "pending"
	}
	if pr != nil && strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		registrationErr := s.ensureReviewRegistrationForPR(ctx, workDir, pr, headSHA)
		headChanged := errors.Is(registrationErr, errReviewRegistrationHeadChanged)
		refreshLivePR := headChanged ||
			(registrationErr == nil && s.reviewRegistrationObserved)
		if refreshLivePR {
			// Registration may observe comments and persist while the live PR
			// changes. Refresh after a confirmed head change or successful
			// observation before allowing the gate to terminalize.
			refreshedPR, refreshErr := lookupPRForExternalGate(ctx, s.deps.githubClient, branch)
			if refreshErr != nil {
				if s.deps.errorLog != nil {
					fmt.Fprintf(s.deps.errorLog, "warning: external gate refresh for branch %q: %v\n", branch, refreshErr)
				}
				// A requested refresh has no safe fallback. Do not reuse a
				// pre-registration snapshot or poll until it can be replaced.
				pr = nil
				gate = "pending"
				refreshUnavailable = true
				err = nil
			} else {
				pr = refreshedPR
				err = nil
			}
		}
	}
	if err == nil && pr != nil {
		gate = checkPRExternalGateForPR(pr, headSHA, true)
	}

	if gate == "none" {
		return "", nil, false
	}
	if refreshUnavailable {
		return s.awaitExternalGateWithDiagnostics(ctx, "pending", nil)
	}
	var diagnostics map[string]any
	if gate != "resolved" && pr != nil {
		diagnostics = s.retainedReviewDiagnostics(ctx, workDir, branch, pr, headSHA)
	}
	if gate == gateReadyToMerge {
		return s.awaitExternalGateWithDiagnostics(ctx, gateReadyToMerge, diagnostics)
	}
	if gate == "resolved" {
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	}
	if gate == "failed" {
		// Decompose the live failure (issue #2595): a rejected review is
		// resume-worthy when retained evidence proves the feedback is
		// actionable at the current head, while CI / mergeability failures
		// keep the hard blocked gate (the agent cannot act on them without
		// operator work anyway). The actionable-feedback await hands the
		// agent the retained review_request so a resumed session can
		// address the exact requested changes.
		if !retainedReviewPRGateFailed(pr) {
			if extras := s.actionableFeedbackExtras(ctx, workDir, pr, headSHA); extras != nil {
				if ctx.Err() != nil {
					return "aborted", nil, true
				}
				return withExternalGateDiagnostics("await", extras, true, diagnostics)
			}
		}
		return s.blockExternalGateWithDiagnostics(ctx, workDir, logPath, runID, "failed", diagnostics)
	}
	if gate == "unavailable" && !initialUnavailable {
		return s.blockExternalGateWithDiagnostics(ctx, workDir, logPath, runID, "unavailable", diagnostics)
	}

	polled := pollPRGateWithHead(ctx, s.deps.githubClient, branch, headSHA, true, s.opts)
	if polled == gateResolved {
		return s.confirmExternalGateWithDiagnostics(ctx, workDir, branch, logPath, runID, diagnostics)
	}
	if polled == gatePollReadyToMerge {
		return s.awaitExternalGateWithDiagnostics(ctx, gateReadyToMerge, diagnostics)
	}
	if polled == gateFailed {
		if !retainedReviewPRGateFailed(pr) {
			if extras := s.actionableFeedbackExtras(ctx, workDir, pr, headSHA); extras != nil {
				if ctx.Err() != nil {
					return "aborted", nil, true
				}
				return withExternalGateDiagnostics("await", extras, true, diagnostics)
			}
		}
		return s.blockExternalGateWithDiagnostics(ctx, workDir, logPath, runID, "failed", diagnostics)
	}
	if polled == gatePollUnavailable || polled == gatePollPRMissing {
		return s.blockExternalGateWithDiagnostics(ctx, workDir, logPath, runID, "unavailable", diagnostics)
	}
	return s.awaitExternalGateWithDiagnostics(ctx, "pending", diagnostics)
}

func withExternalGateDiagnostics(result string, extras map[string]any, handled bool, diagnostics map[string]any) (string, map[string]any, bool) {
	if !handled || result == "aborted" || len(diagnostics) == 0 {
		return result, extras, handled
	}
	return result, mergeBlockerExtras(extras, diagnostics), handled
}

func (s *runSession) blockExternalGateWithDiagnostics(ctx context.Context, workDir, logPath, runID, reason string, diagnostics map[string]any) (string, map[string]any, bool) {
	result, extras, handled := s.blockExternalGate(ctx, workDir, logPath, runID, reason)
	return withExternalGateDiagnostics(result, extras, handled, diagnostics)
}

func (s *runSession) awaitExternalGateWithDiagnostics(ctx context.Context, reason string, diagnostics map[string]any) (string, map[string]any, bool) {
	result, extras, handled := s.awaitExternalGate(ctx, reason)
	return withExternalGateDiagnostics(result, extras, handled, diagnostics)
}

func (s *runSession) confirmExternalGateWithDiagnostics(ctx context.Context, workDir, branch, logPath, runID string, diagnostics map[string]any) (string, map[string]any, bool) {
	result, extras, handled := s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	return withExternalGateDiagnostics(result, extras, handled, diagnostics)
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
	extras["blocker"] = "external-gate"
	extras["gate"] = gateActionableFeedback
	extras["await"] = true
	return extras
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
		return s.blockExternalGate(ctx, workDir, logPath, runID, "unavailable")
	}
	if pr == nil {
		return "", nil, false
	}
	if !reviewTimeoutArtifactsPresentForPR(workDir, pr.Number) {
		return "", nil, false
	}
	if strings.EqualFold(pr.State, "open") && retainedReviewPRGateFailed(pr) {
		return s.blockExternalGate(ctx, workDir, logPath, runID, "failed")
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil {
		return s.blockReviewTimeoutStateError(ctx, workDir, logPath, runID)
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
			return s.blockExternalGate(ctx, workDir, logPath, runID, "failed")
		}
		return s.blockReviewTimeoutStateError(ctx, workDir, logPath, runID)
	}
	if handoff == nil {
		if pr.Merged || strings.EqualFold(pr.State, "merged") {
			return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
		}
		if !strings.EqualFold(pr.State, "open") {
			return s.blockExternalGate(ctx, workDir, logPath, runID, "unavailable")
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
		return s.blockExternalGate(ctx, workDir, logPath, runID, "unavailable")
	}
	reviewDecision := strings.ToUpper(strings.TrimSpace(pr.ReviewDecision))
	if reviewDecision == "CHANGES_REQUESTED" && !handoff.hasActionableFeedback() {
		return s.blockExternalGate(ctx, workDir, logPath, runID, "failed")
	}
	if handoff.hasActionableFeedback() {
		extras := handoff.payloadFor(gateActionableFeedback, actionableFeedbackReason, actionableFeedbackNextAction)
		extras["blocker"] = "external-gate"
		extras["gate"] = gateActionableFeedback
		extras["await"] = true
		return "await", extras, true
	}
	if handoff.Outcome == retainedReviewApproval {
		return s.handleRetainedReviewApproval(ctx, workDir, branch, logPath, runID, pr, currentHead, handoff)
	}
	if handoff.Outcome != retainedReviewTimeout {
		return s.handleRetainedReviewPending(ctx, workDir, branch, logPath, runID, pr, currentHead, handoff)
	}
	extras := handoff.payloadFor(gateReviewTimeout, reviewTimeoutReason, reviewTimeoutNextAction)
	extras["blocker"] = "external-gate"
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
		extras["blocker"] = "external-gate"
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
		return s.blockExternalGate(ctx, workDir, logPath, runID, "failed")
	case "unavailable":
		return s.blockExternalGate(ctx, workDir, logPath, runID, "unavailable")
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
		return s.blockExternalGate(ctx, workDir, logPath, runID, "failed")
	}
	if gate == "unavailable" {
		return s.blockExternalGate(ctx, workDir, logPath, runID, "unavailable")
	}
	extras := handoff.payload()
	extras["blocker"] = "external-gate"
	extras["gate"] = gate
	extras["await"] = true
	return "await", extras, true
}

func (s *runSession) blockReviewTimeoutStateError(ctx context.Context, workDir, logPath, runID string) (string, map[string]any, bool) {
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	s.recordExternalGateBlocker(workDir, logPath, runID, gateReviewTimeoutError)
	return "blocked", map[string]any{
		"blocker":     "external-gate",
		"gate":        gateReviewTimeoutError,
		"reason":      "REVIEW_TIMEOUT_STATE_ERROR",
		"next_action": "repair or remove the invalid retained review request, then continue only after confirming a new review trigger",
	}, true
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
	return s.blockExternalGate(ctx, workDir, logPath, runID, "unverified")
}

func (s *runSession) blockExternalGate(ctx context.Context, workDir, logPath, runID, reason string) (string, map[string]any, bool) {
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	s.recordExternalGateBlocker(workDir, logPath, runID, reason)
	return "blocked", map[string]any{"blocker": "external-gate", "gate": reason}, true
}

// awaitExternalGate returns a non-terminal await result for recoverable
// external gate states. Unlike blockExternalGate, it does not write
// blocker text to task.md or run.log, does not consume retries, and
// does not hold execution capacity.
func (s *runSession) awaitExternalGate(ctx context.Context, reason string) (string, map[string]any, bool) {
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	return "await", map[string]any{"blocker": "external-gate", "gate": reason, "await": true}, true
}

func (s *runSession) recordExternalGateBlocker(workDir, logPath, runID, reason string) {
	failure := fmt.Sprintf("pull request external gate is %s", reason)
	nextAction := "recheck CI and delegated review, then continue the run when intervention is required"
	if reason == "failed" {
		nextAction = "inspect the failed CI or requested review changes, then continue the run to address them"
	}
	if reason == gateReviewTimeoutError {
		nextAction = "repair or remove the invalid retained review request, then continue only after confirming a new review trigger"
	}
	blocker := fmt.Sprintf("\n\n## External Gate\n\n- Failure: %s.\n- Next action: %s.\n", failure, nextAction)
	logSummary := failure
	if reason == gateReadyToMerge {
		nextAction = "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate"
		blocker = fmt.Sprintf("\n\n## External Gate\n\n- State: pull request external gate is ready-to-merge.\n- Next action: %s.\n", nextAction)
		logSummary = "pull request is ready to merge"
	}

	if strings.TrimSpace(workDir) != "" {
		taskPath := filepath.Join(workDir, ".sandman", "task.md")
		content, err := os.ReadFile(taskPath)
		if err != nil && !os.IsNotExist(err) {
			s.logExternalGateWriteError("read task.md", err)
		} else {
			updated := replaceExternalGateBlocker(content, blocker)
			if err := atomicfs.WriteAtomic(taskPath, updated, 0o644); err != nil {
				s.logExternalGateWriteError("write task.md", err)
			}
		}
	}

	if strings.TrimSpace(logPath) != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			s.logExternalGateWriteError("create run.log directory", err)
			return
		}
		file, err := atomicfs.OpenAppend(logPath, 0o644)
		if err != nil {
			s.logExternalGateWriteError("open run.log", err)
			return
		}
		prefixed := NewLinePrefixWriter(runID, file)
		_, writeErr := fmt.Fprintf(prefixed, "external gate %s: %s; next action: %s\n", reason, logSummary, nextAction)
		flushErr := prefixed.Flush()
		closeErr := file.Close()
		if writeErr != nil {
			s.logExternalGateWriteError("write run.log", writeErr)
		}
		if flushErr != nil {
			s.logExternalGateWriteError("flush run.log", flushErr)
		}
		if closeErr != nil {
			s.logExternalGateWriteError("close run.log", closeErr)
		}
	}
}

func (s *runSession) recordReviewTimeoutGateBlocker(workDir, logPath, runID string, handoff *reviewTimeoutHandoff) {
	s.recordReviewOutcomeBlocker(workDir, logPath, runID, handoff, gateReviewTimeout, reviewTimeoutReason, reviewTimeoutNextAction)
}

func (s *runSession) recordReviewOutcomeBlocker(workDir, logPath, runID string, handoff *reviewTimeoutHandoff, gate, reason, nextAction string) {
	request := handoff.Request
	counts := handoff.ResponseCounts
	state := "delegated review request exhausted its deadline"
	if gate == gateActionableFeedback {
		state = "delegated review request has actionable requested changes"
	}
	blocker := fmt.Sprintf(
		"\n\n## External Gate\n\n- State: %s.\n- Reason: %s.\n- Repository: %s.\n- Pull request: #%d.\n- Current head: %s.\n- Trigger: %s.\n- Trigger created: %s.\n- Confirmed: %s.\n- Started: %s.\n- Deadline: %s (%d).\n- Budget: %d seconds.\n- Elapsed: %d seconds.\n- Response counts: top-level=%d, formal=%d, inline=%d.\n- Next action: %s.\n",
		state,
		reason,
		request.Repository,
		request.PullRequest,
		request.HeadSHA,
		request.TriggerID,
		request.TriggerCreatedAt,
		request.ConfirmedAt,
		request.StartedAt,
		request.DeadlineAt,
		request.DeadlineUnixSeconds,
		request.EffectiveTimeout,
		*handoff.State.ElapsedSeconds,
		counts.TopLevel,
		counts.FormalReviews,
		counts.Inline,
		nextAction,
	)

	if strings.TrimSpace(workDir) != "" {
		taskPath := filepath.Join(workDir, ".sandman", "task.md")
		content, err := os.ReadFile(taskPath)
		if err != nil && !os.IsNotExist(err) {
			s.logExternalGateWriteError("read task.md", err)
		} else {
			updated := replaceExternalGateBlocker(content, blocker)
			if err := atomicfs.WriteAtomic(taskPath, updated, 0o644); err != nil {
				s.logExternalGateWriteError("write task.md", err)
			}
		}
	}

	if strings.TrimSpace(logPath) == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		s.logExternalGateWriteError("create run.log directory", err)
		return
	}
	file, err := atomicfs.OpenAppend(logPath, 0o644)
	if err != nil {
		s.logExternalGateWriteError("open run.log", err)
		return
	}
	prefixed := NewLinePrefixWriter(runID, file)
	_, writeErr := fmt.Fprintf(prefixed, "external gate %s: %s; repository %s pull request #%d head %s trigger %s created %s confirmed %s started %s deadline %s budget %d elapsed %d counters top-level=%d formal=%d inline=%d; next action: %s\n", gate, reason, request.Repository, request.PullRequest, request.HeadSHA, request.TriggerID, request.TriggerCreatedAt, request.ConfirmedAt, request.StartedAt, request.DeadlineAt, request.EffectiveTimeout, *handoff.State.ElapsedSeconds, counts.TopLevel, counts.FormalReviews, counts.Inline, nextAction)
	flushErr := prefixed.Flush()
	closeErr := file.Close()
	if writeErr != nil {
		s.logExternalGateWriteError("write run.log", writeErr)
	}
	if flushErr != nil {
		s.logExternalGateWriteError("flush run.log", flushErr)
	}
	if closeErr != nil {
		s.logExternalGateWriteError("close run.log", closeErr)
	}
}

func replaceExternalGateBlocker(content []byte, blocker string) []byte {
	text := string(content)
	const heading = "## External Gate"
	start := strings.Index(text, heading)
	if start < 0 {
		return append(append([]byte(nil), content...), []byte(blocker)...)
	}

	end := len(text)
	rest := text[start+len(heading):]
	if next := strings.Index(rest, "\n## "); next >= 0 {
		end = start + len(heading) + next + 1
	}
	updated := text[:start] + strings.TrimSpace(blocker) + "\n\n" + text[end:]
	return []byte(updated)
}

func (s *runSession) logExternalGateWriteError(operation string, err error) {
	if s.deps.errorLog != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: %s for external gate blocker: %v\n", operation, err)
	}
}
