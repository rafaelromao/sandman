package batch

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// AFK continuity (issue #2916). A started Run whose pull request is open at
// its current head never becomes terminal-failed when the only missing
// evidence is review, CI, or a review request the runtime did not observe:
// it waits for external recovery or proceeds to merge. This file holds the
// two rules that enforce it where lifecycle decisions become results:
//
//   - a lifecycle relaunch that leaves the resume signature unchanged (no new
//     head, request, or response) is a stall, and a stall or an exhausted
//     relaunch allowance waits instead of failing;
//   - a lifecycle failure is terminal only when its reason is permanent.

const (
	resumeStalledReason     = "RESUME_STALLED"
	resumeStalledNextAction = "the last implementation relaunch produced no new pull-request head or review request; waiting for a new head, a new review response or request, or a resolved gate"
	stalledResumesKey       = "stalled_resumes"
	maxStalledResumes       = 16

	// reviewFeedbackStalledReason ends a Run whose informal review feedback
	// relaunch left the head, the review request and its responses unchanged
	// (#2999): the implementor found nothing it could do, and no operation is
	// resolving the feedback. The evidence names the feedback in the retained
	// review request and the stalled resume signature under stalledResumeKey.
	reviewFeedbackStalledReason     = "REVIEW_FEEDBACK_STALLED"
	reviewFeedbackStalledNextAction = "inspect the stalled review feedback, address or answer it, then continue with `sandman run --continue`"
	stalledResumeKey                = "stalled_resume"
	stalledReasonKey                = "stalled_reason"
	stallRemediationNextAction      = "inspect the current pull-request remediation evidence and continue in a fresh session"
)

// resumeSignature identifies what a lifecycle relaunch is asked to act on:
// the selected gate and reason, the live pull-request head, and (where
// relevant) the review request and identities of its responses. A failed CI
// repair is scoped to its check execution instead: review delivery or approval
// does not advance that work. CI rollup and merge state are excluded so a
// flapping check cannot make the same evidence look new.
func resumeSignature(extras map[string]any, liveHead string) string {
	gate, _ := extras["gate"].(string)
	reason, _ := extras["reason"].(string)
	head := strings.ToLower(strings.TrimSpace(liveHead))
	if head == "" {
		head, _ = extras["head_sha"].(string)
	}
	signature := map[string]any{"gate": gate, "reason": reason, "head": strings.ToLower(strings.TrimSpace(head))}
	if gate == "ci-failure" && reason == "CI_FAILURE" {
		if executionID := strings.TrimSpace(stringEvidence(extras, "ci_execution_id")); executionID != "" {
			signature["ci_execution_id"] = executionID
		}
	} else if request, ok := extras["review_request"]; ok {
		var decoded any
		if data, err := json.Marshal(request); err == nil && json.Unmarshal(data, &decoded) == nil {
			if object, ok := decoded.(map[string]any); ok {
				signature["trigger"] = object["trigger_id"]
			}
			ids := map[string]bool{}
			collectResponseIDs(decoded, ids)
			list := make([]string, 0, len(ids))
			for id := range ids {
				list = append(list, id)
			}
			sort.Strings(list)
			signature["responses"] = list
		}
	}
	if blind, ok := extras[blindReviewKey].(map[string]any); ok {
		signature["blind_response"] = blind["comment_id"]
		signature["blind_verdict"] = blind["verdict"]
	}
	data, _ := json.Marshal(signature)
	return string(data)
}

func collectResponseIDs(value any, ids map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if key == "id" {
				if id, ok := nested.(string); ok && strings.TrimSpace(id) != "" {
					ids[id] = true
				}
				continue
			}
			collectResponseIDs(nested, ids)
		}
	case []any:
		for _, nested := range typed {
			collectResponseIDs(nested, ids)
		}
	}
}

