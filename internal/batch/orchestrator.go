package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/rafaelromao/sandman/internal/scaffold"
)

func generateRunID(issueNum int) string {
	return fmt.Sprintf("run-%d-%d", issueNum, time.Now().UnixNano())
}

func issueRef(num int) *int {
	n := num
	return &n
}

// Orchestrator coordinates parallel AgentRun execution.
type Orchestrator struct {
	githubClient            github.Client
	renderer                prompt.Renderer
	configStore             config.Store
	eventLog                events.EventLog
	runnableFactory         RunnableFactory
	sandboxFactory          SandboxFactory
	baseBranchSync          func(repoPath, sourceBranch string) error
	containerRuntimeFactory ContainerRuntimeFactory
	killTimeout             time.Duration
	errorLog                io.Writer
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

type sandboxExecutionPolicy struct {
	mode           string
	sandboxFactory SandboxFactory
	containerAlloc containerAllocator
	close          func()
}

func (p *sandboxExecutionPolicy) Close() {
	if p != nil && p.close != nil {
		p.close()
	}
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

type batchStartGate struct {
	mu               sync.Mutex
	parallel         int
	delay            time.Duration
	active           int
	nextAllowedStart time.Time
}

func newBatchStartGate(parallel int, delay time.Duration) *batchStartGate {
	if parallel < 1 {
		parallel = 1
	}
	return &batchStartGate{parallel: parallel, delay: delay}
}

func (g *batchStartGate) Acquire(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return err
		}
		now := time.Now()
		if g.active < g.parallel && (g.delay <= 0 || !now.Before(g.nextAllowedStart)) {
			if err := ctx.Err(); err != nil {
				g.mu.Unlock()
				return err
			}
			g.active++
			g.mu.Unlock()
			return nil
		}

		wait := 10 * time.Millisecond
		if g.delay > 0 && now.Before(g.nextAllowedStart) {
			wait = time.Until(g.nextAllowedStart)
		}
		g.mu.Unlock()

		if wait <= 0 {
			wait = 10 * time.Millisecond
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (g *batchStartGate) Release() { g.release(true) }

func (g *batchStartGate) ReleaseWithoutDelay() { g.release(false) }

func (g *batchStartGate) release(applyDelay bool) {
	g.mu.Lock()
	if applyDelay && g.delay > 0 {
		next := time.Now().Add(g.delay)
		if next.After(g.nextAllowedStart) {
			g.nextAllowedStart = next
		}
	}
	g.active--
	g.mu.Unlock()
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
		errorLog:     os.Stderr,
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

	agentName := strings.TrimSpace(req.Agent)
	if agentName == "" {
		agentName = cfg.DefaultAgent
	}
	if agentName == "" {
		agentName = cfg.Agent
	}
	agentCfg, err := cfg.ResolveAgentProvider(agentName)
	if err != nil {
		return nil, err
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		if agentCfg.Preset == "" {
			return nil, fmt.Errorf("model override is only supported for built-in presets")
		}
		agentCfg.Model = model
	}
	if agentCfg.Preset == "pi" {
		provider, modelName, err := config.SplitPiModel(agentCfg.Model)
		if err != nil {
			return nil, err
		}
		agentCfg.ModelProvider = provider
		agentCfg.ModelName = modelName
	}
	if err := sandbox.ValidateAgentConfig(agentName, agentCfg); err != nil {
		return nil, err
	}

	baseBranch := strings.TrimSpace(req.BaseBranch)
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(cfg.Git.BaseBranch)
	}
	if baseBranch == "" {
		baseBranch = "main"
	}

	policy, err := o.resolveSandboxExecutionPolicy(cfg, agentCfg, req, sandboxMode)
	if err != nil {
		return nil, err
	}
	defer policy.Close()

	parallel := req.Parallel
	if parallel == 0 {
		parallel = 4
	}
	startDelay := time.Duration(cfg.StartDelay) * time.Second
	if req.StartDelaySet {
		if req.StartDelay < 0 {
			return nil, fmt.Errorf("start_delay must be 0 or greater")
		}
		startDelay = req.StartDelay
	}

	dependencies := make(map[int][]int, len(req.Issues))
	for _, num := range req.Issues {
		dependencies[num] = uniqueSortedIssues(req.Dependencies[num])
	}
	if _, err := topologicalIssues(dependencies); err != nil {
		return nil, err
	}

	if !req.Continuation && req.PromptConfig.PromptFile == "" {
		req.PromptConfig.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if req.PromptConfig.RenderedPromptFile == "" {
		if req.Continuation {
			req.PromptConfig.RenderedPromptFile = filepath.Join(".", ".sandman", "continue-prompt.md")
		} else {
			req.PromptConfig.RenderedPromptFile = filepath.Join(".", ".sandman", "rendered-prompt.md")
		}
	}
	if !req.Continuation {
		if err := prompt.MaterializePromptFile(req.PromptConfig); err != nil {
			return nil, fmt.Errorf("materialize prompt template: %w", err)
		}
	}
	if len(req.Issues) == 0 && (req.PromptConfig.PromptFlag != "" || req.PromptConfig.TemplateFlag != "") {
		return o.runPromptOnly(ctx, cfg, agentName, agentCfg, func() (gitIdentity, error) { return resolveGitIdentity(".") }, policy.sandboxFactory, policy.containerAlloc, req, baseBranch, startDelay, parallel)
	}

	startGate := newBatchStartGate(parallel, startDelay)
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
	var resolveGitIdentityOnce sync.Once
	resolvedGitIdentity := gitIdentity{}
	resolveGitIdentityErr := error(nil)
	resolveBatchGitIdentity := func() (gitIdentity, error) {
		if o.sandboxFactory != nil || o.runnableFactory != nil || o.containerRuntimeFactory != nil {
			return gitIdentity{}, nil
		}
		resolveGitIdentityOnce.Do(func() {
			resolvedGitIdentity, resolveGitIdentityErr = resolveGitIdentity(".")
			if resolveGitIdentityErr != nil {
				return
			}
			if err := setGitConfigValue(".", "extensions.worktreeConfig", "true"); err != nil {
				resolveGitIdentityErr = fmt.Errorf("enable worktree git config: %w", err)
			}
		})
		return resolvedGitIdentity, resolveGitIdentityErr
	}

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
				res := AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "blocked", Branch: req.Branches[issueNum]}
				o.logBlocked(issueNum, blockedBy)

				mu.Lock()
				results[idx] = res
				statuses[issueNum] = res.Status
				mu.Unlock()
				return
			}

			if err := startGate.Acquire(ctx); err != nil {
				mu.Lock()
				results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "failure", Branch: req.Branches[issueNum]}
				statuses[issueNum] = "failure"
				failureCount++
				mu.Unlock()
				return
			}

			res, started := o.runSingle(ctx, issueNum, cfg, agentName, agentCfg, req.Continuation, req.PreviousRunID, resolveBatchGitIdentity, req.Branches, req.PromptConfig, req.OutputWriter, activeRuns, &activeMu, policy.sandboxFactory, policy.containerAlloc, baseBranch)
			if started {
				defer startGate.Release()
			} else {
				defer startGate.ReleaseWithoutDelay()
			}
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

	if policy.mode == "docker" || policy.mode == "podman" {
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
					IssueRef:  result.Issue,
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

func (o *Orchestrator) resolveSandboxExecutionPolicy(cfg *config.Config, agentCfg config.Agent, req Request, sandboxMode string) (*sandboxExecutionPolicy, error) {
	startOpts, err := buildStartOptions(agentCfg)
	if err != nil {
		return nil, err
	}
	rm := sandbox.DetectRemoteScheme(".")
	if rm == "ssh" {
		startOpts.SSH = true
	}
	startOpts.RemoteScheme = rm

	sbFactory := o.sandboxFactory
	if sbFactory == nil {
		switch sandboxMode {
		case "docker", "podman":
			sbFactory = SharedContainerSandboxFactory{Binary: sandboxMode, RepoPath: "."}
		default:
			sbFactory = defaultSandboxFactory{}
		}
	}

	if sandboxMode != "docker" && sandboxMode != "podman" {
		return &sandboxExecutionPolicy{mode: sandboxMode, sandboxFactory: sbFactory}, nil
	}

	defaultAgent := strings.TrimSpace(cfg.DefaultAgent)
	if defaultAgent == "" {
		defaultAgent = strings.TrimSpace(cfg.Agent)
	}
	if err := scaffold.ValidateDockerfileMetadata(".", cfg.BuildTools, defaultAgent); err != nil {
		return nil, err
	}

	containerCapacity := cfg.ContainerCapacity
	if containerCapacity < 0 {
		return nil, fmt.Errorf("container_capacity must be 0 or greater")
	}
	if containerCapacity == 0 {
		containerCapacity = config.DefaultContainerCapacity
	}
	if req.ContainerCapacitySet {
		if req.ContainerCapacity < 0 {
			return nil, fmt.Errorf("container_capacity must be 0 or greater")
		}
		if req.ContainerCapacity == 0 {
			containerCapacity = config.DefaultContainerCapacity
		} else {
			containerCapacity = req.ContainerCapacity
		}
	}
	if containerCapacity < 1 {
		return nil, fmt.Errorf("container_capacity must be 0 or greater")
	}

	maxContainers := cfg.MaxContainers
	if req.MaxContainersSet {
		maxContainers = req.MaxContainers
	}
	if maxContainers < 0 {
		return nil, fmt.Errorf("max_containers must be 0 or greater")
	}

	cleanup, err := PrepareContainerConfigMounts(".", &startOpts)
	if err != nil {
		return nil, fmt.Errorf("prepare container config mounts: %w", err)
	}

	containerFactory := o.containerRuntimeFactory
	if containerFactory == nil {
		containerFactory = defaultContainerRuntimeFactory{}
	}
	starter := containerFactory.New(sandboxMode)
	image, err := starter.BuildImage(".")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build container image: %w", err)
	}
	pool := newContainerPool(starter, image, ".", startOpts, containerCapacity, maxContainers)
	return &sandboxExecutionPolicy{
		mode:           sandboxMode,
		sandboxFactory: sbFactory,
		containerAlloc: pool,
		close: func() {
			_ = pool.Close()
			cleanup()
		},
	}, nil
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
		IssueRef:  issueRef(issueNum),
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

		gitConfigDir := filepath.Join(home, ".config", "git")
		if _, err := os.Stat(gitConfigDir); err == nil {
			opts.AgentConfigDirs = append(opts.AgentConfigDirs, gitConfigDir)
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

func (o *Orchestrator) runSingle(ctx context.Context, num int, cfg *config.Config, agentName string, agentCfg config.Agent, continuation bool, previousRunID string, resolveGitIdentity func() (gitIdentity, error), branches map[int]string, renderCfg prompt.RenderConfig, outputWriter io.Writer, activeRuns map[int]sandbox.Sandbox, activeMu *sync.Mutex, sbFactory SandboxFactory, containerAlloc containerAllocator, baseBranch string) (AgentRunResult, bool) {
	issue, err := o.githubClient.FetchIssue(num)
	if err != nil {
		fmt.Fprintf(o.errorLog, "error: fetch issue %d: %v\n", num, err)
		return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure"}, false
	}

	branch := branches[num]
	if branch == "" {
		branch = fmt.Sprintf("sandman/%d-%s", issue.Number, slugify(issue.Title))
	}
	if err := o.syncBaseBranch(".", baseBranch); err != nil {
		fmt.Fprintf(o.errorLog, "error: sync base branch for issue %d: %v\n", num, err)
		return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch}, false
	}
	var container sandbox.Container
	if containerAlloc != nil {
		lease, err := containerAlloc.Acquire()
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: acquire container for issue %d: %v\n", num, err)
			return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch}, false
		}
		container = lease.container
		defer lease.Release()
	}

	wt := sbFactory.NewSandbox(".", cfg.WorktreeDir, branch, baseBranch, container)
	if setter, ok := wt.(interface{ SetGitIdentity(string, string) }); ok {
		identity, err := resolveGitIdentity()
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: resolve git identity for issue %d: %v\n", num, err)
			return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch}, false
		}
		setter.SetGitIdentity(identity.Name, identity.Email)
	}
	if err := wt.Start(); err != nil {
		fmt.Fprintf(o.errorLog, "error: start sandbox for issue %d: %v\n", num, err)
		return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch}, false
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
		agentRun.modelProvider = agentCfg.ModelProvider
		agentRun.modelName = agentCfg.ModelName
		agentRun.baseBranch = baseBranch
		agentRun.outputWriter = outputWriter
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
			"base_branch":        baseBranch,
			"prompt_source_type": promptSourceType,
		}
		if continuation {
			payload = map[string]any{"branch": branch, "base_branch": baseBranch, "previous_run_id": previousRunID}
		}
		if promptSourceValue != "" && !continuation {
			payload["prompt_source_value"] = promptSourceValue
		}
		if len(renderCfg.PromptArgs) > 0 {
			payload["prompt_args"] = renderCfg.PromptArgs
		}
		if renderCfg.ReviewCommandSet {
			payload["review_command"] = renderCfg.ReviewCommand
		}
		if agentName != "" {
			payload["agent"] = agentName
		}
		if model := strings.TrimSpace(agentCfg.Model); model != "" {
			payload["model"] = model
		}
		eventType := "run.started"
		if continuation {
			eventType = "run.continued"
		}
		_ = o.eventLog.Log(events.Event{
			Type:      eventType,
			Timestamp: time.Now(),
			RunID:     runID,
			Issue:     num,
			IssueRef:  issueRef(num),
			Payload:   payload,
		})
	}

	if renderCfg.PromptFile == "" {
		renderCfg.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if renderCfg.RenderedPromptFile == "" {
		renderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "rendered-prompt.md")
	}

	result := runnable.Run(ctx, o.renderer, agentCfg.Command, renderCfg)
	if result.Issue == nil {
		result.Issue = issueRef(num)
	}
	if result.IssueNumber == 0 {
		result.IssueNumber = num
	}

	worktreeState := "preserved"

	if o.eventLog != nil {
		_ = o.eventLog.Log(events.Event{
			Type:      "run.finished",
			Timestamp: time.Now(),
			RunID:     runID,
			Issue:     num,
			IssueRef:  issueRef(num),
			Payload: map[string]any{
				"status":         result.Status,
				"branch":         result.Branch,
				"base_branch":    baseBranch,
				"worktree_state": worktreeState,
			},
		})
	}

	return result, true
}

