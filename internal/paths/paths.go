// Package paths resolves every canonical on-disk location for a Sandman repo
// from a single Layout struct. See docs/architecture/disk-layout.md for the
// full on-disk layout (tree, per-artifact table, out-of-layout exceptions).
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
	RepoRoot         string
	SandmanDir       string
	WorktreeDir      string
	BatchesDir       string
	BatchesIndexPath string
	EventsLogPath    string
	ArchiveDir       string
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
		SandmanDir:       filepath.Join(repoRoot, ".sandman"),
		WorktreeDir:      worktreeDir,
		BatchesDir:       filepath.Join(repoRoot, ".sandman", "batches"),
		BatchesIndexPath: filepath.Join(repoRoot, ".sandman", "batches.json"),
		EventsLogPath:    filepath.Join(repoRoot, ".sandman", "events.jsonl"),
		ArchiveDir:       filepath.Join(repoRoot, ".sandman", "archive"),
	}
}
