package cmd

import "testing"

// TestPortalRunsView_ReasonFromStartedPayload_NoAutoMarkerForIssue pins
// issue #1918 slice 2: the post-selection phase is a normal issue
// batch, so the portal's reasonFromStartedPayload MUST return "" (no
// auto marker) for an issue run.started payload. Only run.started
// payloads with run_kind == "auto-select" (the auto-select selector)
// or review == true (review runs) should produce a non-empty reason.
func TestPortalRunsView_ReasonFromStartedPayload_NoAutoMarkerForIssue(t *testing.T) {
	v := &portalRunsView{}

	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name:    "issue single",
			payload: map[string]any{"run_kind": "issue", "batch_id": "260618113825-abcd-42", "issues": []int{42}},
			want:    "",
		},
		{
			name:    "issue multi",
			payload: map[string]any{"run_kind": "issue", "batch_id": "260618113825-abcd-42+1", "issues": []int{42, 43}},
			want:    "",
		},
		{
			name:    "auto-select",
			payload: map[string]any{"run_kind": "auto-select", "batch_id": "260618113825-abcd-auto-5"},
			want:    "auto-select",
		},
		{
			name:    "review",
			payload: map[string]any{"review": true, "pr_number": 42, "batch_id": "260618113825-abcd-PR42"},
			want:    "review",
		},
		{
			name:    "nil payload",
			payload: nil,
			want:    "",
		},
		{
			name:    "empty payload",
			payload: map[string]any{},
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := v.reasonFromStartedPayload(tc.payload)
			if got != tc.want {
				t.Errorf("reasonFromStartedPayload(%v) = %q, want %q", tc.payload, got, tc.want)
			}
		})
	}
}

// TestPortalRunsView_StatusOrDefault_ActiveIssueIsNotAutoSelecting
// pins that an active issue batch (post-selection or otherwise) is
// not auto-select. Only an active run whose reason resolves to
// "auto-select" should map to "auto-select".
func TestPortalRunsView_StatusOrDefault_ActiveIssueIsNotAutoSelecting(t *testing.T) {
	v := &portalRunsView{}

	cases := []struct {
		name     string
		status   string
		active   bool
		isReview bool
		isAuto   bool
		want     string
	}{
		{name: "active issue is running", status: "", active: true, isReview: false, isAuto: false, want: "running"},
		{name: "active review is reviewing", status: "", active: true, isReview: true, isAuto: false, want: "reviewing"},
		{name: "active auto-select is auto-select", status: "", active: true, isReview: false, isAuto: true, want: "auto-select"},
		{name: "completed issue", status: "success", active: false, isReview: false, isAuto: false, want: "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := v.statusOrDefault(tc.status, tc.active, tc.isReview, tc.isAuto)
			if got != tc.want {
				t.Errorf("statusOrDefault(status=%q, active=%v, isReview=%v, isAuto=%v) = %q, want %q",
					tc.status, tc.active, tc.isReview, tc.isAuto, got, tc.want)
			}
		})
	}
}
