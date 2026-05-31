package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/spf13/cobra"
)

// NewRunCmd creates the run command.
func NewRunCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run [issue...]",
		Short: "Run an AFK agent for specific issues",
		Long:  "Run an AFK agent for selected issues and leave worktrees on disk. Prompt or template overrides that omit {{ISSUE_NUMBER}} run without issue lookup. Use --base-branch to fetch a different origin branch before each run starts. Use \"sandman clean\" to delete preserved worktrees.",
		Example: `  sandman run 42 43
  sandman run 42:45
  sandman run :45
  sandman run 42:45 --label bug
  sandman run 42:45 --query "label:bug is:open"
  sandman run --base-branch main 42 43
  sandman run --prompt "Return only OK."
  sandman run --template ./prompt.md`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			promptFlag, _ := cmd.Flags().GetString("prompt")
			templateFlag, _ := cmd.Flags().GetString("template")
			modelFlag, _ := cmd.Flags().GetString("model")
			agentFlag, _ := cmd.Flags().GetString("agent")
			promptArgsRaw, _ := cmd.Flags().GetStringArray("prompt-arg")
			promptArgs := make(map[string]string)
			for _, arg := range promptArgsRaw {
				parts := strings.SplitN(arg, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid --prompt-arg format %q: expected KEY=VALUE", arg)
				}
				promptArgs[parts[0]] = parts[1]
			}

			reviewCommand := cfg.EffectiveReviewCommand()

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
			ralphFlag := cmd.Flags().Lookup("ralph")
			ralphProvided := ralphFlag != nil && ralphFlag.Changed
			ralphCount, _ := cmd.Flags().GetInt("ralph")
			issueSelectionProvided := len(args) > 0 || ralphProvided || label != "" || query != ""

			agentName := strings.TrimSpace(agentFlag)
			if agentName == "" {
				agentName = strings.TrimSpace(cfg.DefaultAgent)
			}
			if agentName == "" {
				agentName = strings.TrimSpace(cfg.Agent)
			}
			if _, err := cfg.ResolveAgentProvider(agentName); err != nil {
				return err
			}

			var issues []int
			if overridePrompt && !issueSelectionProvided {
				if promptNeedsIssueSelection {
					return fmt.Errorf("prompt requires issue selection but no issue selection was provided")
				}
			} else {
				if ralphProvided {
					if len(args) > 0 {
						return fmt.Errorf("cannot combine --ralph with issue arguments")
					}
					if label != "" && query != "" {
						return fmt.Errorf("cannot combine --label with --query")
					}
					if ralphCount <= 0 {
						return fmt.Errorf("--ralph count must be at least 1")
					}
					issues, err = resolveRalphIssues(cmd.Context(), deps.GitHubClient, ralphCount, label, query, ".sandman", agentName, modelFlag, cfg)
					if err != nil {
						return err
					}
				} else if len(args) > 0 {
					selection, orderedIssues, _, hasUnboundedEnd, err := parseIssueSelection(args)
					if err != nil {
						return err
					}

					if label == "" && query == "" && !hasUnboundedEnd {
						issues = append(issues, orderedIssues...)
					} else if label == "" && query == "" {
						issues = append(issues, orderedIssues...)
						seen := make(map[int]struct{}, len(issues))
						for _, number := range issues {
							seen[number] = struct{}{}
						}
						searchResults, err := searchIssues(cmd.Context(), deps.GitHubClient, "is:open")
						if err != nil {
							return err
						}
						if len(searchResults) >= 1000 {
							return fmt.Errorf("issue selection exceeds search result limit")
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
					} else {
						searchQuery := buildIssueQuery(label, query)
						if label == "" && query == "" {
							searchQuery = "is:open"
						}
						searchResults, err := searchIssues(cmd.Context(), deps.GitHubClient, searchQuery)
						if err != nil {
							return err
						}
						if len(searchResults) >= 1000 {
							return fmt.Errorf("issue selection exceeds search result limit")
						}
						issues = filterIssuesBySelection(searchResults, selection, orderedIssues, hasUnboundedEnd)
					}
				} else if label != "" || query != "" {
					searchQuery := buildIssueQuery(label, query)
					searchResults, err := searchIssues(cmd.Context(), deps.GitHubClient, searchQuery)
					if err != nil {
						return err
					}
					issues = extractIssueNumbers(searchResults)
				} else {
					if deps.IsTTY != nil && deps.IsTTY() {
						issues, err = pickIssues(cmd.Context(), deps.GitHubClient, deps.IssuePicker)
						if err != nil {
							return err
						}
					} else {
						return fmt.Errorf("no issues provided")
					}
				}
			}

			if len(issues) == 0 && (!overridePrompt || promptNeedsIssueSelection) {
				return fmt.Errorf("no issues selected")
			}

			baseBranchFlag, _ := cmd.Flags().GetString("base-branch")
			baseBranch := strings.TrimSpace(baseBranchFlag)
			if baseBranch == "" {
				baseBranch = strings.TrimSpace(cfg.Git.BaseBranch)
			}
			if baseBranch == "" {
				baseBranch = "main"
			}

			resolvedBatch, err := batch.NewDependencyResolver(deps.GitHubClient).Resolve(cmd.Context(), issues, includeDependencies)
			if err != nil {
				return fmt.Errorf("resolve dependencies: %w", err)
			}

			parallel, _ := cmd.Flags().GetInt("parallel")
			if parallel == 0 && cfg != nil {
				parallel = cfg.DefaultParallel
			}
			// Let 0 pass through — Orchestrator defaults to 4

			startDelayFlag := cmd.Flags().Lookup("start-delay")
			startDelaySet := startDelayFlag != nil && startDelayFlag.Changed
			startDelay, _ := cmd.Flags().GetInt("start-delay")
			if startDelaySet && startDelay < 0 {
				return fmt.Errorf("start_delay must be 0 or greater")
			}

			sandboxMode, _ := cmd.Flags().GetString("sandbox")
			containerCapacityFlag := cmd.Flags().Lookup("container-capacity")
			containerCapacitySet := containerCapacityFlag != nil && containerCapacityFlag.Changed
			containerCapacity, _ := cmd.Flags().GetInt("container-capacity")
			maxContainersFlag := cmd.Flags().Lookup("max-containers")
			maxContainersSet := maxContainersFlag != nil && maxContainersFlag.Changed
			maxContainers, _ := cmd.Flags().GetInt("max-containers")
			if containerCapacitySet && containerCapacity < 0 {
				return fmt.Errorf("container_capacity must be 0 or greater")
			}
			if maxContainersSet && maxContainers < 0 {
				return fmt.Errorf("max_containers must be 0 or greater")
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

			runDir := daemon.RunDir(".sandman", resolvedBatch.Issues)
			broadcaster := daemon.NewBroadcaster()
			ctlSocket := daemon.NewControlSocket(runDir, broadcaster)

			if err := ctlSocket.Start(); err != nil {
				return err
			}
			defer ctlSocket.Stop()
			defer os.RemoveAll(runDir)
			if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: append([]int(nil), resolvedBatch.Issues...), CreatedAt: time.Now()}); err != nil {
				return err
			}

			result, err := deps.BatchRunner.RunBatch(ctx, batch.Request{
				Issues:               resolvedBatch.Issues,
				Dependencies:         resolvedBatch.Deps,
				Blocked:              resolvedBatch.Blocked,
				Agent:                agentName,
				Model:                strings.TrimSpace(modelFlag),
				BaseBranch:           baseBranch,
				Parallel:             parallel,
				StartDelay:           time.Duration(startDelay) * time.Second,
				StartDelaySet:        startDelaySet,
				Sandbox:              sandboxMode,
				ContainerCapacity:    containerCapacity,
				ContainerCapacitySet: containerCapacitySet,
				MaxContainers:        maxContainers,
				MaxContainersSet:     maxContainersSet,
				OutputWriter:         broadcaster,
				PromptConfig: prompt.RenderConfig{
					PromptFlag:       promptFlag,
					TemplateFlag:     templateFlag,
					ReviewCommand:    reviewCommand,
					ReviewCommandSet: true,
					PromptArgs:       promptArgs,
				},
			})
			if result != nil {
				printSummary(cmd, result)
				for _, run := range result.Runs {
					if strings.TrimSpace(run.WorktreePath) != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "worktree: %s\n", run.WorktreePath)
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
	cmd.Flags().Int("start-delay", 0, "Wait N seconds after any AgentRun finishes before starting the next one; 0 disables the delay")
	cmd.Flags().String("sandbox", "", "Sandbox mode: podman (default), docker, or worktree")
	cmd.Flags().Int("container-capacity", 0, "Maximum concurrent agent runs per container; 0 means unlimited")
	cmd.Flags().Int("max-containers", 0, "Maximum number of containers to run at once; 0 means auto mode")
	cmd.Flags().Bool("include-dependencies", false, "Expand the batch to include transitive blockers")
	cmd.Flags().String("label", "", "Select issues by label")
	cmd.Flags().String("query", "", "Select issues by GitHub search query")
	cmd.Flags().String("prompt", "", "Inline prompt template (overrides --template and .sandman/prompt.md). Omit {{ISSUE_NUMBER}} for prompt-only mode.")
	cmd.Flags().String("template", "", "Path to prompt template file (overrides .sandman/prompt.md). Omit {{ISSUE_NUMBER}} for prompt-only mode.")
	cmd.Flags().String("model", "", "Override agent model for built-in presets")
	cmd.Flags().String("agent", "", "Built-in agent preset (opencode or pi)")
	cmd.Flags().String("base-branch", "", "Base branch to fetch from origin before each AgentRun starts")
	cmd.Flags().StringArray("prompt-arg", nil, "Custom template substitution KEY=VALUE (repeatable)")

	cmd.Flags().Int("ralph", 0, "Delegate the N lowest-numbered open issues labeled ready-for-agent")
	if pf := cmd.Flags().Lookup("ralph"); pf != nil {
		pf.NoOptDefVal = "1"
	}

	return cmd
}

