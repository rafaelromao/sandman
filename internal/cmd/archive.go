package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/spf13/cobra"
)

type runActivityProbe func(runPath string) bool

func stripSockets(batchDir string) {
	_ = filepath.Walk(batchDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		matched, _ := filepath.Match("*sock*", filepath.Base(path))
		if matched {
			_ = os.Remove(path)
		}
		return nil
	})
}

func NewArchiveCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive",
		Short: "Archive completed run directories",
		Long:  "Move a run directory from .sandman/batches/<id> to .sandman/archive/<id> after confirming the run's daemon is no longer live. Use 'archive run' to move a single run by id, 'archive older-than <days>' to bulk-archive every dead run older than the given age, or 'archive stale' to recover unterminated runs in dead batches (emitting run.aborted) and then archive every dead-and-terminal run.",
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
			return runArchiveRun(cmd, args[0], probe, deps.RepoRoot)
		},
	}
}

func newArchiveOlderThanCmd(deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "older-than <days>",
		Short: "Archive dead run directories older than N days",
		Long:  "Move every dead run directory whose index entry CreatedAt is older than <days> days to .sandman/archive/. Live daemons are never archived regardless of age.",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveOlderThan(cmd, args[0], deps.RepoRoot)
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

	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}

	if err := idx.EnsureStatus(); err != nil {
		return fmt.Errorf("ensure status: %w", err)
	}

	var archived int
	now := time.Now().UTC()
	for _, entry := range idx.Entries {
		if entry.Status != batchindex.StatusActive {
			continue
		}
		if daemon.IsRunActive(entry.Path) {
			continue
		}
		archivePath := filepath.Join(layout.ArchiveDir, entry.ID)
		if _, err := os.Stat(archivePath); err == nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "skip %q: archive already exists\n", entry.ID)
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat archive target %q: %w", archivePath, err)
		}
		if err := os.MkdirAll(layout.ArchiveDir, 0755); err != nil {
			return fmt.Errorf("create archive dir: %w", err)
		}
		if err := os.Rename(entry.Path, archivePath); err != nil {
			return fmt.Errorf("move batch %q: %w", entry.ID, err)
		}
		stripSockets(archivePath)
		if err := idx.SetArchived(entry.ID, archivePath, now); err != nil {
			return fmt.Errorf("set archived in index: %w", err)
		}
		archived++
	}

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Recovered %d stale runs and archived %d dead batches.\n", recovered, archived)
	return nil
}

func runArchiveRun(cmd *cobra.Command, id string, probe runActivityProbe, repoRoot string) error {
	if repoRoot == "" {
		repoRoot = "."
	}
	layout := paths.NewLayout(&config.Config{}, repoRoot)

	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}

	entry := idx.Resolve(id)
	if entry == nil {
		return fmt.Errorf("batch %q not found in index", id)
	}

	if entry.Status != batchindex.StatusActive {
		return fmt.Errorf("batch %q is not active (status=%s); refusing to archive", id, entry.Status)
	}

	if probe != nil && probe(entry.Path) {
		return fmt.Errorf("batch %q is still active; stop the daemon before archiving", id)
	}

	archivePath := filepath.Join(layout.ArchiveDir, id)
	if _, err := os.Stat(archivePath); err == nil {
		return fmt.Errorf("archive %q already exists", id)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat archive target: %w", err)
	}

	if err := os.MkdirAll(layout.ArchiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}

	if err := os.Rename(entry.Path, archivePath); err != nil {
		return fmt.Errorf("move batch dir: %w", err)
	}
	stripSockets(archivePath)

	now := time.Now().UTC()
	if err := idx.SetArchived(id, archivePath, now); err != nil {
		return fmt.Errorf("set archived in index: %w", err)
	}

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Archived batch %q\n", id)
	return nil
}

func archivePortalRun(repoRoot, runID string) error {
	layout := paths.NewLayout(&config.Config{}, repoRoot)

	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}

	entry := idx.Resolve(runID)
	if entry == nil {
		return fmt.Errorf("batch %q not found in index", runID)
	}

	if entry.Status != batchindex.StatusActive {
		return fmt.Errorf("batch %q is not active (status=%s)", runID, entry.Status)
	}

	archivePath := filepath.Join(layout.ArchiveDir, runID)
	if info, err := os.Stat(archivePath); err == nil && info.IsDir() {
		return fmt.Errorf("archive %q already exists", runID)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat archive target: %w", err)
	}

	if err := os.MkdirAll(layout.ArchiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}

	if err := os.Rename(entry.Path, archivePath); err != nil {
		return fmt.Errorf("move batch dir: %w", err)
	}
	stripSockets(archivePath)

	now := time.Now().UTC()
	if err := idx.SetArchived(runID, archivePath, now); err != nil {
		return fmt.Errorf("set archived in index: %w", err)
	}

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	return nil
}

func runArchiveOlderThan(cmd *cobra.Command, daysArg string, repoRoot string) error {
	if repoRoot == "" {
		repoRoot = "."
	}

	days, err := strconv.Atoi(daysArg)
	if err != nil {
		return fmt.Errorf("days %q is not a non-negative integer", daysArg)
	}
	if days < 0 {
		return fmt.Errorf("days %d is negative; must be non-negative", days)
	}

	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	layout := paths.NewLayout(&config.Config{}, repoRoot)

	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}

	if err := idx.EnsureStatus(); err != nil {
		return fmt.Errorf("ensure status: %w", err)
	}

	if err := os.MkdirAll(layout.ArchiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}

	var archived int
	now := time.Now().UTC()
	for _, entry := range idx.Entries {
		if entry.Status != batchindex.StatusActive {
			continue
		}
		if daemon.IsRunActive(entry.Path) {
			continue
		}
		createdAt, err := archiveBatchCreatedAt(entry)
		if err != nil {
			return err
		}
		if !createdAt.Before(cutoff) {
			continue
		}
		archivePath := filepath.Join(layout.ArchiveDir, entry.ID)
		if _, err := os.Stat(archivePath); err == nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "skip %q: archive already exists\n", entry.ID)
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat archive target %q: %w", archivePath, err)
		}
		if err := os.Rename(entry.Path, archivePath); err != nil {
			return fmt.Errorf("move batch %q: %w", entry.ID, err)
		}
		stripSockets(archivePath)
		if err := idx.SetArchived(entry.ID, archivePath, now); err != nil {
			return fmt.Errorf("set archived in index: %w", err)
		}
		archived++
	}

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Archived %d batch(es) older than %d day(s)\n", archived, days)
	return nil
}

func archiveBatchCreatedAt(entry batchindex.Entry) (time.Time, error) {
	manifest, err := batchindex.ReadManifest(entry.Path)
	if err == nil && !manifest.CreatedAt.IsZero() {
		return manifest.CreatedAt.UTC(), nil
	}
	if err != nil && !os.IsNotExist(err) {
		return time.Time{}, fmt.Errorf("read batch manifest for %q: %w", entry.ID, err)
	}

	info, err := os.Stat(entry.Path)
	if err != nil {
		return time.Time{}, fmt.Errorf("stat batch dir for %q: %w", entry.ID, err)
	}
	return info.ModTime().UTC(), nil
}
