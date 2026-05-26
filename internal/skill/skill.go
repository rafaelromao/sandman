package skill

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed sandman/SKILL.md
var defaultSkill string

// DefaultSkill returns the embedded Sandman skill definition.
func DefaultSkill() string { return defaultSkill }

// Install writes the embedded Sandman skill into the shared agent skills directory.
func Install(homeDir string) error {
	if homeDir == "" {
		return fmt.Errorf("home dir required")
	}

	target := filepath.Join(homeDir, ".agents", "skills", "sandman", "SKILL.md")
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check skill file: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create skill directory: %w", err)
	}
	if err := os.WriteFile(target, []byte(DefaultSkill()), 0o644); err != nil {
		return fmt.Errorf("write skill file: %w", err)
	}
	return nil
}

// InstallDefault writes the embedded skill into the current user's shared skills directory.
func InstallDefault() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	return Install(homeDir)
}
