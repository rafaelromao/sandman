package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var retryCmd = &cobra.Command{
	Use:   "retry [issue-number]",
	Short: "Retry the last agent run for an issue",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "retry is not yet implemented")
	},
}
