package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/spf13/cobra"
)

type gitRunner interface {
	removeWorktree(path string) error
	pruneAndDeleteBranch(branch string) error
	removeOrphanBranches() (int, error)
}

type realGitRunner struct {
	repoPath string
}

func newRealGitRunner(repoPath string) *realGitRunner {
	if repoPath == "" {
		repoPath = "."
	}
	return &realGitRunner{repoPath: repoPath}
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

type cleanAction struct {
	BatchID   string
	BatchPath string
	Worktree  string
	Kind      batchindex.Kind
	IsUnavail bool
}

func NewCleanCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Clean up sandbox resources and stale worktrees",
		RunE: func(cmd *cobra.Command, args []string) error {
			archived, _ := cmd.Flags().GetBool("archived")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			stale, _ := cmd.Flags().GetBool("stale")
			orphaned, _ := cmd.Flags().GetBool("orphaned")

			if stale && (archived || dryRun || orphaned) {
				return fmt.Errorf("--stale is mutually exclusive with --archived, --dry-run, and --orphaned")
			}
			if orphaned && (archived || stale) {
				return fmt.Errorf("--orphaned is mutually exclusive with --archived and --stale")
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

			repoRoot := deps.RepoRoot
			if repoRoot == "" {
				repoRoot = "."
			}
			layout := paths.NewLayout(cfg, repoRoot)

			gr := deps.GitRunner
			if gr == nil {
				gr = newRealGitRunner(layout.RepoRoot)
			}

			if stale {
				eventsList, err := deps.EventLog.Read()
				if err != nil {
					return fmt.Errorf("read event log: %w", err)
				}
				staleRemoved, staleErr := daemon.CleanupStaleRunSnapshots(layout.SandmanDir)
				if staleErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: cleanup stale run snapshots: %v\n", staleErr)
				}
				recovered, deadDirs, err := runCleanStale(layout, eventsList, deps.EventLog)
				if err != nil {
					return fmt.Errorf("recover stale runs: %w", err)
				}
				_ = staleRemoved
				fmt.Fprintf(cmd.OutOrStdout(), "Recovered %d stale runs as aborted across %d dead directories.\n", recovered, deadDirs)
				return nil
			}

			if orphaned {
				return runCleanOrphaned(cmd, deps, layout, dryRun)
			}

			idx, err := batchindex.Load(layout.BatchesIndexPath)
			if err != nil {
				return fmt.Errorf("load batches index: %w", err)
			}

			if err := idx.EnsureStatus(); err != nil {
				return fmt.Errorf("ensure status: %w", err)
			}

			var targetStatus batchindex.Status
			if archived {
				targetStatus = batchindex.StatusArchived
			} else {
				targetStatus = batchindex.StatusActive
			}

			actions := collectCleanActions(idx, targetStatus)

			if dryRun {
				printDryRun(cmd, actions)
				return nil
			}

			removed, err := executeClean(actions, gr, idx, layout)
			if err != nil {
				return fmt.Errorf("execute clean: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Removed %d batch entries.\n", removed)

			return nil
		},
	}
	cmd.Flags().Bool("archived", false, "Remove archived batches (combined with unavailable)")
	cmd.Flags().Bool("dry-run", false, "Print intended deletions without performing I/O")
	cmd.Flags().Bool("stale", false, "Recover stale runs in dead batches by emitting run.aborted events")
	cmd.Flags().Bool("orphaned", false, "Remove orphaned test batch directories (no matching run.started event and no live daemon socket)")
	return cmd
}

func collectCleanActions(idx *batchindex.Index, targetStatus batchindex.Status) []cleanAction {
	var actions []cleanAction
	for _, entry := range idx.Entries {
		if entry.Status == targetStatus || entry.Status == batchindex.StatusUnavailable {
			action := cleanAction{
				BatchID:   entry.ID,
				BatchPath: entry.Path,
				Kind:      entry.Kind,
				IsUnavail: entry.Status == batchindex.StatusUnavailable,
			}
			if !action.IsUnavail && entry.Path != "" {
				manifest, err := batchindex.ReadManifest(entry.Path)
				if err == nil && manifest.WorktreePath != "" {
					action.Worktree = manifest.WorktreePath
				}
			}
			actions = append(actions, action)
		}
	}
	return actions
}

func printDryRun(cmd *cobra.Command, actions []cleanAction) {
	if len(actions) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No batches to clean.\n")
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Would remove %d batch entries:\n", len(actions))
	for _, a := range actions {
		what := "batch"
		if a.Kind != "" {
			what = string(a.Kind)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  - [%s] %s (path: %s", what, a.BatchID, a.BatchPath)
		if a.Worktree != "" {
			fmt.Fprintf(cmd.OutOrStdout(), ", worktree: %s", a.Worktree)
		}
		fmt.Fprintf(cmd.OutOrStdout(), ")\n")
	}
}

func executeClean(actions []cleanAction, gr gitRunner, idx *batchindex.Index, layout paths.Layout) (int, error) {
	if len(actions) == 0 {
		return 0, nil
	}

	actionIDs := make(map[string]bool)
	for _, a := range actions {
		actionIDs[a.BatchID] = true
	}

	var removed int
	for _, a := range actions {
		if a.Worktree != "" {
			if err := gr.removeWorktree(a.Worktree); err != nil {
				_ = gr.pruneAndDeleteBranch(filepath.Base(a.Worktree))
			}
		}
		if a.BatchPath != "" && !a.IsUnavail {
			_ = os.RemoveAll(a.BatchPath)
		}
		removed++
	}

	var kept []batchindex.Entry
	for _, entry := range idx.Entries {
		if !actionIDs[entry.ID] {
			kept = append(kept, entry)
		}
	}
	idx.Entries = kept

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return 0, fmt.Errorf("save batches index: %w", err)
	}

	return removed, nil
}

func runCleanStale(layout paths.Layout, eventsList []events.Event, log events.EventLog) (recovered, deadDirs int, err error) {
	return daemon.RecoverStaleRuns(layout.SandmanDir, eventsList, log)
}

func runCleanOrphaned(cmd *cobra.Command, deps Dependencies, layout paths.Layout, dryRun bool) error {
	probe := deps.RunActivityProbe
	if probe == nil {
		probe = daemon.IsRunActive
	}
	plan, err := daemon.PlanOrphanedTestBatches(layout.SandmanDir, deps.EventLog, probe)
	if err != nil {
		return fmt.Errorf("plan orphaned batches: %w", err)
	}

	if dryRun {
		if len(plan) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No orphaned batch directories found.")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Would remove %d orphaned batch director(ies):\n", len(plan))
		for _, p := range plan {
			fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", p)
		}
		return nil
	}

	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}

	pruned := make(map[string]struct{}, len(plan))
	for _, p := range plan {
		pruned[filepath.Base(p)] = struct{}{}
	}
	var kept []batchindex.Entry
	for _, entry := range idx.Entries {
		if _, drop := pruned[entry.ID]; drop {
			continue
		}
		kept = append(kept, entry)
	}
	idx.Entries = kept

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	removed, err := daemon.CleanupOrphanedTestBatches(layout.SandmanDir, deps.EventLog, probe)
	if err != nil {
		return fmt.Errorf("cleanup orphaned batches: %w", err)
	}

	if len(removed) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No orphaned batch directories found.")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed %d orphaned batch director(ies):\n", len(removed))
	for _, p := range removed {
		fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", p)
	}
	return nil
}
