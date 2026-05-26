package skill

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const embeddedSkillRoot = "sandman"

//go:embed sandman/**
var embeddedSkills embed.FS

// DefaultSkill returns the embedded Sandman skill definition.
func DefaultSkill() string {
	data, err := fs.ReadFile(embeddedSkills, embeddedSkillRoot+"/SKILL.md")
	if err != nil {
		panic(fmt.Sprintf("read embedded skill: %v", err))
	}
	return string(data)
}

// Install writes the embedded Sandman skill into the shared agent skills directory.
func Install(homeDir string) error {
	if homeDir == "" {
		return fmt.Errorf("home dir required")
	}

	targetDir := filepath.Join(homeDir, ".agents", "skills", embeddedSkillRoot)
	targetSkill := filepath.Join(targetDir, "SKILL.md")
	if _, err := os.Stat(targetSkill); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check skill file: %w", err)
	}

	if err := fs.WalkDir(embeddedSkills, embeddedSkillRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk embedded skill %q: %w", path, err)
		}

		rel := strings.TrimPrefix(path, embeddedSkillRoot)
		rel = strings.TrimPrefix(rel, "/")

		targetPath := targetDir
		if rel != "" {
			targetPath = filepath.Join(targetDir, filepath.FromSlash(rel))
		}

		if d.IsDir() {
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return fmt.Errorf("create skill directory %q: %w", targetPath, err)
			}
			return nil
		}

		data, err := fs.ReadFile(embeddedSkills, path)
		if err != nil {
			return fmt.Errorf("read embedded skill file %q: %w", path, err)
		}
		if err := os.WriteFile(targetPath, data, 0o644); err != nil {
			return fmt.Errorf("write skill file %q: %w", targetPath, err)
		}
		return nil
	}); err != nil {
		return err
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
