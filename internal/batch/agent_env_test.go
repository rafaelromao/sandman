package batch

import "testing"

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: "", want: "''"},
		{name: "plain", value: "token", want: "'token'"},
		{name: "apostrophe", value: "it's", want: "'it'\"'\"'s'"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellQuote(tc.value); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyAgentEnv_EmptyEnvReturnsCommand(t *testing.T) {
	if got := applyAgentEnv("printenv AGENT_TOKEN", nil); got != "printenv AGENT_TOKEN" {
		t.Fatalf("got %q, want %q", got, "printenv AGENT_TOKEN")
	}
}

func TestApplyAgentEnv_ExportsSortedQuotedVariables(t *testing.T) {
	got := applyAgentEnv("printenv AGENT_TOKEN", map[string]string{
		"BETA":  "two words",
		"ALPHA": "it's fine",
	})
	want := "export ALPHA='it'\"'\"'s fine'; export BETA='two words'; printenv AGENT_TOKEN"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
