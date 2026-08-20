package batch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
)

// lifecycleAction is the terminal or non-terminal action an implementation-PR
// lifecycle state maps to. The merged-terminal outcomes landed in slice 1; the
// recoverable live-gate states (failed / pending / ready-to-merge) gained their
// await action in slice 2 as the terminal external-gate arms were removed.
type lifecycleAction int

const (
	lifecycleNone lifecycleAction = iota
	lifecycleSuccess
	lifecycleFailure
	lifecycleAwait
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
// from local retained review artifacts. The adapter currently uses the shape
// to preserve evidence while the live PR state remains authoritative.
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
//   - lifecycleNone means the adapter still needs more evidence or a PR
//     lookup before this decision can be applied.
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

// decidedAwait returns a handled recoverable-await decision for a live gate.
// The adapter still enriches these awaits with retained review evidence
// (actionable-feedback / informal) before they are emitted.
func decidedAwait(gate lifecycleGate) lifecycleDecision {
	return lifecycleDecision{
		action:  lifecycleAwait,
		gate:    gate,
		handled: true,
	}
}

// decideImplementationPRLifecycle folds the current implementation-PR
// lifecycle state into a single decision. It is the pure core of the gate
// consolidation (issue #2596): slices fold each external-gate arm into this
// point so handleExternalGate/handleReviewTimeoutGate become thin adapters.
//
// Slice 1 owned the merged (resolved) outcomes. Slice 2 extended the decision
// point to be total over the live open-PR gates: failed / pending /
// ready-to-merge await (recoverable), and a closed-without-merge PR is a
// terminal policy failure. Retained-review decomposition (actionable and
// informal feedback) stays in the adapter for now and folds into the decision
// point in slice 4 (B4.1).
func decideImplementationPRLifecycle(in implementationPRFacts) lifecycleDecision {
	if in.pr == nil {
		return unhandled(lifecycleGateNone)
	}
	gate := lifecycleGate(checkPRExternalGateForPR(in.pr, in.headSHA, true))
	switch gate {
	case lifecycleGateResolved:
		if in.mergeFacts == nil {
			// The PR is merged but the adapter has not supplied the
			// merge-intent facts yet. Ask for them and recompute.
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
		// lookups failed) — the merged-but-unverifiable defensive arm (B2.5).
		// Leave the state to the adapter's confirmExternalGate, which resolves
		// it to a terminal policy failure rather than guessing here.
		return unhandled(lifecycleGateResolved)
	case lifecycleGateFailed, lifecycleGatePending, lifecycleGateReady:
		// Recoverable live-gate states (B2.1, B2.2, B2.6): the run stays
		// non-terminal and can be resumed when the PR state changes.
		return decidedAwait(gate)
	case lifecycleGateUnavailable:
		// A non-open, non-merged PR is closed without a merge (B2.4): an
		// irrecoverable policy outcome that can never await.
		return lifecycleDecision{
			action:  lifecycleFailure,
			gate:    lifecycleGateUnavailable,
			handled: true,
		}
	default:
		return unhandled(gate)
	}
}

// lifecycleStatusRepr maps a decided action to the status string the run
// session records, keeping the decision point independent of runSession.
func lifecycleStatusRepr(d lifecycleDecision) string {
	switch d.action {
	case lifecycleSuccess:
		return "success"
	case lifecycleFailure:
		return "failure"
	case lifecycleAwait:
		return "await"
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
// routes every PR state through decideImplementationPRLifecycle. The retained
// review evidence (actionable / informal feedback) enriches the recoverable
// awaits below; the merged-but-unverifiable defensive arm resolves through
// confirmExternalGate as a terminal policy failure (B2.5).
func (s *runSession) handleLifecycleDecision(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	if s.deps.githubClient == nil {
		return "", nil, false
	}
	headSHA := s.currentGateHead(workDir)
	if !hostPathsReady {
		headSHA = ""
	}
	pr, err := lookupPRForExternalGate(ctx, s.deps.githubClient, branch)
	gate := lifecycleGateNone
	refreshUnavailable := false
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
		// B2.3: a transient refresh failure awaits the recoverable pending
		// gate instead of terminalizing.
		return s.awaitExternalGateWithDiagnostics(ctx, string(lifecycleGatePending), nil)
	}

	// The single lifecycle decision point owns every PR state. Merged PRs
	// resolve success/failure (asking for the merge-intent facts when they are
	// missing); the recoverable open-PR gates await below with retained
	// evidence enrichment.
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
	if decision.action == lifecycleSuccess || decision.action == lifecycleFailure {
		return lifecycleStatusRepr(decision), lifecycleFailureExtras(decision, s.issueNumber), true
	}
	if decision.gate == lifecycleGateResolved {
		// Merged-but-unverifiable (B2.5): confirmExternalGate resolves the
		// defensive arm to a terminal failure with completion extras — never
		// blocked/unverified.
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	}

	var diagnostics map[string]any
	if gate != lifecycleGateResolved && pr != nil {
		diagnostics = s.retainedReviewDiagnostics(ctx, workDir, branch, pr, headSHA)
	}
	switch decision.gate {
	case lifecycleGateFailed:
		// Decompose the live failure (issue #2595): a rejected review is
		// resume-worthy when retained evidence proves the feedback is
		// actionable at the current head; CI / mergeability failures await
		// (B2.1, B2.2) — the run stays non-terminal and releases capacity.
		if result, extras, handled := s.resumeWorthyActionableFeedback(ctx, workDir, pr, headSHA, diagnostics); result != "" {
			return result, extras, handled
		}
		return s.awaitExternalGateWithDiagnostics(ctx, string(lifecycleGateFailed), diagnostics)
	case lifecycleGateReady:
		// B2.6: a clean exit with a ready-to-merge PR awaits; the in-session
		// resume loop relaunches the agent on this gate.
		return s.awaitExternalGateWithDiagnostics(ctx, string(lifecycleGateReady), diagnostics)
	}
	// Pending (B2.2) and transient lookups (B2.3): retained informal evidence
	// first, then the plain recoverable await.
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
