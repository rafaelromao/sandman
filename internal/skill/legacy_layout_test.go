package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncUpgradesHistoricalSkillLayouts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seed      func(*testing.T, string, string)
		oldReview bool
	}{
		{
			name: "before code-review rename",
			seed: func(t *testing.T, _, root string) {
				for _, rel := range strings.Fields(preCodeReviewLayout) {
					path := filepath.Join(root, filepath.FromSlash(rel))
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("Historical file: "+rel), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			},
			oldReview: true,
		},
		{
			name: "before code-review evaluations",
			seed: func(t *testing.T, home, root string) {
				seedUnmanifestedSkillTree(t, home, root)
				if err := os.RemoveAll(filepath.Join(root, "code-review", "evals")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "entire review directory renamed including evaluations",
			seed: func(t *testing.T, home, root string) {
				seedUnmanifestedSkillTree(t, home, root)
				if err := os.Rename(filepath.Join(root, "code-review"), filepath.Join(root, "self-review")); err != nil {
					t.Fatal(err)
				}
			},
			oldReview: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
			tc.seed(t, home, root)

			if err := Sync(SyncOptions{HomeDir: home}); err != nil {
				t.Fatalf("upgrade historical skill tree: %v", err)
			}
			if tc.oldReview {
				if _, err := os.Stat(filepath.Join(root, "self-review")); !os.IsNotExist(err) {
					t.Fatalf("obsolete self-review directory: %v", err)
				}
			}
			m, err := readManifest(root)
			if err != nil || m == nil {
				t.Fatalf("read generated skill manifest: manifest = %v, err = %v", m, err)
			}
			for _, rel := range []string{"code-review/SKILL.md", "code-review/evals/localized-defect/case.yaml"} {
				path := filepath.FromSlash(rel)
				data, err := os.ReadFile(filepath.Join(root, path))
				if err != nil || string(data) != readEmbeddedSkill(t, rel) {
					t.Fatalf("current skill file %q was not installed verbatim: %v", rel, err)
				}
				if m.Files[path] == "" {
					t.Errorf("generated manifest is missing %q", rel)
				}
			}
			if ok, err := matchesManifest(root, m); err != nil || !ok {
				t.Fatalf("generated manifest does not verify installed files: match = %t, err = %v", ok, err)
			}
		})
	}
}

func TestSyncRejectsUnrecognizedLegacySkillLayouts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove string
		add    string
	}{
		{name: "unexpected file", add: "unmanaged.md"},
		{name: "missing unrelated file", remove: "tdd/mocking.md"},
		{name: "partial evaluations", remove: "code-review/evals/localized-defect/case.yaml"},
		{name: "same file count with unexpected path", remove: "tdd/mocking.md", add: "tdd/custom.md"},
		{name: "mixed old and new review paths", remove: "code-review/SKILL.md", add: "self-review/SKILL.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
			seedUnmanifestedSkillTree(t, home, root)
			if tc.remove != "" {
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(tc.remove))); err != nil {
					t.Fatal(err)
				}
			}
			var extra string
			if tc.add != "" {
				extra = filepath.Join(root, filepath.FromSlash(tc.add))
				if err := os.MkdirAll(filepath.Dir(extra), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(extra, []byte("user file"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			err := Sync(SyncOptions{HomeDir: home})
			if err == nil || !strings.Contains(err.Error(), "local edits") {
				t.Fatalf("expected sync to reject unmanaged layout, got %v", err)
			}
			if extra != "" {
				if data, err := os.ReadFile(extra); err != nil || string(data) != "user file" {
					t.Fatalf("sync changed the user file: %q, %v", data, err)
				}
			}
			if m, err := readManifest(root); err != nil || m != nil {
				t.Fatalf("rejected sync wrote a manifest: manifest = %v, err = %v", m, err)
			}
		})
	}
}

func TestSyncDoesNotUseLegacyMatchingAfterManifestMismatch(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	path := filepath.Join(root, "code-review", "SKILL.md")
	if err := os.WriteFile(path, []byte("local changes"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Sync(SyncOptions{HomeDir: home})
	if err == nil || !strings.Contains(err.Error(), "local edits") {
		t.Fatalf("expected sync to reject manifest mismatch, got %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "local changes" {
		t.Fatalf("sync changed the edited skill: %q, %v", data, err)
	}
}

func seedUnmanifestedSkillTree(t *testing.T, home, root string) {
	t.Helper()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, manifestFileName)); err != nil {
		t.Fatal(err)
	}
}
