package cmd

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/spf13/cobra"
)

// NewStrandedCmd creates the `sandman stranded` subcommand. It prints a
// remediation line for every stranded worktree under the configured
// worktree_base, or nothing if none are found. Output is non-destructive.
func NewStrandedCmd(deps Dependencies) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "stranded",
		Short: "Print stranded worktrees and remediation commands",
		Long: `Detect sandman-managed worktrees whose HEAD points to a different branch
than the directory name expects, and print a remediation command for each.
Non-destructive: never checks out branches or removes worktrees.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if cfg == nil {
				cfg = &config.Config{}
			}
			if cfg.WorktreeDir == "" {
				cfg.WorktreeDir = config.DefaultWorktreeDir
			}
			worktreeBase, err := filepath.Abs(cfg.WorktreeDir)
			if err != nil {
				return fmt.Errorf("resolve worktree_dir: %w", err)
			}
			results := sandbox.ListStrandedWorktrees(".", worktreeBase)
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(results)
			}
			out := cmd.OutOrStdout()
			for _, r := range results {
				actual := r.ActualBranch
				if actual == "" {
					actual = "detached HEAD"
				}
				if sandbox.BranchExists(".", strings.TrimPrefix(r.ExpectedBranch, "refs/heads/")) {
					shortBranch := strings.TrimPrefix(r.ExpectedBranch, "refs/heads/")
					fmt.Fprintf(out, "Worktree %s is on %s, expected %s. Run: git -C %s checkout -f %s\n",
						r.Path, actual, r.ExpectedBranch, r.Path, shortBranch)
				} else {
					fmt.Fprintf(out, "Worktree %s is on %s, expected %s (branch does not exist locally)\n",
						r.Path, actual, r.ExpectedBranch)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit results as JSON")
	return cmd
}
