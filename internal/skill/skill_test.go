package skill

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallWritesEmbeddedSkill(t *testing.T) {
	home := t.TempDir()
	reviewCommand := "/review-please"

	if err := Install(home, reviewCommand); err != nil {
		t.Fatalf("install skill: %v", err)
	}

	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	var checked int
	err := fs.WalkDir(embeddedSkills, embeddedSkillRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel := strings.TrimPrefix(path, embeddedSkillRoot+"/")
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}

		if bytes.Contains(got, []byte("{{REVIEW_COMMAND}}")) {
			t.Fatalf("installed file %s still contains unreplaced {{REVIEW_COMMAND}}", rel)
		}

		if bytes.Contains(got, []byte(reviewCommand)) {
			checked++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk installed skill tree: %v", err)
	}
	if checked == 0 {
		t.Fatal("expected embedded skill files to be installed")
	}
}

func TestInstallLeavesExistingSkillUntouched(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", "sandman", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("custom skill"), 0o644); err != nil {
		t.Fatalf("seed existing skill: %v", err)
	}

	if err := Install(home, "/review-please"); err != nil {
		t.Fatalf("install skill: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read existing skill: %v", err)
	}
	if string(got) != "custom skill" {
		t.Fatalf("expected existing skill to stay untouched, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "implement", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("expected install to skip existing skill tree, got err=%v", err)
	}
}
