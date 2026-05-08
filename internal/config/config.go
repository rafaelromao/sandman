package config

// Config holds the loaded Sandman configuration.
type Config struct {
	Agent           string            `yaml:"agent"`
	DefaultParallel int               `yaml:"default_parallel"`
	WorktreeDir     string            `yaml:"worktree_dir"`
	PRTemplate      string            `yaml:"pr_template"`
	Sandbox         string            `yaml:"sandbox"`
	Git             GitConfig         `yaml:"git"`
	AgentProviders  map[string]Agent  `yaml:"agents"`
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
