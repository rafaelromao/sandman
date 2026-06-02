package batch

import (
	"bytes"
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

func resolveRetries(req Request, cfg *config.Config) int {
	if req.Retries >= 0 {
		return req.Retries
	}
	if cfg != nil && cfg.Retries >= 0 {
		return cfg.Retries
	}
	return 0
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
	retryReset              func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error
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
	dead      bool
}

type containerAliveChecker interface {
	Alive() (bool, error)
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
		if (g.parallel <= 0 || g.active < g.parallel) && (g.delay <= 0 || !now.Before(g.nextAllowedStart)) {
			if err := ctx.Err(); err != nil {
				g.mu.Unlock()
				return err
			}
			if g.parallel > 0 {
				g.active++
			}
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
	if g.parallel <= 0 {
		if applyDelay && g.delay > 0 {
			next := time.Now().Add(g.delay)
			if next.After(g.nextAllowedStart) {
				g.nextAllowedStart = next
			}
		}
		g.mu.Unlock()
		return
	}
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
		if p.pruneDeadLocked() {
			continue
		}

		var best *pooledContainer
		var pending *pooledContainer
		for _, entry := range p.shared {
			if entry.dead || entry.startErr != nil || (p.capacity > 0 && entry.active >= p.capacity) {
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
	if entry.dead && entry.active == 0 {
		if entry.container != nil {
			_ = entry.container.Stop()
		}
		p.removeShared(entry)
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *containerPool) pruneDeadLocked() bool {
	changed := false
	kept := p.shared[:0]
	for _, entry := range p.shared {
		if entry.dead {
			if entry.active == 0 {
				changed = true
				if entry.container != nil {
					_ = entry.container.Stop()
				}
				continue
			}
			kept = append(kept, entry)
			continue
		}
		if entry.ready && !containerAlive(entry.container) {
			entry.dead = true
			changed = true
			if entry.active == 0 {
				if entry.container != nil {
					_ = entry.container.Stop()
				}
				continue
			}
		}
		kept = append(kept, entry)
	}
	if changed {
		p.shared = kept
		p.cond.Broadcast()
	}
	return changed
}

func containerAlive(container sandbox.Container) bool {
	checker, ok := container.(containerAliveChecker)
	if !ok {
		return true
	}
	alive, err := checker.Alive()
	return err == nil && alive
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
	retries := resolveRetries(req, cfg)

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

	isContainer := sandboxMode == "docker" || sandboxMode == "podman"

	parallel := req.Parallel
	if parallel < 0 {
		return nil, fmt.Errorf("parallel must be 0 or greater")
	}
	startDelay := time.Duration(cfg.StartDelay) * time.Second
	if req.StartDelaySet {
		if req.StartDelay < 0 {
			return nil, fmt.Errorf("start_delay must be 0 or greater")
		}
		startDelay = req.StartDelay
	}

	containerCapacity := 0
	maxContainers := 0
	if isContainer {
		containerCapacity = cfg.ContainerCapacity
		if req.ContainerCapacitySet {
			containerCapacity = req.ContainerCapacity
		}
		maxContainers = cfg.MaxContainers
		if req.MaxContainersSet {
			maxContainers = req.MaxContainers
		}
	}

	effectiveParallel := parallel
	if isContainer && maxContainers > 0 && containerCapacity > 0 {
		totalSlots := containerCapacity * maxContainers
		if effectiveParallel == 0 || totalSlots < effectiveParallel {
			effectiveParallel = totalSlots
		}
	}

	dependencies := make(map[int][]int, len(req.Issues))
	order := make([]int, 0, len(req.Issues))
	for _, num := range req.Issues {
		dependencies[num] = uniqueIssues(req.Dependencies[num])
		order = append(order, num)
	}
	if _, err := topologicalIssues(dependencies, order); err != nil {
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

	dangerouslySkipPermissions := req.DangerouslySkipPermissions
	if dangerouslySkipPermissions == nil {
		dangerouslySkipPermissions = &isContainer
	}

	if len(req.Issues) == 0 && (req.PromptConfig.PromptFlag != "" || req.PromptConfig.TemplateFlag != "") {
		return o.runPromptOnly(ctx, cfg, agentName, agentCfg, func() (gitIdentity, error) { return resolveGitIdentity(".") }, policy.sandboxFactory, policy.containerAlloc, req, baseBranch, startDelay, parallel, retries, *dangerouslySkipPermissions)
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

	// Start-order lock: serializes ready goroutines in spawn order when
	// effective start capacity is 1. Each goroutine receives a turn at spawn
	// time and waits for its turn before proceeding, so issue N+1 cannot start
	// before issue N has finished when only one AgentRun can run at a time.
	var turnMu sync.Mutex
	var turnCond = sync.NewCond(&turnMu)
	servingTurn := 0

	for i, num := range req.Issues {
		wg.Add(1)
		go func(idx, issueNum int, blockers []int, turn int) {
			defer wg.Done()
			defer close(completed[issueNum])

			advanceTurn := func() {
				if effectiveParallel != 1 {
					return
				}
				turnMu.Lock()
				servingTurn++
				turnCond.Broadcast()
				turnMu.Unlock()
			}

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

			if effectiveParallel == 1 {
				turnMu.Lock()
				waiting := true
				for waiting {
					if err := ctx.Err(); err != nil {
						turnMu.Unlock()
						return
					}
					if turn == servingTurn {
						waiting = false
						continue
					}
					turnCond.Wait()
				}
				turnMu.Unlock()
			}

			if err := startGate.Acquire(ctx); err != nil {
				advanceTurn()
				mu.Lock()
				results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "failure", Branch: req.Branches[issueNum]}
				statuses[issueNum] = "failure"
				failureCount++
				mu.Unlock()
				return
			}

			res, started := o.runSingle(ctx, issueNum, cfg, agentName, agentCfg, req.Continuation, req.PreviousRunID, resolveBatchGitIdentity, req.Branches, req.PromptConfig, req.OutputWriter, activeRuns, &activeMu, policy.sandboxFactory, policy.containerAlloc, baseBranch, blockers, req.Blocked[issueNum], retries, *dangerouslySkipPermissions)
			defer advanceTurn()
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
		}(i, num, dependencies[num], i)
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
	if req.ContainerCapacitySet {
		if req.ContainerCapacity < 0 {
			return nil, fmt.Errorf("container_capacity must be 0 or greater")
		}
		containerCapacity = req.ContainerCapacity
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

func (o *Orchestrator) runSingle(ctx context.Context, num int, cfg *config.Config, agentName string, agentCfg config.Agent, continuation bool, previousRunID string, resolveGitIdentity func() (gitIdentity, error), branches map[int]string, renderCfg prompt.RenderConfig, outputWriter io.Writer, activeRuns map[int]sandbox.Sandbox, activeMu *sync.Mutex, sbFactory SandboxFactory, containerAlloc containerAllocator, baseBranch string, blockers []int, externalBlockers []int, retries int, dangerouslySkipPermissions bool) (AgentRunResult, bool) {
	issue, err := o.githubClient.FetchIssue(num)
	if err != nil {
		fmt.Fprintf(o.errorLog, "error: fetch issue %d: %v\n", num, err)
		return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure"}, false
	}

	branch := branches[num]
	if branch == "" {
		branch = fmt.Sprintf("sandman/%d-%s", issue.Number, slugify(issue.Title))
	}
	if !continuation {
		if err := o.syncBaseBranch(".", baseBranch); err != nil {
			fmt.Fprintf(o.errorLog, "error: sync base branch for issue %d: %v\n", num, err)
			return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch}, false
		}
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

	blockedBy, err := o.recheckBlockedBy(ctx, append(blockers, externalBlockers...))
	if err != nil {
		fmt.Fprintf(o.errorLog, "error: recheck blockers for issue %d: %v\n", num, err)
		_ = wt.Stop()
		return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch}, false
	}
	if len(blockedBy) > 0 {
		res := AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "blocked", Branch: branch}
		o.logBlocked(num, blockedBy)
		_ = wt.Stop()
		return res, false
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

	attempts := retries + 1
	var result AgentRunResult
	logPath := filepath.Join(".", ".sandman", "logs", fmt.Sprintf("%d.log", num))
	for attempt := 0; attempt < attempts; attempt++ {
		attemptRenderCfg := renderCfg
		headSHA := ""
		if attempt > 0 {
			if head, err := currentBranchHeadFn(wt.WorkDir()); err == nil {
				headSHA = head
			}
			openPR, prLookupErr := findOpenPRByBranch(o.githubClient, branch)
			contCtxPath := filepath.Join(wt.WorkDir(), ".sandman", "continuation-context.md")
			if content, err := os.ReadFile(contCtxPath); err == nil {
				if openPR != nil {
					attemptRenderCfg.ContinuePrompt = buildPRReviewContinuationPrompt(string(content))
					attemptRenderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "continue-prompt.md")
				} else {
					attemptRenderCfg.ContinuePrompt = buildRetryPrompt(string(content))
					attemptRenderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "continue-prompt.md")
				}
			} else {
				prFound := false
				if openPR != nil {
					attemptRenderCfg.ContinuePrompt = "Continue with sandman-pr-review until the PR is merged"
					attemptRenderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "continue-prompt.md")
					prFound = true
				} else if prLookupErr == nil {
					merged, err := checkPRMergedAtHead(o.githubClient, branch, headSHA)
					if err != nil {
						fmt.Fprintf(o.errorLog, "error: check PR merged status for issue %d: %v\n", num, err)
						return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch, RetriesTotal: attempt}, false
					}
					prFound = merged
				}
				if !prFound {
					if prLookupErr != nil {
						fmt.Fprintf(o.errorLog, "error: lookup PR for issue %d: %v\n", num, prLookupErr)
						return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch, RetriesTotal: attempt}, false
					}
					if err := o.resetRetryBranch(ctx, wt, branch, baseBranch); err != nil {
						fmt.Fprintf(o.errorLog, "error: reset retry branch for issue %d: %v\n", num, err)
						return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch, RetriesTotal: attempt}, false
					}
				}
			}
			if err := logRetryMarker(logPath, attempt, retries); err != nil {
				fmt.Fprintf(o.errorLog, "error: write retry marker for issue %d: %v\n", num, err)
				return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch, RetriesTotal: attempt}, false
			}
		} else if err := logRunMarker(logPath, attempt, retries); err != nil {
			fmt.Fprintf(o.errorLog, "error: write run marker for issue %d: %v\n", num, err)
			return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure", Branch: branch, RetriesTotal: attempt}, false
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
			agentRun.dangerouslySkipPermissions = &dangerouslySkipPermissions
		}

		result = runnable.Run(ctx, o.renderer, agentCfg.Command, attemptRenderCfg)
		if result.Issue == nil {
			result.Issue = issueRef(num)
		}
		if result.IssueNumber == 0 {
			result.IssueNumber = num
		}
		result.RetriesTotal = attempt + 1
		if result.Status == "success" || parseLogForCompletion(logPath) {
			result.Status = "success"
			break
		}
		if headSHA == "" {
			if head, err := currentBranchHeadFn(wt.WorkDir()); err == nil {
				headSHA = head
			}
		}
		if headSHA != "" {
			if merged, err := checkPRMergedAtHead(o.githubClient, branch, headSHA); err == nil && merged {
				result.Status = "success"
				break
			}
		}
	}

	terminalEventType, terminalStatus := terminalRunEvent(ctx, result.Status)
	result.Status = terminalStatus

	worktreeState := "preserved"

	if o.eventLog != nil {
		retriesDone := result.RetriesTotal - 1
		if retriesDone < 0 {
			retriesDone = 0
		}
		_ = o.eventLog.Log(events.Event{
			Type:      terminalEventType,
			Timestamp: time.Now(),
			RunID:     runID,
			Issue:     num,
			IssueRef:  issueRef(num),
			Payload: map[string]any{
				"status":         terminalStatus,
				"branch":         result.Branch,
				"base_branch":    baseBranch,
				"worktree_state": worktreeState,
				"retries_total":  retries,
				"retries_done":   retriesDone,
			},
		})
	}

	return result, true
}

func (o *Orchestrator) recheckBlockedBy(ctx context.Context, blockers []int) ([]int, error) {
	blockers = uniqueIssues(blockers)
	if len(blockers) == 0 {
		return nil, nil
	}

	blockedBy := make([]int, 0, len(blockers))
	for _, blocker := range blockers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		issue, err := o.githubClient.FetchIssue(blocker)
		if err != nil {
			return nil, fmt.Errorf("fetch blocker issue %d: %w", blocker, err)
		}
		if !isClosedIssue(issue) {
			blockedBy = append(blockedBy, blocker)
		}
	}

	return blockedBy, nil
}

