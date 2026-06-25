package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/runid"
	"github.com/spf13/cobra"
)

type cachedGitHubClient struct {
	github.Client
	mu       sync.Mutex
	issues   map[int]*github.Issue
	comments map[int][]github.IssueComment
}

func newCachedGitHubClient(client github.Client) *cachedGitHubClient {
	return &cachedGitHubClient{
		Client:   client,
		issues:   make(map[int]*github.Issue),
		comments: make(map[int][]github.IssueComment),
	}
}

func (c *cachedGitHubClient) FetchIssue(number int) (*github.Issue, error) {
	return getOrFill(&c.mu, c.issues, number, func() (*github.Issue, error) {
		return c.Client.FetchIssue(number)
	})
}

func (c *cachedGitHubClient) ListIssueComments(number int) ([]github.IssueComment, error) {
	return getOrFill(&c.mu, c.comments, number, func() ([]github.IssueComment, error) {
		comments, err := c.Client.ListIssueComments(number)
		if err != nil {
			return nil, err
		}
		if comments == nil {
			comments = []github.IssueComment{}
		}
		return comments, nil
	})
}

func getOrFill[K comparable, V any](mu *sync.Mutex, cache map[K]V, key K, fill func() (V, error)) (V, error) {
	mu.Lock()
	if v, ok := cache[key]; ok {
		mu.Unlock()
		return v, nil
	}
	mu.Unlock()

	v, err := fill()
	if err != nil {
		var zero V
		return zero, err
	}
	mu.Lock()
	cache[key] = v
	mu.Unlock()
	return v, nil
}

