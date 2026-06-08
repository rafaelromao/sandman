package cmd

import (
	"fmt"
	"strings"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/spf13/cobra"
)

// NewReviewCmd creates the `sandman review` command. One-shot mode is the
// only mode implemented in this slice; the daemon mode lands in a follow-up
// issue and is documented in ADR-0014.
//
// Required: --pr <N>. Optional overrides: --agent, --model, --sandbox.
func NewReviewCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Run a Sandman agent to review a pull request",
		Long: "Run a Sandman agent to review a pull request. One-shot mode posts a single " +
			"review comment; daemon mode (default) is not yet implemented in this slice.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			prNumber, err := cmd.Flags().GetInt("pr")
			if err != nil {
				return fmt.Errorf("read --pr flag: %w", err)
			}
			if prNumber <= 0 {
				return fmt.Errorf("--pr is required and must be a positive integer")
			}

			pr, err := deps.GitHubClient.FetchPR(prNumber)
			if err != nil {
				return fmt.Errorf("fetch PR #%d: %w", prNumber, err)
			}

			agentFlag, _ := cmd.Flags().GetString("agent")
			modelFlag, _ := cmd.Flags().GetString("model")
			sandboxFlag, _ := cmd.Flags().GetString("sandbox")

			reviewAgentName := strings.TrimSpace(agentFlag)
			if reviewAgentName == "" {
				reviewAgentName = cfg.EffectiveReviewAgent()
			}
			if _, err := cfg.ResolveAgentProvider(reviewAgentName); err != nil {
				return err
			}

			reviewModel := strings.TrimSpace(modelFlag)
			if reviewModel == "" {
				reviewModel = cfg.EffectiveReviewModel()
			}

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
				sandboxMode = "worktree"
			}

			if _, err := deps.BatchRunner.RunBatch(cmd.Context(), batch.Request{
				Agent:   reviewAgentName,
				Model:   reviewModel,
				Sandbox: sandboxMode,
				PromptConfig: prompt.RenderConfig{
					PromptFlag: rendered,
				},
			}); err != nil {
				return fmt.Errorf("run review batch: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().Int("pr", 0, "Pull request number to review (required for one-shot mode)")
	cmd.Flags().String("agent", "", "Override default_review_agent for this run")
	cmd.Flags().String("model", "", "Override default_review_model for this run")
	cmd.Flags().String("sandbox", "", "Sandbox mode for the review run (default: worktree)")

	return cmd
}
