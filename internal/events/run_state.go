package events

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// RunState projects a run's lifecycle from the append-only event log.
type RunState struct {
	RunID    string
	Started  Event
	Finished *Event
}

// ProjectRunStates folds events into run states keyed by run ID.
func ProjectRunStates(events []Event) []RunState {
	states := make(map[string]*RunState)
	order := make([]string, 0, len(events))

	getOrCreate := func(runID string) *RunState {
		if state, ok := states[runID]; ok {
			return state
		}
		state := &RunState{RunID: runID}
		states[runID] = state
		order = append(order, runID)
		return state
	}

	for _, event := range events {
		if event.RunID == "" {
			continue
		}
		switch event.Type {
		case "run.started", "run.continued":
			state := getOrCreate(event.RunID)
			state.Started = event
		case "run.blocked":
			state := getOrCreate(event.RunID)
			state.Started = event
			finished := event
			state.Finished = &finished
		case "run.queued":
			state := getOrCreate(event.RunID)
			state.Started = event
			finished := event
			state.Finished = &finished
		case "run.finished", "run.aborted", "run.cancelled":
			state := getOrCreate(event.RunID)
			finished := event
			state.Finished = &finished
		}
	}

	runs := make([]RunState, 0, len(order))
	for _, runID := range order {
		runs = append(runs, *states[runID])
	}
	return runs
}

// IssueLabel returns the human-facing issue label for the run.
func (r RunState) IssueLabel() string {
	if r.IsReview() && r.RunID != "" {
		return r.RunID
	}
	if r.IsPromptOnly() {
		return "prompt-only"
	}
	return fmt.Sprintf("#%d", r.IssueNumber())
}

// IssueNumber returns the GitHub issue number, if any.
func (r RunState) IssueNumber() int {
	if r.Started.IssueRef != nil {
		return *r.Started.IssueRef
	}
	if r.Finished != nil && r.Finished.IssueRef != nil {
		return *r.Finished.IssueRef
	}
	if r.Started.Issue != 0 {
		return r.Started.Issue
	}
	if r.Finished != nil {
		return r.Finished.Issue
	}
	return 0
}

// IsPromptOnly reports whether the run was started without issue data.
func (r RunState) IsPromptOnly() bool {
	return r.IssueNumber() == 0 && r.Started.IssueRef == nil && (r.Finished == nil || r.Finished.IssueRef == nil)
}

// IsReview reports whether the run was tagged as a review-agent run. The
// orchestrator sets payload["review"] = true on the run.started (and
// run.finished) event when the batch was issued by `sandman review`
// (one-shot or daemon). Implementation runs leave the key absent.
func (r RunState) IsReview() bool {
	if flag, ok := payloadBool(r.Started.Payload, "review"); ok && flag {
		return true
	}
	if r.Finished != nil {
		if flag, ok := payloadBool(r.Finished.Payload, "review"); ok && flag {
			return true
		}
	}
	return false
}

// IsActive reports whether the run has not finished yet.
func (r RunState) IsActive() bool {
	return r.Finished == nil
}

// Status returns the terminal status from the finished event.
func (r RunState) Status() string {
	return runStatusFromFinished(r.Finished).String()
}

// runStatusFromFinished maps a finished event to the corresponding
// RunStatus, preserving the exact input→string contract the portal
// and orchestrator rely on. An unfinished run maps to RunStatusZero
// (String() == ""); a run.blocked event maps to RunStatusBlocked; a
// run.queued event to RunStatusQueued; run.aborted and legacy
// run.cancelled events to RunStatusAborted; any other type (typically
// run.finished) reads the payload's status field verbatim, mapping
// named strings to their named constants and unknown strings to
// RunStatusUnknown (which String()-round-trips the raw value).
func runStatusFromFinished(finished *Event) RunStatus {
	if finished == nil {
		return RunStatusZero
	}
	switch finished.Type {
	case "run.blocked":
		return RunStatusBlocked
	case "run.queued":
		return RunStatusQueued
	case "run.aborted", "run.cancelled":
		return RunStatusAborted
	}
	status, _ := finished.Payload["status"].(string)
	return RunStatusFromPayload(status)
}

// Branch returns the run branch from the first event that recorded one.
func (r RunState) Branch() string {
	if branch, ok := payloadString(r.Started.Payload, "branch"); ok && branch != "" {
		return branch
	}
	if r.Finished != nil {
		if branch, ok := payloadString(r.Finished.Payload, "branch"); ok && branch != "" {
			return branch
		}
	}
	return ""
}

// Duration returns the elapsed time between start and finish.
func (r RunState) Duration() time.Duration {
	if r.Finished == nil || r.Started.Timestamp.IsZero() || r.Finished.Timestamp.IsZero() {
		return 0
	}
	return r.Finished.Timestamp.Sub(r.Started.Timestamp).Round(time.Second)
}

// RetriesTotal returns the configured retry count recorded on the terminal
// run.finished (or run.aborted) payload, or 0 when the run has not finished,
// the payload predates the run-idle-timeout enrichment, or the value
// cannot be coerced to an integer.
func (r RunState) RetriesTotal() int {
	if r.Finished == nil {
		return 0
	}
	v, _ := payloadInt(r.Finished.Payload, "retries_total")
	return v
}

// RetriesDone returns the retry iterations actually executed, recorded on
// the terminal run.finished (or run.aborted) payload, or 0 when the run has
// not finished, the payload predates the run-idle-timeout enrichment, or
// the value cannot be coerced to an integer.
func (r RunState) RetriesDone() int {
	if r.Finished == nil {
		return 0
	}
	v, _ := payloadInt(r.Finished.Payload, "retries_done")
	return v
}

func payloadString(payload map[string]any, key string) (string, bool) {
	if payload == nil {
		return "", false
	}
	v, ok := payload[key]
	if !ok {
		return "", false
	}
	str, ok := v.(string)
	return str, ok
}

func payloadBool(payload map[string]any, key string) (bool, bool) {
	if payload == nil {
		return false, false
	}
	v, ok := payload[key]
	if !ok {
		return false, false
	}
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		return b == "true", true
	}
	return false, false
}

func payloadInt(payload map[string]any, key string) (int, bool) {
	if payload == nil {
		return 0, false
	}
	v, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		if n > int64(math.MaxInt) || n < int64(math.MinInt) {
			return 0, false
		}
		return int(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		if n != math.Trunc(n) {
			return 0, false
		}
		if n > float64(math.MaxInt) || n < float64(math.MinInt) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		if i > int64(math.MaxInt) || i < int64(math.MinInt) {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}
