package batch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

// lifecycleAction is the terminal or non-terminal action an implementation-PR
// lifecycle state maps to. The merged-terminal outcomes landed in slice 1; the
// recoverable live-gate states (failed / pending / ready-to-merge) gained their
// await action in slice 2 as recoverable pull-request states moved into the
// runtime-owned lifecycle decision.
type lifecycleAction int

const (
	lifecycleNone lifecycleAction = iota
	lifecycleSuccess
	lifecycleFailure
	lifecycleAwait
	lifecycleResume
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

// errLifecycleObservationTestStop lets package tests bound a foreground wait
// without turning it into cancellation. Production wait implementations never
// return this sentinel.
var errLifecycleObservationTestStop = errors.New("stop lifecycle observation for test")

// retainedReviewEvidence is the set of facts the decision point can derive
// from local retained review artifacts. The adapter currently uses the shape
// to preserve evidence while the live PR state remains authoritative.
type retainedReviewEvidence struct {
	present          bool
	outcome          retainedReviewOutcome
	actionable       bool
	informalFeedback []informalFeedbackEvidence
	payload          map[string]any
	stateError       bool
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
	extras            map[string]any
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
// lifecycle state and retained review evidence into one decision. It is pure:
// callers gather live PR facts and decode local evidence before invoking it.
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
		// Both merge checks came back false on a resolved PR. A merged PR
		// whose completion evidence cannot be verified is a terminal policy
		// failure; it must not fall through to the ordinary success path.
		return lifecycleDecision{
			action:            lifecycleFailure,
			gate:              lifecycleGateResolved,
			handled:           true,
			completionFailure: true,
		}
	case lifecycleGateFailed, lifecycleGatePending, lifecycleGateReady:
		return decideRecoverableLifecycle(gate, in.pr, in.retainedEvidence)
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

func decideRecoverableLifecycle(gate lifecycleGate, pr *github.PR, evidence retainedReviewEvidence) lifecycleDecision {
	// CI failures and merge conflicts are branch-owned work, not external work
	// that can make progress while the agent waits. Handle these typed facts
	// before the legacy gate/evidence compatibility rules below.
	if pr != nil && strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") {
		return lifecycleRemediationDecision("ci-failure", "CI_FAILURE", "inspect current-head CI with gh pr checks and repair the failing checks", pr, evidence.payload)
	}
	if pr != nil && (strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") || strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING")) {
		return lifecycleRemediationDecision("merge-conflict", "MERGE_CONFLICT", "rebase or merge the base branch, resolve conflicts, and push a new head", pr, evidence.payload)
	}
	if evidence.stateError && gate != lifecycleGateFailed {
		return lifecycleDecision{
			action:  lifecycleAwait,
			gate:    lifecycleGate(gateReviewTimeoutError),
			handled: true,
			extras:  map[string]any{"gate": gateReviewTimeoutError, "await": true},
		}
	}
	reviewChangesRequested := pr != nil && strings.EqualFold(strings.TrimSpace(pr.ReviewDecision), "CHANGES_REQUESTED")
	hardFailure := pr != nil && (strings.EqualFold(strings.TrimSpace(pr.StatusCheckRollup), "failure") || strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "DIRTY") || strings.EqualFold(strings.TrimSpace(pr.MergeStateStatus), "CONFLICTING"))
	if evidence.actionable && (gate != lifecycleGateFailed || (reviewChangesRequested && !hardFailure)) {
		return lifecycleDecision{
			action:  lifecycleResume,
			gate:    lifecycleGate(gateActionableFeedback),
			handled: true,
			extras:  evidence.payload,
		}
	}
	if gate != lifecycleGateFailed && len(evidence.informalFeedback) > 0 {
		return lifecycleDecision{
			action:  lifecycleResume,
			gate:    lifecycleGate(gateActionableFeedback),
			handled: true,
			extras:  evidence.payload,
		}
	}
	if gate == lifecycleGateReady && evidence.outcome == retainedReviewApproval {
		return lifecycleDecision{
			action:  lifecycleResume,
			gate:    lifecycleGate(gateReadyToMerge),
			handled: true,
			extras:  evidence.payload,
		}
	}
	if evidence.outcome == retainedReviewTimeout && gate == lifecycleGatePending {
		return lifecycleDecision{
			action:  lifecycleAwait,
			gate:    lifecycleGate(gateReviewTimeout),
			handled: true,
			extras:  evidence.payload,
		}
	}
	decision := decidedAwait(gate)
	if evidence.payload != nil && (gate != lifecycleGateFailed || evidence.actionable && reviewChangesRequested && !hardFailure) {
		decision.extras = cloneLifecycleExtras(evidence.payload)
		decision.extras["gate"] = string(gate)
		decision.extras["await"] = true
	}
	return decision
}

