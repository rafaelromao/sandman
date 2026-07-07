package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"gopkg.in/yaml.v3"
)

// Defaults for optional config fields.
const (
	DefaultAgent             = "opencode"
	DefaultModel             = "opencode/big-pickle"
	DefaultReviewAgent       = "opencode"
	DefaultReviewModel       = "opencode/big-pickle"
	DefaultBuildToolsPreset  = "generic"
	DefaultReviewCommand     = "/sandman review"
	DefaultParallel          = 1
	DefaultReviewParallel    = 1
	DefaultStartDelay        = 0
	DefaultRunIdleTimeout    = 1800
	DefaultRetries           = 3
	DefaultContainerCapacity = 4
	DefaultMaxContainers     = 0
	DefaultWorktreeDir       = ".sandman/worktrees"
	DefaultSandbox           = "podman"
	DefaultAutoMaxCount      = 50
)

// Config holds the loaded Sandman configuration.
type Config struct {
	DefaultAgent          string           `yaml:"agent"`
	DefaultModel          string           `yaml:"model"`
	DefaultReviewAgent    string           `yaml:"review_agent"`
	DefaultReviewModel    string           `yaml:"review_model"`
	BuildTools            string           `yaml:"build_tools"`
	ReviewCommand         string           `yaml:"review_command"`
	DefaultParallel       int              `yaml:"parallel"`
	DefaultReviewParallel int              `yaml:"parallel_reviews"`
	StartDelay            int              `yaml:"start_delay"`
	RunIdleTimeout        int              `yaml:"run_idle_timeout"`
	Retries               int              `yaml:"retries"`
	ContainerCapacity     int              `yaml:"container_capacity"`
	MaxContainers         int              `yaml:"max_containers"`
	AutoMaxCount          int              `yaml:"-"`
	WorktreeDir           string           `yaml:"worktree_dir"`
	Sandbox               string           `yaml:"sandbox"`
	Agents                map[string]Agent `yaml:"agents,omitempty"`
	Git                   GitConfig        `yaml:"git"`
	Agent                 string           `yaml:"-"`
	AgentProviders        map[string]Agent `yaml:"-"`
}

// GitConfig holds git-specific settings.
type GitConfig struct {
	BaseBranch string `yaml:"base_branch"`
}

// Agent holds a configured agent provider or a custom override.
type Agent struct {
	Name                   string            `yaml:"name,omitempty"`
	Preset                 string            `yaml:"preset,omitempty"`
	Command                string            `yaml:"command,omitempty"`
	Model                  string            `yaml:"model,omitempty"`
	ModelProvider          string            `yaml:"-"`
	ModelName              string            `yaml:"-"`
	Env                    map[string]string `yaml:"env,omitempty"`
	ConfigDirs             []string          `yaml:"config_dirs,omitempty"`
	ConfigFiles            []string          `yaml:"config_files,omitempty"`
	KeychainAuth           bool              `yaml:"keychain_auth,omitempty"`
	OpencodePermissionMode string            `yaml:"-"`
}

// AgentPreset defines the built-in defaults for a provider preset.
type AgentPreset struct {
	DisplayName      string
	Command          string
	Env              map[string]string
	ConfigDirs       []string
	ConfigFiles      []string
	SnapshotExcludes []string
	LiveMounts       []string
	KeychainAuth     bool
}

// OpencodePermissionExternalDirectoryAllow is the OPENCODE_PERMISSION value
// shipped with the opencode preset. It only allow-lists the subagent
// external_directory permission that was hanging, so it avoids overwriting
// unrelated explicit deny rules in OpenCode config while still preventing the
// subagent permission.asked hang observed in Sandman containers.
const OpencodePermissionExternalDirectoryAllow = `{"external_directory":"allow"}`

