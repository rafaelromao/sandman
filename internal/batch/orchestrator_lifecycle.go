package batch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
)

// lifecycleAction is the terminal or non-terminal action an implementation-PR
// lifecycle state maps to. The merged-terminal outcomes land here first;
// pending / failed / ready-to-merge / retained-review states migrate into this
// decision point in later slices and gain their own actions then.
type lifecycleAction int

const (
	lifecycleNone lifecycleAction = iota
	lifecycleSuccess
	lifecycleFailure
)

// lifecycleGate mirrors the string gates that checkPRExternalGateForPR emits
// so the decision point can match on a closed set instead of raw strings.
type lifecycleGate string

const (
	lifecycleGateNone         lifecycleGate = "none"
	lifecycleGatePending      lifecycleGate = "pending"
	lifecycleGateReady        lifecycleGate = "ready-to-merge"
	lifecycleGateResolved     lifecycleGate = "resolved"
	lifecycleGateFailed       lifecycleGate = "failed"
	lifecycleGateUnavailable  lifecycleGate = "unavailable"
	lifecycleGateOther        lifecycleGate = "other"
	lifecycleGateRetainedOnly lifecycleGate = "retained-only"
)

// retainedReviewEvidence is the set of facts the decision point can derive
// from local retained review artifacts. Slice 1 only reserves the shape;
// handleReviewTimeoutGate and the retained-approval arms fold into the
// decision point in a later slice and start populating it then.
type retainedReviewEvidence struct {
	present bool
}

// mergedMergeFacts holds the merge-intent facts the adapter gathers from the
// live PR (and the closing-reference repair records). A nil value means the
// adapter has not gathered them yet; the decision point asks for them before
// it can resolve a merged outcome.
type mergedMergeFacts struct {
	mergedWithClosingIntent bool
	mergedWithoutClosingRef bool
}

// implementationPRFacts is the immutable input to
// decideImplementationPRLifecycle. It is deliberately narrow: the host paths
// are not part of the decision, so the direct-call adapter gathers them before
// invoking the decision point.
type implementationPRFacts struct {
	pr               *github.PR
	headSHA          string
	mergeFacts       *mergedMergeFacts
	retainedEvidence retainedReviewEvidence
}

// lifecycleDecision is the outcome of decideImplementationPRLifecycle. The
// adapter interprets it:
//   - lifecycleNone means the decision point does not own this state yet, so
//     the legacy handler keeps the arm (Slice 1: every non-resolved gate).
//   - handled reports whether execution may stop on this decision.
//   - completionFailure flags failure outcomes that should carry the merge
//     completion diagnostic payload.
//   - needMergeFacts instructs the adapter to gather the merge-intent facts and
//     invoke the decision point a second time.
type lifecycleDecision struct {
	action            lifecycleAction
	gate              lifecycleGate
	handled           bool
	completionFailure bool
	needMergeFacts    bool
}

// unhandled is the zero-ish decision returned when the lifecycle state still
// belongs to the legacy handler.
func unhandled(gate lifecycleGate) lifecycleDecision {
	return lifecycleDecision{gate: gate}
}

// decideImplementationPRLifecycle folds the current implementation-PR
// lifecycle state into a single decision. It is the pure core of the gate
// consolidation (issue #2596): slices fold each external-gate arm into this
// point so handleExternalGate/handleReviewTimeoutGate become thin adapters.
//
// Slice 1 owns only the merged (resolved) outcomes. Every other gate returns
// unhandled; the legacy handler keeps those arms unchanged until the later
// slices migrate them.
func decideImplementationPRLifecycle(in implementationPRFacts) lifecycleDecision {
	if in.pr == nil {
		return unhandled(lifecycleGateNone)
	}
	gate := lifecycleGate(checkPRExternalGateForPR(in.pr, in.headSHA, true))
	if gate != lifecycleGateResolved {
		return unhandled(gate)
	}
	if in.mergeFacts == nil {
		// The PR is merged but the adapter has not supplied the merge-intent
		// facts yet. Ask for them and recompute.
		return lifecycleDecision{
			gate:           lifecycleGateResolved,
			needMergeFacts: true,
		}
	}
	if in.mergeFacts.mergedWithClosingIntent {
		return lifecycleDecision{
			action:  lifecycleSuccess,
			gate:    lifecycleGateResolved,
			handled: true,
		}
	}
	if in.mergeFacts.mergedWithoutClosingRef {
		return lifecycleDecision{
			action:            lifecycleFailure,
			gate:              lifecycleGateResolved,
			handled:           true,
			completionFailure: true,
		}
	}
	// Both merge checks came back false on a resolved PR (e.g. the live
	// lookups failed). Leave the state to the legacy handler rather than
	// guessing a terminal outcome.
	return unhandled(lifecycleGateResolved)
}

// lifecycleStatusRepr maps a decided action to the status string the run
// session records, keeping the decision point independent of runSession.
func lifecycleStatusRepr(d lifecycleDecision) string {
	switch d.action {
	case lifecycleSuccess:
		return "success"
	case lifecycleFailure:
		return "failure"
	default:
		return ""
	}
}

// lifecycleFailureExtras returns the terminal extras for a lifecycle failure
// decision, deferring to the existing completion diagnostic for merged
// PRs missing the closing reference.
func lifecycleFailureExtras(d lifecycleDecision, issueNumber int) map[string]any {
	if d.completionFailure {
		return mergeCompletionFailureExtras(nil, issueNumber)
	}
	return nil
}