// NewRunCmd creates the run command.
func NewRunCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run [issue...]",
		Short: "Run an AFK agent for specific issues",
		Long:  "Run an AFK agent for selected issues and leave worktrees on disk. Prompt or template overrides that omit {{ISSUE_NUMBER}} run without issue lookup. Use --continue to replay the latest AgentRun for each selected issue with its prior handoff and stored settings. Use --base-branch to fetch a different origin branch before each run starts. Use \"sandman clean\" to delete preserved worktrees.",
		Example: `  sandman run 42 43
  sandman run 42:45
  sandman run :45
  sandman run 42:45 --label bug
  sandman run 42:45 --query "label:bug is:open"
  sandman run --base-branch main 42 43
  sandman run --continue 42
  sandman run --prompt "Return only OK."
  sandman run --template ./prompt.md`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			repoRoot := deps.RepoRoot
			if repoRoot == "" {
				var err error
				repoRoot, err = resolveRepoRoot()
				if err != nil {
					return fmt.Errorf("resolve repo root: %w", err)
				}
			}
			sandmanDir := filepath.Join(repoRoot, ".sandman")

			overrideFlag, _ := cmd.Flags().GetBool("override")
			continueFlag, _ := cmd.Flags().GetBool("continue")
			if overrideFlag && continueFlag {
				return MarkUsage(fmt.Errorf("--override cannot be combined with --continue"))
			}
			if err := requireReviewDaemon(cfg.EffectiveReviewCommand(), sandmanDir); err != nil {
				return err
			}
			githubClient := newCachedGitHubClient(deps.GitHubClient)

			promptFlag, _ := cmd.Flags().GetString("prompt")
			templateFlag, _ := cmd.Flags().GetString("template")
			modelFlag, _ := cmd.Flags().GetString("model")
			agentFlag, _ := cmd.Flags().GetString("agent")
			promptArgsRaw, _ := cmd.Flags().GetStringArray("prompt-arg")
			promptArgs := make(map[string]string)
			for _, arg := range promptArgsRaw {
				parts := strings.SplitN(arg, "=", 2)
				if len(parts) != 2 {
					return MarkUsage(fmt.Errorf("invalid --prompt-arg format %q: expected KEY=VALUE", arg))
				}
				promptArgs[parts[0]] = parts[1]
			}

			reviewCommand := cfg.EffectiveReviewCommand()

			runIDFlag := cmd.Flags().Lookup("run-id")
			runID, _ := cmd.Flags().GetString("run-id")
			if runIDFlag.Changed {
				if err := runid.IsValidUserRunID(runID); err != nil {
					return MarkUsage(fmt.Errorf("--run-id %v", err))
				}
			}

			selectedPrompt := ""
			overridePrompt := false
			switch {
			case strings.TrimSpace(promptFlag) != "":
				selectedPrompt = promptFlag
				overridePrompt = true
			case strings.TrimSpace(templateFlag) != "":
				content, err := os.ReadFile(templateFlag)
				if err != nil {
					return fmt.Errorf("read template file: %w", err)
				}
				selectedPrompt = string(content)
				overridePrompt = true
			}
			promptNeedsIssueSelection := overridePrompt && promptRequiresIssueSelection(prompt.ApplySubstitutions(selectedPrompt, prompt.RenderConfig{ReviewCommand: reviewCommand, PromptArgs: promptArgs}))

			label, _ := cmd.Flags().GetString("label")
			query, _ := cmd.Flags().GetString("query")

			includeDependencies, _ := cmd.Flags().GetBool("include-dependencies")
			autoFlag := cmd.Flags().Lookup("auto")
			autoProvided := autoFlag != nil && autoFlag.Changed
			countFlag := cmd.Flags().Lookup("count")
			countProvided := countFlag != nil && countFlag.Changed
			autoCount, _ := cmd.Flags().GetInt("count")
			issueSelectionProvided := len(args) > 0 || autoProvided || label != "" || query != ""

			if runID != "" && issueSelectionProvided {
				return MarkUsage(fmt.Errorf("--run-id cannot be combined with issue selection, --auto, --label, or --query"))
			}

			if countProvided && autoCount < 0 {
				return MarkUsage(fmt.Errorf("--count must be 0 or greater"))
			}

			if autoProvided {
				includeDependencies = true
			}

			agentName := strings.TrimSpace(agentFlag)
			if agentName == "" {
				agentName = strings.TrimSpace(cfg.DefaultAgent)
			}
			if agentName == "" {
				agentName = strings.TrimSpace(cfg.Agent)
			}
			agentCfg, err := cfg.ResolveAgentProvider(agentName)
			if err != nil {
				return err
			}

			var issues []int
			var autoIssueRunID string
			if overridePrompt && !issueSelectionProvided {
				if promptNeedsIssueSelection {
					return MarkUsage(fmt.Errorf("prompt requires issue selection but no issue selection was provided"))
				}
			} else {
				if autoProvided {
					if label != "" && query != "" {
						return MarkUsage(fmt.Errorf("cannot combine --label with --query"))
					}
					effectiveCount := effectiveAutoCount(autoCount, countProvided, cfg.AutoMaxCount)
					candidates, err := resolveAutoCandidates(cmd.Context(), githubClient, args, label, query, cmd.ErrOrStderr())
					if err != nil {
						return err
					}
					candidates, err = expandPRDs(cmd.Context(), githubClient, candidates, cmd.ErrOrStderr())
					if err != nil {
						return err
					}
					resolvedQuery := resolveAutoQuery(label, query)
					autoTS, autoShortID := "", ""
					issues, autoTS, autoShortID, err = resolveAutoIssues(cmd.Context(), githubClient, effectiveCount, candidates, sandmanDir, agentName, modelFlag, cfg, resolvedQuery, deps.EventLog)
					if err != nil {
						return err
					}
					if autoTS != "" && autoShortID != "" {
						firstSubject := ""
						if len(issues) > 0 {
							firstSubject = fmt.Sprintf("%d", issues[0])
						}
						batchID := runid.NewBatchID(runid.KindIssue, len(issues), firstSubject, autoTS, autoShortID)
						issueRunDir := daemon.RunDir(sandmanDir, batchID)
						if err := os.MkdirAll(issueRunDir, 0o700); err != nil {
							return fmt.Errorf("create issue batch dir: %w", err)
						}
						autoIssueRunID = runid.NewRunID(runid.KindIssue, fmt.Sprintf("%d", issues[0]), autoTS, autoShortID)
						if deps.EventLog != nil {
							if err := deps.EventLog.Log(events.Event{
								Type:      "run.started",
								Timestamp: time.Now(),
								RunID:     autoIssueRunID,
								Issue:     issues[0],
								Payload: map[string]any{
									"run_kind": "issue",
									"issues":   append([]int(nil), issues...),
								},
							}); err != nil {
								return fmt.Errorf("log issue run.started: %w", err)
							}
						}
						runID = batchID
					}
				} else if len(args) > 0 {
					selection, orderedIssues, _, hasUnboundedEnd, err := parseIssueSelection(args)
					if err != nil {
						return MarkUsage(err)
					}

					if label == "" && query == "" {
						numbersForFilter := orderedIssues
						if hasUnboundedEnd {
							numbersForFilter = explicitIssueNumbers(selection)
						}
						issues, err = filterClosedIssues(numbersForFilter, githubClient.SearchIssues, githubClient.FetchIssue, cmd.ErrOrStderr())
						if err != nil {
							if hasUnboundedEnd && errors.Is(err, errAllExplicitClosed) {
								issues = nil
							} else {
								return err
							}
						}
						if hasUnboundedEnd {
							seen := make(map[int]struct{}, len(issues))
							for _, number := range issues {
								seen[number] = struct{}{}
							}
							searchResults, err := searchIssues(cmd.Context(), githubClient, "is:open")
							if err != nil {
								return err
							}
							if len(searchResults) >= 1000 {
								return MarkUsage(fmt.Errorf("issue selection exceeds search result limit"))
							}
							for _, issue := range searchResults {
								if !selection.matches(issue.Number) {
									continue
								}
								if _, ok := seen[issue.Number]; ok {
									continue
								}
								seen[issue.Number] = struct{}{}
								issues = append(issues, issue.Number)
							}
						}
					} else if querySupportsLocalFiltering(query) {
						resolved, err := resolveIssuesLocally(githubClient, orderedIssues, label, query)
						if err != nil {
							return err
						}
						issues = append(issues, resolved...)
						if hasUnboundedEnd {
							searchResults, err := searchIssues(cmd.Context(), githubClient, buildIssueQuery(label, query))
							if err != nil {
								return err
							}
							if len(searchResults) >= 1000 {
								return MarkUsage(fmt.Errorf("issue selection exceeds search result limit"))
							}
							for _, issue := range searchResults {
								if !selection.matches(issue.Number) || !issueMatchesFilters(&issue, label, query) {
									continue
								}
								if !containsIssue(issues, issue.Number) {
									issues = append(issues, issue.Number)
								}
							}
						}
					} else {
						searchQuery := buildIssueQuery(label, query)
						if label == "" && query == "" {
							searchQuery = "is:open"
						}
						searchResults, err := searchIssues(cmd.Context(), githubClient, searchQuery)
						if err != nil {
							return err
						}
						if len(searchResults) >= 1000 {
							return MarkUsage(fmt.Errorf("issue selection exceeds search result limit"))
						}
						issues = filterIssuesBySelection(searchResults, selection, orderedIssues, hasUnboundedEnd)
					}
				} else if label != "" || query != "" {
					searchQuery := buildIssueQuery(label, query)
					searchResults, err := searchIssues(cmd.Context(), githubClient, searchQuery)
					if err != nil {
						return err
					}
					issues = extractIssueNumbers(searchResults)
				} else {
					if !(runID != "" && continueFlag) {
						if runID != "" {
							return MarkUsage(fmt.Errorf("--run-id requires --prompt or --template for prompt-only mode"))
						}
						if deps.IsTTY != nil && deps.IsTTY() {
							issues, err = pickIssues(cmd.Context(), githubClient, deps.IssuePicker)
							if err != nil {
								return err
							}
						} else {
							return MarkUsage(fmt.Errorf("no issues provided"))
						}
					}
				}
			}

			if len(issues) == 0 && (!overridePrompt || promptNeedsIssueSelection) && !(continueFlag && runID != "") {
				return MarkUsage(fmt.Errorf("no issues selected"))
			}

			if len(issues) > 0 {
				issues, err = expandPRDs(cmd.Context(), githubClient, issues, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
			}

			baseBranchFlag, _ := cmd.Flags().GetString("base-branch")
			baseBranch := strings.TrimSpace(baseBranchFlag)
			if baseBranch == "" {
				baseBranch = strings.TrimSpace(cfg.Git.BaseBranch)
			}
			if baseBranch == "" {
				baseBranch = "main"
			}

			resolvedBatch, err := batch.NewDependencyResolver(githubClient).Resolve(cmd.Context(), issues, includeDependencies)
			if err != nil {
				return fmt.Errorf("resolve dependencies: %w", err)
			}

			parallelFlag := cmd.Flags().Lookup("parallel")
			parallelSet := parallelFlag != nil && parallelFlag.Changed
			parallel, _ := cmd.Flags().GetInt("parallel")
			if !parallelSet && cfg != nil {
				parallel = cfg.DefaultParallel
			}
			if parallelSet && parallel < 0 {
				return MarkUsage(fmt.Errorf("parallel must be 0 or greater"))
			}

			startDelayFlag := cmd.Flags().Lookup("start-delay")
			startDelaySet := startDelayFlag != nil && startDelayFlag.Changed
			startDelay, _ := cmd.Flags().GetInt("start-delay")
			if startDelaySet && startDelay < 0 {
				return MarkUsage(fmt.Errorf("start_delay must be 0 or greater"))
			}

			runIdleTimeoutFlag := cmd.Flags().Lookup("run-idle-timeout")
			runIdleTimeoutSet := runIdleTimeoutFlag != nil && runIdleTimeoutFlag.Changed
			runIdleTimeout, _ := cmd.Flags().GetInt("run-idle-timeout")
			if runIdleTimeoutSet && runIdleTimeout < 0 {
				return MarkUsage(fmt.Errorf("run_idle_timeout must be 0 or greater"))
			}

			sandboxMode, _ := cmd.Flags().GetString("sandbox")
			containerCapacityFlag := cmd.Flags().Lookup("container-capacity")
			containerCapacitySet := containerCapacityFlag != nil && containerCapacityFlag.Changed
			containerCapacity, _ := cmd.Flags().GetInt("container-capacity")
			maxContainersFlag := cmd.Flags().Lookup("max-containers")
			maxContainersSet := maxContainersFlag != nil && maxContainersFlag.Changed
			maxContainers, _ := cmd.Flags().GetInt("max-containers")
			if containerCapacitySet && containerCapacity < 0 {
				return MarkUsage(fmt.Errorf("container_capacity must be 0 or greater"))
			}
			if maxContainersSet && maxContainers < 0 {
				return MarkUsage(fmt.Errorf("max_containers must be 0 or greater"))
			}

			retriesFlag := cmd.Flags().Lookup("retries")
			retriesSet := retriesFlag != nil && retriesFlag.Changed
			retries, _ := cmd.Flags().GetInt("retries")
			if retriesSet && retries < 0 {
				return MarkUsage(fmt.Errorf("retries must be 0 or greater"))
			}
			if !retriesSet {
				retries = -1
			}

			if autoProvided {
				if !parallelSet {
					parallel = 1
				}
				if !containerCapacitySet {
					containerCapacity = 1
					containerCapacitySet = true
				}
				if !maxContainersSet {
					maxContainers = 1
					maxContainersSet = true
				}
				if !retriesSet {
					retries = 3
				}
			}

			dangerouslySkipPermFlag := cmd.Flags().Lookup("dangerously-skip-permissions")
			dangerouslySkipPermSet := dangerouslySkipPermFlag != nil && dangerouslySkipPermFlag.Changed
			var dangerouslySkipPerm *bool
			if dangerouslySkipPermSet {
				val, _ := cmd.Flags().GetBool("dangerously-skip-permissions")
				dangerouslySkipPerm = &val
			}

			reconcileStrandedFlag := cmd.Flags().Lookup("reconcile-stranded")
			noReconcileStrandedFlag := cmd.Flags().Lookup("no-reconcile-stranded")
			reconcileStrandedSet := reconcileStrandedFlag != nil && reconcileStrandedFlag.Changed
			noReconcileStrandedSet := noReconcileStrandedFlag != nil && noReconcileStrandedFlag.Changed
			var reconcileStranded *bool
			if reconcileStrandedSet {
				val, _ := cmd.Flags().GetBool("reconcile-stranded")
				reconcileStranded = &val
			} else if noReconcileStrandedSet {
				val, _ := cmd.Flags().GetBool("no-reconcile-stranded")
				val = !val
				reconcileStranded = &val
			}
			modes := make(map[int]batch.IssueMode)
			previousRunIDs := make(map[int]string)
			branches := make(map[int]string)
			baseBranches := make(map[int]string)
			taskPrompts := make(map[int]string)
			continueIssues := make([]int, 0, len(resolvedBatch.Issues))
			if continueFlag {
				modeEvents := []events.Event{}
				if deps.EventLog != nil {
					modeEvents, err = deps.EventLog.Read()
					if err != nil {
						return fmt.Errorf("read event log: %w", err)
					}
				}
				lastRuns := lastRunPerIssue(modeEvents, resolvedBatch.Issues)
				worktreeBase := cfg.WorktreeDir
				if strings.TrimSpace(worktreeBase) == "" {
					worktreeBase = ".sandman/worktrees"
				}
				for _, num := range resolvedBatch.Issues {
					lastRun := lastRuns[num]
					if lastRun.RunID == "" {
						modes[num] = batch.ModeOverride
						fmt.Fprintf(cmd.ErrOrStderr(), "[--continue] promoting #%d to override (no prior started/continued run)\n", num)
						continue
					}
					continueIssues = append(continueIssues, num)
					branch, ok := payloadString(lastRun.Payload, "branch")
					if !ok || strings.TrimSpace(branch) == "" {
						return fmt.Errorf("missing branch in previous run for issue #%d", num)
					}
					baseBranchValue, ok := payloadString(lastRun.Payload, "base_branch")
					if !ok || strings.TrimSpace(baseBranchValue) == "" {
						return fmt.Errorf("missing base branch in previous run for issue #%d", num)
					}
					worktreePath := filepath.Join(worktreeBase, branch)
					if info, err := os.Stat(worktreePath); err != nil {
						if os.IsNotExist(err) {
							return fmt.Errorf("worktree %q is missing; use \"sandman run\" instead", worktreePath)
						}
						return fmt.Errorf("check worktree %q: %w", worktreePath, err)
					} else if !info.IsDir() {
						return fmt.Errorf("worktree %q is missing; use \"sandman run\" instead", worktreePath)
					}
					taskPath := filepath.Join(worktreePath, ".sandman", "task.md")
					content, err := readTaskPrompt(cmd, taskPath)
					if err != nil {
						return fmt.Errorf("read task %q for issue #%d: %w", taskPath, num, err)
					}
					modes[num] = batch.ModeContinue
					previousRunIDs[num] = lastRun.RunID
					branches[num] = strings.TrimSpace(branch)
					baseBranches[num] = strings.TrimSpace(baseBranchValue)
					taskPrompts[num] = prompt.ContinuationTaskPrompt(content)
				}
			}
			var continuationReq batch.Request
			var hasContinuationReq bool
			if continueFlag && (len(continueIssues) > 0 || runID != "") {
				continuationReq, err = buildContinuationRequest(cmd, deps, cfg, continueIssues, runID)
				if err != nil {
					return err
				}
				hasContinuationReq = true
			}
			if overrideFlag {
				modes = make(map[int]batch.IssueMode, len(resolvedBatch.Issues))
				for _, num := range resolvedBatch.Issues {
					modes[num] = batch.ModeOverride
				}
			}

			req := batch.Request{
				Issues:                     resolvedBatch.Issues,
				Dependencies:               resolvedBatch.Deps,
				Blocked:                    resolvedBatch.Blocked,
				Agent:                      agentName,
				Model:                      resolveModel(modelFlag, cfg.DefaultModel, agentCfg.Preset),
				BaseBranch:                 baseBranch,
				Mode:                       modes,
				PreviousRunIDs:             previousRunIDs,
				Branches:                   branches,
				BaseBranches:               baseBranches,
				TaskPrompts:                taskPrompts,
				Retries:                    retries,
				Parallel:                   parallel,
				StartDelay:                 time.Duration(startDelay) * time.Second,
				StartDelaySet:              startDelaySet,
				RunIdleTimeout:             runIdleTimeout,
				RunIdleTimeoutSet:          runIdleTimeoutSet,
				Sandbox:                    sandboxMode,
				RequireDockerfile:          true,
				ContainerCapacity:          containerCapacity,
				ContainerCapacitySet:       containerCapacitySet,
				MaxContainers:              maxContainers,
				MaxContainersSet:           maxContainersSet,
				DangerouslySkipPermissions: dangerouslySkipPerm,
				StrandedReconcile:          reconcileStranded,
				PromptConfig: prompt.RenderConfig{
					PromptFlag:       promptFlag,
					TemplateFlag:     templateFlag,
					ReviewCommand:    reviewCommand,
					ReviewCommandSet: true,
					PromptArgs:       promptArgs,
				},
			}
			if hasContinuationReq {
				if runID != "" && len(resolvedBatch.Issues) == 0 {
					req = continuationReq
				} else if len(continueIssues) == len(resolvedBatch.Issues) {
					req.Agent = continuationReq.Agent
					req.Model = continuationReq.Model
					req.BaseBranch = continuationReq.BaseBranch
					req.Retries = continuationReq.Retries
					req.Parallel = continuationReq.Parallel
					req.StartDelay = continuationReq.StartDelay
					req.StartDelaySet = continuationReq.StartDelaySet
					req.RunIdleTimeout = continuationReq.RunIdleTimeout
					req.RunIdleTimeoutSet = continuationReq.RunIdleTimeoutSet
					req.Sandbox = continuationReq.Sandbox
					req.ContainerCapacity = continuationReq.ContainerCapacity
					req.ContainerCapacitySet = continuationReq.ContainerCapacitySet
					req.MaxContainers = continuationReq.MaxContainers
					req.MaxContainersSet = continuationReq.MaxContainersSet
					req.DangerouslySkipPermissions = continuationReq.DangerouslySkipPermissions
					req.StrandedReconcile = continuationReq.StrandedReconcile
					req.PromptConfig.ReviewCommand = continuationReq.PromptConfig.ReviewCommand
					req.PromptConfig.ReviewCommandSet = continuationReq.PromptConfig.ReviewCommandSet
				}
				for k, v := range continuationReq.PreviousRunIDs {
					req.PreviousRunIDs[k] = v
				}
				for k, v := range continuationReq.Branches {
					req.Branches[k] = v
				}
				for k, v := range continuationReq.BaseBranches {
					req.BaseBranches[k] = v
				}
				for k, v := range continuationReq.TaskPrompts {
					req.TaskPrompts[k] = v
				}
				if continuationReq.PromptConfig.TaskPrompt != "" {
					req.PromptConfig.TaskPrompt = continuationReq.PromptConfig.TaskPrompt
				}
				if continuationReq.PromptConfig.PromptFlag != "" {
					req.PromptConfig.PromptFlag = continuationReq.PromptConfig.PromptFlag
				}
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				select {
				case <-sigCh:
					cancel()
				case <-ctx.Done():
				}
			}()

			if staleRemoved, err := daemon.CleanupStaleRunSnapshots(sandmanDir); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: cleanup stale run snapshots: %v\n", err)
			} else if staleRemoved > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Cleaned %d stale run-owned config snapshots from previous runs\n", staleRemoved)
			}

			sessionRunID := runID
			if len(req.Issues) > 0 {
				ts, shortid, err := runid.NewBatch()
				if err != nil {
					return fmt.Errorf("generate batch id: %w", err)
				}
				req.RunTS = ts
				req.RunShortID = shortid
				sessionRunID = runid.NewBatchID(runid.KindIssue, len(req.Issues), fmt.Sprintf("%d", req.Issues[0]), ts, shortid)
			} else if overridePrompt {
				ts, shortid, err := runid.NewBatch()
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to generate unique batch ID: %v; falling back to timestamp-based name\n", err)
				} else {
					req.RunID = runID
					req.BatchTS = ts
					req.BatchShortID = shortid
					sessionRunID = runid.NewBatchID(runid.KindPromptOnly, 1, runID, ts, shortid)
				}
			}

			// Boot the run session: MkdirAll → WriteManifest →
			// ControlSocket.Start → CommandServer.Start, in that fixed
			// order. The daemon.RunSession helper collapses the four
			// steps into a single call so the ordering is structural
			// (issue #1024). A run.started event is only emitted after
			// Prepare returns nil, so the orchestrator can never see a
			// half-bootstrapped run. Close is deferred first so it runs
			// last: the listener stops before the run dir is removed.
			rs := daemon.NewRunSession(sandmanDir, sessionRunID)
			// The comma-ok form matters: a typed-nil interface
			// (`var c IssueCommander = (*concrete)(nil)`) is
			// non-nil but unusable, and `Prepare` would then
			// call `cmdServer.Start` with a nil commander. By
			// keeping the `ok` signal we can pass a real `nil`
			// when the BatchRunner does not satisfy
			// IssueCommander, and `--continue` already
			// suppresses the cmd.sock server.
			var commander daemon.IssueCommander
			if !continueFlag {
				if c, ok := deps.BatchRunner.(daemon.IssueCommander); ok {
					commander = c
				}
			}
			manifest := daemon.BatchManifest{Issues: append([]int(nil), req.Issues...), CreatedAt: time.Now(), BatchId: autoIssueRunID, RunKind: "issue"}
			if err := rs.Prepare(manifest, commander); err != nil {
				_ = rs.Close()
				// A daemon without a control socket is invisible
				// to the portal and cannot be aborted by the user,
				// so it must not run (issue #1024 acceptance
				// criterion: "logs a fatal error and aborts before
				// emitting any event"). Surface the failure
				// loudly on stderr so operators see it in their
				// CI logs and shell history.
				fmt.Fprintf(cmd.ErrOrStderr(), "fatal: cannot bootstrap run session: %v\n", err)
				return err
			}
			defer rs.Close()

			relRunDir, err := filepath.Rel(repoRoot, rs.RunDir())
			if err != nil {
				return fmt.Errorf("rel run dir: %w", err)
			}
			req.OutputWriter = rs.Broadcaster()
			req.RunDir = relRunDir

			result, err := deps.BatchRunner.RunBatch(ctx, req)
			if result != nil {
				printSummary(cmd, result)
				for _, run := range result.Runs {
					if strings.TrimSpace(run.WorktreePath) != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "worktree: %s\n", run.WorktreePath)
					}
				}
			}
			if err != nil {
				if errors.Is(err, batch.ErrAborted) {
					return &ExitCodedError{Code: 130, Msg: "batch aborted by operator", Err: err}
				}
				return fmt.Errorf("run batch: %w", err)
			}

			return nil
		},
	}
	cmd.Flags().Int("parallel", 0, "Limit parallel execution")
	cmd.Flags().Int("retries", 0, "Retry failed AgentRuns up to N times")
	cmd.Flags().Int("start-delay", 0, "Wait N seconds after any AgentRun finishes before starting the next one; 0 disables the delay")
	cmd.Flags().Int("run-idle-timeout", 0, "Treat an AgentRun as stuck if it produces no output for N seconds; 0 disables the timeout")
	cmd.Flags().String("sandbox", "", "Sandbox mode: podman (default), docker, or worktree")
	cmd.Flags().Int("container-capacity", 0, "Maximum concurrent agent runs per container; 0 means unlimited")
	cmd.Flags().Int("max-containers", 0, "Maximum number of containers to run at once; 0 means auto mode")
	cmd.Flags().Bool("include-dependencies", false, "Expand the batch to include transitive blockers")
	cmd.Flags().String("label", "", "Select issues by label")
	cmd.Flags().String("query", "", "Select issues by GitHub search query")
	cmd.Flags().String("prompt", "", "Inline prompt template (overrides --template and .sandman/prompt.md). Omit {{ISSUE_NUMBER}} for prompt-only mode.")
	cmd.Flags().String("template", "", "Path to prompt template file (overrides .sandman/prompt.md). Omit {{ISSUE_NUMBER}} for prompt-only mode.")
	cmd.Flags().String("model", "", "Override agent model for built-in presets")
	cmd.Flags().String("run-id", "", "Batch-level identifier for prompt-only runs; may contain alphanumeric characters, hyphens, and underscores (max 64 chars); cannot be combined with issue selection")
	cmd.Flags().String("agent", "", "Built-in agent preset (opencode)")
	cmd.Flags().String("base-branch", "", "Base branch to fetch from origin before each AgentRun starts")
	cmd.Flags().StringArray("prompt-arg", nil, "Custom template substitution KEY=VALUE (repeatable)")

	cmd.Flags().Bool("auto", false, "Auto Mode — let Sandman choose which issues to run, capped to --count or auto_max_count from config")
	cmd.Flags().Int("count", 0, "Candidate cap for Auto Mode; 0 means unlimited (use auto_max_count from config when not set on the CLI)")

	cmd.Flags().Bool("dangerously-skip-permissions", false, "Skip opencode permission prompts (auto-approves non-denied actions); default is true for container runs, false for worktree runs")

	cmd.Flags().Bool("override", false, "Clear existing artifacts (worktree, branch, logs, events) before running; force-checkout worktree to expected branch on mismatch or detached HEAD")
	cmd.Flags().Bool("reconcile-stranded", true, "Auto-recover stranded worktrees when the main repo is checked out on a sandman/N-… branch (use --no-reconcile-stranded to disable)")
	cmd.Flags().Bool("no-reconcile-stranded", false, "Opt out of stranded-worktree auto-recovery (negative form of --reconcile-stranded)")
	cmd.Flags().Bool("continue", false, "Continue the latest AgentRun for each selected issue by reusing the prior handoff and stored settings")

	return cmd
}

