package reviewquota

import (
	"testing"
	"time"
)

func TestClockChargesOnlyTimeOutsideRecordedQuotaPauses(t *testing.T) {
	start := time.Unix(1000, 0)
	at := func(minutes int) time.Time { return start.Add(time.Duration(minutes) * time.Minute) }
	original := at(30)
	pause := func(from, to int) Pause { return Pause{StartedAt: at(from), RecoveredAt: at(to)} }
	for _, tc := range []struct {
		name   string
		state  State
		want   int
		paused bool
	}{
		{name: "legacy pause with no start", state: State{Paused: true}, want: 30},
		{name: "recorded pause still open after original deadline", state: State{Paused: true, StartedAt: at(12)}, want: 30, paused: true},
		{name: "request confirmed during pause", state: State{CompletedPauses: []Pause{pause(-10, 12)}}, want: 42},
		{name: "zero overlap", state: State{CompletedPauses: []Pause{pause(-10, 0)}}, want: 30},
		{name: "two pauses by effective rather than original deadline", state: State{CompletedPauses: []Pause{pause(12, 40), pause(45, 75)}}, want: 88},
		{name: "second pause after adjusted expiry", state: State{CompletedPauses: []Pause{pause(12, 40), pause(58, 75)}}, want: 58},
		{name: "open pause after adjusted expiry", state: State{Paused: true, StartedAt: at(58), CompletedPauses: []Pause{pause(12, 40)}}, want: 58},
		{name: "overlapping evidence cannot extend budget", state: State{CompletedPauses: []Pause{pause(12, 40), pause(38, 45)}}, want: 30},
		{name: "recovery committed during read still looks paused at observation instant", state: State{CompletedPauses: []Pause{pause(12, 110)}}, want: 30, paused: true},
		{name: "future pause does not erase prior credit", state: State{CompletedPauses: []Pause{pause(12, 90), pause(105, 120)}}, want: 108},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, paused := tc.state.Clock(start, original, at(100))
			if !got.Equal(at(tc.want)) || paused != tc.paused {
				t.Fatalf("clock = (%v, paused=%t), want (%v, paused=%t)", got, paused, at(tc.want), tc.paused)
			}
		})
	}
}
