package cmd

import (
	"fmt"
	"strconv"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/spf13/cobra"
)

// NewRunCmd creates the run command.
func NewRunCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run [issue...]",
		Short: "Run an AFK agent for specific issues",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("no issues provided")
			}

			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			issues := make([]int, len(args))
			for i, arg := range args {
				n, err := strconv.Atoi(arg)
				if err != nil {
					return fmt.Errorf("invalid issue number %q: %w", arg, err)
				}
				issues[i] = n
			}

			parallel, _ := cmd.Flags().GetInt("parallel")
			if parallel == 0 && cfg != nil {
				parallel = cfg.DefaultParallel
			}
			// Let 0 pass through — Orchestrator defaults to 4

			preserve, _ := cmd.Flags().GetBool("preserve")
			debug, _ := cmd.Flags().GetBool("debug")
			sandboxMode, _ := cmd.Flags().GetString("sandbox")
			isolatedContainers, _ := cmd.Flags().GetBool("isolated-containers")

			result, err := deps.BatchRunner.RunBatch(cmd.Context(), batch.Request{
				Issues:             issues,
				Parallel:           parallel,
				Preserve:           preserve,
				Debug:              debug,
				Sandbox:            sandboxMode,
				IsolatedContainers: isolatedContainers,
			})
			if result != nil {
				printSummary(cmd, result)
				for _, run := range result.Runs {
					if run.DebugInfo != "" {
						fmt.Fprint(cmd.OutOrStdout(), run.DebugInfo)
					}
				}
			}
			if err != nil {
				return fmt.Errorf("run batch: %w", err)
			}

			return nil
		},
	}
	cmd.Flags().Int("parallel", 0, "Limit parallel execution")
	cmd.Flags().Bool("preserve", false, "Preserve worktrees after successful runs")
	cmd.Flags().Bool("debug", false, "Print worktree path and instructions after failure")
	cmd.Flags().String("sandbox", "", "Sandbox mode: worktree, docker, or podman")
	cmd.Flags().Bool("isolated-containers", false, "Use one container per agent instead of a shared container")
	return cmd
}

func printSummary(cmd *cobra.Command, result *batch.Result) {
	var successCount, failureCount int
	for _, run := range result.Runs {
		if run.Status == "success" {
			successCount++
		} else {
			failureCount++
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Summary: %d succeeded, %d failed\n", successCount, failureCount)
	for _, run := range result.Runs {
		fmt.Fprintf(cmd.OutOrStdout(), "  #%d  %s  %s\n", run.IssueNumber, run.Status, run.Branch)
	}
}