func buildIssueQuery(label, query string) string {
	var groups []string

	if label != "" {
		groups = append(groups, "label:"+label)
	}

	if query != "" {
		groups = append(groups, "("+query+")")
	}

	groups = append(groups, "is:open")

	return strings.Join(groups, " ")
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

func resolveRalphIssues(ctx context.Context, client github.Client, count int, label, query, sandmanDir, agentName, modelFlag string, cfg *config.Config) ([]int, error) {
	if priorityPromptFileExists(sandmanDir) {
		return runSelectionPhase(ctx, client, count, label, query, sandmanDir, agentName, modelFlag, cfg)
	}

	searchQuery := resolveRalphQuery(label, query)
	ghIssues, err := client.SearchIssues(searchQuery)
	if err != nil {
		return nil, fmt.Errorf("search issues: %w", err)
	}
	if len(ghIssues) == 0 {
		return nil, fmt.Errorf("no issues ready for agent")
	}

	sort.Slice(ghIssues, func(i, j int) bool {
		return ghIssues[i].Number < ghIssues[j].Number
	})

	if count > len(ghIssues) {
		count = len(ghIssues)
	}

	numbers := make([]int, count)
	for i := 0; i < count; i++ {
		numbers[i] = ghIssues[i].Number
	}
	return numbers, nil
}

func pickIssues(ctx context.Context, client github.Client, picker IssuePicker) ([]int, error) {
	ghIssues, err := client.SearchIssues("is:open")
	if err != nil {
		return nil, fmt.Errorf("list open issues: %w", err)
	}
	return picker.Select(ghIssues)
}

func printSummary(cmd *cobra.Command, result *batch.Result) {
	var successCount, failureCount, blockedCount int
	for _, run := range result.Runs {
		if run.Status == "success" {
			successCount++
		} else if run.Status == "blocked" {
			blockedCount++
		} else {
			failureCount++
		}
	}

	if blockedCount > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Summary: %d succeeded, %d failed, %d blocked\n", successCount, failureCount, blockedCount)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Summary: %d succeeded, %d failed\n", successCount, failureCount)
	}
	for _, run := range result.Runs {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  %s\n", formatIssueLabel(run.IssueNumber, run.Issue), run.Status, run.Branch)
	}
}

func formatIssueLabel(issueNumber int, issue *int) string {
	if issue == nil && issueNumber == 0 {
		return "prompt-only"
	}
	return fmt.Sprintf("#%d", issueNumber)
}

func promptRequiresIssueSelection(promptText string) bool {
	return strings.Contains(promptText, "{{ISSUE_NUMBER}}") || strings.Contains(promptText, "{{ISSUE_TITLE}}") || strings.Contains(promptText, "{{ISSUE_BODY}}")
}