func (o *Orchestrator) resetRetryBranch(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
	if o.retryReset != nil {
		return o.retryReset(ctx, sb, branch, baseBranch)
	}

	var output bytes.Buffer
	command := fmt.Sprintf("git checkout -B %s %s && git reset --hard && git clean -fd", shellQuote(branch), shellQuote(baseBranch))
	if err := sb.Exec(ctx, command, &output, &output); err != nil {
		return fmt.Errorf("reset retry branch: %w\n%s", err, output.String())
	}
	return nil
}

func (o *Orchestrator) writeRetryMarker(issueNum int, branch string, attempt, retries int) error {
	logDir := filepath.Join(".", ".sandman", "logs")
	logName := fmt.Sprintf("%d.log", issueNum)
	if issueNum == 0 {
		name := strings.NewReplacer("/", "-", string(os.PathSeparator), "-", " ", "-").Replace(branch)
		if name == "" {
			name = "prompt-only"
		}
		logName = name + ".log"
	}
	return logRetryMarker(filepath.Join(logDir, logName), attempt, retries)
}

func (o *Orchestrator) runPromptOnly(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, resolveGitIdentity func() (gitIdentity, error), sbFactory SandboxFactory, containerAlloc containerAllocator, req Request, baseBranch string, startDelay time.Duration, parallel int, retries int, dangerouslySkipPermissions bool) (*Result, error) {
	_ = startDelay
	_ = parallel
	branch := promptOnlyBranch(req.PromptConfig)
	result, started := o.runPromptOnlySingle(ctx, cfg, agentName, agentCfg, resolveGitIdentity, branch, req.PromptConfig, req.OutputWriter, sbFactory, containerAlloc, baseBranch, retries, dangerouslySkipPermissions)
	if !started {
		return &Result{Runs: []AgentRunResult{result}}, fmt.Errorf("prompt-only run failed")
	}
	if result.Status != "success" {
		return &Result{Runs: []AgentRunResult{result}}, fmt.Errorf("prompt-only run failed")
	}
	return &Result{Runs: []AgentRunResult{result}}, nil
}

