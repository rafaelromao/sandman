package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/review"
	"github.com/spf13/cobra"
)

// reviewDaemonRunner is the function used to build and run the review
// daemon. Tests override it to avoid actually polling GitHub.
var reviewDaemonRunner = runReviewDaemon

// NewReviewCmd creates the `sandman review` command. When --pr is provided
// the command runs in one-shot mode (post a single review comment and
// exit). When --pr is omitted, the command starts the review daemon:
// it polls open PRs every 60s for `/sandman review` comments and launches
// review agents serially. The daemon writes log lines to .sandman/review.sock
// (exposed via `sandman attach`) and shuts down cleanly on SIGINT/SIGTERM.
func NewReviewCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Run a Sandman agent to review a pull request",
		Long: "Run a Sandman agent to review a pull request. With --pr, posts a single " +
			"review comment and exits. Without --pr, starts the review daemon that polls " +
			"open PRs every 60s for /sandman review comments and launches review agents.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			prNumber, err := cmd.Flags().GetInt("pr")
			if err != nil {
				return fmt.Errorf("read --pr flag: %w", err)
			}

			sandboxFlag, _ := cmd.Flags().GetString("sandbox")
			ccFlag, _ := cmd.Flags().GetInt("container-capacity")
			ccSet := cmd.Flags().Changed("container-capacity")
			mcFlag, _ := cmd.Flags().GetInt("max-containers")
			mcSet := cmd.Flags().Changed("max-containers")

			if ccSet && ccFlag < 0 {
				return MarkUsage(fmt.Errorf("container_capacity must be 0 or greater"))
			}
			if mcSet && mcFlag < 0 {
				return MarkUsage(fmt.Errorf("max_containers must be 0 or greater"))
			}

			if prNumber > 0 {
				return runReviewOneShot(cmd, deps, cfg, prNumber)
			}
			return reviewDaemonRunner(cmd.Context(), deps, cfg, sandboxFlag, ccFlag, ccSet, mcFlag, mcSet)
		},
	}

	cmd.Flags().Int("pr", 0, "Pull request number to review (omit to start the review daemon)")
	cmd.Flags().String("agent", "", "Override default_review_agent for this run")
	cmd.Flags().String("model", "", "Override default_review_model for this run")
	cmd.Flags().String("sandbox", "", "Sandbox mode for the review run (default: config value)")
	cmd.Flags().Int("container-capacity", 0, "Container capacity for sandbox containers (default: config value)")
	cmd.Flags().Int("max-containers", 0, "Maximum number of sandbox containers (default: config value)")

	return cmd
}

// runReviewOneShot handles the legacy --pr <N> flow. Kept as a separate
// function so the daemon branch can be tested independently.
func runReviewOneShot(cmd *cobra.Command, deps Dependencies, cfg *config.Config, prNumber int) error {
	pr, err := deps.GitHubClient.FetchPR(prNumber)
	if err != nil {
		return fmt.Errorf("fetch PR #%d: %w", prNumber, err)
	}

	agentFlag, _ := cmd.Flags().GetString("agent")
	modelFlag, _ := cmd.Flags().GetString("model")
	sandboxFlag, _ := cmd.Flags().GetString("sandbox")
	ccFlag, _ := cmd.Flags().GetInt("container-capacity")
	mcFlag, _ := cmd.Flags().GetInt("max-containers")

	reviewAgentName := strings.TrimSpace(agentFlag)
	if reviewAgentName == "" {
		reviewAgentName = cfg.EffectiveReviewAgent()
	}
	if reviewAgentName == "" {
		return fmt.Errorf("review agent is not set; configure review_agent or agent in sandman config")
	}
	if _, err := cfg.ResolveAgentProvider(reviewAgentName); err != nil {
		return err
	}

	reviewModel := strings.TrimSpace(modelFlag)
	if reviewModel == "" {
		reviewModel = cfg.EffectiveReviewModel()
	}
	if reviewModel == "" {
		return fmt.Errorf("review model is not set; configure review_model or model in sandman config")
	}

	repoName, err := deps.GitHubClient.RepoName()
	if err != nil {
		return fmt.Errorf("get repo name: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "repo=%s agent=%s model=%s\n", repoName, reviewAgentName, reviewModel)

	rendered, err := deps.PromptRenderer.RenderReview(prompt.RenderConfig{}, prompt.PRData{
		Number: pr.Number,
		Title:  pr.Title,
		Body:   pr.Body,
	})
	if err != nil {
		return fmt.Errorf("render review prompt: %w", err)
	}

	sandboxMode := strings.TrimSpace(sandboxFlag)
	if sandboxMode == "" {
		sandboxMode = cfg.Sandbox
	}

	if _, err := deps.BatchRunner.RunBatch(cmd.Context(), batch.Request{
		Agent:                reviewAgentName,
		Model:                reviewModel,
		Sandbox:              sandboxMode,
		ContainerCapacity:    ccFlag,
		ContainerCapacitySet: cmd.Flags().Changed("container-capacity"),
		MaxContainers:        mcFlag,
		MaxContainersSet:     cmd.Flags().Changed("max-containers"),
		PromptConfig: prompt.RenderConfig{
			PromptFlag: rendered,
			Branch:     fmt.Sprintf("sandman/review-%d-%d", pr.Number, time.Now().UnixNano()),
		},
		Review:   true,
		PRNumber: pr.Number,
	}); err != nil {
		return fmt.Errorf("run review batch: %w", err)
	}
	return nil
}

// runReviewDaemon wires and runs the review daemon. The cmd layer owns
// the SIGINT/SIGTERM signal handling; the daemon handles the polling
// loop and the in-flight batch cancellation.
func runReviewDaemon(parent context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	socketDir := ".sandman"
	broadcaster := daemon.NewBroadcaster()
	ctlSocket := daemon.NewControlSocketWithName(socketDir, "review.sock", broadcaster)
	d := review.New(socketDir, deps.GitHubClient, deps.PromptRenderer, deps.BatchRunner, cfg, broadcaster)
	d.Sandbox = sandbox
	d.ContainerCapacity = cc
	d.ContainerCapacitySet = ccSet
	d.MaxContainers = mc
	d.MaxContainersSet = mcSet
	d.SetSocket(ctlSocket)
	return d.Run(ctx)
}
