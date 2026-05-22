package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
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
		Long:  "Run an AFK agent for selected issues and leave worktrees on disk. Use \"sandman clean\" to delete preserved worktrees.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			label, _ := cmd.Flags().GetString("label")
			query, _ := cmd.Flags().GetString("query")
			interactive, _ := cmd.Flags().GetBool("interactive")
			includeDependencies, _ := cmd.Flags().GetBool("include-dependencies")
			nextFlag := cmd.Flags().Lookup("next")
			nextProvided := nextFlag != nil && nextFlag.Changed
			nextCount, _ := cmd.Flags().GetInt("next")

			var issues []int
			if nextProvided {
				if len(args) > 0 || label != "" || query != "" {
					return fmt.Errorf("cannot combine --next with issue arguments, --label or --query")
				}
				if nextCount <= 0 {
					return fmt.Errorf("--next count must be at least 1")
				}
				issues, err = resolveNextIssues(cmd.Context(), deps.GitHubClient, nextCount)
				if err != nil {
					return err
				}
			} else if len(args) > 0 {
				if label != "" || query != "" {
					return fmt.Errorf("cannot combine issue arguments with --label or --query")
				}
				issues = make([]int, len(args))
				for i, arg := range args {
					n, err := strconv.Atoi(arg)
					if err != nil {
						return fmt.Errorf("invalid issue number %q: %w", arg, err)
					}
					issues[i] = n
				}
			} else if label != "" {
				searchQuery := "label:" + label + " is:open"
				issues, err = resolveIssues(cmd.Context(), deps.GitHubClient, searchQuery)
				if err != nil {
					return err
				}
			} else if query != "" {
				issues, err = resolveIssues(cmd.Context(), deps.GitHubClient, query)
				if err != nil {
					return err
				}
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

			if len(issues) == 0 {
				return fmt.Errorf("no issues selected")
			}

			if interactive && includeDependencies {
				return fmt.Errorf("cannot combine --include-dependencies with --interactive")
			}

			if interactive && len(issues) > 1 {
				return fmt.Errorf("--interactive requires exactly one issue")
			}

			promptFlag, _ := cmd.Flags().GetString("prompt")
			templateFlag, _ := cmd.Flags().GetString("template")
			reviewCommandFlag, _ := cmd.Flags().GetString("review-command")
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
			if strings.TrimSpace(reviewCommandFlag) != "" {
				reviewCommand = reviewCommandFlag
			}

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

			liveRuns := daemon.NewLiveRunStore(".sandman")
			broadcaster := daemon.NewBroadcaster()
			runID := daemon.NewRunID(resolvedBatch.Issues)
			runMeta := daemon.LiveRun{
				RunID:     runID,
				PID:       os.Getpid(),
				Issues:    append([]int(nil), resolvedBatch.Issues...),
				StartedAt: time.Now().UTC(),
			}
			if err := liveRuns.Register(runMeta); err != nil {
				return fmt.Errorf("write live run metadata: %w", err)
			}

			ctlSocket := daemon.NewControlSocket(liveRuns.RunDir(runID), broadcaster)
			defer func() {
				if err := ctlSocket.Stop(); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warn: close control socket: %v\n", err)
				}
				if err := liveRuns.Remove(runID); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warn: remove live run dir: %v\n", err)
				}
			}()

			if err := ctlSocket.Start(); err != nil {
				return err
			}

			result, err := deps.BatchRunner.RunBatch(ctx, batch.Request{
				Issues:               resolvedBatch.Issues,
				Dependencies:         resolvedBatch.Deps,
				Agent:                agentName,
				Model:                strings.TrimSpace(modelFlag),
				Parallel:             parallel,
				StartDelay:           time.Duration(startDelay) * time.Second,
				StartDelaySet:        startDelaySet,
				Sandbox:              sandboxMode,
				ContainerCapacity:    containerCapacity,
				ContainerCapacitySet: containerCapacitySet,
				MaxContainers:        maxContainers,
				MaxContainersSet:     maxContainersSet,
				Interactive:          interactive,
				OutputWriter:         broadcaster,
				PromptConfig: prompt.RenderConfig{
					PromptFlag:       promptFlag,
					TemplateFlag:     templateFlag,
					ReviewCommand:    reviewCommand,
					ReviewCommandSet: strings.TrimSpace(reviewCommandFlag) != "",
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
	cmd.Flags().Int("container-capacity", 0, "Maximum concurrent agent runs per container; 0 means auto/default mode")
	cmd.Flags().Int("max-containers", 0, "Maximum number of containers to run at once; 0 means auto mode")
	cmd.Flags().Bool("interactive", false, "Run the agent in interactive mode")
	cmd.Flags().Bool("include-dependencies", false, "Expand the batch to include transitive blockers")
	cmd.Flags().String("label", "", "Select issues by label")
	cmd.Flags().String("query", "", "Select issues by GitHub search query")
	cmd.Flags().String("prompt", "", "Inline prompt template (overrides --template and .sandman/prompt.md)")
	cmd.Flags().String("template", "", "Path to prompt template file (overrides .sandman/prompt.md)")
	cmd.Flags().String("review-command", "", "Review command to inject into the prompt template")
	cmd.Flags().String("model", "", "Override agent model for built-in presets")
	cmd.Flags().String("agent", "", "Built-in agent preset (opencode or pi)")
	cmd.Flags().StringArray("prompt-arg", nil, "Custom template substitution KEY=VALUE (repeatable)")

	cmd.Flags().Int("next", 0, "Delegate the N lowest-numbered open issues labeled ready-for-agent")
	if pf := cmd.Flags().Lookup("next"); pf != nil {
		pf.NoOptDefVal = "1"
	}

	return cmd
}

func resolveIssues(ctx context.Context, client github.Client, query string) ([]int, error) {
	ghIssues, err := client.SearchIssues(query)
	if err != nil {
		return nil, fmt.Errorf("search issues: %w", err)
	}
	numbers := make([]int, len(ghIssues))
	for i, issue := range ghIssues {
		numbers[i] = issue.Number
	}
	return numbers, nil
}

func resolveNextIssues(ctx context.Context, client github.Client, count int) ([]int, error) {
	ghIssues, err := client.SearchIssues("label:ready-for-agent is:open")
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
		fmt.Fprintf(cmd.OutOrStdout(), "  #%d  %s  %s\n", run.IssueNumber, run.Status, run.Branch)
	}
}
