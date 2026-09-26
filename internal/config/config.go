package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"gopkg.in/yaml.v3"
)

// ErrBreakingContract marks config errors that require an explicit migration
// decision before init may replace the file.
var ErrBreakingContract = errors.New("config contract migration required")

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
	DefaultRunIdleTimeout    = 3600
	DefaultReviewTimeout     = 1800
	MinReviewTimeout         = 240
	DefaultRetries           = 3
	DefaultContainerCapacity = 4
	DefaultMaxContainers     = 0
	DefaultWorktreeDir       = ".sandman/worktrees"
	DefaultSandbox           = "podman"
	DefaultCleanupWorktrees  = true
)

// Config holds the loaded Sandman configuration.
type Config struct {
	DefaultAgent          string           `yaml:"agent"`
	DefaultModel          string           `yaml:"model"`
	Variant               string           `yaml:"variant"`
	DefaultReviewAgent    string           `yaml:"review_agent"`
	DefaultReviewModel    string           `yaml:"review_model"`
	ReviewVariant         string           `yaml:"review_variant"`
	BuildTools            string           `yaml:"build_tools"`
	ReviewCommand         string           `yaml:"review_command"`
	DefaultParallel       int              `yaml:"parallel"`
	DefaultReviewParallel int              `yaml:"parallel_reviews"`
	StartDelay            int              `yaml:"start_delay"`
	RunIdleTimeout        int              `yaml:"run_idle_timeout"`
	ReviewTimeout         int              `yaml:"review_timeout,omitempty"`
	ContextErrorPhrases   []string         `yaml:"context_error_phrases,omitempty"`
	Retries               int              `yaml:"retries"`
	ContainerCapacity     int              `yaml:"container_capacity"`
	MaxContainers         int              `yaml:"max_containers"`
	WorktreeDir           string           `yaml:"worktree_dir"`
	Sandbox               string           `yaml:"sandbox"`
	CleanupWorktrees      *bool            `yaml:"cleanup_worktrees,omitempty"`
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
//
// Presets hold data only. Agent-specific run-loop behaviour (flags, session
// reuse, output parsing, failure classification) lives behind the agent
// strategy seam in the batch package, keyed by the same preset name.
type AgentPreset struct {
	DisplayName string
	// DefaultModel is the model a run of this preset uses when neither the
	// command line nor config names one for it; see DefaultModelForAgent.
	DefaultModel     string
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
		DisplayName:  "OpenCode",
		DefaultModel: DefaultModel,
		Command:      `opencode run --format json{{if .ContinueFlag}} --continue{{end}}{{if .SessionFlag}} --session {{.SessionFlag}}{{end}}{{if .DangerouslySkipPermissions}} --dangerously-skip-permissions{{end}}{{if .SessionName}} --title '{{.SessionName}}'{{end}}{{if .ModelFlag}} {{.ModelFlag}}{{end}}{{if .VariantFlag}} {{.VariantFlag}}{{end}} "$(cat {{.PromptFile}})"`,
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
	// The claude preset runs the unmodified Claude Code CLI in print mode.
	// stream-json with --verbose is the only print-mode format that emits
	// output while the agent works, which the idle-timeout heartbeat needs.
	// --continue resumes the worktree's most recent conversation when
	// Sandman selects session reuse, and --name puts the Run's session name
	// on the command line for the lingering-process check.
	"claude": {
		DisplayName:  "Claude Code",
		DefaultModel: "sonnet",
		Command:      `claude -p --output-format stream-json --verbose{{if .ContinueFlag}} --continue{{end}}{{if .DangerouslySkipPermissions}} --dangerously-skip-permissions{{end}}{{if .SessionName}} --name '{{.SessionName}}'{{end}}{{if .ModelFlag}} {{.ModelFlag}}{{end}}{{if .VariantFlag}} {{.VariantFlag}}{{end}} "$(cat {{.PromptFile}})"`,
		Env: map[string]string{
			"DISABLE_AUTOUPDATER":                      "1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			// Print mode otherwise kills background tasks still running 600s
			// after the main turn ends. Sandman's idle timeout and retries
			// already bound a run whose background work hangs.
			"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS": "0",
			// Containers run the agent as root under podman. Claude Code
			// refuses --dangerously-skip-permissions as root unless it runs
			// inside a recognized sandbox.
			"IS_SANDBOX": "1",
		},
		// The container runs with HOME=/, so Claude Code's default config
		// locations resolve to /.claude and /.claude.json, exactly where these
		// host paths are mounted. ~/.agents carries the shared Sandman skill.
		ConfigDirs:  []string{"~/.claude", "~/.agents"},
		ConfigFiles: []string{"~/.claude.json"},
		// Transcripts, caches, and logs stay out of the per-batch snapshot.
		// Credentials, settings, skills, agents, commands, plugins, and
		// CLAUDE.md are copied on purpose. The container writes its own
		// transcripts into the batch snapshot, which is what lets an await
		// re-entry --continue the same conversation.
		SnapshotExcludes: []string{
			"~/.claude/projects",
			"~/.claude/debug",
			"~/.claude/shell-snapshots",
			"~/.claude/todos",
			"~/.claude/statsig",
			"~/.claude/cache",
			"~/.claude/backups",
			"~/.claude/file-history",
			"~/.claude/session-env",
			"~/.claude/local",
			"~/.claude/downloads",
			"~/.claude/telemetry",
			"~/.claude/paste-cache",
			"~/.claude/ide",
			"~/.claude/usage-data",
		},
	},
}

// DefaultModelForAgent returns the default model of the named built-in agent
// preset, or "" when the name is not a built-in preset.
func DefaultModelForAgent(agent string) string {
	return BuiltInAgentPresets[strings.TrimSpace(agent)].DefaultModel
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
		"variant",
		"review_agent",
		"review_model",
		"review_variant",
		"build_tools",
		"review_command",
		"parallel",
		"parallel_reviews",
		"start_delay",
		"run_idle_timeout",
		"review_timeout",
		"retries",
		"container_capacity",
		"max_containers",
		"worktree_dir",
		"sandbox",
		"cleanup_worktrees",
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
		Variant               string           `yaml:"variant"`
		DefaultReviewAgent    string           `yaml:"review_agent"`
		DefaultReviewModel    string           `yaml:"review_model"`
		ReviewVariant         string           `yaml:"review_variant"`
		BuildTools            string           `yaml:"build_tools"`
		ReviewCommand         string           `yaml:"review_command"`
		DefaultParallel       int              `yaml:"parallel"`
		DefaultReviewParallel int              `yaml:"parallel_reviews"`
		StartDelay            int              `yaml:"start_delay"`
		RunIdleTimeout        *int             `yaml:"run_idle_timeout"`
		ReviewTimeout         yaml.Node        `yaml:"review_timeout"`
		ContextErrorPhrases   []string         `yaml:"context_error_phrases"`
		Retries               *int             `yaml:"retries"`
		ContainerCapacity     *int             `yaml:"container_capacity"`
		MaxContainers         *int             `yaml:"max_containers"`
		WorktreeDir           string           `yaml:"worktree_dir"`
		Sandbox               string           `yaml:"sandbox"`
		CleanupWorktrees      *bool            `yaml:"cleanup_worktrees"`
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
		Variant:               strings.TrimSpace(raw.Variant),
		DefaultReviewAgent:    raw.DefaultReviewAgent,
		DefaultReviewModel:    raw.DefaultReviewModel,
		ReviewVariant:         strings.TrimSpace(raw.ReviewVariant),
		BuildTools:            raw.BuildTools,
		ReviewCommand:         raw.ReviewCommand,
		ContextErrorPhrases:   raw.ContextErrorPhrases,
		DefaultParallel:       raw.DefaultParallel,
		DefaultReviewParallel: raw.DefaultReviewParallel,
		StartDelay:            raw.StartDelay,
		WorktreeDir:           raw.WorktreeDir,
		Sandbox:               raw.Sandbox,
		CleanupWorktrees:      raw.CleanupWorktrees,
		Agents:                raw.Agents,
		Git:                   GitConfig{BaseBranch: raw.Git.BaseBranch},
	}

	if raw.Git.LegacyBranch != nil {
		return nil, fmt.Errorf("validate config: %w: git.default_branch was renamed to git.base_branch", ErrBreakingContract)
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
	reviewTimeout, err := parseReviewTimeout(&raw.ReviewTimeout)
	if err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	cfg.ReviewTimeout = reviewTimeout
	if cfg.ContextErrorPhrases, err = normalizeContextErrorPhrases(cfg.ContextErrorPhrases); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
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
		cfg.DefaultReviewModel = cfg.defaultReviewModelFor(cfg.DefaultReviewAgent)
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

// ValidateReviewTimeout validates the delegated review response budget in seconds.
func ValidateReviewTimeout(value int) error {
	if value < MinReviewTimeout {
		return fmt.Errorf("review_timeout must be at least %d seconds", MinReviewTimeout)
	}
	return nil
}

func parseReviewTimeout(node *yaml.Node) (int, error) {
	if node == nil || node.Kind == 0 {
		return DefaultReviewTimeout, nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return 0, fmt.Errorf("review_timeout must be an integer number of seconds (minimum %d)", MinReviewTimeout)
	}
	value, err := strconv.Atoi(node.Value)
	if err != nil {
		return 0, fmt.Errorf("review_timeout must be an integer number of seconds (minimum %d)", MinReviewTimeout)
	}
	if err := ValidateReviewTimeout(value); err != nil {
		return 0, err
	}
	return value, nil
}

func normalizeContextErrorPhrases(phrases []string) ([]string, error) {
	if len(phrases) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(phrases))
	seen := make(map[string]struct{}, len(phrases))
	for _, phrase := range phrases {
		phrase = strings.ToLower(strings.TrimSpace(phrase))
		if phrase == "" {
			return nil, fmt.Errorf("context_error_phrases cannot contain an empty phrase")
		}
		if _, ok := seen[phrase]; ok {
			return nil, fmt.Errorf("context_error_phrases cannot contain duplicate phrase %q", phrase)
		}
		seen[phrase] = struct{}{}
		normalized = append(normalized, phrase)
	}
	return normalized, nil
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
			return resolveAgentOverride(name, agent)
		}
	}

	if c != nil && c.Agents != nil {
		if agent, ok := c.Agents[name]; ok {
			return resolveAgentOverride(name, agent)
		}
	}

	if preset, ok := BuiltInAgentPresets[name]; ok {
		return preset.Agent(name), nil
	}

	return Agent{}, fmt.Errorf("agent %q not found in config", name)
}

