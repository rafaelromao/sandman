package skill

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncLinksSharedSkillIntoClaudeCode(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/sandman review"}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}
	shared := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	skillsDir := filepath.Join(home, ".claude", "skills")

	links, err := claudeSkillLinks(shared)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) < 2 {
		t.Fatalf("links = %v, want the router plus sub-skills", links)
	}
	for _, link := range links {
		linkPath := filepath.Join(skillsDir, link.name)
		target, err := os.Readlink(linkPath)
		if err != nil {
			t.Fatalf("read link %s: %v", linkPath, err)
		}
		if target != link.target {
			t.Fatalf("link %s -> %s, want %s", linkPath, target, link.target)
		}
		skillFile := filepath.Join(linkPath, "SKILL.md")
		if got := skillFrontmatterName(t, skillFile); got != link.name {
			t.Fatalf("%s declares skill name %q, want the link name %q so Claude Code resolves Skill %q", skillFile, got, link.name, link.name)
		}
	}
	if _, err := os.Lstat(filepath.Join(skillsDir, claudeSkillLinkPrefix+"implement")); err != nil {
		t.Fatalf("sandman-implement link missing: %v", err)
	}
}

func TestSyncRepointsClaudeCodeSkillLinksIdempotently(t *testing.T) {
	home := t.TempDir()
	skillsDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(skillsDir, embeddedSkillRoot)
	if err := os.Symlink(filepath.Join(home, "old-location"), stale); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/sandman review"}); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	target, err := os.Readlink(stale)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".agents", "skills", embeddedSkillRoot); target != want {
		t.Fatalf("stale link -> %s, want %s", target, want)
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("temporary link %s left behind", entry.Name())
		}
	}
}

func TestSyncLeavesRealClaudeCodeSkillDirectoryUntouched(t *testing.T) {
	home := t.TempDir()
	own := filepath.Join(home, ".claude", "skills", claudeSkillLinkPrefix+"implement")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("my own skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/sandman review", Out: &out}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}
	info, err := os.Lstat(own)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("operator directory replaced: info=%v err=%v", info, err)
	}
	data, err := os.ReadFile(filepath.Join(own, "SKILL.md"))
	if err != nil || string(data) != "my own skill\n" {
		t.Fatalf("operator skill changed: %q, %v", data, err)
	}
	if !strings.Contains(out.String(), "warning: Claude Code skill link") || !strings.Contains(out.String(), "not a symlink") {
		t.Fatalf("sync output = %q, want a skip warning", out.String())
	}
	if _, err := os.Readlink(filepath.Join(home, ".claude", "skills", embeddedSkillRoot)); err != nil {
		t.Fatalf("router link not created alongside the skipped directory: %v", err)
	}
}

func skillFrontmatterName(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	inFrontmatter := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			if inFrontmatter {
				break
			}
			inFrontmatter = true
			continue
		}
		if inFrontmatter && strings.HasPrefix(line, "name:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "name:")), `"'`)
		}
	}
	return ""
}
