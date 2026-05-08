package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Clean up sandbox resources and stale worktrees",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "clean is not yet implemented")
	},
}