func resolveAgentOverride(name string, agent Agent) (Agent, error) {
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
	case "variant":
		return strings.TrimSpace(c.Variant), nil
	case "review_agent":
		return c.EffectiveReviewAgent(), nil
	case "review_model":
		return c.EffectiveReviewModel(), nil
	case "review_variant":
		return c.EffectiveReviewVariant(), nil
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
	case "review_timeout":
		return fmt.Sprintf("%d", c.EffectiveReviewTimeout()), nil
	case "retries":
		return fmt.Sprintf("%d", c.Retries), nil
	case "container_capacity":
		return fmt.Sprintf("%d", c.ContainerCapacity), nil
	case "max_containers":
		return fmt.Sprintf("%d", c.MaxContainers), nil
	case "worktree_dir":
		return c.WorktreeDir, nil
	case "sandbox":
		return c.Sandbox, nil
	case "cleanup_worktrees":
		return strconv.FormatBool(c.EffectiveCleanupWorktrees()), nil
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
	minimum   int
	target    func(*Config) *int
}

var intSetFields = []intSetField{
	{name: "parallel", allowZero: false, target: func(c *Config) *int { return &c.DefaultParallel }},
	{name: "parallel_reviews", allowZero: false, target: func(c *Config) *int { return &c.DefaultReviewParallel }},
	{name: "start_delay", allowZero: true, target: func(c *Config) *int { return &c.StartDelay }},
	{name: "run_idle_timeout", allowZero: true, target: func(c *Config) *int { return &c.RunIdleTimeout }},
	{name: "review_timeout", minimum: MinReviewTimeout, target: func(c *Config) *int { return &c.ReviewTimeout }},
	{name: "retries", allowZero: true, target: func(c *Config) *int { return &c.Retries }},
	{name: "container_capacity", allowZero: true, target: func(c *Config) *int { return &c.ContainerCapacity }},
	{name: "max_containers", allowZero: true, target: func(c *Config) *int { return &c.MaxContainers }},
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
	case "variant":
		c.Variant = strings.TrimSpace(value)
	case "review_agent":
		if _, err := c.ResolveAgentProvider(strings.TrimSpace(value)); err != nil {
			return err
		}
		c.DefaultReviewAgent = strings.TrimSpace(value)
	case "review_model":
		c.DefaultReviewModel = value
	case "review_variant":
		c.ReviewVariant = strings.TrimSpace(value)
	case "build_tools":
		c.BuildTools = value
	case "review_command":
		c.ReviewCommand = value
	case "worktree_dir":
		c.WorktreeDir = value
	case "sandbox":
		c.Sandbox = value
	case "cleanup_worktrees":
		cleanup, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid value for cleanup_worktrees: %w", err)
		}
		c.CleanupWorktrees = &cleanup
	case "git.base_branch":
		c.Git.BaseBranch = value
	case "git.default_branch":
		return fmt.Errorf("git.default_branch was renamed to git.base_branch")
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}