func buildIssueQuery(label, query string) string {
	var groups []string

	if label != "" {
		groups = append(groups, "label:"+label)
	}

	if query != "" {
		groups = append(groups, query)
	}

	if !queryHasOpenState(query) {
		groups = append(groups, "is:open")
	}

	return strings.Join(groups, " ")
}

func queryHasOpenState(query string) bool {
	for _, token := range strings.Fields(strings.TrimSpace(query)) {
		if token == "is:open" || token == "state:open" {
			return true
		}
	}
	return false
}

func queryHasExplicitState(query string) bool {
	for _, token := range strings.Fields(strings.TrimSpace(query)) {
		switch token {
		case "is:open", "state:open", "is:closed", "state:closed":
			return true
		}
	}
	return false
}

func requiresOpenDefault(label, query string) bool {
	return (label != "" || strings.TrimSpace(query) != "") && !queryHasExplicitState(query)
}

type issueSelection struct {
	exact  map[int]struct{}
	ranges []issueRangeSelection
}

type issueRangeSelection struct {
	start int
	end   int
}

func parseIssueSelection(args []string) (issueSelection, []int, bool, bool, error) {
	selection := issueSelection{exact: make(map[int]struct{}, len(args))}
	orderedIssues := make([]int, 0, len(args))
	hasRanges := false
	hasUnboundedEnd := false

	for _, arg := range args {
		start, end, isRange, err := parseIssueRange(arg)
		if err != nil {
			return issueSelection{}, nil, false, false, fmt.Errorf("invalid issue number %q: %w", arg, err)
		}
		if isRange {
			hasRanges = true
			selection.ranges = append(selection.ranges, issueRangeSelection{start: start, end: end})
			if end == 0 {
				hasUnboundedEnd = true
				continue
			}
			if end-start >= 1000 {
				return issueSelection{}, nil, false, false, fmt.Errorf("range %q expands to more than 1000 issues", arg)
			}
			for n := start; ; n++ {
				orderedIssues = append(orderedIssues, n)
				if n >= end {
					break
				}
			}
			continue
		}

		selection.exact[start] = struct{}{}
		orderedIssues = append(orderedIssues, start)
	}

	return selection, orderedIssues, hasRanges, hasUnboundedEnd, nil
}

