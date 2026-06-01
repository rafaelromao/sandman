package cmd

import "testing"

func TestResolveModel(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		defaultModel string
		preset       string
		want         string
	}{
		{name: "uses flag when provided", model: " gpt-4.1 ", defaultModel: "openai/gpt-4.1", preset: "opencode", want: "gpt-4.1"},
		{name: "falls back to default model for built-in preset", model: "", defaultModel: " openai/gpt-4.1 ", preset: "opencode", want: "openai/gpt-4.1"},
		{name: "returns empty for custom agent without model", model: "", defaultModel: "openai/gpt-4.1", preset: "", want: ""},
		{name: "returns empty when both are blank", model: "", defaultModel: "  ", preset: "opencode", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveModel(tt.model, tt.defaultModel, tt.preset); got != tt.want {
				t.Fatalf("resolveModel() = %q, want %q", got, tt.want)
			}
		})
	}
}