// BuiltInAgentPresets lists the provider presets Sandman knows about without repo-specific config.
var BuiltInAgentPresets = map[string]AgentPreset{
	"opencode": {
		DisplayName: "OpenCode",
		Command:     `opencode run{{if .DangerouslySkipPermissions}} --dangerously-skip-permissions{{end}}{{if .SessionName}} --title '{{.SessionName}}'{{end}}{{if .ModelFlag}} {{.ModelFlag}}{{end}} "$(cat {{.PromptFile}})"`,
		Env: map[string]string{
			"OPENCODE_PERMISSION": OpencodePermissionExternalDirectoryAllow,
		},
		ConfigDirs: []string{
			"~/.config/opencode",
			"~/.local/share/opencode",
			"~/.claude",
			"~/.agents",
		},
		// Mutable runtime state under ~/.local/share/opencode/ is too large to
		// snapshot (hundreds of MB) and not needed for agent invocation.
		// opencode.db* are also listed here so the snapshot copy skips them;
		// the live database files are exposed to the container via LiveMounts
		// instead, so host-side OpenCode sessions can inspect them after the
		// container run.
		SnapshotExcludes: []string{
			"~/.local/share/opencode/token-optimizer",
			"~/.local/share/opencode/storage",
			"~/.local/share/opencode/snapshot",
			"~/.local/share/opencode/tool-output",
			"~/.local/share/opencode/repos",
			"~/.local/share/opencode/log",
			"~/.local/share/opencode/node_modules",
			"~/.local/share/opencode/opencode.db",
			"~/.local/share/opencode/opencode.db-shm",
			"~/.local/share/opencode/opencode.db-wal",
		},
		// Bind-mount the SQLite database (and its WAL/SHM siblings, when
		// present) directly so writes from the container are visible to host-
		// side OpenCode after the run completes. Concurrent agents sharing one
		// container share the same host DB; SQLite WAL mode serialises writes.
		LiveMounts: []string{
			"~/.local/share/opencode/opencode.db",
			"~/.local/share/opencode/opencode.db-shm",
			"~/.local/share/opencode/opencode.db-wal",
		},
	},
}

// Store loads and saves Sandman configuration.
type Store interface {
	Load() (*Config, error)
	Save(cfg *Config) error
}

// SupportedKeys lists config keys exposed by GetValue/SetValue and config list.
func SupportedKeys() []string {
	return []string{
		"agent",
		"model",
		"review_agent",
		"review_model",
		"build_tools",
		"review_command",
		"parallel",
		"parallel_reviews",
		"start_delay",
		"run_idle_timeout",
		"retries",
		"container_capacity",
		"max_containers",
		"auto_max_count",
		"worktree_dir",
		"sandbox",
		"git.base_branch",
	}
}

