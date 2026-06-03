package events

import (
	"fmt"
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
		case "run.finished", "run.cancelled":
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

// IsActive reports whether the run has not finished yet.
func (r RunState) IsActive() bool {
	return r.Finished == nil
}

// Status returns the terminal status from the finished event.
func (r RunState) Status() string {
	if r.Finished == nil {
		return ""
	}
	if r.Finished.Type == "run.blocked" {
		return "blocked"
	}
	if r.Finished.Type == "run.cancelled" {
		return "failure"
	}
	if r.Finished.Type == "run.queued" {
		return "queued"
	}
	status, _ := r.Finished.Payload["status"].(string)
	if status == "" && r.Finished.Type == "run.cancelled" {
		return "failure"
	}
	return status
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
