package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Defaults for optional config fields.
const (
	DefaultAgent             = "opencode"
	DefaultBuildToolsPreset  = "generic"
	DefaultReviewCommand     = "/oc review"
	DefaultParallel          = 4
	DefaultStartDelay        = 0
	DefaultContainerCapacity = 4
	DefaultMaxContainers     = 0
	DefaultWorktreeDir       = ".sandman/worktrees"
	DefaultSandbox           = "podman"
)

// Config holds the loaded Sandman configuration.
type Config struct {
	DefaultAgent      string           `yaml:"default_agent"`
	BuildTools        string           `yaml:"build_tools"`
	ReviewCommand     string           `yaml:"review_command"`
	DefaultParallel   int              `yaml:"default_parallel"`
	StartDelay        int              `yaml:"start_delay"`
	ContainerCapacity int              `yaml:"container_capacity"`
	MaxContainers     int              `yaml:"max_containers"`
	WorktreeDir       string           `yaml:"worktree_dir"`
	Sandbox           string           `yaml:"sandbox"`
	Agents            map[string]Agent `yaml:"agents,omitempty"`
	Git               GitConfig        `yaml:"git"`
	Agent             string           `yaml:"-"`
	AgentProviders    map[string]Agent `yaml:"-"`
}

// GitConfig holds git-specific settings.
type GitConfig struct {
	BaseBranch string `yaml:"base_branch"`
}

// Agent holds a configured agent provider or a custom override.
type Agent struct {
	Name          string            `yaml:"name,omitempty"`
	Preset        string            `yaml:"preset,omitempty"`
	Command       string            `yaml:"command,omitempty"`
	Model         string            `yaml:"model,omitempty"`
	ModelProvider string            `yaml:"-"`
	ModelName     string            `yaml:"-"`
	Env           map[string]string `yaml:"env,omitempty"`
	ConfigDirs    []string          `yaml:"config_dirs,omitempty"`
	ConfigFiles   []string          `yaml:"config_files,omitempty"`
	KeychainAuth  bool              `yaml:"keychain_auth,omitempty"`
}

// AgentPreset defines the built-in defaults for a provider preset.
type AgentPreset struct {
	DisplayName  string
	Command      string
	Env          map[string]string
	ConfigDirs   []string
	ConfigFiles  []string
	KeychainAuth bool
}

// BuiltInAgentPresets lists the provider presets Sandman knows about without repo-specific config.
var BuiltInAgentPresets = map[string]AgentPreset{
	"opencode": {
		DisplayName: "OpenCode",
		Command:     `opencode run{{if .ModelFlag}} {{.ModelFlag}}{{end}} "$(cat {{.PromptFile}})"`,
		ConfigDirs: []string{
			"~/.config/opencode",
			"~/.local/share/opencode",
			"~/.claude",
		},
	},
	"pi": {
		DisplayName: "Pi",
		Command:     `pi --print{{if .ModelProvider}} --provider {{.ModelProvider}}{{end}}{{if .ModelName}} --model {{.ModelName}}{{end}} "$(cat {{.PromptFile}})"`,
		ConfigDirs: []string{
			"~/.pi",
		},
	},
}

// Store loads and saves Sandman configuration.
type Store interface {
	Load() (*Config, error)
	Save(cfg *Config) error
}

