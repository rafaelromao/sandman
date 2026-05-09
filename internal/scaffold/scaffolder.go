package scaffold

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/config"
)

// Options configures the scaffolding behavior.
type Options struct {
	Lang      string // --lang override
	FromImage string // --from-image override
}

// Prompter asks the user for confirmation or selection interactively.
type Prompter interface {
	Confirm(msg string) (bool, error)
	Select(msg string, options []string) (string, error)
}

var languageDetectors = []struct {
	file string
	lang string
}{
	{"go.mod", "go"},
	{"package.json", "node"},
	{"requirements.txt", "python"},
	{"Cargo.toml", "rust"},
}

var dockerfileTemplates = map[string]string{
	"go":     "FROM golang:latest\nWORKDIR /app\n",
	"node":   "FROM node:latest\nWORKDIR /app\n",
	"python": "FROM python:latest\nWORKDIR /app\n",
	"rust":   "FROM rust:latest\nWORKDIR /app\n",
}

// Scaffolder creates the .sandman/ directory and its files.
type Scaffolder struct{}

// Scaffold writes config.yaml, Dockerfile, and prompt.md into .sandman/.
func (s *Scaffolder) Scaffold(repoRoot string, opts Options, p Prompter) error {
	sandmanDir := filepath.Join(repoRoot, ".sandman")

	if info, err := os.Stat(sandmanDir); err == nil && info.IsDir() {
		ok, err := p.Confirm("Directory .sandman/ already exists. Overwrite?")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("init cancelled")
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat .sandman: %w", err)
	}

	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		return fmt.Errorf("create .sandman: %w", err)
	}

	cfg := &config.Config{
		Agent:           "opencode",
		DefaultParallel: 1,
		WorktreeDir:     ".sandman/worktrees",
		Sandbox:         "worktree",
	}

	configPath := filepath.Join(sandmanDir, "config.yaml")
	if err := config.Save(configPath, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	lang, err := s.resolveLanguage(repoRoot, opts, p)
	if err != nil {
		return err
	}

	dockerfile := s.renderDockerfile(lang, opts.FromImage)
	dockerfilePath := filepath.Join(sandmanDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}

	promptPath := filepath.Join(sandmanDir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte{}, 0644); err != nil {
		return fmt.Errorf("write prompt.md: %w", err)
	}

	return nil
}

func (s *Scaffolder) resolveLanguage(repoRoot string, opts Options, p Prompter) (string, error) {
	if opts.Lang != "" {
		return opts.Lang, nil
	}

	var detected []string
	for _, d := range languageDetectors {
		path := filepath.Join(repoRoot, d.file)
		if _, err := os.Stat(path); err == nil {
			detected = append(detected, d.lang)
		}
	}

	if len(detected) == 1 {
		return detected[0], nil
	}

	if len(detected) > 1 {
		return p.Select("Multiple languages detected. Choose one:", detected)
	}

	return p.Select("No language detected. Choose one:", []string{"go", "node", "python", "rust"})
}

func (s *Scaffolder) renderDockerfile(lang, fromImage string) string {
	if fromImage != "" {
		return fmt.Sprintf("FROM %s\nWORKDIR /app\n", fromImage)
	}
	tmpl, ok := dockerfileTemplates[lang]
	if !ok {
		return "FROM ubuntu:latest\nWORKDIR /app\n"
	}
	return tmpl
}
