package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadAgentsConfig(t *testing.T, agents string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("agent: claude\nagents:\n"+agents), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

func TestLoad_ClaudePermissionModeResolvesOntoTheAgent(t *testing.T) {
	cfg, err := loadAgentsConfig(t, "  claude:\n    permission_mode: acceptEdits\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	agent, err := cfg.ResolveAgentProvider("claude")
	if err != nil {
		t.Fatalf("ResolveAgentProvider: %v", err)
	}
	if agent.PermissionMode != "acceptEdits" {
		t.Fatalf("permission mode = %q, want acceptEdits", agent.PermissionMode)
	}
	if agent.Command != BuiltInAgentPresets["claude"].Command {
		t.Fatal("a permission mode must keep the preset's built-in command")
	}
}

func TestLoad_PermissionModeUnsetKeepsBuiltInHandling(t *testing.T) {
	cfg, err := loadAgentsConfig(t, "  claude:\n    model: opus\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	agent, err := cfg.ResolveAgentProvider("claude")
	if err != nil {
		t.Fatalf("ResolveAgentProvider: %v", err)
	}
	if agent.PermissionMode != "" {
		t.Fatalf("permission mode = %q, want built-in (empty)", agent.PermissionMode)
	}
}

func TestLoad_RejectsUnusablePermissionModes(t *testing.T) {
	for _, tc := range []struct {
		name, agents string
		want         []string
	}{
		{
			name:   "unknown mode",
			agents: "  claude:\n    permission_mode: yolo\n",
			want:   []string{"agents.claude.permission_mode", `"yolo"`, "acceptEdits", "dontAsk"},
		},
		{
			name:   "plan mode cannot change anything unattended",
			agents: "  claude:\n    permission_mode: plan\n",
			want:   []string{"agents.claude.permission_mode", "plan", "unattended"},
		},
		{
			name:   "agent without a command-line permission mode",
			agents: "  opencode:\n    permission_mode: acceptEdits\n",
			want:   []string{"agents.opencode.permission_mode", "OpenCode", "OPENCODE_PERMISSION"},
		},
		{
			name:   "preset-less custom provider",
			agents: "  mine:\n    command: my-agent {{.PromptFile}}\n    permission_mode: acceptEdits\n",
			want:   []string{"agents.mine.permission_mode", "custom command"},
		},
		{
			name:   "custom command under the claude preset",
			agents: "  claude:\n    command: claude -p \"$(cat {{.PromptFile}})\"\n    permission_mode: acceptEdits\n",
			want:   []string{"agents.claude.permission_mode", "custom command", "--permission-mode"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAgentsConfig(t, tc.agents)
			if err == nil {
				t.Fatal("Load accepted an unusable permission mode")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}