// stallsOnRepeat reports whether one unchanged relaunch already proves there
// is nothing to do. Review feedback the implementor addressed leaves a new
// head or a new request; a feedback relaunch that leaves neither found
// nothing to fix (the #2914 loop, where an approval read as feedback made the
// implementor ask for a formal review instead). Merge, CI and conflict work
// keep the documented in-session allowance, because those may need several
// attempts on the same head.
func stallsOnRepeat(extras map[string]any) bool {
	gate, _ := extras["gate"].(string)
	// Genuine requested changes keep their full relaunch allowance before
	// their terminal outcome.
	return gate == gateBlindReviewRequired || (gate == gateActionableFeedback && !formalChangesRequested(extras))
}

// formalChangesRequested reports genuine repair evidence: a formal
// CHANGES_REQUESTED review, a review daemon publication whose
// machine-readable verdict requests changes, or a daemon approval its own
// verdict sections contradict (an approval override, #2998). Only genuine
// requested changes keep a terminal outcome once the relaunch allowance cannot
// address them. The override is derived from the retained report; the
// approval_override flag next to it is evidence only.
func formalChangesRequested(extras map[string]any) bool {
	reason, _ := extras["reason"].(string)
	if reason == actionableFeedbackReason {
		return true
	}
	if reason != informalFeedbackReason {
		return false
	}
	for _, body := range retainedFeedbackBodies(extras) {
		if daemonRequestedChanges(body) {
			return true
		}
	}
	return false
}

// stalledAwaitExtras turns a resume the implementor cannot advance into a
// wait that remembers the stalled signature, so runtime observation does not
// relaunch on the same evidence. Earlier stalled signatures are kept so an
// oscillating decision stays bounded.
func stalledAwaitExtras(extras map[string]any, signature string, prior []string) map[string]any {
	wait := cloneLifecycleExtras(extras)
	wait["await"] = true
	wait["resume_stalled"] = true
	wait["stalled_reason"] = wait["reason"]
	wait["reason"] = resumeStalledReason
	wait["next_action"] = resumeStalledNextAction
	wait[stalledResumesKey] = appendStalledResume(prior, signature)
	return wait
}

// stalledTerminalEvidence renders the terminal failure of a relaunch that
// stopped on a terminal row of the outcome table, the one place that chooses
// its reason. A stalled informal-feedback relaunch ends as
// REVIEW_FEEDBACK_STALLED and records the signature that stalled; every other
// row ends as the exhausted remediation budget. The result carries no wait
// marker, so the Run finishes instead of waiting.
func stalledTerminalEvidence(row continuityRow, evidence map[string]any, signature string) map[string]any {
	if row.cause == causeStall && row.gate == gateActionableFeedback && row.reason == informalFeedbackReason {
		failure := classifiedLifecycleFailureEvidence(row.gate, evidence, reviewFeedbackStalledReason, reviewFeedbackStalledNextAction)
		failure[stalledResumeKey] = signature
		return failure
	}
	if next, owned := exhaustedWorkNextActions[row.gate]; owned && row.reason != actionableFeedbackReason {
		failure := remediationBudgetFailureEvidence(row.gate, evidence, next)
		failure[stalledReasonKey] = stringEvidence(evidence, "reason")
		failure[stalledResumeKey] = signature
		if blind, ok := evidence[blindReviewKey]; ok {
			failure[blindReviewKey] = blind
		}
		return failure
	}
	return remediationBudgetFailureEvidence(row.gate, evidence, stallRemediationNextAction)
}

// exhaustedWorkNextActions names, for each owned-work gate that ends the Run
// when it stalls or uses up its relaunch allowance (#3001), what the operator
// fixes before continuing. A gate absent from the map ends with the generic
// remediation next action.
var exhaustedWorkNextActions = map[string]string{
	"ci-failure":     "CI repair could not make the current-head checks pass: inspect them with `gh pr checks`, fix the failure, then continue with `sandman run --continue`",
	gateReadyToMerge: "merge work could not finish: inspect why the approved current head is not merged (required checks, mergeability, branch protection), fix it, then continue with `sandman run --continue`",
}

