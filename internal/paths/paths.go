// Package paths resolves every canonical on-disk location for a Sandman repo
// from a single Layout struct. See docs/architecture/disk-layout.md for the
// full on-disk layout (tree, per-artifact table, out-of-layout exceptions).
package paths

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

// Layout groups the canonical on-disk locations for a Sandman repo. Every
// field is resolved against RepoRoot so the orchestrator, agent run, portal,
// and clean command stop hand-rolling filepath.Join(".sandman", ...).
//
// The layout covers four groups:
//
//	(a) Scaffold state under SandmanDir: DockerfilePath, ConfigPath,
//	    PromptPath.
//	(b) Repo-global state: BatchesIndexPath, EventsLogPath, plus StateDir
//	    and its children (PromptVersionPath, BadgeControlFilePath,
//	    PRHeadShaPath, PRAddressedCommentsPath).
//	(c) Per-batch + per-row tree under BatchesDir: BatchDir, BatchManifestPath,
//	    BatchSocketPath, BatchConfigSnapshotDir, RunFolder, RunLogPath,
//	    RunSocketPath, RunManifestPath, ReviewStatePath, RunConfigSnapshotDir.
//	(d) Review-daemon state under ReviewsDir(): ReviewPromptPath,
//	    QualityRulesPath, ReviewSocketPath.
//
// Out-of-repo tempdirs (os.TempDir()/sandman-config-*,
// os.TempDir()/sandman-gitconfig-*) and ~/.agents/skills/sandman/** are
// intentionally not exposed on the layout.
//
// Note: the StateDir field doubles as its accessor; there is no StateDir()
// method because Go forbids a method and a field with the same name on the
// same struct.
type Layout struct {
	RepoRoot         string
	SandmanDir       string
	WorktreeDir      string
	BatchesDir       string
	BatchesIndexPath string
	EventsLogPath    string
	ArchiveDir       string
	StateDir         string
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

// ReviewsDir returns the review-daemon directory: <repo>/.sandman/reviews
func (l Layout) ReviewsDir() string {
	return filepath.Join(l.SandmanDir, "reviews")
}

// ReviewPromptPath returns the review prompt file: <repo>/.sandman/reviews/review-prompt.md
func (l Layout) ReviewPromptPath() string {
	return filepath.Join(l.ReviewsDir(), "review-prompt.md")
}

// QualityRulesPath returns the quality rules file: <repo>/.sandman/reviews/quality-rules.md
func (l Layout) QualityRulesPath() string {
	return filepath.Join(l.ReviewsDir(), "quality-rules.md")
}

// ReviewSocketPath returns the review daemon socket: <repo>/.sandman/reviews/review.sock
func (l Layout) ReviewSocketPath() string {
	return filepath.Join(l.ReviewsDir(), "review.sock")
}

// DockerfilePath returns the sandbox Dockerfile: <repo>/.sandman/Dockerfile
func (l Layout) DockerfilePath() string {
	return filepath.Join(l.SandmanDir, "Dockerfile")
}

// ConfigPath returns the sandman config file: <repo>/.sandman/config.yaml
func (l Layout) ConfigPath() string {
	return filepath.Join(l.SandmanDir, "config.yaml")
}

// PromptPath returns the default task prompt file: <repo>/.sandman/prompt.md
func (l Layout) PromptPath() string {
	return filepath.Join(l.SandmanDir, "prompt.md")
}

// RunLogPath returns the run log file: <batchesDir>/<batchID>/runs/<runID>/run.log
func (l Layout) RunLogPath(batchID, runID string) string {
	return filepath.Join(l.RunFolder(batchID, runID), "run.log")
}

// RunSocketPath returns the run socket: <batchesDir>/<batchID>/runs/<runID>/run.sock
func (l Layout) RunSocketPath(batchID, runID string) string {
	return filepath.Join(l.RunFolder(batchID, runID), "run.sock")
}

// RunManifestPath returns the run manifest file: <batchesDir>/<batchID>/runs/<runID>/run.json
func (l Layout) RunManifestPath(batchID, runID string) string {
	return filepath.Join(l.RunFolder(batchID, runID), "run.json")
}

// ReviewStatePath returns the per-run review state file: <batchesDir>/<batchID>/runs/<runID>/review-state.json
func (l Layout) ReviewStatePath(batchID, runID string) string {
	return filepath.Join(l.RunFolder(batchID, runID), "review-state.json")
}

// DecisionFile returns the per-run decision file:
// <batchesDir>/<batchID>/runs/<runID>/decision.md.
//
// This is the run-folder location, the canonical home for the
// decision file consumed by slice-1 verdict readers that locate
// the file via `<portalRun.RunDir>/decision.md` (issue #1937).
//
// Note: this is NOT the review worktree's decision.md path. Issue
// #1953 deliberately moved the *review* decision.md into the
// per-row worktree (the agent's CWD), so review-side readers must
// continue to use the worktree-derived path computed by
// `Daemon.reviewDecisionPath` in `internal/review/daemon.go`. This
// helper exists for non-review verdict readers and for any future
// callers that want a single artifact-path constructor for the
// run-folder location.
func (l Layout) DecisionFile(batchID, runID string) string {
	return filepath.Join(l.RunFolder(batchID, runID), "decision.md")
}

// RunConfigSnapshotDir returns the per-run config snapshot directory: <batchesDir>/<batchID>/runs/<runID>/config
func (l Layout) RunConfigSnapshotDir(batchID, runID string) string {
	return filepath.Join(l.RunFolder(batchID, runID), "config")
}

// BatchManifestPath returns the batch manifest file: <batchesDir>/<batchID>/batch.json
func (l Layout) BatchManifestPath(batchID string) string {
	return filepath.Join(l.BatchDir(batchID), "batch.json")
}

// BatchSocketPath returns the batch socket: <batchesDir>/<batchID>/batch.sock
func (l Layout) BatchSocketPath(batchID string) string {
	return filepath.Join(l.BatchDir(batchID), "batch.sock")
}

// BatchConfigSnapshotDir returns the per-batch config snapshot directory: <batchesDir>/<batchID>/config
func (l Layout) BatchConfigSnapshotDir(batchID string) string {
	return filepath.Join(l.BatchDir(batchID), "config")
}

// PromptVersionPath returns the prompt version marker: <repo>/.sandman/state/.prompt-version
func (l Layout) PromptVersionPath() string {
	return filepath.Join(l.StateDir, ".prompt-version")
}

// BadgeControlFilePath returns the badge hook control file: <repo>/.sandman/state/.built_with_sandman
func (l Layout) BadgeControlFilePath() string {
	return filepath.Join(l.StateDir, ".built_with_sandman")
}

// PRHeadShaPath returns the cached PR head sha file: <repo>/.sandman/state/<N>.head_sha
func (l Layout) PRHeadShaPath(prNumber int) string {
	return filepath.Join(l.StateDir, strconv.Itoa(prNumber)+".head_sha")
}

// PRAddressedCommentsPath returns the addressed-comments file for a PR: <repo>/.sandman/state/<N>.addressed_comments
func (l Layout) PRAddressedCommentsPath(prNumber int) string {
	return filepath.Join(l.StateDir, strconv.Itoa(prNumber)+".addressed_comments")
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
		StateDir:         filepath.Join(repoRoot, ".sandman", "state"),
	}
}
