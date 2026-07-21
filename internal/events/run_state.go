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
	// Retries holds every run.retry event emitted against this run, in
	// input (events.jsonl) order. It is the projection's view of the
	// retry timeline; state-level helpers (LiveAttempt,
	// LastRetryReason) and any future consumer (e.g. the active-row
	// chip) read it directly so both code paths agree. The slice is
	// append-only — run.started and run.continued reset Finished but
	// never reset Retries — because the orchestrator emits a
	// run.retry only after the matching run.started (see ADR-0035
	// for the projection rule).
	Retries []Event
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
		state := getOrCreate(event.RunID)
		switch event.Type {
		case "run.started", "run.continued":
			state.Started = event
			state.Finished = nil
		case "run.blocked":
			state.Started = event
			finished := event
			state.Finished = &finished
		case "run.queued":
			state.Started = event
			finished := event
			state.Finished = &finished
		case "run.finished", "run.aborted", "run.cancelled":
			finished := event
			state.Finished = &finished
		case "run.retry":
			state.Retries = append(state.Retries, event)
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
// (the review daemon). Implementation runs leave the key absent.
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

// RunKind returns the taxonomy tag for the run as a string. It is the
// canonical reader for the run's kind ("review", "prompt-only", or
// "issue"). It mirrors the IsReview / IsPromptOnly predicates so each
// branch matches the corresponding helper.
func (r RunState) RunKind() string {
	if r.IsReview() {
		return "review"
	}
	if r.IsPromptOnly() {
		return "prompt-only"
	}
	return "issue"
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
// run.queued event to RunStatusQueued; run.aborted, plus legacy
// run.cancelled for back-compat, to RunStatusAborted; any other type (typically
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

// BatchID returns the batch identifier from the started event payload.
func (r RunState) BatchID() string {
	if id, ok := payloadString(r.Started.Payload, "batch_id"); ok && id != "" {
		return id
	}
	if r.Finished != nil {
		if id, ok := payloadString(r.Finished.Payload, "batch_id"); ok && id != "" {
			return id
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

// RetriesTotal returns the configured retry count from the finished payload.
func (r RunState) RetriesTotal() int {
	if r.Finished == nil {
		return 0
	}
	v, _ := payloadInt(r.Finished.Payload, "retries_total")
	return v
}

// RetriesDone returns the count of retries actually executed by the run
// (initial run excluded), read from the `retries_done` key of the
// finished-run payload (run.finished / run.aborted). At terminal time
// it matches the active-row `LiveAttempt` value for the same run, so
// the active row and the finished row render the same number for the
// same run — `emitTerminal` in `internal/batch/orchestrator.go` writes
// `retries_done` as `AgentRunResult.RetriesTotal - 1`, while
// `LiveAttempt` returns the equivalent value computed from the live
// `run.retry` stream. Returns 0 when the run has not finished yet, or
// when the finished payload omits the key (legacy shapes).
func (r RunState) RetriesDone() int {
	if r.Finished == nil {
		return 0
	}
	v, _ := payloadInt(r.Finished.Payload, "retries_done")
	return v
}

// LiveAttempt returns the count of retries that have actually occurred
// (initial run excluded), reading from the projected Retries slice so
// the answer is meaningful before `run.finished` is written. The
// orchestrator's `run.retry` payload carries `attempt` as the
// about-to-start 1-indexed attempt number, where the loop counter
// starts at 0 for the initial run — so the i-th retry event writes
// `attempt: i+1`. This helper therefore walks the slice and reports
// the maximum `attempt - 1`, with each candidate clamped at 0 so a
// malformed payload (`attempt: 0` or any non-positive value) cannot
// drag the running best below 0 and cannot produce a negative retry
// count. At terminal time it matches `RetriesDone` for the same run,
// so the active row and the finished row render the same number for
// the same run. Returns 0 when no retries exist.
func (r RunState) LiveAttempt() int {
	best := 0
	for _, event := range r.Retries {
		v, ok := payloadInt(event.Payload, "attempt")
		if !ok {
			continue
		}
		retries := v - 1
		if retries < 0 {
			retries = 0
		}
		if retries > best {
			best = retries
		}
	}
	return best
}

// LastRetryReason returns the `reason` of the most recent `run.retry`
// event. "Most recent" means the event with the largest Timestamp; ties
// are broken by last-encountered in the events.jsonl file order (which
// is the order in ProjectRunStates saw the events, and the order the
// Retries slice is built in). Returns "" when no retries exist, or when
// the most recent retry payload omits the `reason` key (the current
// orchestrator shape; this is intentional — the helper is reader-side,
// and the writer-side fix is a separate concern).
func (r RunState) LastRetryReason() string {
	var latest *Event
	for i := range r.Retries {
		event := r.Retries[i]
		if latest == nil || !event.Timestamp.Before(latest.Timestamp) {
			copy := event
			latest = &copy
		}
	}
	if latest == nil {
		return ""
	}
	if latest.Payload == nil {
		return ""
	}
	v, _ := latest.Payload["reason"].(string)
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
