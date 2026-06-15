package paths

import (
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

// Layout groups the canonical on-disk locations for a Sandman repo. Every
// field is resolved against RepoRoot so the orchestrator, agent run, portal,
// and clean command stop hand-rolling filepath.Join(".sandman", ...).
type Layout struct {
	RepoRoot      string
	SandmanDir    string
	WorktreeDir   string
	LogDir        string
	EventsLogPath string
	ArchiveDir    string
	RunsDir       string
}

// NewLayout resolves a Layout for the given repo root, honoring cfg.WorktreeDir
// when set and falling back to ".sandman/worktrees" otherwise. All other
// fields are joined under RepoRoot using the canonical .sandman prefix.
func NewLayout(cfg *config.Config, repoRoot string) Layout {
	worktreeDir := filepath.Join(repoRoot, ".sandman", "worktrees")
	if cfg != nil {
		if raw := strings.TrimSpace(cfg.WorktreeDir); raw != "" {
			if filepath.IsAbs(raw) {
				worktreeDir = raw
			} else {
				worktreeDir = filepath.Join(repoRoot, raw)
			}
		}
	}
	return Layout{
		RepoRoot:      repoRoot,
		SandmanDir:    filepath.Join(repoRoot, ".sandman"),
		WorktreeDir:   worktreeDir,
		LogDir:        filepath.Join(repoRoot, ".sandman", "logs"),
		EventsLogPath: filepath.Join(repoRoot, ".sandman", "events.jsonl"),
		ArchiveDir:    filepath.Join(repoRoot, ".sandman", "archive"),
		RunsDir:       filepath.Join(repoRoot, ".sandman", "runs"),
	}
}

// SafeLogFilename translates a branch name (or any string with /, space, or
// path-separator characters) into a single filename-safe component. Returns
// "prompt-only" when the input is empty.
func (l Layout) SafeLogFilename(branch string) string {
	name := strings.NewReplacer("/", "-", string(filepath.Separator), "-", " ", "-").Replace(branch)
	if name == "" {
		return "prompt-only"
	}
	return name
}
