package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// NewCleanCmd creates the clean command.
func NewCleanCmd(manager sandbox.Sandbox) *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Clean up sandbox resources and stale worktrees",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = manager.Stop()
			fmt.Fprintln(cmd.OutOrStdout(), "clean is not yet implemented")
			return nil
		},
	}
}
