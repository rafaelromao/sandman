package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/spf13/cobra"
)

// NewContinueCmd creates the continue command.
func NewContinueCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "continue [issue-number...] <prompt-text>",
		Short: "Continue the last agent run for one or more issues in a batch",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			issues, promptText, err := parseContinueArgs(args)
			if err != nil {
				return err
			}

			eventsList, err := deps.EventLog.Read()
			if err != nil {
				return fmt.Errorf("read event log: %w", err)
			}

			lastRuns := lastRunPerIssue(eventsList, issues)

			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			worktreeBase := cfg.WorktreeDir
			if strings.TrimSpace(worktreeBase) == "" {
				worktreeBase = ".sandman/worktrees"
			}

			branches := make(map[int]string, len(issues))
			baseBranches := make(map[int]string, len(issues))
			previousRunIDs := make(map[int]string, len(issues))
			worktreePaths := make(map[int]string, len(issues))
			for _, num := range issues {
				lastRun := lastRuns[num]
				if lastRun.RunID == "" {
					return fmt.Errorf("no previous run found for issue #%d", num)
				}
				branch, ok := payloadString(lastRun.Payload, "branch")
				if !ok || strings.TrimSpace(branch) == "" {
					return fmt.Errorf("missing branch in previous run for issue #%d", num)
				}
				baseBranch, ok := payloadString(lastRun.Payload, "base_branch")
				if !ok || strings.TrimSpace(baseBranch) == "" {
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
				branches[num] = branch
				baseBranches[num] = strings.TrimSpace(baseBranch)
				previousRunIDs[num] = lastRun.RunID
				worktreePaths[num] = worktreePath
			}

			// Replay agent/model/review settings from the first issue's last run.
			// Per-issue replay is tracked by #443.
			firstIssue := issues[0]
			firstLastRun := lastRuns[firstIssue]

			reviewCommand := effectiveReviewCommand(cfg)
			if storedReviewCommand, ok := payloadString(firstLastRun.Payload, "review_command"); ok && strings.TrimSpace(storedReviewCommand) != "" {
				reviewCommand = storedReviewCommand
			}

			agentName := strings.TrimSpace(cmdFlag(cmd, "agent"))
			if agentName == "" {
				if storedAgent, ok := payloadString(firstLastRun.Payload, "agent"); ok {
					agentName = strings.TrimSpace(storedAgent)
				}
			}
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

			model := resolveModel(cmdFlag(cmd, "model"), cfg.DefaultModel, agentCfg.Preset)

			dangerouslySkipPermFlag := cmd.Flags().Lookup("dangerously-skip-permissions")
			dangerouslySkipPermSet := dangerouslySkipPermFlag != nil && dangerouslySkipPermFlag.Changed
			var dangerouslySkipPerm *bool
			if dangerouslySkipPermSet {
				val, _ := cmd.Flags().GetBool("dangerously-skip-permissions")
				dangerouslySkipPerm = &val
			}

			// Continuation prompt is currently built from the first issue's
			// context. Per-issue prompt rendering is tracked by #443.
			continuePrompt := promptText
			firstContextPath := filepath.Join(worktreePaths[firstIssue], ".sandman", "continuation-context.md")
			if content, err := os.ReadFile(firstContextPath); err != nil {
				if !os.IsNotExist(err) {
					return fmt.Errorf("read continuation context %q: %w", firstContextPath, err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: missing continuation context %q; continuing with bare prompt\n", firstContextPath)
			} else {
				priorContext := strings.TrimSpace(stripContinuationContextHeader(string(content)))
				if priorContext != "" {
					continuePrompt = buildContinuationPrompt(promptText, priorContext)
				}
			}

			req := batch.Request{
				Issues:                     issues,
				Branches:                   branches,
				Agent:                      agentName,
				Model:                      model,
				BaseBranch:                 baseBranches[firstIssue],
				Continuation:               true,
				PreviousRunIDs:             previousRunIDs,
				DangerouslySkipPermissions: dangerouslySkipPerm,
				PromptConfig: prompt.RenderConfig{
					ContinuePrompt:   continuePrompt,
					ReviewCommand:    reviewCommand,
					ReviewCommandSet: true,
				},
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)
			go func() {
				select {
				case <-sigCh:
					cancel()
				case <-ctx.Done():
				}
			}()

			runDir := daemon.RunDir(".sandman", issues)
			broadcaster := daemon.NewBroadcaster()
			ctlSocket := daemon.NewControlSocket(runDir, broadcaster)

			if err := ctlSocket.Start(); err != nil {
				return err
			}
			defer ctlSocket.Stop()
			defer os.RemoveAll(runDir)
			if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: issues, CreatedAt: time.Now()}); err != nil {
				return err
			}

			req.OutputWriter = broadcaster

			result, err := deps.BatchRunner.RunBatch(ctx, req)
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
	cmd.Flags().Bool("dangerously-skip-permissions", false, "Skip opencode permission prompts (auto-approves non-denied actions); default is true for container runs, false for worktree runs")

	return cmd
}

// lastRunPerIssue scans the event log once and returns the latest run.started
// or run.continued event for each requested issue.
func lastRunPerIssue(eventsList []events.Event, issues []int) map[int]events.Event {
	wanted := make(map[int]struct{}, len(issues))
	for _, num := range issues {
		wanted[num] = struct{}{}
	}
	lastRuns := make(map[int]events.Event, len(issues))
	for _, e := range eventsList {
		if e.Type != "run.started" && e.Type != "run.continued" {
			continue
		}
		if _, ok := wanted[e.Issue]; !ok {
			continue
		}
		lastRuns[e.Issue] = e
	}
	return lastRuns
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
