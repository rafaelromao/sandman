package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/spf13/cobra"
)

// runActivityProbe reports whether a run directory is currently owned by
// a live daemon. It exists as an interface so tests can substitute a
// deterministic probe without spinning up a real command server.
type runActivityProbe func(runPath string) bool

// NewArchiveCmd creates the archive command, which moves a run directory
// from .sandman/runs/<id> to .sandman/archive/<id>. Refuses to archive
// runs whose daemon is still live.
func NewArchiveCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive",
		Short: "Archive completed run directories",
		Long:  "Move a run directory from .sandman/runs/<id> to .sandman/archive/<id> after confirming the run's daemon is no longer live. Use 'archive run' to move a single run by id, 'archive older-than <days>' to bulk-archive every dead run older than the given age, or 'archive stale' to recover unterminated runs in dead batches (emitting run.aborted) and then archive every dead-and-terminal run.",
	}
	cmd.AddCommand(newArchiveRunCmd(deps))
	cmd.AddCommand(newArchiveOlderThanCmd(deps))
	cmd.AddCommand(newArchiveStaleCmd(deps))
	return cmd
}

func newArchiveRunCmd(deps Dependencies) *cobra.Command {
	probe := deps.RunActivityProbe
	if probe == nil {
		probe = daemon.IsRunActive
	}
	return &cobra.Command{
		Use:     "run <id>",
		Aliases: []string{"batch"},
		Short:   "Archive a single run directory",
		Args:    wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveRun(cmd, args[0], probe)
		},
	}
}

func newArchiveOlderThanCmd(deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "older-than <days>",
		Short: "Archive dead run directories older than N days",
		Long:  "Move every dead run directory under .sandman/runs/ whose manifest CreatedAt (or directory mtime when the manifest is missing) is older than <days> days to .sandman/archive/. Live daemons are never archived regardless of age.",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveOlderThan(cmd, args[0])
		},
	}
}

func newArchiveStaleCmd(deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "stale",
		Short: "Recover unterminated runs in dead batches and archive every dead-and-terminal run directory",
		Long:  "Chain the same status-fix logic as 'clean --stale' to emit run.aborted events for unterminated runs in dead batches, then move every dead-and-terminal run directory to .sandman/archive/. Live batches are skipped entirely.",
		Args:  wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveStale(cmd, deps)
		},
	}
}

func runArchiveStale(cmd *cobra.Command, deps Dependencies) error {
	eventsList, err := deps.EventLog.Read()
	if err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	repoRoot := deps.RepoRoot
	if repoRoot == "" {
		repoRoot = "."
	}
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	recovered, _, err := runCleanStale(layout, eventsList, deps.EventLog)
	if err != nil {
		return fmt.Errorf("recover stale runs: %w", err)
	}

	dead, err := daemon.FindDeadRunBatches(layout.SandmanDir)
	if err != nil {
		return fmt.Errorf("scan run directories: %w", err)
	}

	if err := ensureArchiveDir(); err != nil {
		return err
	}

	var archived int
	for _, batch := range dead {
		if err := moveBatchToArchive(cmd, batch); err != nil {
			return err
		}
		archived++
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Recovered %d stale runs and archived %d dead batches.\n", recovered, archived)
	return nil
}

// ensureArchiveDir creates the .sandman/archive root on first use so bulk
// commands surface a stable directory even when no batches qualify for
// archival.
func ensureArchiveDir() error {
	if err := os.MkdirAll(filepath.Join(".sandman", "archive"), 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}
	return nil
}

// moveBatchToArchive relocates a dead run directory from .sandman/runs/<id>
// to .sandman/archive/<id>, creating the archive root on first use. If a
// destination already exists the move is skipped (and a skip message is
// written to the command's stderr) so the existing archive is left
// untouched.
func moveBatchToArchive(cmd *cobra.Command, batch daemon.DeadBatch) error {
	archiveDir := filepath.Join(".sandman", "archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}
	id := filepath.Base(batch.RunDir)
	dest := filepath.Join(archiveDir, id)
	if _, err := os.Stat(dest); err == nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "skip %q: archive already exists\n", id)
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat archive target %q: %w", dest, err)
	}
	if err := os.Rename(batch.RunDir, dest); err != nil {
		return fmt.Errorf("move run %q: %w", id, err)
	}
	return nil
}

func runArchiveRun(cmd *cobra.Command, id string, probe runActivityProbe) error {
	runDir := filepath.Join(".sandman", "runs", id)
	if _, err := os.Stat(runDir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("run %q not found in .sandman/runs/", id)
		}
		return fmt.Errorf("stat run dir: %w", err)
	}

	if probe(runDir) {
		return fmt.Errorf("run %q is still active; stop the daemon before archiving", id)
	}

	archiveDir := filepath.Join(".sandman", "archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}

	dest := filepath.Join(archiveDir, id)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("archive %q already exists", id)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat archive target: %w", err)
	}

	if err := os.Rename(runDir, dest); err != nil {
		return fmt.Errorf("move run dir: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Archived run %q\n", id)
	return nil
}

// archivePortalRun relocates a run directory from <repoRoot>/.sandman/batches/<batchID>
// to <repoRoot>/.sandman/archive/<batchID>. Full batch folder archive is Phase 4.
func archivePortalRun(repoRoot, runID string) error {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	runDir := filepath.Join(layout.BatchesDir, runID)
	dest := filepath.Join(layout.ArchiveDir, runID)
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		return fmt.Errorf("archive %q already exists", runID)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat archive target: %w", err)
	}
	if err := os.MkdirAll(layout.ArchiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}
	if err := os.Rename(runDir, dest); err != nil {
		return fmt.Errorf("move run dir: %w", err)
	}
	return nil
}

func runArchiveOlderThan(cmd *cobra.Command, daysArg string) error {
	days, err := strconv.Atoi(daysArg)
	if err != nil {
		return fmt.Errorf("days %q is not a non-negative integer", daysArg)
	}
	if days < 0 {
		return fmt.Errorf("days %d is negative; must be non-negative", days)
	}

	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	if err := ensureArchiveDir(); err != nil {
		return err
	}

	dead, err := daemon.FindDeadRunBatches(".sandman")
	if err != nil {
		return fmt.Errorf("scan run directories: %w", err)
	}

	var archived int
	for _, batch := range dead {
		if batch.RunTimestamp().After(cutoff) {
			continue
		}
		if err := moveBatchToArchive(cmd, batch); err != nil {
			return err
		}
		archived++
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Archived %d run(s) older than %d day(s)\n", archived, days)
	return nil
}
