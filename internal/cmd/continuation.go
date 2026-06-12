package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/spf13/cobra"
)

func buildContinuationRequest(cmd *cobra.Command, deps Dependencies, cfg *config.Config, issues []int, runID string) (batch.Request, error) {
	eventsList, err := deps.EventLog.Read()
	if err != nil {
		return batch.Request{}, fmt.Errorf("read event log: %w", err)
	}

	worktreeBase := cfg.WorktreeDir
	if strings.TrimSpace(worktreeBase) == "" {
		worktreeBase = ".sandman/worktrees"
	}

	lastRuns := lastRunPerIssue(eventsList, issues)
	previousRunIDs := make(map[int]string, len(issues))
	branches := make(map[int]string, len(issues))
	baseBranches := make(map[int]string, len(issues))
	taskPrompts := make(map[int]string, len(issues))
	var taskPromptContent string
	var promptOnlyBaseBranch string
	var promptOnlyBranch string
	modes := make(map[int]batch.IssueMode, len(issues))
	if runID != "" {
		promptOnlyEvent, found := lastPromptOnlyRun(eventsList)
		if !found || promptOnlyEvent.RunID == "" {
			return batch.Request{}, fmt.Errorf("no previous prompt-only run found")
		}
		issues = []int{0}
		lastRuns[0] = promptOnlyEvent
		previousRunIDs[0] = promptOnlyEvent.RunID
		promptOnlyBranch, _ = payloadString(promptOnlyEvent.Payload, "branch")
		promptOnlyBaseBranch, _ = payloadString(promptOnlyEvent.Payload, "base_branch")
	}

	if runID == "" {
		for _, num := range issues {
			lastRun := lastRuns[num]
			if lastRun.RunID == "" {
				return batch.Request{}, fmt.Errorf("no previous run found for issue #%d", num)
			}

			branch, ok := payloadString(lastRun.Payload, "branch")
			if !ok || strings.TrimSpace(branch) == "" {
				return batch.Request{}, fmt.Errorf("missing branch in previous run for issue #%d", num)
			}
			baseBranch, ok := payloadString(lastRun.Payload, "base_branch")
			if !ok || strings.TrimSpace(baseBranch) == "" {
				return batch.Request{}, fmt.Errorf("missing base branch in previous run for issue #%d", num)
			}
			merged, err := batch.CheckPRMergedAtHead(deps.GitHubClient, branch, "")
			if err != nil {
				return batch.Request{}, fmt.Errorf("check merged status for issue #%d: %w", num, err)
			}
			if merged {
				return batch.Request{}, fmt.Errorf("cannot continue issue #%d: PR already merged (branch %q)", num, branch)
			}

			worktreePath := filepath.Join(worktreeBase, branch)
			if info, err := os.Stat(worktreePath); err != nil {
				if os.IsNotExist(err) {
					return batch.Request{}, fmt.Errorf("worktree %q is missing; use \"sandman run\" instead", worktreePath)
				}
				return batch.Request{}, fmt.Errorf("check worktree %q: %w", worktreePath, err)
			} else if !info.IsDir() {
				return batch.Request{}, fmt.Errorf("worktree %q is missing; use \"sandman run\" instead", worktreePath)
			}

			taskPath := filepath.Join(worktreePath, ".sandman", "task.md")
			content, exists, err := batch.ReadTaskContent(taskPath)
			if err != nil {
				return batch.Request{}, fmt.Errorf("read task %q for issue #%d: %w", taskPath, num, err)
			}
			if !exists {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: no handoff found in worktree %q; using empty template\n", branch)
			}

			previousRunIDs[num] = lastRun.RunID
			branches[num] = strings.TrimSpace(branch)
			baseBranches[num] = strings.TrimSpace(baseBranch)
			taskPrompts[num] = prompt.BuildTaskPrompt(prompt.ParseTask(content))
			modes[num] = batch.ModeContinue
		}
	} else {
		promptOnlyEvent := lastRuns[0]
		promptOnlyBranch, _ = payloadString(promptOnlyEvent.Payload, "branch")
		promptOnlyBaseBranch, _ = payloadString(promptOnlyEvent.Payload, "base_branch")
		if promptOnlyBranch == "" {
			return batch.Request{}, fmt.Errorf("missing branch in previous prompt-only run")
		}
		if promptOnlyBaseBranch == "" {
			return batch.Request{}, fmt.Errorf("missing base branch in previous prompt-only run")
		}
		worktreePath := filepath.Join(worktreeBase, promptOnlyBranch)
		if info, err := os.Stat(worktreePath); err != nil {
			if os.IsNotExist(err) {
				return batch.Request{}, fmt.Errorf("worktree %q is missing for prompt-only run; use \"sandman run\" instead", worktreePath)
			}
			return batch.Request{}, fmt.Errorf("check worktree %q: %w", worktreePath, err)
		} else if !info.IsDir() {
			return batch.Request{}, fmt.Errorf("worktree %q is missing for prompt-only run; use \"sandman run\" instead", worktreePath)
		}
		taskPath := filepath.Join(worktreePath, ".sandman", "task.md")
		content, exists, err := batch.ReadTaskContent(taskPath)
		if err != nil {
			return batch.Request{}, fmt.Errorf("read task %q: %w", taskPath, err)
		}
		if !exists {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: no handoff found in worktree %q; using empty template\n", promptOnlyBranch)
		}
		taskPromptContent = prompt.BuildTaskPrompt(prompt.ParseTask(content))
		modes[0] = batch.ModeContinue
	}

	firstIssue := issues[0]
	firstLastRun := lastRuns[firstIssue]

	reviewCommand := effectiveReviewCommand(cfg)
	if storedReviewCommand, ok := payloadString(firstLastRun.Payload, "review_command"); ok && strings.TrimSpace(storedReviewCommand) != "" {
		reviewCommand = strings.TrimSpace(storedReviewCommand)
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
		return batch.Request{}, err
	}

	model := strings.TrimSpace(cmdFlag(cmd, "model"))
	if model == "" {
		if storedModel, ok := payloadString(firstLastRun.Payload, "model"); ok && strings.TrimSpace(storedModel) != "" {
			if storedAgent, ok := payloadString(firstLastRun.Payload, "agent"); !ok || strings.TrimSpace(storedAgent) == "" || strings.TrimSpace(storedAgent) == agentName {
				model = strings.TrimSpace(storedModel)
			}
		}
	}
	if model == "" {
		model = resolveModel("", cfg.DefaultModel, agentCfg.Preset)
	}

	parallel := 0
	if v, ok := payloadInt(firstLastRun.Payload, "parallel"); ok {
		parallel = v
	}
	if flag := cmd.Flags().Lookup("parallel"); flag != nil && flag.Changed {
		parallel, _ = cmd.Flags().GetInt("parallel")
		if parallel < 0 {
			return batch.Request{}, MarkUsage(fmt.Errorf("parallel must be 0 or greater"))
		}
	}

	startDelay := 0
	startDelaySet := false
	if v, ok := payloadInt(firstLastRun.Payload, "start_delay"); ok {
		startDelay = v
		startDelaySet = true
	}
	if flag := cmd.Flags().Lookup("start-delay"); flag != nil && flag.Changed {
		startDelay, _ = cmd.Flags().GetInt("start-delay")
		startDelaySet = true
		if startDelay < 0 {
			return batch.Request{}, MarkUsage(fmt.Errorf("start_delay must be 0 or greater"))
		}
	}

	runIdleTimeout := 0
	runIdleTimeoutSet := false
	if v, ok := payloadInt(firstLastRun.Payload, "run_idle_timeout"); ok {
		runIdleTimeout = v
		runIdleTimeoutSet = true
	}
	if flag := cmd.Flags().Lookup("run-idle-timeout"); flag != nil && flag.Changed {
		runIdleTimeout, _ = cmd.Flags().GetInt("run-idle-timeout")
		runIdleTimeoutSet = true
		if runIdleTimeout < 0 {
			return batch.Request{}, MarkUsage(fmt.Errorf("run_idle_timeout must be 0 or greater"))
		}
	}

	retries := -1
	if v, ok := payloadInt(firstLastRun.Payload, "retries"); ok {
		retries = v
	}
	if flag := cmd.Flags().Lookup("retries"); flag != nil && flag.Changed {
		retries, _ = cmd.Flags().GetInt("retries")
		if retries < 0 {
			return batch.Request{}, MarkUsage(fmt.Errorf("retries must be 0 or greater"))
		}
	}

	sandboxMode := ""
	if v, ok := payloadString(firstLastRun.Payload, "sandbox"); ok {
		sandboxMode = strings.TrimSpace(v)
	}
	if flag := cmd.Flags().Lookup("sandbox"); flag != nil && flag.Changed {
		sandboxMode, _ = cmd.Flags().GetString("sandbox")
	}

	containerCapacity := 0
	containerCapacitySet := false
	if v, ok := payloadInt(firstLastRun.Payload, "container_capacity"); ok {
		containerCapacity = v
	}
	if v, ok := payloadBool(firstLastRun.Payload, "container_capacity_set"); ok {
		containerCapacitySet = v
	}
	if flag := cmd.Flags().Lookup("container-capacity"); flag != nil && flag.Changed {
		containerCapacity, _ = cmd.Flags().GetInt("container-capacity")
		containerCapacitySet = true
		if containerCapacity < 0 {
			return batch.Request{}, MarkUsage(fmt.Errorf("container_capacity must be 0 or greater"))
		}
	}

	maxContainers := 0
	maxContainersSet := false
	if v, ok := payloadInt(firstLastRun.Payload, "max_containers"); ok {
		maxContainers = v
	}
	if v, ok := payloadBool(firstLastRun.Payload, "max_containers_set"); ok {
		maxContainersSet = v
	}
	if flag := cmd.Flags().Lookup("max-containers"); flag != nil && flag.Changed {
		maxContainers, _ = cmd.Flags().GetInt("max-containers")
		maxContainersSet = true
		if maxContainers < 0 {
			return batch.Request{}, MarkUsage(fmt.Errorf("max_containers must be 0 or greater"))
		}
	}

	dangerouslySkipPermFlag := cmd.Flags().Lookup("dangerously-skip-permissions")
	var dangerouslySkipPerm *bool
	if dangerouslySkipPermFlag != nil && dangerouslySkipPermFlag.Changed {
		val, _ := cmd.Flags().GetBool("dangerously-skip-permissions")
		dangerouslySkipPerm = &val
	}

	baseBranch := strings.TrimSpace(baseBranches[firstIssue])
	if flag := cmd.Flags().Lookup("base-branch"); flag != nil && flag.Changed {
		baseBranch, _ = cmd.Flags().GetString("base-branch")
		baseBranch = strings.TrimSpace(baseBranch)
		for num := range baseBranches {
			baseBranches[num] = baseBranch
		}
	}
	if runID != "" {
		baseBranch = promptOnlyBaseBranch
	}
	reqIssues := issues
	if runID != "" {
		reqIssues = nil
	}

	return batch.Request{
		Issues:                     reqIssues,
		Agent:                      agentName,
		Model:                      model,
		BaseBranch:                 baseBranch,
		Mode:                       modes,
		PreviousRunIDs:             previousRunIDs,
		BaseBranches:               baseBranches,
		TaskPrompts:                taskPrompts,
		Retries:                    retries,
		Parallel:                   parallel,
		StartDelay:                 time.Duration(startDelay) * time.Second,
		StartDelaySet:              startDelaySet,
		RunIdleTimeout:             runIdleTimeout,
		RunIdleTimeoutSet:          runIdleTimeoutSet,
		Branches:                   branches,
		Sandbox:                    sandboxMode,
		RequireDockerfile:          true,
		ContainerCapacity:          containerCapacity,
		ContainerCapacitySet:       containerCapacitySet,
		MaxContainers:              maxContainers,
		MaxContainersSet:           maxContainersSet,
		DangerouslySkipPermissions: dangerouslySkipPerm,
		PromptConfig: prompt.RenderConfig{
			Branch:           promptOnlyBranch,
			TaskPrompt:       taskPromptContent,
			ReviewCommand:    reviewCommand,
			ReviewCommandSet: true,
		},
		RunID: runID,
	}, nil
}

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

// lastPromptOnlyRun returns the most recent prompt-only run event.
func lastPromptOnlyRun(eventsList []events.Event) (events.Event, bool) {
	var match events.Event
	var found bool
	for _, e := range eventsList {
		if e.Type != "run.started" && e.Type != "run.continued" {
			continue
		}
		if e.Issue != 0 {
			continue
		}
		if e.RunID == "" {
			continue
		}
		if review, _ := payloadBool(e.Payload, "review"); review {
			continue
		}
		if branch, _ := payloadString(e.Payload, "branch"); branch == "" {
			continue
		}
		match = e
		found = true
	}
	return match, found
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
