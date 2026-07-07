package cmd

import (
	"encoding/json"
	"errors"
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

// stripSockets removes every file whose mode carries ModeSocket from
// the supplied directory tree. It is kept in the cmd package for the
// whole-batch archive path; daemon.ArchiveRow (per-row) uses its own
// StripSockets helper so both call sites share the same semantics
// without a circular import.
func stripSockets(batchDir string) error {
	var lastErr error
	_ = filepath.Walk(batchDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || info.Mode()&os.ModeSocket == 0 {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			lastErr = rmErr
		}
		return nil
	})
	return lastErr
}

func NewArchiveCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive",
		Short: "Archive completed run directories",
		Long:  "Archive run and batch directories after their daemons are gone. 'archive run <runId>' archives a single row by id (per-row), 'archive batch <batchId>' archives an entire batch directory (whole-batch), 'archive older-than <days>' archives each terminal row older than the cutoff (per-row aware), 'archive stale' recovers unterminated rows per run and then archives every terminal row.",
	}
	cmd.AddCommand(newArchiveRunCmd(deps))
	cmd.AddCommand(newArchiveBatchCmd(deps))
	cmd.AddCommand(newArchiveOlderThanCmd(deps))
	cmd.AddCommand(newArchiveStaleCmd(deps))
	return cmd
}

func newArchiveRunCmd(deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "run <runId>",
		Short: "Archive a single row by its run id (per-row)",
		Long:  "Move runs/<runId>/ from .sandman/batches/<batchId>/ to .sandman/archive/<batchId>/runs/<runId>/. The targeted row's run.json Status must be terminal; sibling rows and the batch daemon are left untouched. The CLI and the HTTP /api/runs/archive endpoint share this contract.",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveRun(cmd, args[0], deps.RepoRoot)
		},
	}
}

func newArchiveBatchCmd(deps Dependencies) *cobra.Command {
	probe := deps.RunActivityProbe
	if probe == nil {
		probe = daemon.IsRunActive
	}
	return &cobra.Command{
		Use:   "batch <batchId>",
		Short: "Archive an entire batch directory (whole-batch)",
		Long:  "Move the whole batch dir from .sandman/batches/<batchId>/ to .sandman/archive/<batchId>/. The batch daemon must be gone; sibling rows are not applicable. Whole-batch archive is not exposed via HTTP.",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveBatch(cmd, args[0], probe, deps.RepoRoot)
		},
	}
}

func newArchiveOlderThanCmd(deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "older-than <days>",
		Short: "Archive terminal run rows older than N days (per-row aware)",
		Long:  "Walk every run.json across all batches and archive each terminal row older than the cutoff. Already-archived rows are skipped. Sibling rows and live batch daemons are left untouched.",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveOlderThan(cmd, args[0], deps.RepoRoot)
		},
	}
}

func newArchiveStaleCmd(deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "stale",
		Short: "Recover unterminated rows per run and archive every terminal row",
		Long:  "Chain the same status-fix logic as 'clean --stale' to emit run.aborted events for unterminated runs in dead batches, then walk every run.json and archive each terminal row. Live batches are skipped entirely.",
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

	if err := idx.EnsureStatusWithLayout(repoRoot); err != nil {
		return fmt.Errorf("ensure status: %w", err)
	}

	if err := os.MkdirAll(layout.ArchiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}

	var archived int
	if err := archiveAllTerminalRows(cmd, idx, layout, repoRoot, &archived); err != nil {
		return err
	}

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Recovered %d stale runs and archived %d terminal rows.\n", recovered, archived)
	return nil
}

