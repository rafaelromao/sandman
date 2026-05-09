package config

// FileStore loads and saves config from a YAML file path.
type FileStore struct {
	Path string
}

// Load reads and parses the config file.
func (s *FileStore) Load() (*Config, error) {
	return Load(s.Path)
}

// Save writes the config to the file.
func (s *FileStore) Save(cfg *Config) error {
	return Save(s.Path, cfg)
}

// Ensure FileStore implements Store.
var _ Store = (*FileStore)(nil)
