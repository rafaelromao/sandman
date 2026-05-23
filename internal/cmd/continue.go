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

			continuePrompt := promptText
			contextPath := filepath.Join(worktreePath, ".sandman", "continuation-context.md")
			if content, err := os.ReadFile(contextPath); err != nil {
				if !os.IsNotExist(err) {
					return fmt.Errorf("read continuation context %q: %w", contextPath, err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: missing continuation context %q; continuing with bare prompt\n", contextPath)
			} else {
				priorContext := strings.TrimSpace(stripContinuationContextHeader(string(content)))
				if priorContext != "" {
					continuePrompt = buildContinuationPrompt(promptText, priorContext)
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
					ContinuePrompt:   continuePrompt,
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

func stripContinuationContextHeader(content string) string {
	lines := strings.Split(content, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
		i++
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
	}
	return strings.Join(lines[i:], "\n")
}

func buildContinuationPrompt(promptText, priorContext string) string {
	var b strings.Builder
	b.WriteString("## Prior Context\n\n")
	b.WriteString(strings.TrimSpace(priorContext))
	b.WriteString("\n\n## New Instruction\n\n")
	b.WriteString(strings.TrimSpace(promptText))
	b.WriteString("\n\n## Update Continuation Context\n\n")
	b.WriteString("Before exiting, overwrite `.sandman/continuation-context.md` with an updated summary using this template:\n\n")
	b.WriteString("```markdown\n## Completed\n(what was implemented, committed, or merged)\n\n## Pending\n(what remains unfinished)\n\n## Blockers\n(anything preventing completion)\n\n## Key Decisions\n(significant design choices made)\n\n## Next Step\n(single most important next action)\n```\n")
	return b.String()
}
