package batch

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestRenderCommand_InvalidTemplateReturnsError(t *testing.T) {
	_, err := RenderCommand("opencode {{.Unknown", CommandData{})
	if err == nil {
		t.Fatal("expected error for invalid template syntax")
	}
}

func TestRenderCommand_UnknownFieldReturnsError(t *testing.T) {
	_, err := RenderCommand("opencode --flag {{.Typo}}", CommandData{})
	if err == nil {
		t.Fatal("expected error for unknown template field")
	}
}

func TestRenderCommand_SubstitutesPromptFile(t *testing.T) {
	got, err := RenderCommand("opencode --prompt-file {{.PromptFile}}", CommandData{
		PromptFile: ".sandman/prompt.md",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "opencode --prompt-file .sandman/prompt.md"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderCommand_PlainCommandPassesThrough(t *testing.T) {
	got, err := RenderCommand("opencode", CommandData{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "opencode" {
		t.Errorf("got %q, want %q", got, "opencode")
	}
}

func TestRenderCommand_BuiltInPresets(t *testing.T) {
	presets := map[string]string{
		"opencode":    `opencode run "$(cat .sandman/prompt.md)"`,
		"claude-code": `claude --print "$(cat .sandman/prompt.md)"`,
		"codex":       `codex exec "$(cat .sandman/prompt.md)"`,
		"pi":          `pi --print "$(cat .sandman/prompt.md)"`,
	}

	for key, want := range presets {
		t.Run(key, func(t *testing.T) {
			preset, ok := config.BuiltInAgentPresets[key]
			if !ok {
				t.Fatalf("missing built-in preset %q", key)
			}

			got, err := RenderCommand(preset.Command, CommandData{
				PromptFile: ".sandman/prompt.md",
			})
			if err != nil {
				t.Fatalf("RenderCommand: %v", err)
			}
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}
