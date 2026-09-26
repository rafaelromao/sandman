package batch

import (
	"reflect"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestDockerfileAgentExpectations_DependOnlyOnTheRunAgent(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent: "opencode",
		Agents: map[string]config.Agent{
			"reviewer": {Preset: "claude", Model: "haiku"},
			"mine":     {Command: "mine run"},
		},
	}
	resolve := func(name string) config.Agent {
		t.Helper()
		agent, err := cfg.ResolveAgentProvider(name)
		if err != nil {
			t.Fatalf("resolve %q: %v", name, err)
		}
		return agent
	}
	tests := []struct {
		name         string
		runAgent     string
		wantDefault  string
		wantRequired []string
	}{
		{name: "default agent run checks the default-agent header", runAgent: "opencode", wantDefault: "opencode", wantRequired: []string{"opencode"}},
		{name: "empty request agent means the default agent", runAgent: "", wantDefault: "opencode", wantRequired: []string{"opencode"}},
		{name: "another preset ignores the default agent", runAgent: "claude", wantDefault: "", wantRequired: []string{"claude"}},
		{name: "custom agent requires its preset only", runAgent: "reviewer", wantDefault: "", wantRequired: []string{"claude"}},
		{name: "preset-less agent requires nothing", runAgent: "mine", wantDefault: "", wantRequired: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentName := tt.runAgent
			if agentName == "" {
				agentName = cfg.DefaultAgent
			}
			gotDefault, gotRequired := dockerfileAgentExpectations(cfg.DefaultAgent, tt.runAgent, resolve(agentName))
			if gotDefault != tt.wantDefault || !reflect.DeepEqual(gotRequired, tt.wantRequired) {
				t.Fatalf("expectations = (%q, %v), want (%q, %v)", gotDefault, gotRequired, tt.wantDefault, tt.wantRequired)
			}
		})
	}
}
