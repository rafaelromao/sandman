package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage Sandman configuration",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "config is not yet implemented")
	},
}
