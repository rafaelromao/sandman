package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
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
		Long:  "Move a run directory from .sandman/runs/<id> to .sandman/archive/<id> after confirming the run's daemon is no longer live. Use 'archive run' to move a single run by id, or 'archive older-than <days>' to bulk-archive every dead run older than the given age.",
	}
	cmd.AddCommand(newArchiveRunCmd(deps))
	cmd.AddCommand(newArchiveOlderThanCmd(deps))
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
		Args:    cobra.ExactArgs(1),
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
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveOlderThan(cmd, args[0])
		},
	}
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

func runArchiveOlderThan(cmd *cobra.Command, daysArg string) error {
	days, err := strconv.Atoi(daysArg)
	if err != nil {
		return fmt.Errorf("days %q is not a non-negative integer", daysArg)
	}
	if days < 0 {
		return fmt.Errorf("days %d is negative; must be non-negative", days)
	}

	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	archiveDir := filepath.Join(".sandman", "archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}

	dead, err := daemon.FindDeadRunBatches(".sandman")
	if err != nil {
		return fmt.Errorf("scan run directories: %w", err)
	}

	var archived []string
	for _, batch := range dead {
		ts := batch.RunTimestamp()
		if ts.After(cutoff) {
			continue
		}
		id := filepath.Base(batch.RunDir)
		dest := filepath.Join(archiveDir, id)
		if _, err := os.Stat(dest); err == nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "skip %q: archive already exists\n", id)
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat archive target %q: %w", dest, err)
		}
		if err := os.Rename(batch.RunDir, dest); err != nil {
			return fmt.Errorf("move run %q: %w", id, err)
		}
		archived = append(archived, id)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Archived %d run(s) older than %d day(s)\n", len(archived), days)
	return nil
}
