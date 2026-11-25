// Package reviewquota shares the repository's reviewer quota evidence between
// the review daemon (the only writer) and managed review observers.
package reviewquota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

func Path(reviewsDir string) string {
	return filepath.Join(reviewsDir, "quota-pause.json")
}

type State struct {
	Paused          bool      `json:"paused"`
	PausedUntil     time.Time `json:"paused_until,omitempty"`
	LastProbeAt     time.Time `json:"last_probe_at,omitempty"`
	StartedAt       time.Time `json:"started_at,omitzero"`
	CompletedPauses []Pause   `json:"completed_pauses,omitempty"`
}

type Pause struct {
	StartedAt   time.Time `json:"started_at"`
	RecoveredAt time.Time `json:"recovered_at"`
}

func Read(reviewsDir string) (State, error) {
	data, err := os.ReadFile(Path(reviewsDir))
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	return state, nil
}

// Clock computes the request's deadline from durable pauses in chronological
// order. Its deadline is suspended only if the request still has time left
// when the current pause overlaps it. Never grant a request time that had
// already expired, including across consecutive pauses or a restart.
func (s State) Clock(started, deadline, now time.Time) (adjusted time.Time, paused bool) {
	if started.IsZero() || !started.Before(deadline) {
		return deadline, false
	}
	var previousRecovery time.Time
	for _, p := range s.CompletedPauses {
		if p.StartedAt.IsZero() || !p.StartedAt.Before(p.RecoveredAt) || p.StartedAt.Before(previousRecovery) {
			// A damaged or unordered history isn't evidence of extra time.
			return deadline, false
		}
		previousRecovery = p.RecoveredAt
	}
	adjusted = deadline
	for _, p := range s.CompletedPauses {
		from := later(p.StartedAt, started)
		if from.Before(adjusted) && from.Before(p.RecoveredAt) {
			if p.RecoveredAt.After(now) {
				// A probe may commit between reading the clock and reading
				// the file. At the observation instant this pause was open.
				return adjusted, !from.After(now)
			}
			adjusted = adjusted.Add(p.RecoveredAt.Sub(from))
		}
	}
	if s.Paused && !s.StartedAt.IsZero() && !s.StartedAt.Before(previousRecovery) {
		from := later(s.StartedAt, started)
		paused = from.Before(adjusted) && !from.After(now)
	}
	return adjusted, paused
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
