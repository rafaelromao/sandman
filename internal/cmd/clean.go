package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		if isBranchNotFoundError(err, out) {
			return nil
		}
		return fmt.Errorf("git branch -D: %w\n%s", err, out)
	}
	return nil
}

func isBranchNotFoundError(err error, output []byte) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(string(output) + "\n" + err.Error())
	if !strings.Contains(message, "branch") {
		return false
	}
	for _, phrase := range []string{"not found", "does not exist", "not a valid branch"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
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
	Status    batchindex.Status
	IsUnavail bool
	Branch    string
	Err       error
}

type cleanOutcome struct {
	Action cleanAction
	Err    error
}

type osCleanupRemover struct{}

func (osCleanupRemover) RemoveAll(path string) error { return os.RemoveAll(path) }

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
			remover := deps.CleanupRemover
			if remover == nil {
				remover = osCleanupRemover{}
			}

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
				tempDirs, images, tempErr := runCleanTemps(cmd, deps, layout, false)
				printCleanReport(cmd, nil, nil, tempDirs, images, false)
				return tempErr
			}

			if orphaned {
				return runCleanOrphaned(cmd, deps, layout, dryRun)
			}

			if all {
				eventsList, err := deps.EventLog.Read()
				if err != nil {
					return fmt.Errorf("read event log: %w", err)
				}
				if _, staleErr := daemon.CleanupStaleRunSnapshots(layout.SandmanDir); staleErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: cleanup stale run snapshots: %v\n", staleErr)
				}
				recovered, deadDirs, err := runCleanStale(layout, eventsList, deps.EventLog)
				if err != nil {
					return fmt.Errorf("recover stale runs: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Recovered %d stale runs as aborted across %d dead directories.\n", recovered, deadDirs)

				probe := deps.RunActivityProbe
				if probe == nil {
					probe = daemon.IsRunActive
				}
				orphanPlan, err := daemon.PlanOrphanedTestBatches(layout.SandmanDir, deps.EventLog, probe)
				if err != nil {
					return fmt.Errorf("plan orphaned batches: %w", err)
				}
				idx, err := batchindex.Load(layout.BatchesIndexPath)
				if err != nil {
					return fmt.Errorf("load batches index: %w", err)
				}
				if err := idx.EnsureStatus(); err != nil {
					return fmt.Errorf("ensure status: %w", err)
				}

				actions := collectCleanActions(idx, batchindex.StatusArchived)
				if actions == nil {
					actions = []cleanAction{}
				}

				var orphanRemoved []string
				var outcomes []cleanOutcome
				var cleanErr error
				if !dryRun {
					outcomes, cleanErr = executeClean(actions, gr, layout, remover)
					orphanRemoved, err = cleanupOrphanedBatches(layout, deps.EventLog, probe, remover)
					cleanErr = errors.Join(cleanErr, err)
				} else {
					outcomes, cleanErr = validateCleanActions(actions, layout)
					if orphanErr := validateOrphanPaths(orphanPlan, layout); orphanErr != nil {
						cleanErr = errors.Join(cleanErr, orphanErr)
					}
					orphanRemoved = orphanPlan
				}

				tempDirs, images, tempErr := runCleanTemps(cmd, deps, layout, dryRun)
				reportActions := successfulActions(outcomes)
				printCleanReport(cmd, reportActions, orphanRemoved, tempDirs, images, dryRun)
				cleanErr = errors.Join(cleanErr, tempErr)
				if cleanErr != nil {
					return fmt.Errorf("execute clean: %w", cleanErr)
				}
				return nil
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
				outcomes, cleanErr := validateCleanActions(actions, layout)
				tempDirs, images, tempErr := runCleanTemps(cmd, deps, layout, true)
				printCleanReport(cmd, successfulActions(outcomes), nil, tempDirs, images, true)
				cleanErr = errors.Join(cleanErr, tempErr)
				if cleanErr != nil {
					return fmt.Errorf("validate clean: %w", cleanErr)
				}
				return nil
			}

			outcomes, cleanErr := executeClean(actions, gr, layout, remover)

			tempDirs, images, tempErr := runCleanTemps(cmd, deps, layout, false)
			printCleanReport(cmd, successfulActions(outcomes), nil, tempDirs, images, false)
			cleanErr = errors.Join(cleanErr, tempErr)
			if cleanErr != nil {
				return fmt.Errorf("execute clean: %w", cleanErr)
			}

			return nil
		},
	}
	cmd.Flags().Bool("all", false, "Run every cleanup pass in sequence without touching active batches or their worktrees")
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
				Status:    entry.Status,
				IsUnavail: entry.Status == batchindex.StatusUnavailable,
			}
			if !action.IsUnavail && entry.Path != "" {
				manifest, err := batchindex.ReadManifest(entry.Path)
				if err != nil {
					action.Err = fmt.Errorf("read manifest for %s: %w", entry.ID, err)
				} else {
					action.Worktree = manifest.WorktreePath
					action.Branch = manifest.Branch
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

func executeClean(actions []cleanAction, gr gitRunner, layout paths.Layout, remover CleanupRemover) ([]cleanOutcome, error) {
	if len(actions) == 0 {
		return nil, nil
	}

	var outcomes []cleanOutcome
	var actionErrs []error
	if err := batchindex.Update(layout.BatchesIndexPath, func(current *batchindex.Index) error {
		for _, a := range actions {
			entry := current.Resolve(a.BatchID)
			if entry == nil || entry.Path != a.BatchPath || !cleanStatusCompatible(a.Status, a.IsUnavail, entry.Status) {
				continue
			}
			if a.Err != nil {
				outcomes = append(outcomes, cleanOutcome{Action: a, Err: a.Err})
				actionErrs = append(actionErrs, a.Err)
				continue
			}
			if err := validateCleanAction(a, layout); err != nil {
				outcomes = append(outcomes, cleanOutcome{Action: a, Err: err})
				actionErrs = append(actionErrs, err)
				continue
			}
			if a.Worktree != "" {
				if _, statErr := os.Stat(a.Worktree); statErr == nil {
					if err := gr.removeWorktree(a.Worktree); err != nil {
						err = fmt.Errorf("remove worktree %s: %w", a.Worktree, err)
						outcomes = append(outcomes, cleanOutcome{Action: a, Err: err})
						actionErrs = append(actionErrs, err)
						continue
					}
				} else if !os.IsNotExist(statErr) {
					err := fmt.Errorf("stat worktree %s: %w", a.Worktree, statErr)
					outcomes = append(outcomes, cleanOutcome{Action: a, Err: err})
					actionErrs = append(actionErrs, err)
					continue
				}
			}
			if a.Branch != "" || a.Worktree != "" {
				branch := a.Branch
				if branch == "" {
					branch = filepath.Base(a.Worktree)
				}
				if branchErr := validateOwnedBranch(branch); branchErr != nil {
					err := fmt.Errorf("validate branch %s: %w", branch, branchErr)
					outcomes = append(outcomes, cleanOutcome{Action: a, Err: err})
					actionErrs = append(actionErrs, err)
					continue
				}
				if err := gr.pruneAndDeleteBranch(branch); err != nil && !isBranchNotFoundError(err, nil) {
					err = fmt.Errorf("delete branch %s: %w", branch, err)
					outcomes = append(outcomes, cleanOutcome{Action: a, Err: err})
					actionErrs = append(actionErrs, err)
					continue
				}
			}
			if a.BatchPath != "" && !a.IsUnavail {
				if err := remover.RemoveAll(a.BatchPath); err != nil {
					err = fmt.Errorf("remove batch %s: %w", a.BatchPath, err)
					outcomes = append(outcomes, cleanOutcome{Action: a, Err: err})
					actionErrs = append(actionErrs, err)
					continue
				}
				if err := confirmRemoved(a.BatchPath); err != nil {
					err = fmt.Errorf("confirm batch removal %s: %w", a.BatchPath, err)
					outcomes = append(outcomes, cleanOutcome{Action: a, Err: err})
					actionErrs = append(actionErrs, err)
					continue
				}
			}
			outcomes = append(outcomes, cleanOutcome{Action: a})
			for i := range current.Batches {
				if current.Batches[i].ID == a.BatchID {
					current.Batches = append(current.Batches[:i], current.Batches[i+1:]...)
					break
				}
			}
		}
		return nil
	}); err != nil {
		return outcomes, fmt.Errorf("save batches index: %w", err)
	}
	return outcomes, errors.Join(actionErrs...)
}

func cleanStatusCompatible(planned batchindex.Status, plannedUnavailable bool, current batchindex.Status) bool {
	if plannedUnavailable {
		return current == batchindex.StatusUnavailable
	}
	return current == planned || current == batchindex.StatusUnavailable
}

func successfulActions(outcomes []cleanOutcome) []cleanAction {
	actions := make([]cleanAction, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.Err == nil {
			actions = append(actions, outcome.Action)
		}
	}
	return actions
}

func validateCleanActions(actions []cleanAction, layout paths.Layout) ([]cleanOutcome, error) {
	outcomes := make([]cleanOutcome, 0, len(actions))
	var errs []error
	for _, action := range actions {
		if action.Err != nil {
			outcomes = append(outcomes, cleanOutcome{Action: action, Err: action.Err})
			errs = append(errs, action.Err)
			continue
		}
		if err := validateCleanAction(action, layout); err != nil {
			outcomes = append(outcomes, cleanOutcome{Action: action, Err: err})
			errs = append(errs, err)
			continue
		}
		outcomes = append(outcomes, cleanOutcome{Action: action})
	}
	return outcomes, errors.Join(errs...)
}

func validateCleanAction(action cleanAction, layout paths.Layout) error {
	if action.BatchPath != "" && !action.IsUnavail {
		if err := validateOwnedPath(action.BatchPath, layout.BatchesDir, layout.ArchiveDir); err != nil {
			return fmt.Errorf("validate batch %s: %w", action.BatchID, err)
		}
	}
	if action.Worktree != "" {
		if err := validateOwnedPath(action.Worktree, layout.WorktreeDir); err != nil {
			return fmt.Errorf("validate worktree %s: %w", action.BatchID, err)
		}
	}
	return nil
}

func validateOwnedPath(candidate string, roots ...string) error {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || !filepath.IsAbs(candidate) {
		return fmt.Errorf("path must be absolute and non-empty")
	}
	candidate = filepath.Clean(candidate)
	if candidate == string(filepath.Separator) {
		return fmt.Errorf("path must not be filesystem root")
	}
	for _, root := range roots {
		root, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, candidate)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		resolvedRoot, err := existingResolvedPath(root)
		if err != nil {
			continue
		}
		resolvedCandidate, err := existingResolvedPath(candidate)
		if err != nil {
			continue
		}
		resolvedRel, err := filepath.Rel(resolvedRoot, resolvedCandidate)
		if err == nil && resolvedRel != "." && resolvedRel != ".." && !strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("path is outside trusted roots")
}

func validateOwnedBranch(branch string) error {
	if branch == "" {
		return fmt.Errorf("branch must be non-empty")
	}
	if filepath.IsAbs(branch) || filepath.Clean(branch) != branch || strings.ContainsAny(branch, `\`) {
		return fmt.Errorf("branch name is unsafe")
	}
	// Accept the runtime's branch conventions:
	//   - issue-driven default:   "<n>-<slug>"
	//   - issue-driven feature:   "<feature>/<n>-<slug>"
	//   - legacy issue-driven:   "sandman/<n>-<slug>"
	//   - sidecars:                "sandman/review-<n>-<commentID>", "sandman/built-with-sandman"
	//   - prompt-only:             "sandman/<slug>-<timestamp>"
	// The "sandman/" prefix is the legacy namespace marker; the new
	// convention uses the base branch as the prefix-owner instead.
	// The regex accepts both shapes so clean can remove branches the
	// runtime (or its legacy form) ever created.
	ownedByIssuePattern := issueBranchNamePattern.MatchString(branch)
	ownedByLegacyPrefix := strings.HasPrefix(branch, "sandman/") && len(branch) > len("sandman/")
	if !ownedByIssuePattern && !ownedByLegacyPrefix {
		return fmt.Errorf("branch is not Sandman-owned")
	}
	if strings.Contains(branch, "//") || strings.Contains(branch, "..") || strings.ContainsAny(branch, "~^:?*[\x00") || strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, "/") {
		return fmt.Errorf("branch name is unsafe")
	}
	return nil
}

// issueBranchNamePattern matches the runtime's issue-driven branch shapes:
//   - default-base: "<n>-<slug>"
//   - feature-base: "<feature>/<n>-<slug>" (the feature prefix can be any
//     non-empty path that does not contain a slash or unsafe character).
var issueBranchNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+/\d+-[a-z0-9-]+$|^\d+-[a-z0-9-]+$`)

func existingResolvedPath(path string) (string, error) {
	for current := path; ; current = filepath.Dir(current) {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			rel, relErr := filepath.Rel(current, path)
			if relErr != nil {
				return "", relErr
			}
			return filepath.Join(resolved, rel), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
	}
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
		if orphanErr := validateOrphanPaths(plan, layout); orphanErr != nil {
			return fmt.Errorf("validate orphan paths: %w", orphanErr)
		}
		tempDirs, images, tempErr := runCleanTemps(cmd, deps, layout, true)
		if len(plan) == 0 && len(tempDirs) == 0 && len(images) == 0 {
			if tempErr != nil {
				return tempErr
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Nothing to remove.")
			return nil
		}
		printCleanReport(cmd, nil, plan, tempDirs, images, true)
		return tempErr
	}

	remover := deps.CleanupRemover
	if remover == nil {
		remover = osCleanupRemover{}
	}
	removed, cleanupErr := cleanupOrphanedBatches(layout, deps.EventLog, probe, remover)

	tempDirs, images, tempErr := runCleanTemps(cmd, deps, layout, false)
	cleanupErr = errors.Join(cleanupErr, tempErr)
	if len(removed) == 0 && len(tempDirs) == 0 && len(images) == 0 {
		if cleanupErr == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "Nothing to remove.")
		}
		if cleanupErr != nil {
			return cleanupErr
		}
		return nil
	}
	printCleanReport(cmd, nil, removed, tempDirs, images, false)
	return cleanupErr
}

func validateOrphanPaths(pathsToCheck []string, layout paths.Layout) error {
	var errs []error
	for _, path := range pathsToCheck {
		absolute, err := filepath.Abs(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve orphan path %q: %w", path, err))
			continue
		}
		if err := validateOwnedPath(absolute, layout.BatchesDir); err != nil {
			errs = append(errs, fmt.Errorf("validate orphan path %q: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func cleanupOrphanedBatches(layout paths.Layout, log events.EventLog, probe func(string) bool, remover CleanupRemover) ([]string, error) {
	plan, err := daemon.PlanOrphanedTestBatches(layout.SandmanDir, log, probe)
	if err != nil {
		return nil, fmt.Errorf("plan orphaned batches: %w", err)
	}
	var removed []string
	var cleanupErrs []error
	for _, path := range plan {
		if err := validateOrphanPaths([]string{path}, layout); err != nil {
			cleanupErrs = append(cleanupErrs, err)
			continue
		}
		if err := remover.RemoveAll(path); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove orphan %s: %w", path, err))
			continue
		}
		if err := confirmRemoved(path); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("confirm orphan removal %s: %w", path, err))
			continue
		}
		removed = append(removed, path)
	}
	removedByID := make(map[string]string, len(removed))
	for _, path := range removed {
		absolute, err := filepath.Abs(path)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("resolve removed orphan path %q: %w", path, err))
			continue
		}
		removedByID[filepath.Base(path)] = filepath.Clean(absolute)
	}
	if err := batchindex.Update(layout.BatchesIndexPath, func(idx *batchindex.Index) error {
		kept := idx.Batches[:0]
		for _, entry := range idx.Batches {
			if path, ok := removedByID[entry.ID]; ok && filepath.Clean(entry.Path) == path {
				continue
			}
			kept = append(kept, entry)
		}
		idx.Batches = kept
		return nil
	}); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("update batches index: %w", err))
	}
	return removed, errors.Join(cleanupErrs...)
}