// handleLifecycleDecision turns an AgentRun exit into a terminal or awatable
// lifecycle result. It is the direct-call adapter for the decision point: it
// gathers the host-path facts the pure decision is intentionally blind to, and
// routes only the states the decision point owns into it. Every other gate
// keeps the legacy behavior until the later slices migrate it.
func (s *runSession) handleLifecycleDecision(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	if s.deps.githubClient == nil {
		return "", nil, false
	}
	headSHA := s.currentGateHead(workDir)
	if !hostPathsReady {
		headSHA = ""
	}
	pr, err := lookupPRForExternalGate(ctx, s.deps.githubClient, branch)
	initialUnavailable := err != nil
	refreshUnavailable := false
	gate := lifecycleGateNone
	if err != nil && s.deps.errorLog != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: external gate lookup for branch %q: %v\n", branch, err)
		gate = lifecycleGatePending
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
				gate = lifecycleGatePending
				refreshUnavailable = true
				err = nil
			} else {
				pr = refreshedPR
				err = nil
			}
		}
	}
	if err == nil && pr != nil {
		gate = lifecycleGate(checkPRExternalGateForPR(pr, headSHA, true))
	}

	if gate == lifecycleGateNone {
		return "", nil, false
	}
	if refreshUnavailable {
		return s.awaitExternalGateWithDiagnostics(ctx, string(lifecycleGatePending), nil)
	}

	// The single lifecycle decision point. States it does not own yet keep
	// the legacy arms below (they fold in over the next slices).
	if gate == lifecycleGateResolved && pr != nil {
		decision := decideImplementationPRLifecycle(implementationPRFacts{
			pr:      pr,
			headSHA: headSHA,
		})
		if decision.needMergeFacts {
			decision = decideImplementationPRLifecycle(implementationPRFacts{
				pr:      pr,
				headSHA: headSHA,
				mergeFacts: &mergedMergeFacts{
					mergedWithClosingIntent: checkPRMergedForIssue(ctx, s.deps.githubClient, branch, s.issueNumber),
					mergedWithoutClosingRef: mergedPRMissingClosingReference(ctx, s.deps.githubClient, branch, s.issueNumber),
				},
			})
		}
		if status := lifecycleStatusRepr(decision); status != "" && decision.handled {
			return status, lifecycleFailureExtras(decision, s.issueNumber), true
		}
	}

	var diagnostics map[string]any
	if gate != lifecycleGateResolved && pr != nil {
		diagnostics = s.retainedReviewDiagnostics(ctx, workDir, branch, pr, headSHA)
	}
	if gate == lifecycleGateReady {
		return s.awaitExternalGateWithDiagnostics(ctx, string(lifecycleGateReady), diagnostics)
	}
	if gate == lifecycleGateFailed {
		// Decompose the live failure (issue #2595): a rejected review is
		// resume-worthy when retained evidence proves the feedback is
		// actionable at the current head, while CI / mergeability failures
		// keep the hard blocked gate (the agent cannot act on them without
		// operator work anyway). The actionable-feedback await hands the
		// agent the retained review_request so a resumed session can
		// address the exact requested changes.
		if result, extras, handled := s.resumeWorthyActionableFeedback(ctx, workDir, pr, headSHA, diagnostics); result != "" {
			return result, extras, handled
		}
		return s.blockExternalGateWithDiagnostics(ctx, workDir, logPath, runID, "failed", diagnostics)
	}
	if gate == lifecycleGateUnavailable && !initialUnavailable {
		return s.blockExternalGateWithDiagnostics(ctx, workDir, logPath, runID, "unavailable", diagnostics)
	}

	polled := pollPRGateWithHead(ctx, s.deps.githubClient, branch, headSHA, true, s.opts)
	if polled == gateResolved {
		return s.confirmExternalGateWithDiagnostics(ctx, workDir, branch, logPath, runID, diagnostics)
	}
	if polled == gatePollReadyToMerge {
		return s.awaitExternalGateWithDiagnostics(ctx, string(lifecycleGateReady), diagnostics)
	}
	if polled == gateFailed {
		if result, extras, handled := s.resumeWorthyActionableFeedback(ctx, workDir, pr, headSHA, diagnostics); result != "" {
			return result, extras, handled
		}
		return s.blockExternalGateWithDiagnostics(ctx, workDir, logPath, runID, "failed", diagnostics)
	}
	if polled == gatePollUnavailable || polled == gatePollPRMissing {
		return s.blockExternalGateWithDiagnostics(ctx, workDir, logPath, runID, "unavailable", diagnostics)
	}
	if evidence, informalExtras := s.informalActionableFeedback(ctx, workDir, pr, headSHA); informalExtras != nil {
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
		result, extras, handled := withExternalGateDiagnostics("await", informalExtras, true, diagnostics)
		if handled {
			// The diagnostics merge folds the retained review_request over the
			// extras payload, and the diagnostic copy never carries the
			// informal classification result. Re-inject the evidence into the
			// surviving envelope so resumeEvidenceFor, run.resumed, and the
			// resume prompt all see it.
			if request, ok := extras["review_request"].(map[string]any); ok {
				request["informal_feedback"] = evidence
			}
		}
		return result, extras, handled
	}
	return s.awaitExternalGateWithDiagnostics(ctx, string(lifecycleGatePending), diagnostics)
}
