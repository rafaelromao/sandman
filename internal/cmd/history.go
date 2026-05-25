package cmd

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/spf13/cobra"
)

// NewHistoryCmd creates the history command.
func NewHistoryCmd(log events.EventLog) *cobra.Command {
	return &cobra.Command{
		Use:   "history",
		Short: "Show the event log of all agent runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			eventsList, err := log.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
			}

			runs := events.ProjectRunStates(eventsList)
			var completed []events.RunState
			for _, run := range runs {
				if !run.IsActive() {
					completed = append(completed, run)
				}
			}

			if len(completed) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No completed runs")
				return nil
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Completed runs:")
			for _, run := range completed {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  %s  %s\n", formatRunStateIssueLabel(run), run.Status(), run.Duration(), run.Branch())
			}
			return nil
		},
	}
}
