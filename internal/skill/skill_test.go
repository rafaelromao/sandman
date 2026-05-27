package skill

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncWritesEmbeddedSkill(t *testing.T) {
	home := t.TempDir()
	reviewCommand := "/review-please"

	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: reviewCommand}); err != nil {
		t.Fatalf("sync skill: %v", err)
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

func TestSyncOverwritesManagedTreeWithoutPrompt(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/old-review"}); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/new-review"}); err != nil {
		t.Fatalf("resync skill: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "delegate-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read delegate-review skill: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "/old-review") {
		t.Fatalf("expected old review command to be replaced, got:\n%s", text)
	}
	if !strings.Contains(text, "/new-review") {
		t.Fatalf("expected new review command in skill, got:\n%s", text)
	}
}

func TestSyncRejectsLocalEditsWithoutTTY(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("custom skill"), 0o644); err != nil {
		t.Fatalf("seed custom skill: %v", err)
	}

	err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/review-please"})
	if err == nil {
		t.Fatal("expected local edits error")
	}
	if !strings.Contains(err.Error(), "local edits") {
		t.Fatalf("expected local edits error, got %v", err)
	}
}

func TestSyncPromptsBeforeOverwritingLocalEdits(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("custom skill"), 0o644); err != nil {
		t.Fatalf("seed custom skill: %v", err)
	}

	var out bytes.Buffer
	if err := Sync(SyncOptions{
		HomeDir:       home,
		ReviewCommand: "/review-please",
		Interactive:   true,
		In:            strings.NewReader("y\n"),
		Out:           &out,
	}); err != nil {
		t.Fatalf("expected overwrite after confirmation, got %v", err)
	}
	if !strings.Contains(out.String(), "Overwrite?") {
		t.Fatalf("expected overwrite prompt, got %q", out.String())
	}
}
