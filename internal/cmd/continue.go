package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
		Use:   "continue <issue-number>...",
		Short: "Continue the last agent run for one or more issues in a batch",
		Args:  wrapArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			issues, err := parseContinueArgs(args)
			if err != nil {
				return MarkUsage(err)
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

			if err := requireReviewDaemon(cfg.EffectiveReviewCommand(), ".sandman"); err != nil {
				return err
			}

			worktreeBase := cfg.WorktreeDir
			if strings.TrimSpace(worktreeBase) == "" {
				worktreeBase = ".sandman/worktrees"
			}

			branches := make(map[int]string, len(issues))
			baseBranches := make(map[int]string, len(issues))
			previousRunIDs := make(map[int]string, len(issues))
			handoffPrompts := make(map[int]string, len(issues))
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
				merged, err := batch.CheckPRMergedAtHead(deps.GitHubClient, branch, "")
				if err != nil {
					return fmt.Errorf("check merged status for issue #%d: %w", num, err)
				}
				if merged {
					return fmt.Errorf("cannot continue issue #%d: PR already merged (branch %q)", num, branch)
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

				handoffPath := filepath.Join(worktreePath, ".sandman", "handoff.md")
				content, exists, err := batch.ReadHandoffContent(handoffPath)
				if err != nil {
					return fmt.Errorf("read handoff %q for issue #%d: %w", handoffPath, num, err)
				}
				if !exists {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: no handoff found in worktree %q; using empty template\n", branch)
				}
				handoffPrompts[num] = content
			}

			// Replay agent/model/review settings from the first issue's last run.
			// Per-issue replay is tracked by #443.
			firstIssue := issues[0]
			firstLastRun := lastRuns[firstIssue]

			reviewCommand := effectiveReviewCommand(cfg)
			if storedReviewCommand, ok := payloadString(firstLastRun.Payload, "review_command"); ok && strings.TrimSpace(storedReviewCommand) != "" {
				reviewCommand = storedReviewCommand
			}

			parallel := 0
			if v, ok := payloadInt(firstLastRun.Payload, "parallel"); ok {
				parallel = v
			}

			startDelay := time.Duration(0)
			startDelaySet := false
			if v, ok := payloadInt(firstLastRun.Payload, "start_delay"); ok {
				startDelay = time.Duration(v) * time.Second
				startDelaySet = true
			}

			runIdleTimeout := 0
			runIdleTimeoutSet := false

			retries := -1
			if v, ok := payloadInt(firstLastRun.Payload, "retries"); ok {
				retries = v
			}

			sandboxMode := ""
			if v, ok := payloadString(firstLastRun.Payload, "sandbox"); ok {
				sandboxMode = strings.TrimSpace(v)
			}

			containerCapacity := 0
			containerCapacitySet := false
			if v, ok := payloadInt(firstLastRun.Payload, "container_capacity"); ok {
				containerCapacity = v
			}
			if v, ok := payloadBool(firstLastRun.Payload, "container_capacity_set"); ok {
				containerCapacitySet = v
			}

			maxContainers := 0
			maxContainersSet := false
			if v, ok := payloadInt(firstLastRun.Payload, "max_containers"); ok {
				maxContainers = v
			}
			if v, ok := payloadBool(firstLastRun.Payload, "max_containers_set"); ok {
				maxContainersSet = v
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
			if sandboxFlag := cmd.Flags().Lookup("sandbox"); sandboxFlag != nil && sandboxFlag.Changed {
				sandboxMode, _ = cmd.Flags().GetString("sandbox")
			}
			if parallelFlag := cmd.Flags().Lookup("parallel"); parallelFlag != nil && parallelFlag.Changed {
				parallel, _ = cmd.Flags().GetInt("parallel")
			}
			if startDelayFlag := cmd.Flags().Lookup("start-delay"); startDelayFlag != nil && startDelayFlag.Changed {
				startDelaySecs, _ := cmd.Flags().GetInt("start-delay")
				startDelay = time.Duration(startDelaySecs) * time.Second
				startDelaySet = true
			}
			if runIdleTimeoutFlag := cmd.Flags().Lookup("run-idle-timeout"); runIdleTimeoutFlag != nil && runIdleTimeoutFlag.Changed {
				runIdleTimeoutSecs, _ := cmd.Flags().GetInt("run-idle-timeout")
				if runIdleTimeoutSecs < 0 {
					return MarkUsage(fmt.Errorf("run_idle_timeout must be 0 or greater"))
				}
				runIdleTimeout = runIdleTimeoutSecs
				runIdleTimeoutSet = true
			}
			if retriesFlag := cmd.Flags().Lookup("retries"); retriesFlag != nil && retriesFlag.Changed {
				retries, _ = cmd.Flags().GetInt("retries")
			}
			if containerCapacityFlag := cmd.Flags().Lookup("container-capacity"); containerCapacityFlag != nil && containerCapacityFlag.Changed {
				containerCapacity, _ = cmd.Flags().GetInt("container-capacity")
				containerCapacitySet = true
			}
			if maxContainersFlag := cmd.Flags().Lookup("max-containers"); maxContainersFlag != nil && maxContainersFlag.Changed {
				maxContainers, _ = cmd.Flags().GetInt("max-containers")
				maxContainersSet = true
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

			req := batch.Request{
				Issues:                     issues,
				Branches:                   branches,
				Agent:                      agentName,
				Model:                      model,
				BaseBranch:                 baseBranches[firstIssue],
				Parallel:                   parallel,
				Retries:                    retries,
				StartDelay:                 startDelay,
				StartDelaySet:              startDelaySet,
				RunIdleTimeout:             runIdleTimeout,
				RunIdleTimeoutSet:          runIdleTimeoutSet,
				Sandbox:                    sandboxMode,
				RequireDockerfile:          true,
				ContainerCapacity:          containerCapacity,
				ContainerCapacitySet:       containerCapacitySet,
				MaxContainers:              maxContainers,
				MaxContainersSet:           maxContainersSet,
				Continuation:               true,
				PreviousRunIDs:             previousRunIDs,
				BaseBranches:               baseBranches,
				HandoffPrompts:             handoffPrompts,
				DangerouslySkipPermissions: dangerouslySkipPerm,
				PromptConfig: prompt.RenderConfig{
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

			runDir := daemon.RunDir(".sandman", issues, "")
			broadcaster := daemon.NewBroadcaster()
			ctlSocket := daemon.NewControlSocket(runDir, broadcaster)

			if staleRemoved, err := daemon.CleanupStaleRunSnapshots(".sandman"); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: cleanup stale run snapshots: %v\n", err)
			} else if staleRemoved > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Cleaned %d stale run-owned config snapshots from previous runs\n", staleRemoved)
			}

			if err := ctlSocket.Start(); err != nil {
				return err
			}
			defer ctlSocket.Stop()
			defer os.RemoveAll(runDir)
			if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: issues, CreatedAt: time.Now()}); err != nil {
				return err
			}

			req.OutputWriter = broadcaster
			req.RunDir = runDir

			result, err := deps.BatchRunner.RunBatch(ctx, req)
			if result != nil {
				printSummary(cmd, result)
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

	cmd.Flags().String("model", "", "Override agent model for built-in presets")
	cmd.Flags().String("agent", "", "Built-in agent preset (opencode or pi)")
	cmd.Flags().Int("parallel", 0, "Limit parallel execution")
	cmd.Flags().Int("start-delay", 0, "Wait N seconds after any AgentRun finishes before starting the next one; 0 disables the delay")
	cmd.Flags().Int("run-idle-timeout", 0, "Treat an AgentRun as stuck if it produces no output for N seconds; 0 disables the timeout")
	cmd.Flags().Int("retries", 0, "Retry failed AgentRuns up to N times")
	cmd.Flags().String("sandbox", "", "Sandbox mode: podman (default), docker, or worktree")
	cmd.Flags().Int("container-capacity", 0, "Maximum concurrent agent runs per container; 0 means unlimited")
	cmd.Flags().Int("max-containers", 0, "Maximum number of containers to run at once; 0 means auto mode")
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

func payloadInt(payload map[string]any, key string) (int, bool) {
	if payload == nil {
		return 0, false
	}
	v, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int8:
		return int(n), true
	case int16:
		return int(n), true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case uint:
		return int(n), true
	case uint8:
		return int(n), true
	case uint16:
		return int(n), true
	case uint32:
		return int(n), true
	case uint64:
		return int(n), true
	case float32:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func payloadBool(payload map[string]any, key string) (bool, bool) {
	if payload == nil {
		return false, false
	}
	v, ok := payload[key]
	if !ok {
		return false, false
	}
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(b))
		if err == nil {
			return parsed, true
		}
	}
	return false, false
}
