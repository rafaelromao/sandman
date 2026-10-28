package batch

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/reviewpub"
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
)

// resumeSignature identifies what a lifecycle relaunch is asked to act on:
// the selected gate and reason, the live pull-request head, the review
// request and the identities of its responses. CI rollup and merge state are
// excluded so a flapping check cannot make the same evidence look new.
func resumeSignature(extras map[string]any, liveHead string) string {
	gate, _ := extras["gate"].(string)
	reason, _ := extras["reason"].(string)
	head := strings.ToLower(strings.TrimSpace(liveHead))
	if head == "" {
		head, _ = extras["head_sha"].(string)
	}
	signature := map[string]any{"gate": gate, "reason": reason, "head": strings.ToLower(strings.TrimSpace(head))}
	if request, ok := extras["review_request"]; ok {
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
	return gate == gateActionableFeedback && !formalChangesRequested(extras)
}

// formalChangesRequested reports genuine repair evidence: a formal
// CHANGES_REQUESTED review, or a review daemon publication whose
// machine-readable verdict requests changes. Only genuine requested changes
// keep a terminal outcome once the relaunch allowance cannot address them.
func formalChangesRequested(extras map[string]any) bool {
	reason, _ := extras["reason"].(string)
	if reason == actionableFeedbackReason {
		return true
	}
	if reason != informalFeedbackReason {
		return false
	}
	request, ok := extras["review_request"]
	if !ok {
		return false
	}
	data, err := json.Marshal(request)
	if err != nil {
		return false
	}
	var decoded struct {
		InformalFeedback []struct {
			Body string `json:"body"`
		} `json:"informal_feedback"`
	}
	if json.Unmarshal(data, &decoded) != nil {
		return false
	}
	for _, feedback := range decoded.InformalFeedback {
		if verdict, published := reviewpub.PublishedVerdict(feedback.Body); published && verdict == reviewpub.ChangesRequested {
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

// repairRound identifies the request and head a resume signature acts on.
func repairRound(signature string) string {
	var decoded struct {
		Head    string `json:"head"`
		Trigger any    `json:"trigger"`
	}
	if json.Unmarshal([]byte(signature), &decoded) != nil {
		return signature
	}
	return fmt.Sprintf("%s|%v", decoded.Head, decoded.Trigger)
}

// exhaustionTerminal reports resume reasons whose exhausted remediation is a
// permanent failure rather than a wait: unusable CI wait state has no external
// operation that could resolve it.
func exhaustionTerminal(extras map[string]any) bool {
	reason, _ := extras["reason"].(string)
	return reason == "CI_WAIT_STATE_ERROR"
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
		if existing == signature {
			return true
		}
	}
	return false
}

// Lifecycle failure reasons, classified for the AFK-continuity guard. A
// permanent reason may end a Run whose pull request is open; a recoverable
// one waits for external recovery instead.
var lifecycleFailureReasons = map[string]bool{
	"PULL_REQUEST_CLOSED":                 true,  // closed without merge
	"PULL_REQUEST_MISSING":                true,  // the implementor never published
	"REVIEW_REQUEST_REQUIRED":             true,  // no review request was delivered
	"merged-pr-missing-closing-reference": true,  // merged without verifiable completion
	"CI_WAIT_STATE_ERROR":                 true,  // corrupt CI wait state
	stateErrorReason:                      true,  // corrupt review state, nothing resolving
	lookupGateReason:                      true,  // the pull request was not observed
	"IMPLEMENTOR_ACTION_REQUIRED":         true,  // legacy verification fallback
	"REMEDIATION_BUDGET_EXHAUSTED":        true,  // formal CHANGES_REQUESTED left unrepaired
	idleGateReason:                        false, // nothing resolving yet; waits like rc2
	reviewTimeoutReason:                   false, // expired request; waits like rc2
	"CI_WAIT_TIMEOUT":                     false, // bounded CI wait expired; waits like rc2
}

// lifecycleFailureRecoverable reports whether a lifecycle failure reason
// must wait instead of terminating a Run whose pull request is open at its
// current head. Unknown reasons are permanent: a new terminal reason must be
// classified here, and the continuity contract test enforces that.
func lifecycleFailureRecoverable(reason string) bool {
	permanent, known := lifecycleFailureReasons[reason]
	return known && !permanent
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
