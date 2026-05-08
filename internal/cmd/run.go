package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run [issue-number]",
	Short: "Run an AFK agent for a specific issue",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "run is not yet implemented")
	},
}
