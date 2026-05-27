package skill

import (
	"bufio"
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

type SyncOptions struct {
	HomeDir       string
	ReviewCommand string
	In            io.Reader
	Out           io.Writer
	Interactive   bool
}

// Sync writes the embedded Sandman skill into the shared agent skills directory,
// substituting {{REVIEW_COMMAND}} with the given reviewCommand value.
func Sync(opts SyncOptions) error {
	homeDir := strings.TrimSpace(opts.HomeDir)
	if homeDir == "" {
		return fmt.Errorf("home dir required")
	}

	targetDir := filepath.Join(homeDir, ".agents", "skills", embeddedSkillRoot)
	exists, err := pathExists(targetDir)
	if err != nil {
		return fmt.Errorf("check skill directory: %w", err)
	}
	if exists {
		managed, err := matchesManagedTree(targetDir)
		if err != nil {
			return err
		}
		if !managed {
			if !opts.Interactive {
				return fmt.Errorf("shared sandman skill at %q has local edits; rerun in a TTY to confirm overwrite", targetDir)
			}
			ok, err := confirmOverwrite(opts.In, opts.Out, fmt.Sprintf("Shared sandman skill at %s has local edits. Overwrite?", targetDir))
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("skill sync cancelled")
			}
		}
		if err := os.RemoveAll(targetDir); err != nil {
			return fmt.Errorf("remove existing skill tree %q: %w", targetDir, err)
		}
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
		data = bytes.ReplaceAll(data, []byte("{{REVIEW_COMMAND}}"), []byte(opts.ReviewCommand))
		if err := os.WriteFile(targetPath, data, 0o644); err != nil {
			return fmt.Errorf("write skill file %q: %w", targetPath, err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func matchesManagedTree(targetDir string) (bool, error) {
	expectedFiles, err := embeddedFileSet()
	if err != nil {
		return false, err
	}
	actualFiles, err := diskFileSet(targetDir)
	if err != nil {
		return false, fmt.Errorf("walk installed skill tree: %w", err)
	}
	if len(expectedFiles) != len(actualFiles) {
		return false, nil
	}
	for _, rel := range expectedFiles {
		if _, ok := actualFiles[rel]; !ok {
			return false, nil
		}
		tmpl, err := fs.ReadFile(embeddedSkills, filepath.ToSlash(filepath.Join(embeddedSkillRoot, rel)))
		if err != nil {
			return false, fmt.Errorf("read embedded skill file %q: %w", rel, err)
		}
		actual, err := os.ReadFile(filepath.Join(targetDir, rel))
		if err != nil {
			return false, fmt.Errorf("read installed skill file %q: %w", rel, err)
		}
		if !matchesTemplate(actual, tmpl) {
			return false, nil
		}
	}
	return true, nil
}

func embeddedFileSet() ([]string, error) {
	var files []string
	err := fs.WalkDir(embeddedSkills, embeddedSkillRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, embeddedSkillRoot+"/")
		files = append(files, filepath.FromSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk embedded skill tree: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

func diskFileSet(targetDir string) (map[string]struct{}, error) {
	files := map[string]struct{}{}
	err := filepath.WalkDir(targetDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(targetDir, path)
		if err != nil {
			return err
		}
		files[rel] = struct{}{}
		return nil
	})
	return files, err
}

func matchesTemplate(actual, tmpl []byte) bool {
	const placeholder = "{{REVIEW_COMMAND}}"
	text := string(tmpl)
	if !strings.Contains(text, placeholder) {
		return bytes.Equal(actual, tmpl)
	}
	parts := strings.Split(text, placeholder)
	remaining := string(actual)
	for i, part := range parts {
		if i == 0 {
			if !strings.HasPrefix(remaining, part) {
				return false
			}
			remaining = strings.TrimPrefix(remaining, part)
			continue
		}
		if i == len(parts)-1 {
			return strings.HasSuffix(remaining, part)
		}
		idx := strings.Index(remaining, part)
		if idx < 0 {
			return false
		}
		remaining = remaining[idx+len(part):]
	}
	return true
}

func confirmOverwrite(in io.Reader, out io.Writer, msg string) (bool, error) {
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintf(out, "%s [y/N]: ", msg)
	if in == nil {
		return false, fmt.Errorf("interactive confirmation requires input")
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes", nil
}
