package events

import (
	"encoding/json"
	"time"
)

// Event is a single structured entry in the append-only JSONL event log.
type Event struct {
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	RunID     string         `json:"run_id,omitempty"`
	Issue     int            `json:"-"`
	IssueRef  *int           `json:"-"`
	Payload   map[string]any `json:"payload,omitempty"`
}

func (e Event) MarshalJSON() ([]byte, error) {
	type alias struct {
		Type      string         `json:"type"`
		Timestamp time.Time      `json:"timestamp"`
		RunID     string         `json:"run_id,omitempty"`
		Issue     any            `json:"issue"`
		Payload   map[string]any `json:"payload,omitempty"`
	}

	var issue any
	switch {
	case e.IssueRef != nil:
		issue = e.IssueRef
	case e.Issue != 0:
		issue = e.Issue
	default:
		issue = nil
	}

	return json.Marshal(alias{
		Type:      e.Type,
		Timestamp: e.Timestamp,
		RunID:     e.RunID,
		Issue:     issue,
		Payload:   e.Payload,
	})
}

func (e *Event) UnmarshalJSON(data []byte) error {
	type alias struct {
		Type      string         `json:"type"`
		Timestamp time.Time      `json:"timestamp"`
		RunID     string         `json:"run_id,omitempty"`
		Issue     *int           `json:"issue"`
		Payload   map[string]any `json:"payload,omitempty"`
	}

	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	e.Type = decoded.Type
	e.Timestamp = decoded.Timestamp
	e.RunID = decoded.RunID
	e.Payload = decoded.Payload
	e.IssueRef = decoded.Issue
	if decoded.Issue != nil {
		e.Issue = *decoded.Issue
	} else {
		e.Issue = 0
	}
	return nil
}

// EventLog is the seam for writing and reading events.
type EventLog interface {
	Log(event Event) error
	Read() ([]Event, error)
}
