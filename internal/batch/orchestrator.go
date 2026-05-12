package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func generateRunID(issueNum int) string {
	return fmt.Sprintf("run-%d-%d", issueNum, time.Now().UnixNano())
}

// Orchestrator coordinates parallel AgentRun execution.
type Orchestrator struct {
	githubClient            github.Client
	renderer                prompt.Renderer
	configStore             config.Store
	eventLog                events.EventLog
	runnableFactory         RunnableFactory
	sandboxFactory          SandboxFactory
	containerRuntimeFactory ContainerRuntimeFactory
	killTimeout             time.Duration
}

// NewOrchestrator creates an Orchestrator with the given dependencies.
func NewOrchestrator(githubClient github.Client, renderer prompt.Renderer, configStore config.Store, eventLog events.EventLog) *Orchestrator {
	return &Orchestrator{
		githubClient: githubClient,
		renderer:     renderer,
		configStore:  configStore,
		eventLog:     eventLog,
	}
}

// RunBatch executes the requested AgentRuns in parallel.
func (o *Orchestrator) RunBatch(ctx context.Context, req Request) (*Result, error) {
	cfg, err := o.configStore.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	sandboxMode := req.Sandbox
	if sandboxMode == "" {
		sandboxMode = cfg.Sandbox
	}

	agentCfg, ok := cfg.AgentProviders[cfg.Agent]
	if !ok {
		return nil, fmt.Errorf("agent %q not found in config", cfg.Agent)
	}
	if err := sandbox.ValidateAgentConfig(agentCfg); err != nil {
		return nil, err
	}

	sbFactory := o.sandboxFactory
	if sbFactory == nil {
		switch sandboxMode {
		case "docker", "podman":
			if req.IsolatedContainers {
				sbFactory = ContainerSandboxFactory{Binary: sandboxMode, RepoPath: "."}
			} else {
				sbFactory = SharedContainerSandboxFactory{Binary: sandboxMode, RepoPath: "."}
			}
		default:
			sbFactory = defaultSandboxFactory{}
		}
	}

	startOpts := buildStartOptions(agentCfg)
	remoteScheme := sandbox.DetectRemoteScheme(".")
	if remoteScheme == "ssh" {
		startOpts.SSH = true
	}
	startOpts.RemoteScheme = remoteScheme

	containerStarter := o.containerRuntimeFactory
	if containerStarter == nil {
		containerStarter = defaultContainerRuntimeFactory{}
	}

	var sharedContainer sandbox.Container
	if (sandboxMode == "docker" || sandboxMode == "podman") && !req.IsolatedContainers {
		starter := containerStarter.New(sandboxMode)
		c, err := starter.Start(sandbox.DefaultContainerImage, ".", startOpts)
		if err != nil {
			return nil, fmt.Errorf("start shared container: %w", err)
		}
		sharedContainer = c
		defer sharedContainer.Stop()
	}

	parallel := req.Parallel
	if parallel == 0 {
		parallel = 4
	}

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	results := make([]AgentRunResult, len(req.Issues))
	var mu sync.Mutex
	failureCount := 0

	var activeMu sync.Mutex
	activeRuns := make(map[int]sandbox.Sandbox)

	// Graceful shutdown: on context cancel, SIGTERM all processes, wait 10s, then SIGKILL.
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)

	go func() {
		select {
		case <-ctx.Done():
		case <-shutdownDone:
			return
		}
		timeout := o.killTimeout
		if timeout == 0 {
			timeout = 10 * time.Second
		}

		activeMu.Lock()
		for _, sb := range activeRuns {
			if p := sb.Process(); p != nil {
				p.Signal(syscall.SIGTERM)
			}
		}
		activeMu.Unlock()

		time.Sleep(timeout)

		activeMu.Lock()
		for _, sb := range activeRuns {
			if p := sb.Process(); p != nil {
				p.Kill()
			}
		}
		activeMu.Unlock()
	}()

	for i, num := range req.Issues {
		wg.Add(1)
		go func(idx, issueNum int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := o.runSingle(ctx, issueNum, cfg, req.Preserve, req.Debug, req.Branches, activeRuns, &activeMu, sbFactory, sandboxMode, req.IsolatedContainers, sharedContainer, containerStarter, startOpts)
			mu.Lock()
			results[idx] = res
			if res.Status == "failure" {
				failureCount++
			}
			mu.Unlock()
		}(i, num)
	}

	wg.Wait()

	if failureCount > 0 {
		return &Result{Runs: results}, fmt.Errorf("%d of %d runs failed", failureCount, len(req.Issues))
	}
	return &Result{Runs: results}, nil
}

