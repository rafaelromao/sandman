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
		Short: "Clean up sandbox resources, stale worktrees, and temp files",
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			archived, _ := cmd.Flags().GetBool("archived")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			stale, _ := cmd.Flags().GetBool("stale")
			orphaned, _ := cmd.Flags().GetBool("orphaned")

			if !all && !archived && !stale && !orphaned {
				return MarkUsage(fmt.Errorf("clean requires an explicit mode flag: --all, --archived, --stale, or --orphaned"))
			}

			if stale && (archived || dryRun || orphaned) {
				return fmt.Errorf("--stale is mutually exclusive with --archived, --dry-run, and --orphaned")
			}
			if orphaned && (archived || stale || all) {
				return fmt.Errorf("--orphaned is mutually exclusive with --archived, --stale, and --all")
			}
			if all && (archived || stale || orphaned) {
				return fmt.Errorf("--all is mutually exclusive with --archived, --stale, and --orphaned")
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
				tempDirs, images := runCleanTemps(cmd, deps, layout, false)
				printCleanReport(cmd, nil, nil, tempDirs, images, false)
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

			targetStatus := batchindex.StatusArchived
			if !archived {
				targetStatus = batchindex.StatusActive
			}

			actions := collectCleanActions(idx, targetStatus)
			if actions == nil {
				actions = []cleanAction{}
			}

			if dryRun {
				tempDirs, images := runCleanTemps(cmd, deps, layout, true)
				printCleanReport(cmd, actions, nil, tempDirs, images, true)
				return nil
			}

			if _, err := executeClean(actions, gr, idx, layout); err != nil {
				return fmt.Errorf("execute clean: %w", err)
			}

			tempDirs, images := runCleanTemps(cmd, deps, layout, false)
			printCleanReport(cmd, actions, nil, tempDirs, images, false)

			return nil
		},
	}
	cmd.Flags().Bool("all", false, "Remove active batches (combined with unavailable)")
	cmd.Flags().Bool("archived", false, "Remove archived batches (combined with unavailable)")
	cmd.Flags().Bool("dry-run", false, "Print intended deletions without performing I/O")
	cmd.Flags().Bool("stale", false, "Recover stale runs in dead batches by emitting run.aborted events")
	cmd.Flags().Bool("orphaned", false, "Remove orphaned test batch directories (no matching run.started event and no live daemon socket)")
	cmd.Long = "Clean up sandbox resources, stale worktrees, and Sandman-owned temp files.\n\nAlso removes temp directories under the system temp dir (e.g. /tmp/) that were created\nby Sandman and are no longer in use, as well as container images tagged with the\nsandman-smoke-* prefix. Only Sandman-owned paths are removed; unrelated temp content\nis never touched."
	return cmd
}

func collectCleanActions(idx *batchindex.Index, targetStatus batchindex.Status) []cleanAction {
	var actions []cleanAction
	for _, entry := range idx.Batches {
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

func printCleanReport(cmd *cobra.Command, actions []cleanAction, orphanPaths []string, tempDirs []string, images []string, dryRun bool) {
	out := cmd.OutOrStdout()

	if actions != nil && len(actions) == 0 && len(orphanPaths) == 0 && len(tempDirs) == 0 && len(images) == 0 {
		fmt.Fprintln(out, "Nothing to remove.")
		return
	}

	if len(actions) > 0 {
		if dryRun {
			fmt.Fprintf(out, "Would remove %d batch entries:\n", len(actions))
		} else {
			fmt.Fprintf(out, "Removed %d batch entries:\n", len(actions))
		}
		for _, a := range actions {
			what := "batch"
			if a.Kind != "" {
				what = string(a.Kind)
			}
			fmt.Fprintf(out, "  - [%s] %s (path: %s", what, a.BatchID, a.BatchPath)
			if a.Worktree != "" {
				fmt.Fprintf(out, ", worktree: %s", a.Worktree)
			}
			fmt.Fprintln(out, ")")
		}
	}

	if len(orphanPaths) > 0 {
		if dryRun {
			fmt.Fprintf(out, "Would remove %d orphaned batch director(ies):\n", len(orphanPaths))
		} else {
			fmt.Fprintf(out, "Removed %d orphaned batch director(ies):\n", len(orphanPaths))
		}
		for _, p := range orphanPaths {
			fmt.Fprintf(out, "  - %s\n", p)
		}
	}

	if len(tempDirs) > 0 {
		if dryRun {
			fmt.Fprintf(out, "Would remove %d temp director(ies):\n", len(tempDirs))
		} else {
			fmt.Fprintf(out, "Removed %d temp director(ies):\n", len(tempDirs))
		}
		for _, d := range tempDirs {
			fmt.Fprintf(out, "  - %s\n", d)
		}
	}

	if len(images) > 0 {
		if dryRun {
			fmt.Fprintf(out, "Would remove %d container image(s):\n", len(images))
		} else {
			fmt.Fprintf(out, "Removed %d container image(s):\n", len(images))
		}
		for _, img := range images {
			fmt.Fprintf(out, "  - %s\n", img)
		}
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

	var kept []batchindex.Batch
	for _, entry := range idx.Batches {
		if !actionIDs[entry.ID] {
			kept = append(kept, entry)
		}
	}
	idx.Batches = kept

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
		tempDirs, images := runCleanTemps(cmd, deps, layout, true)
		if len(plan) == 0 && len(tempDirs) == 0 && len(images) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "Nothing to remove.")
			return nil
		}
		printCleanReport(cmd, nil, plan, tempDirs, images, true)
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
	var kept []batchindex.Batch
	for _, entry := range idx.Batches {
		if _, drop := pruned[entry.ID]; drop {
			continue
		}
		kept = append(kept, entry)
	}
	idx.Batches = kept

	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}

	removed, err := daemon.CleanupOrphanedTestBatches(layout.SandmanDir, deps.EventLog, probe)
	if err != nil {
		return fmt.Errorf("cleanup orphaned batches: %w", err)
	}

	tempDirs, images := runCleanTemps(cmd, deps, layout, false)
	if len(removed) == 0 && len(tempDirs) == 0 && len(images) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Nothing to remove.")
		return nil
	}
	printCleanReport(cmd, nil, removed, tempDirs, images, false)
	return nil
}

func runCleanTemps(cmd *cobra.Command, deps Dependencies, layout paths.Layout, dryRun bool) (tempDirs []string, images []string) {
	tc := deps.TempCleaner
	if tc == nil {
		tc = &realTempCleaner{}
	}

	tempDir := os.TempDir()
	dirs, err := tc.ScanTempDirs(tempDir)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: scan temp dirs: %v\n", err)
		return nil, nil
	}

	runtime := tc.ResolveRuntime()
	if runtime != "" {
		images, err = tc.ListContainerImages(runtime)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: list container images: %v\n", err)
		}
	}

	if dryRun {
		return dirs, images
	}

	var removedDirs, removedImgs int
	for _, d := range dirs {
		if err := tc.RemoveTempDir(d); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove temp dir %s: %v\n", d, err)
		} else {
			removedDirs++
		}
	}
	for _, img := range images {
		if err := tc.RemoveContainerImage(runtime, img); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove image %s: %v\n", img, err)
		} else {
			removedImgs++
		}
	}
	return dirs[:removedDirs], images[:removedImgs]
}
