package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds the loaded Sandman configuration.
type Config struct {
	Agent           string           `yaml:"agent"`
	DefaultParallel int              `yaml:"default_parallel"`
	WorktreeDir     string           `yaml:"worktree_dir"`
	PRTemplate      string           `yaml:"pr_template"`
	Sandbox         string           `yaml:"sandbox"`
	Git             GitConfig        `yaml:"git"`
	AgentProviders  map[string]Agent `yaml:"agents"`
}

// GitConfig holds git-specific settings.
type GitConfig struct {
	AuthorName  string `yaml:"author_name"`
	AuthorEmail string `yaml:"author_email"`
}

// Agent holds a configured agent provider.
type Agent struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Env     map[string]string `yaml:"env"`
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

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.DefaultParallel <= 0 {
		cfg.DefaultParallel = 1
	}
	if cfg.WorktreeDir == "" {
		cfg.WorktreeDir = ".sandman/worktrees"
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = "worktree"
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
	case "worktree_dir":
		return c.WorktreeDir, nil
	case "pr_template":
		return c.PRTemplate, nil
	case "sandbox":
		return c.Sandbox, nil
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
	case "worktree_dir":
		c.WorktreeDir = value
	case "pr_template":
		c.PRTemplate = value
	case "sandbox":
		c.Sandbox = value
	case "git.author_name":
		c.Git.AuthorName = value
	case "git.author_email":
		c.Git.AuthorEmail = value
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}