func (s issueSelection) matches(number int) bool {
	if _, ok := s.exact[number]; ok {
		return true
	}
	for _, r := range s.ranges {
		if number < r.start {
			continue
		}
		if r.end == 0 || number <= r.end {
			return true
		}
	}
	return false
}

func filterIssuesBySelection(searchResults []github.Issue, selection issueSelection, orderedIssues []int, hasUnboundedEnd bool) []int {
	if hasUnboundedEnd {
		issues := make([]int, 0, len(searchResults))
		for _, issue := range searchResults {
			if selection.matches(issue.Number) {
				issues = append(issues, issue.Number)
			}
		}
		return issues
	}

	found := make(map[int]struct{}, len(searchResults))
	for _, issue := range searchResults {
		found[issue.Number] = struct{}{}
	}

	issues := make([]int, 0, len(orderedIssues))
	for _, number := range orderedIssues {
		if _, ok := found[number]; ok {
			issues = append(issues, number)
		}
	}
	return issues
}

func containsIssue(numbers []int, target int) bool {
	for _, number := range numbers {
		if number == target {
			return true
		}
	}
	return false
}

func querySupportsLocalFiltering(query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return true
	}
	if strings.Contains(query, ",") {
		return false
	}
	for _, token := range strings.Fields(query) {
		switch {
		case strings.HasPrefix(token, "label:"):
		case token == "is:open", token == "state:open", token == "is:closed", token == "state:closed":
		default:
			return false
		}
	}
	return true
}