func (o *Orchestrator) runPromptOnlySingle(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, resolveGitIdentity func() (gitIdentity, error), branch string, renderCfg prompt.RenderConfig, outputWriter io.Writer, sbFactory SandboxFactory, containerAlloc containerAllocator, baseBranch string, retries int, dangerouslySkipPermissions bool) (AgentRunResult, bool) {
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
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)
	go func() {
		select {
		case <-ctx.Done():
		case <-shutdownDone:
			return
		}

		if p := wt.Process(); p != nil {
			p.Signal(syscall.SIGTERM)
		}
		time.Sleep(10 * time.Second)
		if p := wt.Process(); p != nil {
			p.Kill()
		}
	}()
	defer func() { delete(activeRuns, 0) }()

	factory := o.runnableFactory
	if factory == nil {
		factory = defaultRunnableFactory{}
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

	attempts := retries + 1
	var result AgentRunResult
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := o.resetRetryBranch(ctx, wt, branch, baseBranch); err != nil {
				fmt.Fprintf(o.errorLog, "error: reset retry branch for prompt-only run: %v\n", err)
				return AgentRunResult{Status: "failure", Branch: branch, RetriesTotal: attempt}, false
			}
			if err := o.writeRetryMarker(0, branch, attempt, retries); err != nil {
				fmt.Fprintf(o.errorLog, "error: write retry marker for prompt-only run: %v\n", err)
				return AgentRunResult{Status: "failure", Branch: branch, RetriesTotal: attempt}, false
			}
		} else {
			logPath := filepath.Join(".", ".sandman", "logs", fmt.Sprintf("%s.log", strings.NewReplacer("/", "-", string(os.PathSeparator), "-", " ", "-").Replace(branch)))
			if err := logRunMarker(logPath, attempt, retries); err != nil {
				fmt.Fprintf(o.errorLog, "error: write run marker for prompt-only run: %v\n", err)
				return AgentRunResult{Status: "failure", Branch: branch, RetriesTotal: attempt}, false
			}
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
			agentRun.dangerouslySkipPermissions = &dangerouslySkipPermissions
		}

		result = runnable.Run(ctx, o.renderer, agentCfg.Command, renderCfg)
		result.RetriesTotal = attempt + 1
		if result.Status == "success" {
			break
		}
	}

	terminalEventType, terminalStatus := terminalRunEvent(ctx, result.Status)
	result.Status = terminalStatus
	if o.eventLog != nil {
		retriesDone := result.RetriesTotal - 1
		if retriesDone < 0 {
			retriesDone = 0
		}
		_ = o.eventLog.Log(events.Event{Type: terminalEventType, Timestamp: time.Now(), RunID: runID, Issue: 0, IssueRef: nil, Payload: map[string]any{"status": terminalStatus, "branch": result.Branch, "base_branch": baseBranch, "worktree_state": "preserved", "retries_total": retries, "retries_done": retriesDone}})
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

func terminalRunEvent(ctx context.Context, status string) (string, string) {
	eventType := "run.finished"
	terminalStatus := status
	if terminalStatus == "" {
		terminalStatus = "failure"
	}
	if ctx.Err() != nil && terminalStatus != "success" {
		eventType = "run.cancelled"
		terminalStatus = "failure"
	}
	return eventType, terminalStatus
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
