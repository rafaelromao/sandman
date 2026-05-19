package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
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
			if cfg.WorktreeDir == "" {
				cfg.WorktreeDir = ".sandman/worktrees"
			}

			gr := deps.GitRunner
			if gr == nil {
				gr = newRealGitRunner()
			}

			if all {
				removed, _ := gr.removeOrphanBranches()
				_ = os.RemoveAll(cfg.WorktreeDir)
				_ = os.RemoveAll(".sandman/logs")
				fmt.Fprintf(cmd.OutOrStdout(), "Cleaned %d worktrees and logs\n", removed)
				return nil
			}

			eventsList, err := deps.EventLog.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
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
				if branch != "" {
					wtPath := filepath.Join(cfg.WorktreeDir, branch)
					if err := gr.removeWorktree(wtPath); err != nil {
						_ = gr.pruneAndDeleteBranch(branch)
					}
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