func confirmRemoved(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("path still exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// pruneBatchesIndexByOrphanPlan removes the index entries whose BatchID matches
// the basename of any path in plan, then atomically saves the index. It is the
// shared prune step used by both the standalone --orphaned mode and the --all
// umbrella flag.
func pruneBatchesIndexByOrphanPlan(indexPath string, plan []string) error {
	pruned := make(map[string]string, len(plan))
	for _, p := range plan {
		absolutePath, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("resolve orphan plan path %q: %w", p, err)
		}
		pruned[filepath.Base(p)] = absolutePath
	}
	return batchindex.Update(indexPath, func(idx *batchindex.Index) error {
		var kept []batchindex.Batch
		for _, entry := range idx.Batches {
			if plannedPath, drop := pruned[entry.ID]; drop && filepath.Clean(entry.Path) == filepath.Clean(plannedPath) {
				if _, err := os.Stat(entry.Path); os.IsNotExist(err) {
					continue
				}
			}
			kept = append(kept, entry)
		}
		idx.Batches = kept
		return nil
	})
}

func runCleanTemps(cmd *cobra.Command, deps Dependencies, layout paths.Layout, dryRun bool) (tempDirs []string, images []string, cleanupErr error) {
	tc := deps.TempCleaner
	if tc == nil {
		tc = &realTempCleaner{}
	}

	tempDir := os.TempDir()
	dirs, err := tc.ScanTempDirs(tempDir)
	if err != nil {
		return nil, nil, fmt.Errorf("scan temp dirs: %w", err)
	}

	runtime := tc.ResolveRuntime()
	if runtime != "" {
		images, err = tc.ListContainerImages(runtime)
		if err != nil {
			cleanupErr = fmt.Errorf("list container images: %w", err)
			images = nil
		}
	}

	if dryRun {
		return dirs, images, cleanupErr
	}

	var removalErrs []error
	for _, d := range dirs {
		if err := tc.RemoveTempDir(d); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove temp dir %s: %v\n", d, err)
			removalErrs = append(removalErrs, fmt.Errorf("remove temp dir %s: %w", d, err))
		} else {
			tempDirs = append(tempDirs, d)
		}
	}
	imageCandidates := images
	images = nil
	for _, img := range imageCandidates {
		if err := tc.RemoveContainerImage(runtime, img); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove image %s: %v\n", img, err)
			removalErrs = append(removalErrs, fmt.Errorf("remove image %s: %w", img, err))
		} else {
			images = append(images, img)
		}
	}
	return tempDirs, images, errors.Join(cleanupErr, errors.Join(removalErrs...))
}
