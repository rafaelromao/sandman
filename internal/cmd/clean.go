package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewCleanCmd creates the clean command.
func NewCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Clean up sandbox resources and stale worktrees",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "clean is not yet implemented")
			return nil
		},
	}
}