func issueMatchesFilters(issue *github.Issue, label, query string) bool {
	if issue == nil {
		return false
	}
	if requiresOpenDefault(label, query) && !strings.EqualFold(strings.TrimSpace(issue.State), "open") {
		return false
	}
	if label != "" && !issueHasLabel(issue.Labels, label) {
		return false
	}

	for _, token := range strings.Fields(strings.TrimSpace(query)) {
		switch {
		case strings.HasPrefix(token, "label:"):
			if !issueHasLabel(issue.Labels, strings.Trim(strings.TrimPrefix(token, "label:"), "\"")) {
				return false
			}
		case token == "is:open" || token == "state:open":
			if strings.ToLower(strings.TrimSpace(issue.State)) != "open" {
				return false
			}
		case token == "is:closed" || token == "state:closed":
			if strings.ToLower(strings.TrimSpace(issue.State)) != "closed" {
				return false
			}
		}
	}

	return true
}

func issueHasLabel(labels []string, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	for _, label := range labels {
		if strings.ToLower(strings.TrimSpace(label)) == target {
			return true
		}
	}
	return false
}

func resolveIssuesLocally(client github.Client, numbers []int, label, query string) ([]int, error) {
	issues := make([]int, 0, len(numbers))
	seen := make(map[int]struct{}, len(numbers))
	for _, number := range numbers {
		issue, err := client.FetchIssue(number)
		if err != nil {
			return nil, fmt.Errorf("fetch issue #%d: %w", number, err)
		}
		if !issueMatchesFilters(issue, label, query) {
			continue
		}
		if _, ok := seen[number]; ok {
			continue
		}
		seen[number] = struct{}{}
		issues = append(issues, number)
	}
	return issues, nil
}

