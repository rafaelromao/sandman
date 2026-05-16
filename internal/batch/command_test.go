package batch

import (
	"reflect"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestCommandData_ExposesOnlyPromptFile(t *testing.T) {
	typ := reflect.TypeOf(CommandData{})
	if typ.NumField() != 1 {
		t.Errorf("expected exactly 1 field in CommandData, got %d", typ.NumField())
	}
	field, ok := typ.FieldByName("PromptFile")
	if !ok {
		t.Fatal("expected PromptFile field in CommandData")
	}
	if field.Type.Kind() != reflect.String {
		t.Errorf("expected PromptFile to be string, got %s", field.Type)
	}
}

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
		PromptFile: ".sandman/rendered-prompt.md",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "opencode --prompt-file .sandman/rendered-prompt.md"
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
		"opencode":    `opencode run "$(cat .sandman/rendered-prompt.md)"`,
		"claude-code": `claude --print "$(cat .sandman/rendered-prompt.md)"`,
		"codex":       `codex exec "$(cat .sandman/rendered-prompt.md)"`,
		"pi":          `pi --print "$(cat .sandman/rendered-prompt.md)"`,
	}

	for key, want := range presets {
		t.Run(key, func(t *testing.T) {
			preset, ok := config.BuiltInAgentPresets[key]
			if !ok {
				t.Fatalf("missing built-in preset %q", key)
			}

			got, err := RenderCommand(preset.Command, CommandData{
				PromptFile: ".sandman/rendered-prompt.md",
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
