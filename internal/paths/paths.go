package paths

import (
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

type Layout struct {
	RepoRoot         string
	SandmanDir       string
	WorktreeDir      string
	EventsLogPath    string
	ArchiveDir       string
	BatchesDir       string
	BatchesIndexPath string
}

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
		RepoRoot:         repoRoot,
		SandmanDir:       filepath.Join(repoRoot, ".sandman"),
		WorktreeDir:      worktreeDir,
		EventsLogPath:    filepath.Join(repoRoot, ".sandman", "events.jsonl"),
		ArchiveDir:       filepath.Join(repoRoot, ".sandman", "archive"),
		BatchesDir:       filepath.Join(repoRoot, ".sandman", "batches"),
		BatchesIndexPath: filepath.Join(repoRoot, ".sandman", "batches.json"),
	}
}
