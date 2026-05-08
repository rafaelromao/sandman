package config

// FileLoader loads config from a YAML file path.
type FileLoader struct {
	Path string
}

// Load reads and parses the config file.
func (l *FileLoader) Load() (*Config, error) {
	return Load(l.Path)
}

// Ensure FileLoader implements Loader.
var _ Loader = (*FileLoader)(nil)
