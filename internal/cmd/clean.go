package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/spf13/cobra"
)

// gitRunner abstracts git operations for worktree/branch management.
type gitRunner interface {
	removeWorktree(path string) error
	pruneAndDeleteBranch(branch string) error
	removeOrphanBranches() (int, error)
}

// realGitRunner shells out to git.
type realGitRunner struct {
	repoPath string
}

func newRealGitRunner() *realGitRunner {
	return &realGitRunner{repoPath: "."}
}

func (r *realGitRunner) removeWorktree(path string) error {
	cmd := exec.Command("git", "worktree", "remove", "--force", path)
	cmd.Dir = r.repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w\n%s", err, out)
	}
	return nil
}

func (r *realGitRunner) pruneAndDeleteBranch(branch string) error {
	pruneCmd := exec.Command("git", "worktree", "prune")
	pruneCmd.Dir = r.repoPath
	if out, err := pruneCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree prune: %w\n%s", err, out)
	}
	delCmd := exec.Command("git", "branch", "-D", branch)
	delCmd.Dir = r.repoPath
	if out, err := delCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -D: %w\n%s", err, out)
	}
	return nil
}

func (r *realGitRunner) removeOrphanBranches() (int, error) {
	listCmd := exec.Command("git", "branch", "--list", "sandman/*", "--format", "%(refname:short)")
	listCmd.Dir = r.repoPath
	out, err := listCmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("git branch --list: %w\n%s", err, out)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return 0, nil
	}
	branches := strings.Split(raw, "\n")
	var removed int
	for _, branch := range branches {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			continue
		}
		delCmd := exec.Command("git", "branch", "-D", branch)
		delCmd.Dir = r.repoPath
		if delCmd.Run() != nil {
			continue
		}
		removed++
	}
	pruneCmd := exec.Command("git", "worktree", "prune")
	pruneCmd.Dir = r.repoPath
	_ = pruneCmd.Run()
	return removed, nil
}

// NewCleanCmd creates the clean command.
func NewCleanCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Clean up sandbox resources and stale worktrees",
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			success, _ := cmd.Flags().GetBool("success")
			failed, _ := cmd.Flags().GetBool("failed")
			stale, _ := cmd.Flags().GetBool("stale")

			if !all && !success && !failed && !stale {
				return fmt.Errorf("specify one of --all, --success, --failed, or --stale")
			}

			if stale && (all || success || failed) {
				return fmt.Errorf("--stale is mutually exclusive with --all, --success, and --failed")
			}

			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if cfg == nil {
				cfg = &config.Config{}
			}
			if cfg.WorktreeDir == "" {
				cfg.WorktreeDir = ".sandman/worktrees"
			}

			gr := deps.GitRunner
			if gr == nil {
				gr = newRealGitRunner()
			}

			staleRemoved, staleErr := daemon.CleanupStaleRunSnapshots(".sandman")
			if staleErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: cleanup stale run snapshots: %v\n", staleErr)
			}

			if stale {
				eventsList, err := deps.EventLog.Read()
				if err != nil {
					return fmt.Errorf("read event log: %w", err)
				}
				recovered, deadDirs, err := runCleanStale(eventsList, deps.EventLog)
				if err != nil {
					return fmt.Errorf("recover stale runs: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Recovered %d stale runs as aborted across %d dead directories.\n", recovered, deadDirs)
				return nil
			}

			if all {
				eventsList, err := deps.EventLog.Read()
				if err != nil {
					return fmt.Errorf("read event log: %w", err)
				}
				runs := events.ProjectRunStates(eventsList)
				for _, run := range runs {
					if branch := run.Branch(); branch != "" {
						wtPath := filepath.Join(cfg.WorktreeDir, branch)
						if err := gr.removeWorktree(wtPath); err != nil {
							_ = gr.pruneAndDeleteBranch(branch)
						}
					}
					if issueNum := run.IssueNumber(); issueNum > 0 {
						_ = os.RemoveAll(filepath.Join(".sandman", "logs", fmt.Sprintf("%d.log", issueNum)))
					}
				}
				removed, _ := gr.removeOrphanBranches()
				_ = os.RemoveAll(cfg.WorktreeDir)
				_ = os.RemoveAll(".sandman/logs")
				fmt.Fprintf(cmd.OutOrStdout(), "Cleaned %d stale branches and logs and %d stale run snapshots\n", removed, staleRemoved)
				return nil
			}

			eventsList, err := deps.EventLog.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
			}

			runs := events.ProjectRunStates(eventsList)

			var removed int
			for _, run := range runs {
				status := run.Status()
				if success && status != "success" {
					continue
				}
				if failed && status != "failure" && status != "aborted" {
					continue
				}

				branch := run.Branch()
				if branch != "" {
					wtPath := filepath.Join(cfg.WorktreeDir, branch)
					if err := gr.removeWorktree(wtPath); err != nil {
						_ = gr.pruneAndDeleteBranch(branch)
					}
				}
				if issueNum := run.IssueNumber(); issueNum > 0 {
					_ = os.RemoveAll(filepath.Join(".sandman", "logs", fmt.Sprintf("%d.log", issueNum)))
				}
				removed++
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Cleaned %d runs and %d stale run snapshots\n", removed, staleRemoved)
			return nil
		},
	}
	cmd.Flags().Bool("all", false, "Remove all worktrees and logs")
	cmd.Flags().Bool("success", false, "Remove successful runs only")
	cmd.Flags().Bool("failed", false, "Remove failed runs only")
	cmd.Flags().Bool("stale", false, "Recover stale runs in dead batches by emitting run.aborted events")
	return cmd
}

// runCleanStale performs the body of `sandman clean --stale` against the
// supplied event log. It reads the events, scans `.sandman/runs/` for dead
// batches, and emits a `run.aborted` event for every manifest issue whose
// RunState has not reached a terminal event. Returns the number of runs
// recovered and the number of dead directories processed.
//
// This is the same code path the `--stale` CLI flag executes, factored out
// so other long-lived commands (the portal) can invoke the same logic
// in-process without shelling out.
func runCleanStale(eventsList []events.Event, log events.EventLog) (recovered, deadDirs int, err error) {
	return daemon.RecoverStaleRuns(".sandman", eventsList, log)
}