func buildStartOptions(agentCfg config.Agent) sandbox.StartOptions {
	opts := sandbox.StartOptions{}

	if uid := os.Getuid(); uid >= 0 {
		opts.UserID = fmt.Sprintf("%d", uid)
	}

	if home, err := os.UserHomeDir(); err == nil {
		opts.GitConfigPath = filepath.Join(home, ".gitconfig")
	}

	for _, dir := range agentCfg.ConfigDirs {
		expanded, _ := expandPath(dir)
		if expanded != "" {
			opts.AgentConfigDirs = append(opts.AgentConfigDirs, expanded)
		}
	}

	return opts
}

func expandPath(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, path[1:]), nil
}

func (o *Orchestrator) runSingle(ctx context.Context, num int, cfg *config.Config, preserve bool, debug bool, branches map[int]string, activeRuns map[int]sandbox.Sandbox, activeMu *sync.Mutex, sbFactory SandboxFactory, sandboxMode string, isolated bool, sharedContainer sandbox.Container, containerStarter ContainerRuntimeFactory, startOpts sandbox.StartOptions) AgentRunResult {
	issue, err := o.githubClient.FetchIssue(num)
	if err != nil {
		return AgentRunResult{IssueNumber: num, Status: "failure"}
	}

	branch := branches[num]
	if branch == "" {
		branch = fmt.Sprintf("sandman/%d-%s", issue.Number, slugify(issue.Title))
	}
	// TODO: detect repo root instead of hardcoding "."
	var container sandbox.Container
	if (sandboxMode == "docker" || sandboxMode == "podman") && isolated {
		starter := containerStarter.New(sandboxMode)
		c, err := starter.Start(sandbox.DefaultContainerImage, ".", startOpts)
		if err != nil {
			return AgentRunResult{IssueNumber: num, Status: "failure", Branch: branch}
		}
		container = c
		defer container.Stop()
	} else {
		container = sharedContainer
	}

	wt := sbFactory.NewSandbox(".", cfg.WorktreeDir, branch, cfg.Git.DefaultBranch, container)
	if err := wt.Start(); err != nil {
		return AgentRunResult{IssueNumber: num, Status: "failure", Branch: branch}
	}

	activeMu.Lock()
	activeRuns[num] = wt
	activeMu.Unlock()
	defer func() {
		activeMu.Lock()
		delete(activeRuns, num)
		activeMu.Unlock()
	}()

	factory := o.runnableFactory
	if factory == nil {
		factory = defaultRunnableFactory{}
	}
	runnable := factory.NewRunnable(issue, branch, wt)

	agentCfg, ok := cfg.AgentProviders[cfg.Agent]
	if !ok {
		return AgentRunResult{IssueNumber: num, Status: "failure", Branch: branch}
	}

	runID := generateRunID(num)
	if o.eventLog != nil {
		_ = o.eventLog.Log(events.Event{
			Type:      "run.started",
			Timestamp: time.Now(),
			RunID:     runID,
			Issue:     num,
			Payload:   map[string]any{"branch": branch},
		})
	}

	result := runnable.Run(ctx, o.renderer, agentCfg.Command, o.githubClient, cfg.Git.DefaultBranch)

	worktreeState := "deleted"
	if result.Status == "failure" || preserve {
		worktreeState = "preserved"
	}

	if o.eventLog != nil {
		_ = o.eventLog.Log(events.Event{
			Type:      "run.finished",
			Timestamp: time.Now(),
			RunID:     runID,
			Issue:     num,
			Payload: map[string]any{
				"status":         result.Status,
				"pr_url":         result.PRURL,
				"worktree_state": worktreeState,
			},
		})
	}

	if debug && result.Status == "failure" {
		result.DebugInfo = fmt.Sprintf("Debug: worktree preserved at %s\nRun: cd %s && sh\n", wt.WorkDir(), wt.WorkDir())
	}

	if ctx.Err() == nil && result.Status != "failure" && !preserve {
		if err := wt.Stop(); err != nil && o.eventLog != nil {
			_ = o.eventLog.Log(events.Event{
				Type:      "run.warning",
				Timestamp: time.Now(),
				RunID:     runID,
				Issue:     num,
				Payload:   map[string]any{"message": err.Error()},
			})
		}
	}
	return result
}

func slugify(title string) string {
	var result []rune
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			result = append(result, r)
		} else if r == ' ' || r == '-' || r == '_' {
			if len(result) > 0 && result[len(result)-1] != '-' {
				result = append(result, '-')
			}
		}
	}
	if len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return string(result)
}

// Ensure Orchestrator implements Runner.
var _ Runner = (*Orchestrator)(nil)
