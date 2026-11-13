package skill

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSkillsCLIConfigDiscoversSandmanCatalog(t *testing.T) {
	configPath := filepath.Join("..", "..", "skills.sh.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read skills CLI config: %v", err)
	}
	var config struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse skills CLI config: %v", err)
	}

	for _, want := range []string{"./internal/skill", "./internal/skill/sandman"} {
		found := false
		for _, path := range config.Paths {
			if path == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("skills CLI paths = %v, want %q", config.Paths, want)
		}
	}

	repoRoot := filepath.Join("..", "..")
	sourceRoot := filepath.Join(repoRoot, "internal", "skill")
	manifestCount := 0
	err = filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		discoverable, err := skillManifestDiscoverable(repoRoot, path, config.Paths)
		if err != nil {
			return err
		}
		if !discoverable {
			return fmt.Errorf("skill manifest %q is not discoverable through skills.sh.json", path)
		}
		manifestCount++
		return nil
	})
	if err != nil {
		t.Fatalf("walk skills CLI source: %v", err)
	}
	if manifestCount < 2 {
		t.Fatalf("skills CLI source has %d manifests, want root and nested workflow skills", manifestCount)
	}
}

func skillManifestDiscoverable(repoRoot, manifestPath string, configuredPaths []string) (bool, error) {
	for _, configuredPath := range configuredPaths {
		if !strings.HasPrefix(configuredPath, "./") {
			return false, fmt.Errorf("skills CLI path %q must start with ./", configuredPath)
		}
		containerPath := filepath.Join(repoRoot, filepath.FromSlash(strings.TrimPrefix(configuredPath, "./")))
		rel, err := filepath.Rel(containerPath, manifestPath)
		if err != nil {
			return false, err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 2 || len(parts)-1 > 3 {
			continue
		}

		current := containerPath
		shadowed := false
		for index, part := range parts[:len(parts)-1] {
			if index > 0 {
				if _, err := os.Stat(filepath.Join(current, "SKILL.md")); err == nil {
					shadowed = true
					break
				} else if !os.IsNotExist(err) {
					return false, err
				}
			}
			current = filepath.Join(current, filepath.FromSlash(part))
		}
		if !shadowed {
			return true, nil
		}
	}
	return false, nil
}

func TestSkillsCLIManifestsHaveValidMetadata(t *testing.T) {
	checked := 0
	err := fs.WalkDir(embeddedSkills, embeddedSkillRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		data, err := fs.ReadFile(embeddedSkills, path)
		if err != nil {
			return err
		}
		frontmatter, ok := skillFrontmatter(data)
		if !ok {
			return fmt.Errorf("skill manifest %q has no YAML frontmatter", path)
		}
		var metadata struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
		}
		if err := yaml.Unmarshal(frontmatter, &metadata); err != nil {
			return fmt.Errorf("parse skill manifest %q: %w", path, err)
		}
		if strings.TrimSpace(metadata.Name) == "" || strings.TrimSpace(metadata.Description) == "" {
			return fmt.Errorf("skill manifest %q must define name and description", path)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("expected skill manifests to validate")
	}
}

func skillFrontmatter(data []byte) ([]byte, bool) {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return nil, false
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, false
	}
	return []byte(rest[:end]), true
}

func TestSkillsGuideDocumentsIndependentInstall(t *testing.T) {
	guidePath := filepath.Join("..", "..", "docs", "usage", "skills.md")
	data, err := os.ReadFile(guidePath)
	if err != nil {
		t.Fatalf("read skills guide: %v", err)
	}
	guide := string(data)
	for _, want := range []string{
		"install-skills.sh",
		"--review-command 'YOUR COMMAND'",
		"@codex review",
		"@coderabbitai review",
		"/oc review",
		"Node.js 22.20",
		"--global",
		"does not install or run the Sandman",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("skills guide is missing independent-install detail %q", want)
		}
	}
}
