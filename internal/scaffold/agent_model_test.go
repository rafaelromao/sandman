package scaffold

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requireRefusal(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected init to refuse the pairing, mentioning %q", wants)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestScaffold_RefusesClaudeAgentModelsForAnotherAgentBeforeWriting(t *testing.T) {
	cases := []struct {
		name  string
		opts  Options
		wants []string
	}{
		{
			name: "--model",
			opts: Options{BuildTools: "generic", Agent: "opencode-v2", Model: "claude-agent/sonnet"},
			wants: []string{
				`--model "claude-agent/sonnet" cannot run on agent "opencode-v2"`,
				`set --agent to claude and --model to "sonnet"`,
			},
		},
		{
			name: "--review-model",
			opts: Options{BuildTools: "generic", Agent: "opencode-v2", ReviewModel: "claude-agent/opus"},
			wants: []string{
				`--review-model "claude-agent/opus" cannot run on agent "opencode-v2"`,
				`set --agent to claude and --review-model to "opus"`,
			},
		},
		{
			name:  "--model without --agent on the agent init picks",
			opts:  Options{BuildTools: "generic", Model: "claude-agent/sonnet"},
			wants: []string{`--model "claude-agent/sonnet" cannot run on agent "opencode-v2"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			err := (&Scaffolder{}).Scaffold(dir, tc.opts, &fakePrompter{confirm: true})
			requireRefusal(t, err, tc.wants...)
			if _, statErr := os.Stat(filepath.Join(dir, ".sandman")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a refused init wrote .sandman/: %v", statErr)
			}
		})
	}
}

// A preserved agents.<name>.preset override decides the agent's preset: an
// agent named claude on the opencode-v2 preset is refused a claude-agent model.
func TestScaffold_ReInitHonorsPreservedPresetOverride(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}
	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	override := "agent: opencode-v2\nmodel: openai/gpt-6-luna\nagents:\n  claude:\n    preset: opencode-v2\n"
	if err := os.WriteFile(configPath, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	dockerfileBefore, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}

	err = s.Scaffold(dir, Options{BuildTools: "generic", Agent: "claude", Model: "claude-agent/sonnet"}, &fakePrompter{confirm: true})
	requireRefusal(t, err, `--model "claude-agent/sonnet" cannot run on agent "claude"`)
	if data, _ := os.ReadFile(configPath); string(data) != override {
		t.Errorf("a refused re-init changed config.yaml to %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile")); string(data) != string(dockerfileBefore) {
		t.Error("a refused re-init changed the Dockerfile")
	}
}

// The claude agent is the rule's boundary: init does not refuse it.
func TestScaffold_DoesNotRefuseClaudeAgentModelsOnTheClaudeAgent(t *testing.T) {
	opts := Options{BuildTools: "generic", Agent: "claude", Model: "claude-agent/sonnet", ReviewModel: "claude-agent/opus"}
	if err := (&Scaffolder{}).Scaffold(t.TempDir(), opts, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("init --agent claude with claude-agent models: %v", err)
	}
}

func TestScaffold_ReInitRefusingClaudeAgentModelKeepsTheScaffold(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(dir, ".sandman", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	configBefore, dockerfileBefore := read("config.yaml"), read("Dockerfile")

	err := s.Scaffold(dir, Options{BuildTools: "generic", Agent: "opencode-v2", Model: "claude-agent/sonnet"}, &fakePrompter{confirm: true})
	requireRefusal(t, err, `--model "claude-agent/sonnet" cannot run on agent "opencode-v2"`)
	if read("config.yaml") != configBefore {
		t.Error("a refused re-init changed config.yaml")
	}
	if read("Dockerfile") != dockerfileBefore {
		t.Error("a refused re-init changed the Dockerfile")
	}
}
