package paths

import (
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

// Layout groups the canonical on-disk locations for a Sandman repo. Every
// field is resolved against RepoRoot so the orchestrator, agent run, portal,
// and clean command stop hand-rolling filepath.Join(".sandman", ...).
// BatchesDir and BatchesIndexPath are added in Phase 1; LogDir and RunsDir
// are deprecated but retained for backward compatibility with slices 2+.
type Layout struct {
	RepoRoot         string
	SandmanDir       string
	WorktreeDir      string
	BatchesDir       string
	BatchesIndexPath string
	EventsLogPath    string
	ArchiveDir       string
	LogDir           string
	RunsDir          string
}

// BatchDir returns the root directory for a batch: .sandman/batches/<batchID>
func (l Layout) BatchDir(batchID string) string {
	return filepath.Join(l.BatchesDir, batchID)
}

// RunFolder returns the run folder inside a batch: .sandman/batches/<batchID>/runs/<runID>
// This is where run.json, run.log, and run.sock live.
func (l Layout) RunFolder(batchID, runID string) string {
	return filepath.Join(l.BatchDir(batchID), "runs", runID)
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
		RepoRoot:         repoRoot,
		SandmanDir:       ".sandman",
		WorktreeDir:      worktreeDir,
		BatchesDir:       ".sandman/batches",
		BatchesIndexPath: ".sandman/batches.json",
		EventsLogPath:    ".sandman/events.jsonl",
		ArchiveDir:       ".sandman/archive",
		LogDir:           ".sandman/logs",
		RunsDir:          ".sandman/runs",
	}
}
