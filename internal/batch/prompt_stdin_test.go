package batch

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// fakePromptCLI records the arguments and standard input of each launch, then
// reports success in both the OpenCode and Claude Code stream formats.
const fakePromptCLI = `#!/bin/sh
for arg in "$@"; do printf '%s\n' "$arg"; done > "$SANDMAN_TEST_OUT/args"
cat > "$SANDMAN_TEST_OUT/stdin"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"6f1c2a4e-0b7d-4c1a-9a55-2f0e8d3b7c11"}'
`

// TestBuiltInCommands_DeliverLargePromptOnStdin runs every built-in preset
// command through a worktree sandbox's real shell against a fake CLI. Linux
// refuses a single argument above 128 KiB, so a prompt of that size only
// launches when it is not passed as one.
func TestBuiltInCommands_DeliverLargePromptOnStdin(t *testing.T) {
	task := strings.Repeat("a prompt line with 'quotes' \"doubles\" $HOME `ticks` \\slash\n", 4000)
	if len(task) <= 128*1024 {
		t.Fatalf("test prompt is only %d bytes", len(task))
	}
	binaries := map[string]string{"opencode": "opencode", "opencode-v2": "opencode", "claude": "claude"}
	for name, preset := range config.BuiltInAgentPresets {
		binary, ok := binaries[name]
		if !ok {
			t.Fatalf("preset %q has no fake CLI in this test", name)
		}
		for _, reuse := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "", true: " reusing a session"}[reuse], func(t *testing.T) {
				root := t.TempDir()
				initGitRepo(t, root)
				out := t.TempDir()
				bin := t.TempDir()
				if err := os.WriteFile(filepath.Join(bin, binary), []byte(fakePromptCLI), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("SANDMAN_TEST_OUT", out)
				sb := sandbox.NewWorktreeSandbox(root, filepath.Join(root, ".sandman", "worktrees"), "stdin-prompt", "main")
				if err := sb.Start(sandbox.SandboxStart{}); err != nil {
					t.Fatal(err)
				}
				run := NewAgentRunWithLayout(nil, "stdin-prompt", sb, paths.NewLayout(&config.Config{}, root))
				run.preset, run.reuseSession = name, reuse
				run.runFolder, run.outputWriter = filepath.Join(root, "run"), &bytes.Buffer{}

				result := run.Run(context.Background(), &spyRenderer{}, preset.Command, prompt.RenderConfig{TaskPrompt: task})

				if result.Status != "success" {
					t.Fatalf("launch failed: %+v\n%s", result, run.outputWriter)
				}
				written, err := os.ReadFile(filepath.Join(sb.WorkDir(), ".sandman", "task.md"))
				if err != nil {
					t.Fatal(err)
				}
				received, err := os.ReadFile(filepath.Join(out, "stdin"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(received, written) {
					t.Fatalf("stdin holds %d bytes, want the %d bytes of the Task file", len(received), len(written))
				}
				args, err := os.ReadFile(filepath.Join(out, "args"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(args), "a prompt line") {
					t.Fatalf("prompt content reached the command line")
				}
			})
		}
	}
}

// TestFormerBuiltInCommandKeepsBuiltInLaunchBehaviour covers a config that
// still spells out the claude command which passed the prompt as an argument.
func TestFormerBuiltInCommandKeepsBuiltInLaunchBehaviour(t *testing.T) {
	preset := config.BuiltInAgentPresets["claude"]
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "agents:\n  claude:\n    command: >-\n      " + preset.FormerCommands[0] + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := cfg.ResolveAgentProvider("claude")
	if err != nil {
		t.Fatal(err)
	}

	startOpts, err := buildStartOptions(agent, lookupEnvFrom(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sentinel"}))

	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(startOpts.PassEnv, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("PassEnv = %v, want the preset's credential", startOpts.PassEnv)
	}
	if len(credentialFiles(agent)) == 0 {
		t.Fatal("credential files are not reconciled")
	}
	strategy := strategyFor(agent.Preset, agent.Command)
	if !strategy.AwaitsUsageLimit() || strategy.ModelFlag("sonnet", "") == "" {
		t.Fatalf("launch strategy treats the former command as custom: %#v", strategy)
	}
}
