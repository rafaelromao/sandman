package cmd

import "testing"

func TestPortalBlockedMessage_DistinguishesExternalGate(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name:    "pending",
			payload: map[string]any{"blocker": "external-gate", "gate": "pending"},
			want:    "Blocked while waiting for the external CI/review gate.",
		},
		{
			name:    "failed",
			payload: map[string]any{"blocker": "external-gate", "gate": "failed"},
			want:    "Blocked by a failed external gate.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&portalRunsView{}).portalBlockedMessage(tt.payload); got != tt.want {
				t.Fatalf("portalBlockedMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}
