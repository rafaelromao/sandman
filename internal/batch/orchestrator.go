package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	"github.com/rafaelromao/sandman/internal/scaffold"
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
	defaultBranchSync       func(repoPath, sourceBranch string) error
	containerRuntimeFactory ContainerRuntimeFactory
	killTimeout             time.Duration
}

type containerLease struct {
	container sandbox.Container
	release   func()
}

func (l *containerLease) Release() {
	if l != nil && l.release != nil {
		l.release()
	}
}

type containerAllocator interface {
	Acquire() (*containerLease, error)
}

type pooledContainer struct {
	container sandbox.Container
	active    int
	ready     bool
	startErr  error
}

type containerPool struct {
	starter       sandbox.ContainerStarter
	image         string
	repoPath      string
	startOpts     sandbox.StartOptions
	capacity      int
	maxContainers int

	mu     sync.Mutex
	cond   *sync.Cond
	shared []*pooledContainer
}

func newContainerPool(starter sandbox.ContainerStarter, image, repoPath string, startOpts sandbox.StartOptions, capacity, maxContainers int) *containerPool {
	p := &containerPool{
		starter:       starter,
		image:         image,
		repoPath:      repoPath,
		startOpts:     startOpts,
		capacity:      capacity,
		maxContainers: maxContainers,
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *containerPool) Acquire() (*containerLease, error) {
	return p.acquireShared()
}

func (p *containerPool) acquireShared() (*containerLease, error) {
	p.mu.Lock()
	for {
		var best *pooledContainer
		var pending *pooledContainer
		for _, entry := range p.shared {
			if entry.startErr != nil || entry.active >= p.capacity {
				continue
			}
			if !entry.ready {
				if pending == nil || entry.active < pending.active {
					pending = entry
				}
				continue
			}
			if best == nil || entry.active < best.active {
				best = entry
			}
		}
		if best != nil {
			best.active++
			container := best.container
			p.mu.Unlock()
			return &containerLease{container: container, release: func() { p.releaseShared(best) }}, nil
		}
		if pending != nil {
			pending.active++
			for !pending.ready && pending.startErr == nil {
				p.cond.Wait()
			}
			if pending.startErr != nil {
				err := pending.startErr
				pending.active--
				if pending.active == 0 {
					p.removeShared(pending)
					p.cond.Broadcast()
				}
				p.mu.Unlock()
				return nil, err
			}
			container := pending.container
			p.mu.Unlock()
			return &containerLease{container: container, release: func() { p.releaseShared(pending) }}, nil
		}

		if p.maxContainers == 0 || len(p.shared) < p.maxContainers {
			entry := &pooledContainer{active: 1}
			p.shared = append(p.shared, entry)
			p.mu.Unlock()

			container, err := p.starter.Start(p.image, p.repoPath, p.startOpts)

			p.mu.Lock()
			if err != nil {
				entry.startErr = err
				p.cond.Broadcast()
				entry.active--
				if entry.active == 0 {
					p.removeShared(entry)
				}
				p.mu.Unlock()
				return nil, err
			}

			entry.container = container
			entry.ready = true
			p.cond.Broadcast()
			p.mu.Unlock()

			return &containerLease{container: container, release: func() { p.releaseShared(entry) }}, nil
		}

		p.cond.Wait()
	}
}

func (p *containerPool) releaseShared(entry *pooledContainer) {
	p.mu.Lock()
	entry.active--
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *containerPool) removeShared(entry *pooledContainer) {
	for i, candidate := range p.shared {
		if candidate == entry {
			p.shared = append(p.shared[:i], p.shared[i+1:]...)
			return
		}
	}
}

func (p *containerPool) Close() error {
	p.mu.Lock()
	containers := make([]sandbox.Container, 0, len(p.shared))
	for _, entry := range p.shared {
		if entry.container != nil {
			containers = append(containers, entry.container)
		}
	}
	p.shared = nil
	p.mu.Unlock()

	var err error
	for _, container := range containers {
		err = errors.Join(err, container.Stop())
	}
	return err
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

	resolvedMode, err := sandbox.ResolveRuntime(sandboxMode)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime: %w", err)
	}
	sandboxMode = resolvedMode

	agentCfg, err := cfg.ResolveAgentProvider(cfg.Agent)
	if err != nil {
		return nil, err
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		if agentCfg.Preset == "" {
			return nil, fmt.Errorf("model override is only supported for built-in presets")
		}
		agentCfg.Model = model
	}
	if err := sandbox.ValidateAgentConfig(cfg.Agent, agentCfg); err != nil {
		return nil, err
	}

	sbFactory := o.sandboxFactory
	if sbFactory == nil {
		switch sandboxMode {
		case "docker", "podman":
			sbFactory = SharedContainerSandboxFactory{Binary: sandboxMode, RepoPath: "."}
		default:
			sbFactory = defaultSandboxFactory{}
		}
	}

	startOpts, err := buildStartOptions(agentCfg)
	if err != nil {
		return nil, err
	}
	remoteScheme := sandbox.DetectRemoteScheme(".")
	if remoteScheme == "ssh" {
		startOpts.SSH = true
	}
	startOpts.RemoteScheme = remoteScheme

	containerFactory := o.containerRuntimeFactory
	if containerFactory == nil {
		containerFactory = defaultContainerRuntimeFactory{}
	}

	var containerAlloc containerAllocator
	var pool *containerPool
	if sandboxMode == "docker" || sandboxMode == "podman" {
		if err := scaffold.ValidateDockerfileMetadata(".", cfg.BuildTools, cfg.Agent); err != nil {
			return nil, err
		}

		containerCapacity := cfg.ContainerCapacity
		if containerCapacity == 0 {
			containerCapacity = config.DefaultContainerCapacity
		}
		if req.ContainerCapacitySet {
			containerCapacity = req.ContainerCapacity
		}
		if containerCapacity < 1 {
			return nil, fmt.Errorf("container_capacity must be at least 1")
		}

		maxContainers := cfg.MaxContainers
		if req.MaxContainersSet {
			maxContainers = req.MaxContainers
		}
		if maxContainers < 0 {
			return nil, fmt.Errorf("max_containers must be 0 or greater")
		}

		if len(startOpts.AgentConfigDirs) > 0 || len(startOpts.AgentConfigFiles) > 0 {
			mounts, cleanup, err := sandbox.ResolveConfigMounts(startOpts.AgentConfigDirs, startOpts.AgentConfigFiles)
			if err != nil {
				return nil, fmt.Errorf("resolve config mounts: %w", err)
			}
			startOpts.ConfigMounts = mounts
			defer cleanup()
		}

		starter := containerFactory.New(sandboxMode)
		image, err := starter.BuildImage(".")
		if err != nil {
			return nil, fmt.Errorf("build container image: %w", err)
		}
		pool = newContainerPool(starter, image, ".", startOpts, containerCapacity, maxContainers)
		containerAlloc = pool
		defer func() {
			if pool != nil {
				_ = pool.Close()
			}
		}()
	}

	parallel := req.Parallel
	if parallel == 0 {
		parallel = 4
	}

	dependencies := make(map[int][]int, len(req.Issues))
	for _, num := range req.Issues {
		dependencies[num] = uniqueSortedIssues(req.Dependencies[num])
	}
	if _, err := topologicalIssues(dependencies); err != nil {
		return nil, err
	}

	if len(req.Issues) > 0 && (o.sandboxFactory == nil || o.defaultBranchSync != nil) {
		syncFn := o.defaultBranchSync
		if syncFn == nil {
			syncFn = sandbox.SyncDefaultBranch
		}
		if err := syncFn(".", cfg.Git.DefaultBranch); err != nil {
			return nil, err
		}
	}

	if req.PromptConfig.PromptFile == "" {
		req.PromptConfig.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if req.PromptConfig.RenderedPromptFile == "" {
		req.PromptConfig.RenderedPromptFile = filepath.Join(".", ".sandman", "rendered-prompt.md")
	}
	if err := prompt.MaterializePromptFile(req.PromptConfig); err != nil {
		return nil, fmt.Errorf("materialize prompt template: %w", err)
	}

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	results := make([]AgentRunResult, len(req.Issues))
	var mu sync.Mutex
	failureCount := 0
	statuses := make(map[int]string, len(req.Issues))
	completed := make(map[int]chan struct{}, len(req.Issues))
	for _, num := range req.Issues {
		completed[num] = make(chan struct{})
	}

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
		go func(idx, issueNum int, blockers []int) {
			defer wg.Done()
			defer close(completed[issueNum])

			for _, blocker := range blockers {
				<-completed[blocker]
			}

			blockedBy := make([]int, 0, len(blockers))
			mu.Lock()
			for _, blocker := range blockers {
				if statuses[blocker] != "success" {
					blockedBy = append(blockedBy, blocker)
				}
			}
			mu.Unlock()
			if len(blockedBy) > 0 {
				res := AgentRunResult{IssueNumber: issueNum, Status: "blocked", Branch: req.Branches[issueNum]}
				o.logBlocked(issueNum, blockedBy)

				mu.Lock()
				results[idx] = res
				statuses[issueNum] = res.Status
				mu.Unlock()
				return
			}

			sem <- struct{}{}
			defer func() { <-sem }()

			res := o.runSingle(ctx, issueNum, cfg, agentCfg, req.Preserve, req.Debug, req.Branches, req.Interactive, req.PromptConfig, activeRuns, &activeMu, sbFactory, containerAlloc)
			mu.Lock()
			results[idx] = res
			statuses[issueNum] = res.Status
			if res.Status == "failure" {
				failureCount++
			}
			mu.Unlock()
		}(i, num, dependencies[num])
	}

	wg.Wait()

	if sandboxMode == "docker" || sandboxMode == "podman" {
		for _, result := range results {
			if strings.TrimSpace(result.Branch) == "" {
				continue
			}
			worktreePath := filepath.Join(cfg.WorktreeDir, result.Branch)
			if err := sandbox.RestoreWorktreeGitPaths(".", worktreePath); err != nil && o.eventLog != nil {
				_ = o.eventLog.Log(events.Event{
					Type:      "run.warning",
					Timestamp: time.Now(),
					Issue:     result.IssueNumber,
					Payload: map[string]any{
						"branch":  result.Branch,
						"message": err.Error(),
					},
				})
			}
		}
	}

	if failureCount > 0 {
		return &Result{Runs: results}, fmt.Errorf("%d of %d runs failed", failureCount, len(req.Issues))
	}
	return &Result{Runs: results}, nil
}

