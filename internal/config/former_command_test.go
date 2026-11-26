package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// yamlCommand renders command as a YAML block scalar, which ends the value
// with a newline.
func yamlCommand(command string) string {
	return "    command: |\n      " + command + "\n"
}

func TestResolveAgentProvider_FormerBuiltInCommandMeansBuiltIn(t *testing.T) {
	for _, tc := range []struct{ name, entry, preset, former string }{
		{"opencode by name", "  opencode:\n", "opencode", argumentOpenCodeCommand},
		{"opencode-v2 by name", "  opencode-v2:\n", "opencode-v2", argumentOpenCodeV2Command},
		{"claude by name", "  claude:\n", "claude", argumentClaudeCommand},
		{"aliased preset", "  mine:\n    preset: opencode-v2\n", "opencode-v2", argumentOpenCodeV2Command},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "agents:\n" + tc.entry + yamlCommand(tc.former)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			name := strings.TrimSuffix(strings.TrimSpace(strings.SplitN(tc.entry, "\n", 2)[0]), ":")

			agent, err := cfg.ResolveAgentProvider(name)

			if err != nil {
				t.Fatal(err)
			}
			if want := BuiltInAgentPresets[tc.preset].Command; agent.Command != want {
				t.Fatalf("command = %q, want the built-in %q", agent.Command, want)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != text {
				t.Fatalf("config file was rewritten: %q %v", after, err)
			}
		})
	}
}

func TestLoad_FormerBuiltInCommandAcceptsPermissionMode(t *testing.T) {
	cfg, err := loadConfigText(t, "agents:\n  claude:\n    permission_mode: acceptEdits\n"+yamlCommand(argumentClaudeCommand))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	agent, err := cfg.ResolveAgentProvider("claude")

	if err != nil || agent.PermissionMode != "acceptEdits" || agent.Command != BuiltInAgentPresets["claude"].Command {
		t.Fatalf("agent = %+v, %v", agent, err)
	}
}

func TestResolveAgentProvider_OtherCommandsStayCustom(t *testing.T) {
	custom := `opencode run --format json < "{{.PromptFile}}" --extra`
	for _, tc := range []struct{ name, text, agent, want string }{
		{"edited former command", "agents:\n  opencode:\n" + yamlCommand(argumentOpenCodeCommand+" --extra"), "opencode", argumentOpenCodeCommand + " --extra"},
		{"another preset's former command", "agents:\n  opencode:\n" + yamlCommand(argumentClaudeCommand), "opencode", argumentClaudeCommand},
		{"custom command", "agents:\n  opencode:\n" + yamlCommand(custom), "opencode", custom},
		{"provider without a preset", "agents:\n  mine:\n" + yamlCommand(argumentOpenCodeCommand), "mine", argumentOpenCodeCommand},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfigText(t, tc.text)
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			agent, err := cfg.ResolveAgentProvider(tc.agent)

			if err != nil || strings.TrimSpace(agent.Command) != tc.want {
				t.Fatalf("command = %q (%v), want %q", agent.Command, err, tc.want)
			}
		})
	}
}
