package skill

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// claudeSkillLinkPrefix names the per-mode links: Claude Code discovers
// skills only as ~/.claude/skills/<name>/SKILL.md, one directory level deep,
// so each sub-skill of the shared tree gets its own link named after the
// skill (for example sandman-implement).
const claudeSkillLinkPrefix = embeddedSkillRoot + "-"

var errNotSymlink = errors.New("exists and is not a symlink")

type claudeSkillLink struct {
	name   string
	target string
}

// claudeSkillLinks lists the links that expose the shared skill tree at
// sharedDir to Claude Code: the router skill and one link per sub-skill.
func claudeSkillLinks(sharedDir string) ([]claudeSkillLink, error) {
	entries, err := fs.ReadDir(embeddedSkills, embeddedSkillRoot)
	if err != nil {
		return nil, fmt.Errorf("list embedded sub-skills: %w", err)
	}
	links := []claudeSkillLink{{name: embeddedSkillRoot, target: sharedDir}}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		links = append(links, claudeSkillLink{
			name:   claudeSkillLinkPrefix + entry.Name(),
			target: filepath.Join(sharedDir, entry.Name()),
		})
	}
	sort.Slice(links, func(i, j int) bool { return links[i].name < links[j].name })
	return links, nil
}

// syncClaudeCodeSkillLinks makes the shared skill tree discoverable by Claude
// Code without copying it: ~/.claude/skills/sandman and
// ~/.claude/skills/sandman-<mode> become symlinks into ~/.agents/skills.
// Existing symlinks are repointed; a real file or directory at a link path is
// left untouched with a warning. Failures are warnings because the shared
// tree itself is already installed and other agents do not need the links.
func syncClaudeCodeSkillLinks(homeDir, sharedDir string, out io.Writer) {
	if out == nil {
		out = io.Discard
	}
	skillsDir := filepath.Join(homeDir, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		fmt.Fprintf(out, "warning: Claude Code skills directory %s: %v; Claude Code will not discover the sandman skills\n", skillsDir, err)
		return
	}
	links, err := claudeSkillLinks(sharedDir)
	if err != nil {
		fmt.Fprintf(out, "warning: %v; Claude Code will not discover the sandman skills\n", err)
		return
	}
	for _, link := range links {
		linkPath := filepath.Join(skillsDir, link.name)
		if err := ensureSkillSymlink(linkPath, link.target); err != nil {
			fmt.Fprintf(out, "warning: Claude Code skill link %s: %v; leaving it unchanged\n", linkPath, err)
		}
	}
}

// ensureSkillSymlink points linkPath at target. A missing link is created and
// a symlink with another target is replaced, both through a temporary link and
// an atomic rename, so readers never observe a missing entry.
func ensureSkillSymlink(linkPath, target string) error {
	info, err := os.Lstat(linkPath)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink == 0:
		return errNotSymlink
	case err == nil:
		current, err := os.Readlink(linkPath)
		if err != nil {
			return err
		}
		if current == target {
			return nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	tmp := filepath.Join(filepath.Dir(linkPath), fmt.Sprintf(".%s.tmp-%d-%d", filepath.Base(linkPath), os.Getpid(), time.Now().UnixNano()))
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, linkPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
