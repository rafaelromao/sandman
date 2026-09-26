package cmd

import (
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

// resolveModel picks the model a run forwards to the orchestrator. An
// explicit flag wins. Otherwise the global `model` key applies only to agents
// that share the default agent's built-in preset, so a model written for one
// provider (for example `opencode/big-pickle`) never reaches another. An agent
// on a different built-in preset keeps its own configured model, or falls
// back to its preset's default model. A preset that sets DefaultModelWhenUnset
// (claude) also uses its default model when nothing is configured at all.
// Preset-less custom providers never receive a model.
func resolveModel(model string, cfg *config.Config, agent config.Agent) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	if agent.Preset == "" {
		return ""
	}
	if defaultPreset := defaultAgentPreset(cfg); defaultPreset == "" || defaultPreset == agent.Preset {
		if cfg != nil {
			if global := strings.TrimSpace(cfg.DefaultModel); global != "" {
				return global
			}
		}
		if strings.TrimSpace(agent.Model) != "" || !config.BuiltInAgentPresets[agent.Preset].DefaultModelWhenUnset {
			// Keep the agent's own configured model, or leave the choice to
			// the agent CLI for presets that do not force their default.
			return ""
		}
		return config.DefaultModelForAgent(agent.Preset)
	}
	if strings.TrimSpace(agent.Model) != "" {
		// The orchestrator keeps the agent's own configured model when the
		// request carries none.
		return ""
	}
	return config.DefaultModelForAgent(agent.Preset)
}

// defaultAgentPreset returns the built-in preset of the config default agent,
// or "" when the default agent is a preset-less custom provider.
func defaultAgentPreset(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	name := strings.TrimSpace(cfg.DefaultAgent)
	if name == "" {
		name = strings.TrimSpace(cfg.Agent)
	}
	if name == "" {
		return ""
	}
	agent, err := cfg.ResolveAgentProvider(name)
	if err != nil {
		return ""
	}
	return agent.Preset
}
