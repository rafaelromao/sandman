package cmd

import (
	"fmt"
	"os"
	"path/filepath"

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
		Long:  "Move a run directory from .sandman/runs/<id> to .sandman/archive/<id> after confirming the run's daemon is no longer live.",
	}
	cmd.AddCommand(newArchiveRunCmd(deps))
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
