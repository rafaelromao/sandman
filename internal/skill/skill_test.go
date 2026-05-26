package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallWritesEmbeddedSkill(t *testing.T) {
	home := t.TempDir()

	if err := Install(home); err != nil {
		t.Fatalf("install skill: %v", err)
	}

	path := filepath.Join(home, ".agents", "skills", "sandman", "SKILL.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed skill: %v", err)
	}

	if string(got) != DefaultSkill() {
		t.Fatalf("installed skill mismatch\nwant:\n%s\ngot:\n%s", DefaultSkill(), got)
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

	if err := Install(home); err != nil {
		t.Fatalf("install skill: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read existing skill: %v", err)
	}
	if string(got) != "custom skill" {
		t.Fatalf("expected existing skill to stay untouched, got %q", got)
	}
}
