package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/spf13/cobra"
)

// NewContinueCmd creates the continue command.
func NewContinueCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "continue [issue-number] <prompt-text>",
		Short: "Continue the last agent run for an issue",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			issueNum, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid issue number %q: %w", args[0], err)
			}

			promptText := strings.Join(args[1:], " ")
			if strings.TrimSpace(promptText) == "" {
				return fmt.Errorf("no prompt provided")
			}

			eventsList, err := deps.EventLog.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
			}

			var lastRun events.Event
			for _, e := range eventsList {
				if (e.Type == "run.started" || e.Type == "run.continued") && e.Issue == issueNum {
					lastRun = e
				}
			}

			if lastRun.RunID == "" {
				return fmt.Errorf("no previous run found for issue #%d", issueNum)
			}

			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			branch, ok := payloadString(lastRun.Payload, "branch")
			if !ok || strings.TrimSpace(branch) == "" {
				return fmt.Errorf("missing branch in previous run for issue #%d", issueNum)
			}

			worktreeBase := cfg.WorktreeDir
			if strings.TrimSpace(worktreeBase) == "" {
				worktreeBase = ".sandman/worktrees"
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

			reviewCommand := effectiveReviewCommand(cfg)
			if storedReviewCommand, ok := payloadString(lastRun.Payload, "review_command"); ok && strings.TrimSpace(storedReviewCommand) != "" {
				reviewCommand = storedReviewCommand
			}

			agentName := strings.TrimSpace(cmdFlag(cmd, "agent"))
			if agentName == "" {
				if storedAgent, ok := payloadString(lastRun.Payload, "agent"); ok {
					agentName = strings.TrimSpace(storedAgent)
				}
			}
			if agentName == "" {
				agentName = strings.TrimSpace(cfg.DefaultAgent)
			}
			if agentName == "" {
				agentName = strings.TrimSpace(cfg.Agent)
			}
			if _, err := cfg.ResolveAgentProvider(agentName); err != nil {
				return err
			}

			model := strings.TrimSpace(cmdFlag(cmd, "model"))
			if model == "" {
				if storedModel, ok := payloadString(lastRun.Payload, "model"); ok {
					model = strings.TrimSpace(storedModel)
				}
			}

			req := batch.Request{
				Issues:        []int{issueNum},
				Branches:      map[int]string{issueNum: branch},
				Agent:         agentName,
				Model:         model,
				Continuation:  true,
				PreviousRunID: lastRun.RunID,
				PromptConfig: prompt.RenderConfig{
					ContinuePrompt:   promptText,
					ReviewCommand:    reviewCommand,
					ReviewCommandSet: true,
				},
			}

			result, err := deps.BatchRunner.RunBatch(cmd.Context(), req)
			if result != nil {
				printSummary(cmd, result)
			}
			if err != nil {
				return fmt.Errorf("run batch: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().String("model", "", "Override agent model for built-in presets")
	cmd.Flags().String("agent", "", "Built-in agent preset (opencode or pi)")

	return cmd
}

func cmdFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

func effectiveReviewCommand(cfg *config.Config) string {
	if cfg == nil {
		return config.DefaultReviewCommand
	}
	return cfg.EffectiveReviewCommand()
}

func payloadString(payload map[string]any, key string) (string, bool) {
	v, ok := payload[key]
	if !ok {
		return "", false
	}
	str, ok := v.(string)
	return str, ok
}