// EffectiveCleanupWorktrees returns whether successful implementation worktrees
// are removed after a run. The default preserves the historical behavior.
func (c *Config) EffectiveCleanupWorktrees() bool {
	if c == nil || c.CleanupWorktrees == nil {
		return DefaultCleanupWorktrees
	}
	return *c.CleanupWorktrees
}

func setIntField(c *Config, field intSetField, value string) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid value for %s: %w", field.name, err)
	}
	if field.minimum > 0 && n < field.minimum {
		return fmt.Errorf("%s must be at least %d seconds", field.name, field.minimum)
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

// EffectiveReviewTimeout returns the delegated review response budget in seconds.
func (c *Config) EffectiveReviewTimeout() int {
	if c == nil || c.ReviewTimeout < MinReviewTimeout {
		return DefaultReviewTimeout
	}
	return c.ReviewTimeout
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

// defaultReviewModelFor returns the review model used when review_model is
// unset: the default model of the review agent's built-in preset. Agents
// without a preset default keep the historical DefaultReviewModel.
func (c *Config) defaultReviewModelFor(reviewAgent string) string {
	agent, err := c.ResolveAgentProvider(strings.TrimSpace(reviewAgent))
	if err == nil {
		if model := DefaultModelForAgent(agent.Preset); model != "" {
			return model
		}
	}
	return DefaultReviewModel
}

// EffectiveReviewVariant returns the configured review model variant, or an
// empty string when no review-specific variant is configured.
func (c *Config) EffectiveReviewVariant() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.ReviewVariant)
}