func (o *Orchestrator) logBlocked(issueNum int, blockers []int) {
	if o.eventLog == nil {
		return
	}
	_ = o.eventLog.Log(events.Event{
		Type:      "run.blocked",
		Timestamp: time.Now(),
		RunID:     generateRunID(issueNum),
		Issue:     issueNum,
		Payload:   map[string]any{"blocked_by": blockers},
	})
}

func buildStartOptions(agentCfg config.Agent) (sandbox.StartOptions, error) {
	opts := sandbox.StartOptions{}

	if uid := os.Getuid(); uid >= 0 {
		opts.UserID = fmt.Sprintf("%d", uid)
	}

	if home, err := os.UserHomeDir(); err == nil {
		gitConfig := filepath.Join(home, ".gitconfig")
		if _, err := os.Stat(gitConfig); err == nil {
			opts.GitConfigPath = gitConfig
		}

		ghConfig := filepath.Join(home, ".config", "gh")
		if _, err := os.Stat(ghConfig); err == nil {
			opts.AgentConfigDirs = append(opts.AgentConfigDirs, ghConfig)
		}
	}

	for _, dir := range agentCfg.ConfigDirs {
		expanded, err := expandPath(dir)
		if err != nil {
			return sandbox.StartOptions{}, fmt.Errorf("expand config dir %q: %w", dir, err)
		}
		if expanded != "" {
			opts.AgentConfigDirs = append(opts.AgentConfigDirs, expanded)
		}
	}

	for _, file := range agentCfg.ConfigFiles {
		expanded, err := expandPath(file)
		if err != nil {
			return sandbox.StartOptions{}, fmt.Errorf("expand config file %q: %w", file, err)
		}
		if expanded != "" {
			opts.AgentConfigFiles = append(opts.AgentConfigFiles, expanded)
		}
	}

	return opts, nil
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

func (o *Orchestrator) runSingle(ctx context.Context, num int, cfg *config.Config, agentCfg config.Agent, preserve bool, debug bool, branches map[int]string, interactive bool, renderCfg prompt.RenderConfig, activeRuns map[int]sandbox.Sandbox, activeMu *sync.Mutex, sbFactory SandboxFactory, containerAlloc containerAllocator) AgentRunResult {
	issue, err := o.githubClient.FetchIssue(num)
	if err != nil {
		return AgentRunResult{IssueNumber: num, Status: "failure"}
	}

	branch := branches[num]
	if branch == "" {
		branch = fmt.Sprintf("sandman/%d-%s", issue.Number, slugify(issue.Title))
	}
	var container sandbox.Container
	if containerAlloc != nil {
		lease, err := containerAlloc.Acquire()
		if err != nil {
			return AgentRunResult{IssueNumber: num, Status: "failure", Branch: branch}
		}
		container = lease.container
		defer lease.Release()
	}

	wt := sbFactory.NewSandbox(".", cfg.WorktreeDir, branch, cfg.Git.DefaultBranch, container)
	if err := wt.Start(); err != nil {
		return AgentRunResult{IssueNumber: num, Status: "failure", Branch: branch}
	}

	if cfg.Git.AuthorName != "" && cfg.Git.AuthorEmail != "" {
		for _, kv := range []struct{ key, value string }{
			{"user.name", cfg.Git.AuthorName},
			{"user.email", cfg.Git.AuthorEmail},
		} {
			cmd := exec.Command("git", "config", kv.key, kv.value)
			cmd.Dir = wt.WorkDir()
			if out, err := cmd.CombinedOutput(); err != nil {
				return AgentRunResult{IssueNumber: num, Status: "failure", Branch: branch, DebugInfo: fmt.Sprintf("git config %s: %v\n%s", kv.key, err, out)}
			}
		}
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
	if agentRun, ok := runnable.(*AgentRun); ok {
		agentRun.env = agentCfg.Env
		agentRun.preset = agentCfg.Preset
		agentRun.model = agentCfg.Model
		agentRun.defaultBranch = cfg.Git.DefaultBranch
	}

	runID := generateRunID(num)
	if o.eventLog != nil {
		promptSourceType := "current"
		promptSourceValue := ""
		switch {
		case renderCfg.PromptFlag != "":
			promptSourceType = "prompt"
			promptSourceValue = renderCfg.PromptFlag
		case renderCfg.TemplateFlag != "":
			promptSourceType = "template"
			promptSourceValue = renderCfg.TemplateFlag
		}

		payload := map[string]any{
			"branch":             branch,
			"prompt_source_type": promptSourceType,
		}
		if promptSourceValue != "" {
			payload["prompt_source_value"] = promptSourceValue
		}
		if len(renderCfg.PromptArgs) > 0 {
			payload["prompt_args"] = renderCfg.PromptArgs
		}
		if renderCfg.ReviewCommandSet {
			payload["review_command"] = renderCfg.ReviewCommand
		}
		if model := strings.TrimSpace(agentCfg.Model); model != "" {
			payload["model"] = model
		}
		_ = o.eventLog.Log(events.Event{
			Type:      "run.started",
			Timestamp: time.Now(),
			RunID:     runID,
			Issue:     num,
			Payload:   payload,
		})
	}

	if renderCfg.PromptFile == "" {
		renderCfg.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if renderCfg.RenderedPromptFile == "" {
		renderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "rendered-prompt.md")
	}

	result := runnable.Run(ctx, o.renderer, agentCfg.Command, interactive, renderCfg)

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
				"branch":         result.Branch,
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
