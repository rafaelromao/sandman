package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/rafaelromao/sandman/internal/batch"
)

// NewRetryCmd creates the retry command.
func NewRetryCmd(runner batch.Runner) *cobra.Command {
	return &cobra.Command{
		Use:   "retry [issue-number]",
		Short: "Retry the last agent run for an issue",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "retry is not yet implemented")
			return nil
		},
	}
}
