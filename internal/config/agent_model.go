package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// AgentModelKeys name the config keys or command-line flags an agent and its
// model come from, so a refused pairing names what to change.
type AgentModelKeys struct {
	Agent string
	Model string
}

// ValidateAgentModel refuses model on agent when the model's provider is one
// that only another built-in preset may run (AgentPreset.ExclusiveModelProviders).
// OpenCode v2's claude-agent provider, for one, starts the Claude Code CLI,
// which only the claude preset installs and keeps signed in.
func ValidateAgentModel(keys AgentModelKeys, agentName string, agent Agent, model string) error {
	model = strings.TrimSpace(model)
	owner, preset, ok := exclusiveModelProviderOwner(model)
	if !ok || agent.Preset == owner {
		return nil
	}
	provider, name, _ := strings.Cut(model, "/")
	refusal := fmt.Sprintf("%s %q cannot run on agent %q: the %s provider runs the %s CLI, which only the %s agent installs and keeps signed in",
		keys.Model, model, agentName, provider, preset.DisplayName, owner)
	if keys.Agent == "" {
		return fmt.Errorf("%s; use a model of another provider, or the %s agent with model %q", refusal, owner, name)
	}
	return fmt.Errorf("%s; set %s to %s and %s to %q, or use a model of another provider", refusal, keys.Agent, owner, keys.Model, name)
}

// exclusiveModelProviderOwner returns the built-in preset that alone may run
// model's provider, if any.
func exclusiveModelProviderOwner(model string) (string, AgentPreset, bool) {
	provider, _, ok := strings.Cut(model, "/")
	if !ok {
		return "", AgentPreset{}, false
	}
	provider = strings.TrimSpace(provider)
	names := make([]string, 0, len(BuiltInAgentPresets))
	for name := range BuiltInAgentPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		preset := BuiltInAgentPresets[name]
		if slices.Contains(preset.ExclusiveModelProviders, provider) {
			return name, preset, true
		}
	}
	return "", AgentPreset{}, false
}

// ValidateAgentModels checks every agent and model pairing the config makes:
// model with agent, review_model (as stored) with the review agent, and each
// agents.<name>.model with its agent, in name order.
func (c *Config) ValidateAgentModels() error {
	if err := c.validateAgentModel(AgentModelKeys{Agent: "agent", Model: "model"}, c.DefaultAgent, c.DefaultModel); err != nil {
		return err
	}
	if err := c.validateAgentModel(AgentModelKeys{Agent: "review_agent", Model: "review_model"}, c.EffectiveReviewAgent(), c.DefaultReviewModel); err != nil {
		return err
	}
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		keys := AgentModelKeys{Agent: "agents." + name + ".preset", Model: "agents." + name + ".model"}
		if _, builtIn := BuiltInAgentPresets[name]; builtIn {
			// Changing a built-in agent's preset would make it another agent.
			keys.Agent = ""
		}
		if err := c.validateAgentModel(keys, name, c.Agents[name].Model); err != nil {
			return err
		}
	}
	return nil
}

// validateAgentModel resolves agentName only when model's provider is
// reserved for a preset, so pairings the rule does not concern never depend
// on the agent resolving. An agent that does not resolve has no preset.
func (c *Config) validateAgentModel(keys AgentModelKeys, agentName, model string) error {
	if _, _, ok := exclusiveModelProviderOwner(strings.TrimSpace(model)); !ok {
		return nil
	}
	agentName = strings.TrimSpace(agentName)
	agent, err := c.ResolveAgentProvider(agentName)
	if err != nil {
		agent = Agent{}
	}
	return ValidateAgentModel(keys, agentName, agent, model)
}