// Load reads, parses, validates, and applies defaults to the config file at the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	type rawConfig struct {
		DefaultAgent          string           `yaml:"agent"`
		DefaultModel          string           `yaml:"model"`
		DefaultReviewAgent    string           `yaml:"review_agent"`
		DefaultReviewModel    string           `yaml:"review_model"`
		BuildTools            string           `yaml:"build_tools"`
		ReviewCommand         string           `yaml:"review_command"`
		DefaultParallel       int              `yaml:"parallel"`
		DefaultReviewParallel int              `yaml:"parallel_reviews"`
		StartDelay            int              `yaml:"start_delay"`
		RunIdleTimeout        *int             `yaml:"run_idle_timeout"`
		Retries               *int             `yaml:"retries"`
		ContainerCapacity     *int             `yaml:"container_capacity"`
		MaxContainers         *int             `yaml:"max_containers"`
		AutoMaxCount          *int             `yaml:"auto_max_count"`
		WorktreeDir           string           `yaml:"worktree_dir"`
		Sandbox               string           `yaml:"sandbox"`
		Agents                map[string]Agent `yaml:"agents"`
		Git                   struct {
			BaseBranch   string  `yaml:"base_branch"`
			LegacyBranch *string `yaml:"default_branch"`
		} `yaml:"git"`
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg := Config{
		DefaultAgent:          raw.DefaultAgent,
		DefaultModel:          raw.DefaultModel,
		DefaultReviewAgent:    raw.DefaultReviewAgent,
		DefaultReviewModel:    raw.DefaultReviewModel,
		BuildTools:            raw.BuildTools,
		ReviewCommand:         raw.ReviewCommand,
		DefaultParallel:       raw.DefaultParallel,
		DefaultReviewParallel: raw.DefaultReviewParallel,
		StartDelay:            raw.StartDelay,
		WorktreeDir:           raw.WorktreeDir,
		Sandbox:               raw.Sandbox,
		Agents:                raw.Agents,
		Git:                   GitConfig{BaseBranch: raw.Git.BaseBranch},
	}

	if raw.Git.LegacyBranch != nil {
		return nil, fmt.Errorf("validate config: git.default_branch was renamed to git.base_branch")
	}

	if cfg.DefaultParallel <= 0 {
		cfg.DefaultParallel = DefaultParallel
	}
	if cfg.DefaultReviewParallel <= 0 {
		cfg.DefaultReviewParallel = DefaultReviewParallel
	}
	if cfg.StartDelay < 0 {
		return nil, fmt.Errorf("validate config: start_delay must be 0 or greater")
	}
	if raw.RunIdleTimeout == nil {
		cfg.RunIdleTimeout = DefaultRunIdleTimeout
	} else if *raw.RunIdleTimeout < 0 {
		return nil, fmt.Errorf("validate config: run_idle_timeout must be 0 or greater")
	} else {
		cfg.RunIdleTimeout = *raw.RunIdleTimeout
	}
	if raw.Retries == nil {
		cfg.Retries = DefaultRetries
	} else if *raw.Retries < 0 {
		return nil, fmt.Errorf("validate config: retries must be 0 or greater")
	} else {
		cfg.Retries = *raw.Retries
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
	if raw.AutoMaxCount == nil {
		cfg.AutoMaxCount = DefaultAutoMaxCount
	} else if *raw.AutoMaxCount < 0 {
		return nil, fmt.Errorf("validate config: auto_max_count must be 0 or greater")
	} else {
		cfg.AutoMaxCount = *raw.AutoMaxCount
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
	if strings.TrimSpace(cfg.DefaultReviewAgent) == "" {
		cfg.DefaultReviewAgent = DefaultReviewAgent
	}
	if strings.TrimSpace(cfg.DefaultReviewModel) == "" {
		cfg.DefaultReviewModel = DefaultReviewModel
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
	if _, err := cfg.ResolveAgentProvider(cfg.DefaultAgent); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

// Save writes the config to the given path as YAML.
//
// The write goes through a unique temp file (atomicfs.WriteAtomicJSON
// with a random suffix) and is committed via os.Rename. A process
// crash or interrupted write leaves the destination untouched — readers
// either see the previous-good file or the new file, never a torn mix.
// The temp file is chmod'd to 0600 so the persisted config keeps its
// owner-only mode.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return atomicfs.WriteAtomic(path, data, 0600)
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
	agent := Agent{
		Preset:       preset,
		Command:      p.Command,
		Env:          copyStringMap(p.Env),
		ConfigDirs:   append([]string(nil), p.ConfigDirs...),
		ConfigFiles:  append([]string(nil), p.ConfigFiles...),
		KeychainAuth: p.KeychainAuth,
	}
	if _, ok := p.Env["OPENCODE_PERMISSION"]; ok {
		agent.OpencodePermissionMode = "builtin"
	}
	return agent
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
		if agent.Env == nil {
			agent.Env = make(map[string]string, len(override.Env))
		}
		for k, v := range override.Env {
			agent.Env[k] = v
		}
		if _, ok := override.Env["OPENCODE_PERMISSION"]; ok {
			agent.OpencodePermissionMode = "custom"
		}
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

// GetValue returns the string representation of a config field by its dot-notation key.
func (c *Config) GetValue(key string) (string, error) {
	switch strings.ToLower(key) {
	case "agent":
		return c.DefaultAgent, nil
	case "model":
		return c.DefaultModel, nil
	case "review_agent":
		return c.EffectiveReviewAgent(), nil
	case "review_model":
		return c.EffectiveReviewModel(), nil
	case "build_tools":
		return c.EffectiveBuildTools(), nil
	case "review_command":
		return c.EffectiveReviewCommand(), nil
	case "parallel":
		return fmt.Sprintf("%d", c.DefaultParallel), nil
	case "parallel_reviews":
		return fmt.Sprintf("%d", c.EffectiveReviewParallel()), nil
	case "start_delay":
		return fmt.Sprintf("%d", c.StartDelay), nil
	case "run_idle_timeout":
		return fmt.Sprintf("%d", c.RunIdleTimeout), nil
	case "retries":
		return fmt.Sprintf("%d", c.Retries), nil
	case "container_capacity":
		return fmt.Sprintf("%d", c.ContainerCapacity), nil
	case "max_containers":
		return fmt.Sprintf("%d", c.MaxContainers), nil
	case "auto_max_count":
		return fmt.Sprintf("%d", c.AutoMaxCount), nil
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

type intSetField struct {
	name      string
	allowZero bool
	target    func(*Config) *int
}

var intSetFields = []intSetField{
	{name: "parallel", allowZero: false, target: func(c *Config) *int { return &c.DefaultParallel }},
	{name: "parallel_reviews", allowZero: false, target: func(c *Config) *int { return &c.DefaultReviewParallel }},
	{name: "start_delay", allowZero: true, target: func(c *Config) *int { return &c.StartDelay }},
	{name: "run_idle_timeout", allowZero: true, target: func(c *Config) *int { return &c.RunIdleTimeout }},
	{name: "retries", allowZero: true, target: func(c *Config) *int { return &c.Retries }},
	{name: "container_capacity", allowZero: true, target: func(c *Config) *int { return &c.ContainerCapacity }},
	{name: "max_containers", allowZero: true, target: func(c *Config) *int { return &c.MaxContainers }},
	{name: "auto_max_count", allowZero: true, target: func(c *Config) *int { return &c.AutoMaxCount }},
}

// SetValue updates a config field by its dot-notation key.
func (c *Config) SetValue(key, value string) error {
	normalized := strings.ToLower(key)
	for _, field := range intSetFields {
		if normalized != field.name {
			continue
		}
		return setIntField(c, field, value)
	}
	switch normalized {
	case "agent":
		if _, err := c.ResolveAgentProvider(strings.TrimSpace(value)); err != nil {
			return err
		}
		c.DefaultAgent = strings.TrimSpace(value)
		c.Agent = c.DefaultAgent
	case "model":
		c.DefaultModel = value
	case "review_agent":
		if _, err := c.ResolveAgentProvider(strings.TrimSpace(value)); err != nil {
			return err
		}
		c.DefaultReviewAgent = strings.TrimSpace(value)
	case "review_model":
		c.DefaultReviewModel = value
	case "build_tools":
		c.BuildTools = value
	case "review_command":
		c.ReviewCommand = value
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

func setIntField(c *Config, field intSetField, value string) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid value for %s: %w", field.name, err)
	}
	if field.allowZero {
		if n < 0 {
			return fmt.Errorf("%s must be 0 or greater", field.name)
		}
	} else if n <= 0 {
		return fmt.Errorf("%s must be greater than 0", field.name)
	}
	*field.target(c) = n
	return nil
}

// EffectiveBuildTools returns the configured BuildTools preset, defaulting to generic.
func (c *Config) EffectiveBuildTools() string {
	if c == nil || strings.TrimSpace(c.BuildTools) == "" {
		return DefaultBuildToolsPreset
	}
	return c.BuildTools
}

// EffectiveReviewCommand returns the configured review command, defaulting to /sandman review.
func (c *Config) EffectiveReviewCommand() string {
	if c == nil || strings.TrimSpace(c.ReviewCommand) == "" {
		return DefaultReviewCommand
	}
	return c.ReviewCommand
}

// EffectiveReviewAgent returns the configured review agent, falling back to
// DefaultAgent and finally the DefaultAgent constant.
func (c *Config) EffectiveReviewAgent() string {
	if c == nil {
		return DefaultAgent
	}
	if name := strings.TrimSpace(c.DefaultReviewAgent); name != "" {
		return name
	}
	if name := strings.TrimSpace(c.DefaultAgent); name != "" {
		return name
	}
	return DefaultAgent
}

// EffectiveReviewParallel returns the effective parallel_reviews value.
func (c *Config) EffectiveReviewParallel() int {
	if c == nil || c.DefaultReviewParallel <= 0 {
		return DefaultReviewParallel
	}
	return c.DefaultReviewParallel
}

// EffectiveReviewModel returns the configured review model, falling back to
// DefaultModel (which itself stays empty unless configured to preserve the
// per-agent default).
func (c *Config) EffectiveReviewModel() string {
	if c == nil {
		return ""
	}
	if model := strings.TrimSpace(c.DefaultReviewModel); model != "" {
		return model
	}
	return strings.TrimSpace(c.DefaultModel)
}
