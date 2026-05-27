package cmd

import (
	"fmt"
	"sort"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/spf13/cobra"
)

// NewStatusCmd creates the status command.
func NewStatusCmd(log events.EventLog) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the status of active agent runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			eventsList, err := log.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
			}

			runs := events.ProjectRunStates(eventsList)
			var active []events.RunState
			for _, run := range runs {
				if run.IsActive() {
					active = append(active, run)
				}
			}

			if len(active) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No active runs")
				return nil
			}

			sort.Slice(active, func(i, j int) bool {
				if active[i].IsPromptOnly() || active[j].IsPromptOnly() {
					return active[i].Started.Timestamp.Before(active[j].Started.Timestamp)
				}
				return active[i].IssueNumber() < active[j].IssueNumber()
			})
			fmt.Fprintln(cmd.OutOrStdout(), "Active runs:")
			for _, run := range active {
				elapsed := time.Since(run.Started.Timestamp).Round(time.Second)
				fmt.Fprintf(cmd.OutOrStdout(), "  %s  elapsed %s\n", formatRunStateIssueLabel(run), elapsed)
			}
			return nil
		},
	}
}
