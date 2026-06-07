package batch

import (
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

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
	if got := applyAgentEnv("printenv AGENT_TOKEN", nil, ""); got != "printenv AGENT_TOKEN" {
		t.Fatalf("got %q, want %q", got, "printenv AGENT_TOKEN")
	}
}

func TestApplyAgentEnv_ExportsSortedQuotedVariables(t *testing.T) {
	got := applyAgentEnv("printenv AGENT_TOKEN", map[string]string{
		"BETA":  "two words",
		"ALPHA": "it's fine",
	}, "")
	want := "export ALPHA='it'\"'\"'s fine'; export BETA='two words'; printenv AGENT_TOKEN"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApplyAgentEnv_OpencodePresetRendersPermissionExportForDangerousRuns(t *testing.T) {
	preset, ok := config.BuiltInAgentPresets["opencode"]
	if !ok {
		t.Fatal("expected opencode preset to exist")
	}
	agent := preset.Agent("opencode")
	if _, ok := preset.Env["OPENCODE_PERMISSION"]; !ok {
		t.Fatal("expected opencode preset env to carry OPENCODE_PERMISSION")
	}

	got := applyAgentEnv(`opencode run --dangerously-skip-permissions "$(cat .sandman/rendered-prompt.md)"`, agent.Env, agent.OpencodePermissionMode)

	wantPrefix := "export OPENCODE_PERMISSION='"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("expected rendered opencode command to start with %q, got:\n%s", wantPrefix, got)
	}
	if !strings.HasSuffix(got, "'; opencode run --dangerously-skip-permissions \"$(cat .sandman/rendered-prompt.md)\"") {
		t.Fatalf("expected rendered opencode command to end with the opencode run invocation, got:\n%s", got)
	}
}

func TestApplyAgentEnv_DoesNotExportOpencodePermissionForNonDangerousRuns(t *testing.T) {
	preset, ok := config.BuiltInAgentPresets["opencode"]
	if !ok {
		t.Fatal("expected opencode preset to exist")
	}
	agent := preset.Agent("opencode")

	got := applyAgentEnv(`opencode run "$(cat .sandman/rendered-prompt.md)"`, agent.Env, agent.OpencodePermissionMode)
	want := `opencode run "$(cat .sandman/rendered-prompt.md)"`
	if got != want {
		t.Fatalf("expected non-dangerous opencode command to stay unchanged, got:\n%s", got)
	}
}

func TestApplyAgentEnv_PreservesUserOpencodePermissionOverride(t *testing.T) {
	got := applyAgentEnv(`opencode run "$(cat .sandman/rendered-prompt.md)"`, map[string]string{
		"OPENCODE_PERMISSION": `{"external_directory":"allow"}`,
	}, "custom")
	if !strings.HasPrefix(got, "export OPENCODE_PERMISSION='") {
		t.Fatalf("expected user OPENCODE_PERMISSION to be preserved, got:\n%s", got)
	}
}
