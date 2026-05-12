package cmd

import (
	"fmt"
	"time"

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

			started := make(map[string]events.Event)
			var completed []events.Event
			for _, e := range eventsList {
				switch e.Type {
				case "run.started":
					started[e.RunID] = e
				case "run.finished":
					if _, ok := started[e.RunID]; ok {
						completed = append(completed, e)
					}
				}
			}

			if len(completed) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No completed runs")
				return nil
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Completed runs:")
			for _, e := range completed {
				status, _ := e.Payload["status"].(string)
				branch, _ := e.Payload["branch"].(string)
				startedEvt := started[e.RunID]
				duration := e.Timestamp.Sub(startedEvt.Timestamp).Round(time.Second)
				fmt.Fprintf(cmd.OutOrStdout(), "  #%d  %s  %s  %s\n", e.Issue, status, duration, branch)
			}
			return nil
		},
	}
}
