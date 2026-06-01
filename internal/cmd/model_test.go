package cmd

import "testing"

func TestResolveModel(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		defaultModel string
		want         string
	}{
		{name: "uses flag when provided", model: " gpt-4.1 ", defaultModel: "openai/gpt-4.1", want: "gpt-4.1"},
		{name: "falls back to default model", model: "", defaultModel: " openai/gpt-4.1 ", want: "openai/gpt-4.1"},
		{name: "returns empty when both are blank", model: "", defaultModel: "  ", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveModel(tt.model, tt.defaultModel); got != tt.want {
				t.Fatalf("resolveModel() = %q, want %q", got, tt.want)
			}
		})
	}
}