func (o *Orchestrator) runPromptOnly(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, resolveGitIdentity func() (gitIdentity, error), sbFactory SandboxFactory, containerAlloc containerAllocator, req Request, baseBranch string, startDelay time.Duration, parallel int) (*Result, error) {
	_ = startDelay
	_ = parallel
	branch := promptOnlyBranch(req.PromptConfig)
	result, started := o.runPromptOnlySingle(ctx, cfg, agentName, agentCfg, resolveGitIdentity, branch, req.PromptConfig, req.OutputWriter, sbFactory, containerAlloc, baseBranch)
	if !started {
		return &Result{Runs: []AgentRunResult{result}}, fmt.Errorf("prompt-only run failed")
	}
	return &Result{Runs: []AgentRunResult{result}}, nil
}

func (o *Orchestrator) runPromptOnlySingle(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, resolveGitIdentity func() (gitIdentity, error), branch string, renderCfg prompt.RenderConfig, outputWriter io.Writer, sbFactory SandboxFactory, containerAlloc containerAllocator, baseBranch string) (AgentRunResult, bool) {
	if err := o.syncBaseBranch(".", baseBranch); err != nil {
		fmt.Fprintf(o.errorLog, "error: sync base branch for prompt-only run: %v\n", err)
		return AgentRunResult{Status: "failure", Branch: branch}, false
	}
	var container sandbox.Container
	if containerAlloc != nil {
		lease, err := containerAlloc.Acquire()
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: acquire container for prompt-only run: %v\n", err)
			return AgentRunResult{Status: "failure", Branch: branch}, false
		}
		container = lease.container
		defer lease.Release()
	}

	wt := sbFactory.NewSandbox(".", cfg.WorktreeDir, branch, baseBranch, container)
	if setter, ok := wt.(interface{ SetGitIdentity(string, string) }); ok {
		identity, err := resolveGitIdentity()
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: resolve git identity for prompt-only run: %v\n", err)
			return AgentRunResult{Status: "failure", Branch: branch}, false
		}
		setter.SetGitIdentity(identity.Name, identity.Email)
	}
	if err := wt.Start(); err != nil {
		fmt.Fprintf(o.errorLog, "error: start sandbox for prompt-only run: %v\n", err)
		return AgentRunResult{Status: "failure", Branch: branch}, false
	}

	activeRuns := map[int]sandbox.Sandbox{0: wt}
	defer func() { delete(activeRuns, 0) }()

	factory := o.runnableFactory
	if factory == nil {
		factory = defaultRunnableFactory{}
	}
	runnable := factory.NewRunnable(nil, branch, wt)
	if agentRun, ok := runnable.(*AgentRun); ok {
		agentRun.env = agentCfg.Env
		agentRun.preset = agentCfg.Preset
		agentRun.model = agentCfg.Model
		agentRun.modelProvider = agentCfg.ModelProvider
		agentRun.modelName = agentCfg.ModelName
		agentRun.baseBranch = baseBranch
		agentRun.outputWriter = outputWriter
	}

	runID := generateRunID(0)
	if o.eventLog != nil {
		promptSourceType := "current"
		payload := map[string]any{"branch": branch, "base_branch": baseBranch, "prompt_source_type": "prompt"}
		if renderCfg.PromptFlag != "" {
			promptSourceType = "prompt"
			payload["prompt_source_value"] = renderCfg.PromptFlag
		} else if renderCfg.TemplateFlag != "" {
			promptSourceType = "template"
			payload["prompt_source_value"] = renderCfg.TemplateFlag
		}
		payload["prompt_source_type"] = promptSourceType
		if len(renderCfg.PromptArgs) > 0 {
			payload["prompt_args"] = renderCfg.PromptArgs
		}
		if renderCfg.ReviewCommandSet {
			payload["review_command"] = renderCfg.ReviewCommand
		}
		if agentName != "" {
			payload["agent"] = agentName
		}
		if model := strings.TrimSpace(agentCfg.Model); model != "" {
			payload["model"] = model
		}
		_ = o.eventLog.Log(events.Event{Type: "run.started", Timestamp: time.Now(), RunID: runID, Issue: 0, IssueRef: nil, Payload: payload})
	}

	if renderCfg.PromptFile == "" {
		renderCfg.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if renderCfg.RenderedPromptFile == "" {
		renderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "rendered-prompt.md")
	}

	result := runnable.Run(ctx, o.renderer, agentCfg.Command, renderCfg)
	if o.eventLog != nil {
		_ = o.eventLog.Log(events.Event{Type: "run.finished", Timestamp: time.Now(), RunID: runID, Issue: 0, IssueRef: nil, Payload: map[string]any{"status": result.Status, "branch": result.Branch, "base_branch": baseBranch, "worktree_state": "preserved"}})
	}

	return result, true
}

func promptOnlyBranch(cfg prompt.RenderConfig) string {
	source := strings.TrimSpace(cfg.PromptFlag)
	if source == "" && cfg.TemplateFlag != "" {
		if data, err := os.ReadFile(cfg.TemplateFlag); err == nil {
			source = string(data)
		} else {
			source = filepath.Base(cfg.TemplateFlag)
		}
	}
	slug := slugify(source)
	if slug == "" {
		slug = "prompt-only"
	}
	return fmt.Sprintf("sandman/%s-%d", slug, time.Now().UnixNano())
}

func (o *Orchestrator) syncBaseBranch(repoPath, baseBranch string) error {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return nil
	}
	syncFn := o.baseBranchSync
	if syncFn == nil {
		if o.sandboxFactory != nil {
			return nil
		}
		syncFn = sandbox.SyncBaseBranch
	}
	return syncFn(repoPath, baseBranch)
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
