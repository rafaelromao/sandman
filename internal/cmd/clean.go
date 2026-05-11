package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/spf13/cobra"
)

// NewCleanCmd creates the clean command.
func NewCleanCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Clean up sandbox resources and stale worktrees",
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			success, _ := cmd.Flags().GetBool("success")
			failed, _ := cmd.Flags().GetBool("failed")

			if !all && !success && !failed {
				return fmt.Errorf("specify one of --all, --success, or --failed")
			}

			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if cfg == nil {
				cfg = &config.Config{}
			}

			eventsList, err := deps.EventLog.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
			}

			if all {
				worktreeDir := cfg.WorktreeDir
				if worktreeDir == "" {
					worktreeDir = ".sandman/worktrees"
				}
				_ = os.RemoveAll(worktreeDir)
				_ = os.RemoveAll(".sandman/logs")
				fmt.Fprintln(cmd.OutOrStdout(), "Cleaned all worktrees and logs")
				return nil
			}

			started := make(map[string]map[string]any)
			issues := make(map[string]int)
			statuses := make(map[string]string)
			for _, e := range eventsList {
				switch e.Type {
				case "run.started":
					started[e.RunID] = e.Payload
					issues[e.RunID] = e.Issue
				case "run.finished":
					s, _ := e.Payload["status"].(string)
					statuses[e.RunID] = s
				}
			}

			var removed int
			for runID, payload := range started {
				status := statuses[runID]
				if success && status != "success" {
					continue
				}
				if failed && status != "failure" {
					continue
				}

				branch, _ := payload["branch"].(string)
				if branch != "" && cfg.WorktreeDir != "" {
					wtPath := filepath.Join(cfg.WorktreeDir, branch)
					_ = os.RemoveAll(wtPath)
				}
				if issueNum := issues[runID]; issueNum > 0 {
					_ = os.RemoveAll(filepath.Join(".sandman", "logs", fmt.Sprintf("%d.log", issueNum)))
				}
				removed++
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Cleaned %d runs\n", removed)
			return nil
		},
	}
	cmd.Flags().Bool("all", false, "Remove all worktrees and logs")
	cmd.Flags().Bool("success", false, "Remove successful runs only")
	cmd.Flags().Bool("failed", false, "Remove failed runs only")
	return cmd
}
