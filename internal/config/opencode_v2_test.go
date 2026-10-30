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
	if len(preset.LiveMounts) != 0 || len(preset.LiveMountDirs) != 1 || preset.LiveMountDirs[0] != "~/.local/share/opencode" {
		t.Fatalf("v2 must share the data directory for newly created sidecars: files=%v dirs=%v", preset.LiveMounts, preset.LiveMountDirs)
	}
}
