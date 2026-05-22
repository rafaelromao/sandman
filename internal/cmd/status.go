package cmd

import (
	"fmt"
	"sort"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/spf13/cobra"
)

// NewStatusCmd creates the status command.
func NewStatusCmd(log events.EventLog) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the status of current and recent agent runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			liveRuns, err := daemon.NewLiveRunStore(".sandman").List()
			if err != nil {
				return fmt.Errorf("list live runs: %w", err)
			}

			if len(liveRuns) > 0 {
				sort.Slice(liveRuns, func(i, j int) bool {
					if !liveRuns[i].StartedAt.Equal(liveRuns[j].StartedAt) {
						return liveRuns[i].StartedAt.Before(liveRuns[j].StartedAt)
					}
					return liveRuns[i].RunID < liveRuns[j].RunID
				})

				fmt.Fprintln(cmd.OutOrStdout(), "Active runs:")
				for _, run := range liveRuns {
					elapsed := time.Since(run.StartedAt).Round(time.Second)
					fmt.Fprintf(cmd.OutOrStdout(), "  %s  issues %s  elapsed %s\n", run.RunID, formatIssueList(run.Issues), elapsed)
				}
				return nil
			}

			eventsList, err := log.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
			}

			started := make(map[string]events.Event)
			finished := make(map[string]bool)
			for _, e := range eventsList {
				switch e.Type {
				case "run.started":
					started[e.RunID] = e
				case "run.finished":
					finished[e.RunID] = true
				}
			}

			var active []events.Event
			for runID, e := range started {
				if !finished[runID] {
					active = append(active, e)
				}
			}

			if len(active) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No active runs")
				return nil
			}

			sort.Slice(active, func(i, j int) bool {
				return active[i].Issue < active[j].Issue
			})
			fmt.Fprintln(cmd.OutOrStdout(), "Active runs:")
			for _, e := range active {
				elapsed := time.Since(e.Timestamp).Round(time.Second)
				fmt.Fprintf(cmd.OutOrStdout(), "  #%d  elapsed %s\n", e.Issue, elapsed)
			}
			return nil
		},
	}
}