// Load reads, parses, validates, and applies defaults to the config file at the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	type rawConfig struct {
		DefaultAgent      string           `yaml:"default_agent"`
		BuildTools        string           `yaml:"build_tools"`
		ReviewCommand     string           `yaml:"review_command"`
		DefaultParallel   int              `yaml:"default_parallel"`
		StartDelay        int              `yaml:"start_delay"`
		ContainerCapacity *int             `yaml:"container_capacity"`
		MaxContainers     *int             `yaml:"max_containers"`
		WorktreeDir       string           `yaml:"worktree_dir"`
		Sandbox           string           `yaml:"sandbox"`
		Agents            map[string]Agent `yaml:"agents"`
		Git               struct {
			BaseBranch    string  `yaml:"base_branch"`
			DefaultBranch *string `yaml:"default_branch"`
		} `yaml:"git"`
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg := Config{
		DefaultAgent:    raw.DefaultAgent,
		BuildTools:      raw.BuildTools,
		ReviewCommand:   raw.ReviewCommand,
		DefaultParallel: raw.DefaultParallel,
		StartDelay:      raw.StartDelay,
		WorktreeDir:     raw.WorktreeDir,
		Sandbox:         raw.Sandbox,
		Agents:          raw.Agents,
		Git:             GitConfig{BaseBranch: raw.Git.BaseBranch},
	}

	if raw.Git.DefaultBranch != nil {
		return nil, fmt.Errorf("validate config: git.default_branch was renamed to git.base_branch")
	}

	if cfg.DefaultParallel <= 0 {
		cfg.DefaultParallel = DefaultParallel
	}
	if cfg.StartDelay < 0 {
		return nil, fmt.Errorf("validate config: start_delay must be 0 or greater")
	}
	if cfg.BuildTools == "" {
		cfg.BuildTools = DefaultBuildToolsPreset
	}
	if strings.TrimSpace(cfg.ReviewCommand) == "" {
		cfg.ReviewCommand = DefaultReviewCommand
	}
	if raw.ContainerCapacity == nil {
		cfg.ContainerCapacity = DefaultContainerCapacity
	} else if *raw.ContainerCapacity < 0 {
		return nil, fmt.Errorf("validate config: container_capacity must be 0 or greater")
	} else {
		cfg.ContainerCapacity = *raw.ContainerCapacity
	}
	if raw.MaxContainers == nil {
		cfg.MaxContainers = DefaultMaxContainers
	} else if *raw.MaxContainers < 0 {
		return nil, fmt.Errorf("validate config: max_containers must be 0 or greater")
	} else {
		cfg.MaxContainers = *raw.MaxContainers
	}
	if cfg.WorktreeDir == "" {
		cfg.WorktreeDir = DefaultWorktreeDir
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = DefaultSandbox
	}
	if cfg.Git.BaseBranch == "" {
		cfg.Git.BaseBranch = "main"
	}

	if strings.TrimSpace(cfg.DefaultAgent) == "" {
		cfg.DefaultAgent = DefaultAgent
	}
	cfg.Agent = cfg.DefaultAgent
	cfg.AgentProviders = make(map[string]Agent, len(BuiltInAgentPresets))
	for name := range BuiltInAgentPresets {
		agent, err := cfg.ResolveAgentProvider(name)
		if err != nil {
			return nil, fmt.Errorf("validate config: %w", err)
		}
		cfg.AgentProviders[name] = agent
	}
	for name, agent := range cfg.Agents {
		cfg.AgentProviders[name] = agent
	}
	agentCfg, err := cfg.ResolveAgentProvider(cfg.DefaultAgent)
	if err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	if agentCfg.Preset == "pi" {
		if _, _, err := SplitPiModel(agentCfg.Model); err != nil {
			return nil, fmt.Errorf("validate config: %w", err)
		}
	}

	return &cfg, nil
}

// Save writes the config to the given path as YAML.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// ResolveAgentProvider returns the configured agent provider, applying preset defaults when needed.
func (c *Config) ResolveAgentProvider(name string) (Agent, error) {
	if c != nil && c.AgentProviders != nil {
		if agent, ok := c.AgentProviders[name]; ok {
			if preset := agent.Preset; preset != "" {
				if builtin, ok := BuiltInAgentPresets[preset]; ok {
					return builtin.AgentWithOverrides(preset, agent), nil
				}
			}
			if builtin, ok := BuiltInAgentPresets[name]; ok {
				return builtin.AgentWithOverrides(name, agent), nil
			}
			if agent.Command != "" {
				return agent, nil
			}
		}
	}

	if preset, ok := BuiltInAgentPresets[name]; ok {
		return preset.Agent(name), nil
	}

	return Agent{}, fmt.Errorf("agent %q not found in config", name)
}

func (p AgentPreset) Agent(preset string) Agent {
	return Agent{
		Preset:       preset,
		Command:      p.Command,
		Env:          copyStringMap(p.Env),
		ConfigDirs:   append([]string(nil), p.ConfigDirs...),
		ConfigFiles:  append([]string(nil), p.ConfigFiles...),
		KeychainAuth: p.KeychainAuth,
	}
}

