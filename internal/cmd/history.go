package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Show the event log of all agent runs",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "history is not yet implemented")
	},
}
