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
	BatchesDir       string
	BatchesIndexPath string
	EventsLogPath    string
	ArchiveDir       string
	// Deprecated: LogDir is kept for backward compatibility. Use run folder logging instead.
	LogDir string
	// Deprecated: RunsDir is kept for backward compatibility. Use BatchesDir instead.
	RunsDir string
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
		LogDir:           filepath.Join(repoRoot, ".sandman", "logs"),
		RunsDir:          filepath.Join(repoRoot, ".sandman", "runs"),
	}
}
