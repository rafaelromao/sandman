package config

import (
	"fmt"
	"os"

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

// Loader loads and validates Sandman configuration.
type Loader interface {
	Load() (*Config, error)
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