func appendStalledResume(prior []string, signature string) []any {
	list := make([]any, 0, len(prior)+1)
	for _, existing := range prior {
		if existing != signature {
			list = append(list, existing)
		}
	}
	list = append(list, signature)
	if len(list) > maxStalledResumes {
		list = list[len(list)-maxStalledResumes:]
	}
	return list
}

// stalledResumes reads the stalled signatures carried by an await payload.
func stalledResumes(payload map[string]any) []string {
	var list []string
	switch typed := payload[stalledResumesKey].(type) {
	case []any:
		for _, value := range typed {
			if signature, ok := value.(string); ok {
				list = append(list, signature)
			}
		}
	case []string:
		list = append(list, typed...)
	}
	return list
}

// latestStalledResumes folds the Run's events and returns the stalled
// signatures of its current wait. A read failure is returned so the caller
// fails closed: it keeps waiting rather than relaunching on evidence that may
// already have stalled.
func latestStalledResumes(log events.EventLog, runID string) ([]string, error) {
	if log == nil || strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	states, err := events.ReadRunStates(log)
	if err != nil {
		return nil, err
	}
	state, ok := states[runID]
	if !ok || state.AwaitEvent == nil {
		return nil, nil
	}
	return stalledResumes(state.AwaitEvent.Payload), nil
}

// repairRound identifies the relaunch allowance in use, independently of
// resumeSignature. A new request can change the feedback evidence, but only
// a new head starts a fresh feedback allowance. Other gates use request and head.
func repairRound(evidence map[string]any, liveHead string) string {
	head := strings.ToLower(strings.TrimSpace(liveHead))
	if head == "" {
		head = strings.ToLower(strings.TrimSpace(stringEvidence(evidence, "head_sha")))
	}
	if stringEvidence(evidence, "gate") == gateActionableFeedback {
		return head
	}
	var trigger any
	if request, ok := evidence["review_request"]; ok {
		if data, err := json.Marshal(request); err == nil {
			var object map[string]any
			if json.Unmarshal(data, &object) == nil {
				trigger = object["trigger_id"]
			}
		}
	}
	return fmt.Sprintf("%s|%v", head, trigger)
}

// reviewFeedbackAllowance is shared by the active row's sequential executor
// entries and observations. It is not restored from persisted evidence.
type reviewFeedbackAllowance struct {
	usedByHead  map[string]int
	hasLaunched bool
}

func (a *reviewFeedbackAllowance) usedFor(head string) int {
	if a == nil {
		return 0
	}
	return a.usedByHead[head]
}

func (a *reviewFeedbackAllowance) charge(head string) {
	if a.usedByHead == nil {
		a.usedByHead = make(map[string]int)
	}
	a.usedByHead[head]++
}

// continuityHoldPayload reports a wait entered by the AFK-continuity rules
// (a stalled relaunch or a continuity hold). Such a wait has no operation
// deadline: its recovery identity is the Run itself.
func continuityHoldPayload(payload map[string]any) bool {
	stalled, _ := payload["resume_stalled"].(bool)
	hold, _ := payload["continuity_hold"].(bool)
	return stalled || hold
}

func continuityHoldOperation(runID string) string {
	return "hold:" + runID
}

// ownedWorkHold is the continuity hold for owned work nothing can advance. It
// remembers the owned-work signature so observation does not relaunch the
// implementor on the same evidence.
func (s *runSession) ownedWorkHold(extras map[string]any, signature string, stalled []string) map[string]any {
	external, _ := extras["external_gate"].(string)
	hold := continuityHold(extras, external)
	hold[stalledResumesKey] = appendStalledResume(stalled, signature)
	return hold
}

func containsSignature(list []string, signature string) bool {
	for _, existing := range list {
		if resumeSignaturesEquivalent(existing, signature) {
			return true
		}
	}
	return false
}

