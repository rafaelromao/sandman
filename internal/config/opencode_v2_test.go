package config

import (
	"strings"
	"testing"
)

func TestOpenCodeV2Preset(t *testing.T) {
	cfg := &Config{}
	cfg.DefaultAgent, cfg.DefaultReviewAgent = "opencode-v2", "opencode-v2"
	agent, err := cfg.ResolveAgentProvider("opencode-v2")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Preset != "opencode-v2" || !strings.Contains(agent.Command, "--auto") || strings.Contains(agent.Command, "--variant") {
		t.Fatalf("wrong v2 command: %+v", agent)
	}
	if _, found := agent.Env["OPENCODE_PERMISSION"]; found {
		t.Fatal("v2 inherited v1 permission defaults")
	}
	preset := BuiltInAgentPresets[agent.Preset]
	if len(preset.LiveMounts) != 3 {
		t.Fatalf("v2 must share live credential database and sidecars: %v", preset.LiveMounts)
	}
}