// explicitIssueNumbers returns the explicitly-listed numbers from a
// selection in their original (unordered) form. Used by unbounded-range
// callers that want the filter to only classify explicit numbers,
// leaving the range extension to the is:open search that follows.
func explicitIssueNumbers(selection issueSelection) []int {
	numbers := make([]int, 0, len(selection.exact))
	for n := range selection.exact {
		numbers = append(numbers, n)
	}
	return numbers
}

// errAllExplicitClosed is returned by filterClosedIssues when every
// input issue is closed. The name predates the range-vs-explicit
// parity fix in #923 and is preserved for callers that match on it
// via errors.Is. Callers propagate it as a plain runtime error so the
// usage banner is suppressed in executeRoot.
var errAllExplicitClosed = errors.New("all explicit issues are closed")

// filterClosedIssues returns the subset of numbers that are still open.
// Every input number is treated equivalently: closed issues produce a
// stderr warning so the operator can see why they were dropped, and a
// fully-closed input returns errAllExplicitClosed so callers can
// suppress the cobra usage banner.
//
// The implementation prefers a single repo-wide is:open search to avoid
// one gh CLI call per issue (the per-issue fetch path was a 2x cost on
// bounded ranges of N). If the search hits the GitHub result limit
// (1000) or fails, it falls back to per-issue fetch, where individual
// fetch errors are logged and skipped rather than aborting the batch.
//
// Callers extending the result with their own range source (e.g. an
// unbounded-end range) should pass only the explicit numbers via
// explicitIssueNumbers so the filter classifies them as a single
// coherent batch.
func filterClosedIssues(numbers []int, searchFn func(string) ([]github.Issue, error), fetchFn func(int) (*github.Issue, error), stderr io.Writer) ([]int, error) {
	openSet, searchHitLimit, searchErr := loadOpenIssueSet(searchFn)
	if searchErr == nil && !searchHitLimit {
		filtered, allClosed := filterClosedByOpenSet(numbers, openSet, stderr)
		if allClosed {
			return nil, errAllExplicitClosed
		}
		return filtered, nil
	}

	filtered := make([]int, 0, len(numbers))
	closedCount := 0
	for _, n := range numbers {
		issue, err := fetchFn(n)
		if err != nil {
			fmt.Fprintf(stderr, "Warning: could not fetch issue #%d: %v\n", n, err)
			continue
		}
		if github.IsIssueClosed(issue) {
			fmt.Fprintf(stderr, "Issue #%d is closed, skipping\n", n)
			closedCount++
			continue
		}
		filtered = append(filtered, n)
	}
	if len(filtered) == 0 && closedCount > 0 {
		return nil, errAllExplicitClosed
	}
	return filtered, nil
}

