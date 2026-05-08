package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new Sandman project in the current directory",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "init is not yet implemented")
	},
}
