package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/rafaelromao/sandman/internal/events"
)

// NewHistoryCmd creates the history command.
func NewHistoryCmd(reader events.Reader) *cobra.Command {
	return &cobra.Command{
		Use:   "history",
		Short: "Show the event log of all agent runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := reader.Read()
			if err != nil {
				// For the placeholder, we ignore the error
			}
			fmt.Fprintln(cmd.OutOrStdout(), "history is not yet implemented")
			return nil
		},
	}
}
