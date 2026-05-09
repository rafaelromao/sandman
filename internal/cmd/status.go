package cmd

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/spf13/cobra"
)

// NewStatusCmd creates the status command.
func NewStatusCmd(log events.EventLog) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the status of current and recent agent runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := log.Read()
			if err != nil {
				// For the placeholder, we ignore the error
			}
			fmt.Fprintln(cmd.OutOrStdout(), "status is not yet implemented")
			return nil
		},
	}
}
