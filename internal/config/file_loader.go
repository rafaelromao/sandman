package config

import "fmt"

// FileLoader loads config from a YAML file path.
type FileLoader struct {
	Path string
}

// Load reads and parses the config file.
func (l *FileLoader) Load() (*Config, error) {
	return nil, fmt.Errorf("config loading not yet implemented")
}

// Ensure FileLoader implements Loader.
var _ Loader = (*FileLoader)(nil)
