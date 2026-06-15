package events

import "testing"

func TestRunStatus(t *testing.T) {
	t.Run("String returns lower-case name for each named constant", func(t *testing.T) {
		cases := []struct {
			status RunStatus
			want   string
		}{
			{RunStatusSuccess, "success"},
			{RunStatusFailure, "failure"},
			{RunStatusAborted, "aborted"},
			{RunStatusBlocked, "blocked"},
			{RunStatusQueued, "queued"},
		}
		for _, tc := range cases {
			if got := tc.status.String(); got != tc.want {
				t.Errorf("RunStatus(%v).String() = %q, want %q", tc.status, got, tc.want)
			}
		}
	})

	t.Run("Zero String returns empty string", func(t *testing.T) {
		if got := RunStatusZero.String(); got != "" {
			t.Errorf("RunStatusZero.String() = %q, want empty string", got)
		}
	})

	t.Run("Unknown String returns the carried raw string", func(t *testing.T) {
		s := RunStatusFromPayload("anything-else")
		if got := s.String(); got != "anything-else" {
			t.Errorf("RunStatusFromPayload(%q).String() = %q, want %q", "anything-else", got, "anything-else")
		}
	})

	t.Run("IsTerminal is true for terminal statuses and false for the rest", func(t *testing.T) {
		cases := []struct {
			status RunStatus
			want   bool
		}{
			{RunStatusZero, false},
			{RunStatusSuccess, true},
			{RunStatusFailure, true},
			{RunStatusAborted, true},
			{RunStatusBlocked, true},
			{RunStatusQueued, false},
			{RunStatusUnknown, false},
		}
		for _, tc := range cases {
			if got := tc.status.IsTerminal(); got != tc.want {
				t.Errorf("RunStatus(%v).IsTerminal() = %v, want %v", tc.status, got, tc.want)
			}
		}
	})

	t.Run("IsSuccess is true only for Success", func(t *testing.T) {
		cases := []struct {
			status RunStatus
			want   bool
		}{
			{RunStatusZero, false},
			{RunStatusSuccess, true},
			{RunStatusFailure, false},
			{RunStatusAborted, false},
			{RunStatusBlocked, false},
			{RunStatusQueued, false},
			{RunStatusUnknown, false},
		}
		for _, tc := range cases {
			if got := tc.status.IsSuccess(); got != tc.want {
				t.Errorf("RunStatus(%v).IsSuccess() = %v, want %v", tc.status, got, tc.want)
			}
		}
	})

	t.Run("IsFailure is true only for Failure", func(t *testing.T) {
		cases := []struct {
			status RunStatus
			want   bool
		}{
			{RunStatusZero, false},
			{RunStatusSuccess, false},
			{RunStatusFailure, true},
			{RunStatusAborted, false},
			{RunStatusBlocked, false},
			{RunStatusQueued, false},
			{RunStatusUnknown, false},
		}
		for _, tc := range cases {
			if got := tc.status.IsFailure(); got != tc.want {
				t.Errorf("RunStatus(%v).IsFailure() = %v, want %v", tc.status, got, tc.want)
			}
		}
	})

	t.Run("IsAborted is true only for Aborted", func(t *testing.T) {
		cases := []struct {
			status RunStatus
			want   bool
		}{
			{RunStatusZero, false},
			{RunStatusSuccess, false},
			{RunStatusFailure, false},
			{RunStatusAborted, true},
			{RunStatusBlocked, false},
			{RunStatusQueued, false},
			{RunStatusUnknown, false},
		}
		for _, tc := range cases {
			if got := tc.status.IsAborted(); got != tc.want {
				t.Errorf("RunStatus(%v).IsAborted() = %v, want %v", tc.status, got, tc.want)
			}
		}
	})

	t.Run("RunStatusFromPayload maps named strings to named constants", func(t *testing.T) {
		cases := []struct {
			in   string
			want RunStatus
		}{
			{"success", RunStatusSuccess},
			{"failure", RunStatusFailure},
			{"aborted", RunStatusAborted},
			{"blocked", RunStatusBlocked},
			{"queued", RunStatusQueued},
		}
		for _, tc := range cases {
			if got := RunStatusFromPayload(tc.in); got != tc.want {
				t.Errorf("RunStatusFromPayload(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	})

	t.Run("RunStatusFromPayload maps unknown strings to Unknown and round-trips", func(t *testing.T) {
		cases := []string{"timeout", "weird", "running", ""}
		for _, in := range cases {
			got := RunStatusFromPayload(in)
			if got.code != runStatusCodeUnknown {
				t.Errorf("RunStatusFromPayload(%q) code = %v, want Unknown", in, got.code)
			}
			if got.String() != in {
				t.Errorf("RunStatusFromPayload(%q).String() = %q, want %q", in, got.String(), in)
			}
		}
	})
}