// resumeSignaturesEquivalent preserves historical stalled signatures while
// comparing failed-CI work by its actual identity. Older signatures included
// review trigger/response identities; those fields never advance CI repair.
// If an older signature has no CI execution identity, retain its hold
// conservatively because there is no baseline from which to prove progress.
func resumeSignaturesEquivalent(left, right string) bool {
	if left == right {
		return true
	}
	var a, b struct {
		Gate          string `json:"gate"`
		Reason        string `json:"reason"`
		Head          string `json:"head"`
		CIExecutionID string `json:"ci_execution_id"`
	}
	if json.Unmarshal([]byte(left), &a) != nil || json.Unmarshal([]byte(right), &b) != nil {
		return false
	}
	if a.Gate != "ci-failure" || b.Gate != "ci-failure" || a.Reason != "CI_FAILURE" || b.Reason != "CI_FAILURE" {
		return false
	}
	if a.Head != b.Head {
		return false
	}
	return a.CIExecutionID == "" || b.CIExecutionID == "" || a.CIExecutionID == b.CIExecutionID
}

// lifecycleFailureReasons is the failure-reason view of the continuity
// outcome table: reason -> permanent. A permanent reason may end a Run whose
// pull request is open; a recoverable one holds instead.
var lifecycleFailureReasons = continuityOutcomes.failureReasons()

// lifecycleFailureRecoverable reports whether a lifecycle failure reason
// holds instead of terminating a Run whose pull request is open at its
// current head. Unknown reasons are permanent: a new terminal reason must be
// classified in the outcome table, and the continuity contract enforces that.
func lifecycleFailureRecoverable(reason string) bool {
	return continuityOutcomes.outcome(continuityRow{reason: reason, cause: causeFailure}) != outcomeTerminal
}

// failureContinuity applies the classified failure outcome only when the
// failure belongs to the current open pull-request head. Completion failures,
// closed/stale pull requests and unknown reasons remain terminal.
func failureContinuity(table continuityTable, decision lifecycleDecision, failure map[string]any, pr *github.PR, head string) (lifecycleVerdict, bool) {
	if decision.action != lifecycleFailure || decision.completionFailure ||
		pr == nil || !strings.EqualFold(strings.TrimSpace(pr.State), "open") ||
		strings.TrimSpace(head) == "" ||
		!strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(head)) {
		return lifecycleVerdict{}, false
	}
	cause := decision.cause
	if cause == "" {
		cause = causeFailure
	}
	row := continuityRow{reason: stringEvidence(failure, "reason"), cause: cause}
	if cause != causeFailure {
		row.gate = string(decision.gate)
	}
	if table.outcome(row) != outcomeHold {
		return lifecycleVerdict{}, false
	}
	return lifecycleVerdict{
		Action:   lifecycleAwait,
		Evidence: continuityHold(failure, string(decision.gate)),
	}, true
}

// idleGateAwaitsReview reports an idle-gate owned-work outcome whose only
// missing evidence is a review nobody has requested through Sandman: no
// delegated review command applies and no formal CHANGES_REQUESTED review is
// outstanding. The pull request's current head must be known.
func idleGateAwaitsReview(extras map[string]any) bool {
	reason, _ := extras["reason"].(string)
	decision, _ := extras["review_decision"].(string)
	head, _ := extras["expected_head_sha"].(string)
	return reason == idleGateReason && strings.TrimSpace(head) != "" &&
		!strings.EqualFold(strings.TrimSpace(decision), "CHANGES_REQUESTED")
}

// continuityHold turns a recoverable lifecycle failure into a wait. The
// extras keep the failure's evidence; the wait carries its gate.
func continuityHold(failure map[string]any, gate string) map[string]any {
	wait := cloneLifecycleExtras(failure)
	if wait == nil {
		wait = map[string]any{}
	}
	if gate == "" {
		gate, _ = wait["external_gate"].(string)
	}
	if gate == "" {
		gate = string(lifecycleGatePending)
	}
	wait["gate"] = gate
	wait["await"] = true
	wait["continuity_hold"] = true
	return wait
}