// archiveAllTerminalRows walks every runs/<runID>/run.json across all
// batches and archives each terminal row via daemon.ArchiveRow. It
// honours per-row Runs records (already-archived rows are skipped)
// and skips live batches entirely. The counter is updated in place.
func archiveAllTerminalRows(cmd *cobra.Command, idx *batchindex.Index, layout paths.Layout, repoRoot string, archived *int) error {
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		if daemon.IsRunActive(entry.Path) {
			continue
		}
		runDirs, err := listRunDirs(entry.Path)
		if err != nil {
			return err
		}
		for _, runID := range runDirs {
			if rec := idx.RunRecordFor(entry.ID, runID); rec != nil && rec.Status == batchindex.RunRecordStatusArchived {
				continue
			}
			manifestPath := filepath.Join(entry.Path, "runs", runID, "run.json")
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				continue
			}
			var manifest batchindex.RunManifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				continue
			}
			if !isTerminalRunManifestStatusLocal(manifest.Status) {
				continue
			}
			if idx.RunRecordFor(entry.ID, runID) == nil {
				idx.AddRun(entry.ID, batchindex.RunRecord{RunID: runID, Status: batchindex.RunRecordStatusActive})
			}
			rec, err := daemon.ArchiveRow(repoRoot, entry, runID)
			if err != nil {
				var alreadyArchived *daemon.AlreadyArchivedError
				if errors.As(err, &alreadyArchived) {
					if markErr := idx.MarkRunArchived(entry.ID, runID, alreadyArchived.ArchivePath); markErr != nil {
						return fmt.Errorf("mark run archived: %w", markErr)
					}
					continue
				}
				return fmt.Errorf("archive run %q in batch %q: %w", runID, entry.ID, err)
			}
			if err := idx.MarkRunArchived(entry.ID, runID, rec.ArchivePath); err != nil {
				return fmt.Errorf("mark run archived: %w", err)
			}
			*archived++
		}
	}
	return nil
}

