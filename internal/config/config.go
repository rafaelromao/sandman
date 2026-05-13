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
	DefaultParallel          = 4
	DefaultContainerCapacity = 4
	DefaultMaxContainers     = 0
	DefaultWorktreeDir       = ".sandman/worktrees"
	DefaultSandbox           = "podman"
)

// Config holds the loaded Sandman configuration.
type Config struct {
	Agent             string           `yaml:"agent"`
	DefaultParallel   int              `yaml:"default_parallel"`
	ContainerCapacity int              `yaml:"container_capacity"`
	MaxContainers     int              `yaml:"max_containers"`
	WorktreeDir       string           `yaml:"worktree_dir"`
	Sandbox           string           `yaml:"sandbox"`
	Git               GitConfig        `yaml:"git"`
	AgentProviders    map[string]Agent `yaml:"agents"`
}

// GitConfig holds git-specific settings.
type GitConfig struct {
	DefaultBranch string `yaml:"default_branch"`
	AuthorName    string `yaml:"author_name"`
	AuthorEmail   string `yaml:"author_email"`
}

// Agent holds a configured agent provider.
type Agent struct {
	Name         string            `yaml:"name"`
	Command      string            `yaml:"command"`
	Env          map[string]string `yaml:"env"`
	ConfigDirs   []string          `yaml:"config_dirs"`
	KeychainAuth bool              `yaml:"keychain_auth"`
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
		Agent             string           `yaml:"agent"`
		DefaultParallel   int              `yaml:"default_parallel"`
		ContainerCapacity *int             `yaml:"container_capacity"`
		MaxContainers     *int             `yaml:"max_containers"`
		WorktreeDir       string           `yaml:"worktree_dir"`
		Sandbox           string           `yaml:"sandbox"`
		Git               GitConfig        `yaml:"git"`
		AgentProviders    map[string]Agent `yaml:"agents"`
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg := Config{
		Agent:           raw.Agent,
		DefaultParallel: raw.DefaultParallel,
		WorktreeDir:     raw.WorktreeDir,
		Sandbox:         raw.Sandbox,
		Git:             raw.Git,
		AgentProviders:  raw.AgentProviders,
	}

	if cfg.DefaultParallel <= 0 {
		cfg.DefaultParallel = DefaultParallel
	}
	if raw.ContainerCapacity == nil {
		cfg.ContainerCapacity = DefaultContainerCapacity
	} else if *raw.ContainerCapacity <= 0 {
		return nil, fmt.Errorf("validate config: container_capacity must be at least 1")
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
	if cfg.Git.DefaultBranch == "" {
		cfg.Git.DefaultBranch = "main"
	}

	if cfg.Agent == "" {
		return nil, fmt.Errorf("validate config: agent is required")
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

// GetValue returns the string representation of a config field by its dot-notation key.
func (c *Config) GetValue(key string) (string, error) {
	switch strings.ToLower(key) {
	case "agent":
		return c.Agent, nil
	case "default_parallel":
		return fmt.Sprintf("%d", c.DefaultParallel), nil
	case "container_capacity":
		return fmt.Sprintf("%d", c.ContainerCapacity), nil
	case "max_containers":
		return fmt.Sprintf("%d", c.MaxContainers), nil
	case "worktree_dir":
		return c.WorktreeDir, nil
	case "sandbox":
		return c.Sandbox, nil
	case "git.default_branch":
		return c.Git.DefaultBranch, nil
	case "git.author_name":
		return c.Git.AuthorName, nil
	case "git.author_email":
		return c.Git.AuthorEmail, nil
	default:
		return "", fmt.Errorf("unknown config key: %s", key)
	}
}

// SetValue updates a config field by its dot-notation key.
func (c *Config) SetValue(key, value string) error {
	switch strings.ToLower(key) {
	case "agent":
		c.Agent = value
	case "default_parallel":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid value for default_parallel: %w", err)
		}
		if n <= 0 {
			return fmt.Errorf("default_parallel must be greater than 0")
		}
		c.DefaultParallel = n
	case "container_capacity":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid value for container_capacity: %w", err)
		}
		if n <= 0 {
			return fmt.Errorf("container_capacity must be at least 1")
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
	case "git.default_branch":
		c.Git.DefaultBranch = value
	case "git.author_name":
		c.Git.AuthorName = value
	case "git.author_email":
		c.Git.AuthorEmail = value
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}