// loadOpenIssueSet runs the repo-wide is:open search and returns a set
// of open issue numbers. When the search hits GitHub's 1000-result
// limit, the set is unreliable (an open issue past the cutoff would
// look closed) and the caller should fall back to per-issue fetch.
func loadOpenIssueSet(searchFn func(string) ([]github.Issue, error)) (map[int]struct{}, bool, error) {
	results, err := searchFn("is:open")
	if err != nil {
		return nil, false, err
	}
	if len(results) >= 1000 {
		return nil, true, nil
	}
	openSet := make(map[int]struct{}, len(results))
	for _, issue := range results {
		openSet[issue.Number] = struct{}{}
	}
	return openSet, false, nil
}

func filterClosedByOpenSet(numbers []int, openSet map[int]struct{}, stderr io.Writer) ([]int, bool) {
	filtered := make([]int, 0, len(numbers))
	closedCount := 0
	for _, n := range numbers {
		if _, open := openSet[n]; !open {
			fmt.Fprintf(stderr, "Issue #%d is closed, skipping\n", n)
			closedCount++
			continue
		}
		filtered = append(filtered, n)
	}
	if len(filtered) == 0 && closedCount > 0 {
		return filtered, true
	}
	return filtered, false
}

func extractIssueNumbers(ghIssues []github.Issue) []int {
	numbers := make([]int, len(ghIssues))
	for i, issue := range ghIssues {
		numbers[i] = issue.Number
	}
	return numbers
}

func searchIssues(ctx context.Context, client github.Client, query string) ([]github.Issue, error) {
	ghIssues, err := client.SearchIssues(query)
	if err != nil {
		return nil, fmt.Errorf("search issues: %w", err)
	}
	return ghIssues, nil
}

func resolveIssues(ctx context.Context, client github.Client, query string) ([]int, error) {
	ghIssues, err := searchIssues(ctx, client, query)
	if err != nil {
		return nil, err
	}
	return extractIssueNumbers(ghIssues), nil
}

func pickIssues(ctx context.Context, client github.Client, picker IssuePicker) ([]int, error) {
	ghIssues, err := client.SearchIssues("is:open")
	if err != nil {
		return nil, fmt.Errorf("list open issues: %w", err)
	}
	return picker.Select(ghIssues)
}

