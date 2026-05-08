package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewRunCmd creates the run command.
func NewRunCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run [issue...]",
		Short: "Run an AFK agent for specific issues",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "run is not yet implemented")
			return nil
		},
	}
	cmd.Flags().Int("parallel", 4, "Limit parallel execution")
	return cmd
}