func lifecycleRemediationDecision(gate, reason, nextAction string, pr *github.PR, retained map[string]any) lifecycleDecision {
	extras := cloneLifecycleExtras(retained)
	if extras == nil {
		extras = map[string]any{}
	}
	extras["pull_request"] = pr.Number
	extras["head_sha"] = pr.HeadRefOid
	extras["reason"] = reason
	extras["next_action"] = nextAction
	return lifecycleDecision{action: lifecycleResume, gate: lifecycleGate(gate), handled: true, extras: extras}
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
	case lifecycleResume:
		return "resume"
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

// handleLifecycleDecision turns an AgentRun exit into the result selected by
// the lifecycle decision point. It gathers live PR facts and retained review
// evidence, while event and prompt writing remain adapter concerns.
func (s *runSession) handleLifecycleDecision(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	return s.handleLifecycleDecisionWithPublication(ctx, workDir, branch, logPath, runID, hostPathsReady, s.mode != ModeContinue)
}

func (s *runSession) handleLifecycleDecisionAfterAgent(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	return s.handleLifecycleDecisionWithPublication(ctx, workDir, branch, logPath, runID, hostPathsReady, true)
}

func (s *runSession) handleLifecycleDecisionWithPublication(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady, awaitPublication bool) (string, map[string]any, bool) {
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
	if err != nil {
		if s.deps.errorLog != nil {
			fmt.Fprintf(s.deps.errorLog, "warning: external gate lookup for branch %q: %v\n", branch, err)
		}
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
		if err == nil && pr == nil && awaitPublication && strings.TrimSpace(s.issueState) != "" && !strings.EqualFold(strings.TrimSpace(s.issueState), "closed") {
			// PR publication can lag behind a clean agent exit. Keep the
			// session foreground until the pull request becomes observable.
			return "await", map[string]any{"gate": string(lifecycleGatePending), "await": true}, true
		}
		return "", nil, false
	}
	if refreshUnavailable {
		// B2.3: a transient refresh failure awaits the recoverable pending
		// gate instead of terminalizing.
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
		return "await", map[string]any{"gate": string(lifecycleGatePending), "await": true}, true
	}
	if err != nil {
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
		return "await", map[string]any{"gate": string(lifecycleGatePending), "await": true}, true
	}

	// Merged PRs resolve before retained evidence is consulted, so stale or
	// malformed review records cannot override verified completion.
	var mergeFacts *mergedMergeFacts
	decision := decideImplementationPRLifecycle(implementationPRFacts{
		pr:      pr,
		headSHA: headSHA,
	})
	if decision.needMergeFacts {
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
		merged := pr != nil && (pr.Merged || strings.EqualFold(strings.TrimSpace(pr.State), "merged"))
		mergeFacts = &mergedMergeFacts{
			mergedWithClosingIntent: merged && pr.ClosesIssue(s.issueNumber),
			mergedWithoutClosingRef: merged && !pr.ClosesIssue(s.issueNumber),
		}
		decision = decideImplementationPRLifecycle(implementationPRFacts{
			pr:         pr,
			headSHA:    headSHA,
			mergeFacts: mergeFacts,
		})
		if ctx.Err() != nil {
			return "aborted", nil, true
		}
	}
	if decision.action == lifecycleSuccess || decision.action == lifecycleFailure {
		return lifecycleStatusRepr(decision), lifecycleFailureExtras(decision, s.issueNumber), true
	}
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	evidence := s.retainedLifecycleEvidence(ctx, workDir, pr, headSHA)
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	decision = decideImplementationPRLifecycle(implementationPRFacts{
		pr:               pr,
		headSHA:          headSHA,
		mergeFacts:       mergeFacts,
		retainedEvidence: evidence,
	})
	if decision.action == lifecycleSuccess || decision.action == lifecycleFailure {
		return lifecycleStatusRepr(decision), lifecycleFailureExtras(decision, s.issueNumber), true
	}
	if !decision.handled {
		return "", nil, false
	}
	extras := decision.extras
	if diagnostics := s.retainedReviewDiagnostics(ctx, workDir, branch, pr, headSHA); len(diagnostics) > 0 {
		extras = mergeLifecycleDiagnostics(extras, diagnostics)
	}
	if extras == nil {
		extras = map[string]any{"gate": string(decision.gate), "await": true}
	}
	if decision.action == lifecycleResume {
		// Live typed remediation outranks retained review labels.
		extras["gate"] = string(decision.gate)
	} else if _, ok := extras["gate"]; !ok {
		extras["gate"] = string(decision.gate)
	}
	extras["await"] = true
	status := lifecycleStatusRepr(decision)
	if status == "resume" && decision.gate == lifecycleGateReady {
		// Ready-to-merge remains an await until the normal merge lifecycle
		// consumes its live approval gate; only actionable feedback is an
		// explicit feedback resume at this adapter boundary.
		status = "await"
	}
	return status, extras, true
}

func cloneLifecycleExtras(extras map[string]any) map[string]any {
	clone := make(map[string]any, len(extras))
	for key, value := range extras {
		clone[key] = value
	}
	return clone
}

func mergeLifecycleDiagnostics(extras, diagnostics map[string]any) map[string]any {
	if len(diagnostics) == 0 {
		return extras
	}
	if extras == nil {
		extras = map[string]any{}
	}
	for key, value := range diagnostics {
		if key != "review_request" {
			extras[key] = value
			continue
		}
		diagnosticRequest, diagnosticOK := value.(map[string]any)
		evidenceRequest, evidenceOK := extras[key].(map[string]any)
		if !diagnosticOK || !evidenceOK {
			extras[key] = value
			continue
		}
		merged := make(map[string]any, len(diagnosticRequest)+len(evidenceRequest))
		for requestKey, requestValue := range diagnosticRequest {
			merged[requestKey] = requestValue
		}
		for requestKey, requestValue := range evidenceRequest {
			merged[requestKey] = requestValue
		}
		extras[key] = merged
	}
	return extras
}

func (s *runSession) lifecyclePollIntervals(extras map[string]any) []time.Duration {
	if len(s.opts.lifecyclePollPlan) > 0 {
		valid := true
		for _, interval := range s.opts.lifecyclePollPlan {
			if interval < 0 {
				valid = false
				break
			}
		}
		if valid {
			return append([]time.Duration(nil), s.opts.lifecyclePollPlan...)
		}
	}
	if request, ok := extras["review_request"].(map[string]any); ok {
		if raw, ok := request["poll_plan"].([]int); ok && len(raw) > 0 {
			plan := make([]time.Duration, 0, len(raw))
			valid := true
			for _, seconds := range raw {
				if seconds < 0 {
					valid = false
					break
				}
				plan = append(plan, time.Duration(seconds)*time.Second)
			}
			if valid && len(plan) > 0 {
				return plan
			}
		}
		if raw, ok := request["poll_plan"].([]any); ok && len(raw) > 0 {
			plan := make([]time.Duration, 0, len(raw))
			valid := true
			for _, value := range raw {
				seconds, ok := value.(float64)
				if !ok || seconds < 0 || seconds != float64(int(seconds)) {
					valid = false
					break
				}
				plan = append(plan, time.Duration(int(seconds))*time.Second)
			}
			if valid && len(plan) > 0 {
				return plan
			}
		}
	}
	plan := make([]time.Duration, 0, len(implementationReviewPollPlan))
	for _, seconds := range implementationReviewPollPlan {
		plan = append(plan, time.Duration(seconds)*time.Second)
	}
	return plan
}

func (s *runSession) waitForLifecyclePoll(ctx context.Context, interval time.Duration) error {
	if s.opts.lifecycleWait != nil {
		return s.opts.lifecycleWait(ctx, interval)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// observeLifecycle keeps a recoverable lifecycle decision in the foreground.
// The final configured interval repeats after the initial plan. A resume-worthy
// transition is returned to the caller so the existing bounded resume path can
// relaunch without consuming an agent retry.
func (s *runSession) observeLifecycle(ctx context.Context, workDir, branch, logPath, runID string, result AgentRunResult, extras map[string]any, hostPathsReady bool) (string, map[string]any, bool) {
	plan := s.lifecyclePollIntervals(extras)
	for index := 0; ; index++ {
		if deadline, ok := lifecycleDeadline(extras); ok && !time.Now().Before(deadline) {
			return "resume", map[string]any{
				"gate":        gateReviewTimeout,
				"reason":      reviewTimeoutReason,
				"next_action": reviewTimeoutNextAction,
			}, true
		}
		interval := plan[len(plan)-1]
		if index < len(plan) {
			interval = plan[index]
		}
		if err := s.waitForLifecyclePoll(ctx, interval); err != nil {
			if errors.Is(err, errLifecycleObservationTestStop) {
				return "await", extras, true
			}
			return "aborted", nil, true
		}
		status, nextExtras, handled := s.handleLifecycleDecisionAfterAgent(ctx, workDir, branch, logPath, runID, hostPathsReady)
		if !handled {
			status = "await"
			nextExtras = map[string]any{"gate": string(lifecycleGatePending), "await": true}
		}
		if status == "resume" {
			status = "await"
		}
		if status == "await" {
			gate, _ := nextExtras["gate"].(string)
			if (gate == gateReadyToMerge || gate == gateActionableFeedback) && s.resumeCount < s.resumeCapFor() {
				return status, nextExtras, true
			}
			s.emitAwait(ctx, runID, result, nextExtras)
			continue
		}
		return status, nextExtras, true
	}
}

func lifecycleDeadline(extras map[string]any) (time.Time, bool) {
	request, ok := extras["review_request"].(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	seconds, ok := request["deadline_unix_seconds"].(float64)
	if !ok || seconds <= 0 || seconds != float64(int64(seconds)) {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0), true
}