// listRunDirs returns the names of every directory under
// <batchDir>/runs/. A missing runs/ directory yields an empty slice;
// any other error is returned.
func listRunDirs(batchDir string) ([]string, error) {
	runsDir := filepath.Join(batchDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %q: %w", runsDir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// isTerminalRunManifestStatusLocal mirrors the package-private
// isTerminalRunManifestStatus in portal.go without importing it, so
// the bulk archive path can stay self-contained.
func isTerminalRunManifestStatusLocal(s batchindex.RunManifestStatus) bool {
	switch s {
	case batchindex.RunManifestStatusSuccess,
		batchindex.RunManifestStatusFailure,
		batchindex.RunManifestStatusAborted,
		batchindex.RunManifestStatusBlocked:
		return true
	}
	return false
}

// runArchiveRun is the CLI per-row archive path. It validates the row
// is terminal, dispatches to daemon.ArchiveRow, and writes the
// resulting RunRecord into the entry's Runs slice. The targeted row's
// run.json Status must be terminal; the batch daemon may be alive or
// dead (a sibling row keeps working either way).
func runArchiveRun(cmd *cobra.Command, runID string, repoRoot string) error {
	if repoRoot == "" {
		repoRoot = "."
	}
	layout := paths.NewLayout(&config.Config{}, repoRoot)

	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}

	entry := resolveBatchEntryForRunID(idx, runID)
	if entry == nil {
		return fmt.Errorf("run %q not found in index", runID)
	}

	if entry.Status == batchindex.StatusArchived {
		return fmt.Errorf("batch %q is already archived", entry.ID)
	}

	if rec := idx.RunRecordFor(entry.ID, runID); rec != nil && rec.Status == batchindex.RunRecordStatusArchived && rec.ArchivePath != "" {
		return fmt.Errorf("run %q is already archived at %q", runID, rec.ArchivePath)
	}

	if idx.RunRecordFor(entry.ID, runID) == nil {
		idx.AddRun(entry.ID, batchindex.RunRecord{RunID: runID, Status: batchindex.RunRecordStatusActive})
	}

	rec, err := daemon.ArchiveRow(repoRoot, entry, runID)
	if err != nil {
		return err
	}

	if err := idx.MarkRunArchived(entry.ID, runID, rec.ArchivePath); err != nil {
		return fmt.Errorf("mark run archived: %w", err)
	}

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Archived run %q in batch %q at %s\n", runID, entry.ID, rec.ArchivePath)
	return nil
}

// runArchiveBatch is the CLI whole-batch archive path. It moves the
// entire batch dir to .sandman/archive/<batchId>/, strips sockets,
// and updates the entry's Status to archived. Not exposed via HTTP.
func runArchiveBatch(cmd *cobra.Command, batchID string, probe runActivityProbe, repoRoot string) error {
	if repoRoot == "" {
		repoRoot = "."
	}
	layout := paths.NewLayout(&config.Config{}, repoRoot)

	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}

	entry := idx.Resolve(batchID)
	if entry == nil {
		return fmt.Errorf("batch %q not found in index", batchID)
	}

	if entry.Status != batchindex.StatusActive {
		return fmt.Errorf("batch %q is not active (status=%s); refusing to archive", batchID, entry.Status)
	}

	if probe != nil && probe(entry.Path) {
		return fmt.Errorf("batch %q is still active; stop the daemon before archiving", batchID)
	}

	archivePath := filepath.Join(layout.ArchiveDir, batchID)
	if _, err := os.Stat(archivePath); err == nil {
		return fmt.Errorf("archive %q already exists", batchID)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat archive target: %w", err)
	}

	if err := os.MkdirAll(layout.ArchiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}

	if err := os.Rename(entry.Path, archivePath); err != nil {
		return fmt.Errorf("move batch dir: %w", err)
	}
	if err := stripSockets(archivePath); err != nil {
		return fmt.Errorf("strip sockets from archived batch %q: %w", batchID, err)
	}

	now := time.Now().UTC()
	if err := idx.SetArchived(batchID, archivePath, now); err != nil {
		return fmt.Errorf("set archived in index: %w", err)
	}

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Archived batch %q\n", batchID)
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

	if err := idx.EnsureStatusWithLayout(repoRoot); err != nil {
		return fmt.Errorf("ensure status: %w", err)
	}

	if err := os.MkdirAll(layout.ArchiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}

	var archived int
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		if daemon.IsRunActive(entry.Path) {
			continue
		}
		runDirs, err := listRunDirs(entry.Path)
		if err != nil {
			return err
		}
		for _, runID := range runDirs {
			if rec := idx.RunRecordFor(entry.ID, runID); rec != nil && rec.Status == batchindex.RunRecordStatusArchived {
				continue
			}
			manifestPath := filepath.Join(entry.Path, "runs", runID, "run.json")
			info, err := os.Stat(manifestPath)
			if err != nil {
				continue
			}
			manifest, err := batchindex.ReadManifest(filepath.Dir(manifestPath))
			if err != nil {
				continue
			}
			createdAt := manifest.CreatedAt
			if createdAt.IsZero() {
				createdAt = info.ModTime()
			}
			if !createdAt.UTC().Before(cutoff) {
				continue
			}
			if !isTerminalRunManifestStatusLocal(manifest.Status) {
				continue
			}
			if idx.RunRecordFor(entry.ID, runID) == nil {
				idx.AddRun(entry.ID, batchindex.RunRecord{RunID: runID, Status: batchindex.RunRecordStatusActive})
			}
			rec, err := daemon.ArchiveRow(repoRoot, entry, runID)
			if err != nil {
				var alreadyArchived *daemon.AlreadyArchivedError
				if errors.As(err, &alreadyArchived) {
					if markErr := idx.MarkRunArchived(entry.ID, runID, alreadyArchived.ArchivePath); markErr != nil {
						return fmt.Errorf("mark run archived: %w", markErr)
					}
					continue
				}
				return fmt.Errorf("archive run %q in batch %q: %w", runID, entry.ID, err)
			}
			if err := idx.MarkRunArchived(entry.ID, runID, rec.ArchivePath); err != nil {
				return fmt.Errorf("mark run archived: %w", err)
			}
			archived++
		}
	}

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Archived %d terminal row(s) older than %d day(s)\n", archived, days)
	return nil
}
