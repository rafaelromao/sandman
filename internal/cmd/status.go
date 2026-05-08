package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the status of current and recent agent runs",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "status is not yet implemented")
	},
}