func (p AgentPreset) AgentWithOverrides(preset string, override Agent) Agent {
	agent := p.Agent(preset)
	if override.Name != "" {
		agent.Name = override.Name
	}
	if override.Preset != "" {
		agent.Preset = override.Preset
	}
	if override.Command != "" {
		agent.Command = override.Command
	}
	if override.Model != "" {
		agent.Model = override.Model
	}
	if len(override.Env) > 0 {
		agent.Env = copyStringMap(override.Env)
	}
	if len(override.ConfigDirs) > 0 {
		agent.ConfigDirs = append([]string(nil), override.ConfigDirs...)
	}
	if len(override.ConfigFiles) > 0 {
		agent.ConfigFiles = append([]string(nil), override.ConfigFiles...)
	}
	if override.KeychainAuth {
		agent.KeychainAuth = true
	}
	return agent
}

func copyStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// SplitPiModel splits a Pi model value in provider/model form.
func SplitPiModel(model string) (string, string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", nil
	}
	provider, name, ok := strings.Cut(model, "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(name) == "" {
		return "", "", fmt.Errorf("pi model must use provider/model format, got %q", model)
	}
	return strings.TrimSpace(provider), strings.TrimSpace(name), nil
}

// GetValue returns the string representation of a config field by its dot-notation key.
func (c *Config) GetValue(key string) (string, error) {
	switch strings.ToLower(key) {
	case "default_agent":
		return c.DefaultAgent, nil
	case "build_tools":
		return c.EffectiveBuildTools(), nil
	case "review_command":
		return c.EffectiveReviewCommand(), nil
	case "default_parallel":
		return fmt.Sprintf("%d", c.DefaultParallel), nil
	case "start_delay":
		return fmt.Sprintf("%d", c.StartDelay), nil
	case "container_capacity":
		return fmt.Sprintf("%d", c.ContainerCapacity), nil
	case "max_containers":
		return fmt.Sprintf("%d", c.MaxContainers), nil
	case "worktree_dir":
		return c.WorktreeDir, nil
	case "sandbox":
		return c.Sandbox, nil
	case "git.base_branch":
		return c.Git.BaseBranch, nil
	case "git.default_branch":
		return "", fmt.Errorf("git.default_branch was renamed to git.base_branch")
	default:
		return "", fmt.Errorf("unknown config key: %s", key)
	}
}

// SetValue updates a config field by its dot-notation key.
func (c *Config) SetValue(key, value string) error {
	switch strings.ToLower(key) {
	case "default_agent":
		if _, err := c.ResolveAgentProvider(strings.TrimSpace(value)); err != nil {
			return err
		}
		c.DefaultAgent = strings.TrimSpace(value)
		c.Agent = c.DefaultAgent
	case "build_tools":
		c.BuildTools = value
	case "review_command":
		c.ReviewCommand = value
	case "default_parallel":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid value for default_parallel: %w", err)
		}
		if n <= 0 {
			return fmt.Errorf("default_parallel must be greater than 0")
		}
		c.DefaultParallel = n
	case "start_delay":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid value for start_delay: %w", err)
		}
		if n < 0 {
			return fmt.Errorf("start_delay must be 0 or greater")
		}
		c.StartDelay = n
	case "container_capacity":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid value for container_capacity: %w", err)
		}
		if n < 0 {
			return fmt.Errorf("container_capacity must be 0 or greater")
		}
		c.ContainerCapacity = n
	case "max_containers":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid value for max_containers: %w", err)
		}
		if n < 0 {
			return fmt.Errorf("max_containers must be 0 or greater")
		}
		c.MaxContainers = n
	case "worktree_dir":
		c.WorktreeDir = value
	case "sandbox":
		c.Sandbox = value
	case "git.base_branch":
		c.Git.BaseBranch = value
	case "git.default_branch":
		return fmt.Errorf("git.default_branch was renamed to git.base_branch")
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}

// EffectiveBuildTools returns the configured BuildTools preset, defaulting to generic.
func (c *Config) EffectiveBuildTools() string {
	if c == nil || strings.TrimSpace(c.BuildTools) == "" {
		return DefaultBuildToolsPreset
	}
	return c.BuildTools
}

// EffectiveReviewCommand returns the configured review command, defaulting to /oc review.
func (c *Config) EffectiveReviewCommand() string {
	if c == nil || strings.TrimSpace(c.ReviewCommand) == "" {
		return DefaultReviewCommand
	}
	return c.ReviewCommand
}