// expandPRDs runs the PRD resolver on the input issue list and returns the
// expanded list. Empty input short-circuits to avoid wasted fetches. Any PRD
// resolution error is wrapped as a regular command error (not a usage error)
// because the input was syntactically valid.
func expandPRDs(ctx context.Context, client github.Client, issues []int, stderr io.Writer) ([]int, error) {
	if len(issues) == 0 {
		return issues, nil
	}
	prdResolver := batch.NewPRDResolver(client, stderr)
	expanded, err := prdResolver.Resolve(ctx, issues)
	if err != nil {
		return nil, fmt.Errorf("resolve PRDs: %w", err)
	}
	return expanded, nil
}

// effectiveAutoCount resolves the candidate cap for Auto Mode.
//
// Precedence: explicit --count > cfg.AutoMaxCount (0 means unlimited) > DefaultAutoMaxCount.
// A return value of 0 means "no cap". Negative values cannot reach this helper
// because --count validation and Load() reject them.
func effectiveAutoCount(cliCount int, cliCountProvided bool, cfgAutoMaxCount int) int {
	if cliCountProvided {
		return cliCount
	}
	if cfgAutoMaxCount > 0 {
		return cfgAutoMaxCount
	}
	if cfgAutoMaxCount == 0 {
		return 0
	}
	return config.DefaultAutoMaxCount
}

// resolveAutoCandidates returns the ordered candidate set for Auto Mode.
//
// Args + filter: filter the args against the label/query.
// Args alone: filter the args to drop closed issues.
// Filter alone: search GitHub and return the issue numbers.
// Neither: search GitHub for ready-for-agent (the default).
func resolveAutoCandidates(ctx context.Context, client github.Client, args []string, label, query string, stderr io.Writer) ([]int, error) {
	if len(args) > 0 {
		selection, orderedIssues, _, hasUnboundedEnd, err := parseIssueSelection(args)
		if err != nil {
			return nil, err
		}
		if label == "" && query == "" {
			numbersForFilter := orderedIssues
			if hasUnboundedEnd {
				numbersForFilter = explicitIssueNumbers(selection)
			}
			candidates, err := filterClosedIssues(numbersForFilter, client.SearchIssues, client.FetchIssue, stderr)
			if err != nil {
				if hasUnboundedEnd && errors.Is(err, errAllExplicitClosed) {
					return nil, nil
				}
				return nil, err
			}
			if hasUnboundedEnd {
				seen := make(map[int]struct{}, len(candidates))
				for _, n := range candidates {
					seen[n] = struct{}{}
				}
				searchResults, err := searchIssues(ctx, client, "is:open")
				if err != nil {
					return nil, err
				}
				if len(searchResults) >= 1000 {
					return nil, fmt.Errorf("issue selection exceeds search result limit")
				}
				for _, issue := range searchResults {
					if !selection.matches(issue.Number) {
						continue
					}
					if _, ok := seen[issue.Number]; ok {
						continue
					}
					seen[issue.Number] = struct{}{}
					candidates = append(candidates, issue.Number)
				}
			}
			return candidates, nil
		}
		if querySupportsLocalFiltering(query) {
			resolved, err := resolveIssuesLocally(client, orderedIssues, label, query)
			if err != nil {
				return nil, err
			}
			if hasUnboundedEnd {
				searchResults, err := searchIssues(ctx, client, buildIssueQuery(label, query))
				if err != nil {
					return nil, err
				}
				if len(searchResults) >= 1000 {
					return nil, fmt.Errorf("issue selection exceeds search result limit")
				}
				for _, issue := range searchResults {
					if !selection.matches(issue.Number) || !issueMatchesFilters(&issue, label, query) {
						continue
					}
					if !containsIssue(resolved, issue.Number) {
						resolved = append(resolved, issue.Number)
					}
				}
			}
			return resolved, nil
		}
		searchQuery := buildIssueQuery(label, query)
		searchResults, err := searchIssues(ctx, client, searchQuery)
		if err != nil {
			return nil, err
		}
		if len(searchResults) >= 1000 {
			return nil, fmt.Errorf("issue selection exceeds search result limit")
		}
		return filterIssuesBySelection(searchResults, selection, orderedIssues, hasUnboundedEnd), nil
	}
	searchQuery := resolveAutoQuery(label, query)
	searchResults, err := searchIssues(ctx, client, searchQuery)
	if err != nil {
		return nil, fmt.Errorf("search issues: %w", err)
	}
	return extractIssueNumbers(searchResults), nil
}

func printSummary(cmd *cobra.Command, result *batch.Result) {
	var successCount, failureCount, abortedCount, blockedCount int
	for _, run := range result.Runs {
		switch run.Status {
		case "success":
			successCount++
		case "blocked":
			blockedCount++
		case "aborted":
			abortedCount++
		default:
			failureCount++
		}
	}

	parts := []string{}
	if successCount > 0 {
		parts = append(parts, fmt.Sprintf("%d succeeded", successCount))
	}
	if failureCount > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failureCount))
	}
	if abortedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d aborted", abortedCount))
	}
	if blockedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", blockedCount))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Summary: %s\n", strings.Join(parts, ", "))
	for _, run := range result.Runs {
		status := run.Status
		if run.RetriesTotal > 1 {
			status = fmt.Sprintf("%s (%d retries)", status, run.RetriesTotal-1)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  %s\n", formatIssueLabel(run.IssueNumber, run.Issue, run.Review, run.RunID), status, run.Branch)
	}
}

func formatIssueLabel(issueNumber int, issue *int, review bool, runID string) string {
	if review && runID != "" {
		return runID
	}
	if issue == nil && issueNumber == 0 {
		return "prompt-only"
	}
	return fmt.Sprintf("#%d", issueNumber)
}

func promptRequiresIssueSelection(promptText string) bool {
	return strings.Contains(promptText, "{{ISSUE_NUMBER}}") || strings.Contains(promptText, "{{ISSUE_TITLE}}") || strings.Contains(promptText, "{{ISSUE_BODY}}")
}
