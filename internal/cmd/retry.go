package cmd

import (
	"fmt"
	"strconv"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/spf13/cobra"
)

// NewRetryCmd creates the retry command.
func NewRetryCmd(deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "retry [issue-number]",
		Short: "Retry the last agent run for an issue",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("no issue provided")
			}

			issueNum, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid issue number %q: %w", args[0], err)
			}

			eventsList, err := deps.EventLog.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
			}

			var lastRun events.Event
			for _, e := range eventsList {
				if e.Type == "run.started" && e.Issue == issueNum {
					lastRun = e
				}
			}

			if lastRun.RunID == "" {
				return fmt.Errorf("no previous run found for issue #%d", issueNum)
			}

			branch, _ := lastRun.Payload["branch"].(string)
			req := batch.Request{
				Issues:   []int{issueNum},
				Branches: map[int]string{issueNum: branch},
			}
			result, err := deps.BatchRunner.RunBatch(cmd.Context(), req)
			if result != nil {
				printSummary(cmd, result)
			}
			if err != nil {
				return fmt.Errorf("run batch: %w", err)
			}

			return nil
		},
	}
}
