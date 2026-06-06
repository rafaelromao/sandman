package batch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

func issueRef(num int) *int {
	n := num
	return &n
}

var branchExists = sandbox.BranchExists
var branchValidationEnabled = true

func resolveRetries(req Request, cfg *config.Config) int {
	if req.Retries >= 0 {
		return req.Retries
	}
	if cfg != nil && cfg.Retries >= 0 {
		return cfg.Retries
	}
	return 0
}

func gitTopLevel(repoPath string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func (o *Orchestrator) validateBatchBranches(req Request) error {
	if !branchValidationEnabled || req.Continuation || len(req.Issues) == 0 {
		return nil
	}

	repoRoot, err := gitTopLevel(".")
	if err != nil {
		return fmt.Errorf("resolve repo root for branch validation: %w", err)
	}

	var conflicts []string
	seenConflict := make(map[string]struct{}, len(req.Issues))
	for _, num := range req.Issues {
		issue, err := o.githubClient.FetchIssue(num)
		if err != nil {
			if o.errorLog != nil {
				fmt.Fprintf(o.errorLog, "error: fetch issue %d for branch validation: %v\n", num, err)
			}
			return fmt.Errorf("fetch issue %d for branch validation: %w", num, err)
		}

		for _, branch := range collectIssueBranches(num, issue.Title, req.Branches[num], o.eventLog) {
			if !branchExists(repoRoot, branch) {
				continue
			}
			key := fmt.Sprintf("#%d (%s)", num, branch)
			if _, ok := seenConflict[key]; ok {
				continue
			}
			seenConflict[key] = struct{}{}
			conflicts = append(conflicts, key)
		}
	}

	if len(conflicts) == 0 {
		return nil
	}

	return fmt.Errorf("refusing to start batch: branches already exist from previous runs: %s. Delete them with git branch -D <branch> or re-run with --force", strings.Join(conflicts, ", "))
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
	baseBranchSyncMu        sync.Mutex
	containerRuntimeFactory ContainerRuntimeFactory
	retryReset              func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error
	killTimeout             time.Duration
	errorLog                io.Writer

	issueCancelsMu sync.Mutex
	issueCancels   map[int]context.CancelFunc
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

// effectiveParallelCap returns the effective parallel concurrency after applying
// the container pool capacity cap. In auto mode (maxContainers == 0) the cap
// equals containerCapacity, since one auto-scaled container can run at most
// containerCapacity AgentRuns concurrently. The parallel == 0 (unlimited)
// semantics are preserved: an unlimited parallel request is never capped down
// to a finite number.
func effectiveParallelCap(parallel, containerCapacity, maxContainers int) int {
	if parallel == 0 {
		return 0
	}
	if containerCapacity <= 0 {
		return parallel
	}
	var totalSlots int
	if maxContainers == 0 {
		totalSlots = containerCapacity
	} else {
		totalSlots = containerCapacity * maxContainers
	}
	if totalSlots < parallel {
		return totalSlots
	}
	return parallel
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
	p.mu.Lock()

	for {
		if p.pruneDeadLocked() {
			continue
		}

		if best := p.pickReadyLocked(); best != nil {
			best.active++
			container := best.container
			p.mu.Unlock()
			return &containerLease{container: container, release: func() { p.releaseShared(best) }}, nil
		}

		if p.hasPendingLocked() {
			p.cond.Wait()
			continue
		}

		if p.maxContainers > 0 && len(p.shared) >= p.maxContainers {
			p.cond.Wait()
			continue
		}

		entry := &pooledContainer{active: 1}
		p.shared = append(p.shared, entry)
		p.mu.Unlock()

		container, err := p.starter.Start(p.image, p.repoPath, p.startOpts)

		p.mu.Lock()
		if err != nil {
			entry.startErr = err
			entry.active--
			if entry.active == 0 {
				p.removeShared(entry)
			}
			p.cond.Broadcast()
			p.mu.Unlock()
			return nil, err
		}
		entry.container = container
		entry.ready = true
		p.cond.Broadcast()
		p.mu.Unlock()
		return &containerLease{container: container, release: func() { p.releaseShared(entry) }}, nil
	}
}

func (p *containerPool) pickReadyLocked() *pooledContainer {
	var best *pooledContainer
	for _, entry := range p.shared {
		if !entry.ready || entry.dead || entry.startErr != nil {
			continue
		}
		if p.capacity > 0 && entry.active >= p.capacity {
			continue
		}
		if best == nil || entry.active < best.active {
			best = entry
		}
	}
	return best
}

func (p *containerPool) hasPendingLocked() bool {
	for _, entry := range p.shared {
		if !entry.ready && !entry.dead && entry.startErr == nil {
			return true
		}
	}
	return false
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
		issueCancels: make(map[int]context.CancelFunc),
	}
}

// AbortIssue cancels the context of a single in-flight AgentRun, leaving
// siblings untouched. If the issue is not currently tracked (already finished
// or never started), it returns ErrNoSuchIssue. AbortIssue is safe to call
// concurrently with RunBatch.
func (o *Orchestrator) AbortIssue(issueNumber int) error {
	o.issueCancelsMu.Lock()
	cancel, ok := o.issueCancels[issueNumber]
	o.issueCancelsMu.Unlock()
	if !ok {
		return ErrNoSuchIssue
	}
	cancel()
	return nil
}

func (o *Orchestrator) registerIssueCancel(issueNumber int, cancel context.CancelFunc) {
	o.issueCancelsMu.Lock()
	o.issueCancels[issueNumber] = cancel
	o.issueCancelsMu.Unlock()
}

func (o *Orchestrator) unregisterIssueCancel(issueNumber int) {
	o.issueCancelsMu.Lock()
	delete(o.issueCancels, issueNumber)
	o.issueCancelsMu.Unlock()
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

	containerCapacityForLog := cfg.ContainerCapacity
	if req.ContainerCapacitySet {
		containerCapacityForLog = req.ContainerCapacity
	}
	maxContainersForLog := cfg.MaxContainers
	if req.MaxContainersSet {
		maxContainersForLog = req.MaxContainers
	}

	containerCapacity := 0
	maxContainers := 0
	if isContainer {
		containerCapacity = containerCapacityForLog
		maxContainers = maxContainersForLog
		if containerCapacity < 0 {
			return nil, fmt.Errorf("container_capacity must be 0 or greater")
		}
		if maxContainers < 0 {
			return nil, fmt.Errorf("max_containers must be 0 or greater")
		}
	}

	effectiveParallel := parallel
	if isContainer {
		effectiveParallel = effectiveParallelCap(parallel, containerCapacity, maxContainers)
	}

	dependencies := make(map[int][]int, len(req.Issues))
	order := make([]int, 0, len(req.Issues))
	for _, num := range req.Issues {
		dependencies[num] = uniqueIssues(req.Dependencies[num])
		order = append(order, num)
	}
	ordered, err := topologicalIssues(dependencies, order)
	if err != nil {
		return nil, err
	}
	inputIndex := make(map[int]int, len(req.Issues))
	for idx, num := range req.Issues {
		inputIndex[num] = idx
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

	if req.Force {
		for _, num := range req.Issues {
			issue, err := o.githubClient.FetchIssue(num)
			if err != nil {
				fmt.Fprintf(o.errorLog, "error: fetch issue %d for force-clean: %v\n", num, err)
				continue
			}
			branches := collectIssueBranches(num, issue.Title, req.Branches[num], o.eventLog)
			for _, branch := range branches {
				ClearIssueArtifacts(num, branch, cfg.WorktreeDir, filepath.Join(".sandman", "logs"), o.eventLog, o.errorLog)
			}
		}
	}

	if err := o.validateBatchBranches(req); err != nil {
		return nil, err
	}

	dangerouslySkipPermissions := req.DangerouslySkipPermissions
	if dangerouslySkipPermissions == nil {
		dangerouslySkipPermissions = &isContainer
	}

	if len(req.Issues) == 0 && (req.PromptConfig.PromptFlag != "" || req.PromptConfig.TemplateFlag != "") {
		return o.runPromptOnly(ctx, cfg, agentName, agentCfg, func() (gitIdentity, error) { return resolveGitIdentity(".") }, policy.sandboxFactory, policy.containerAlloc, req, baseBranch, startDelay, parallel, retries, sandboxMode, containerCapacityForLog, req.ContainerCapacitySet, maxContainersForLog, req.MaxContainersSet, *dangerouslySkipPermissions)
	}

	startGate := newBatchStartGate(effectiveParallel, startDelay)
	var wg sync.WaitGroup
	results := make([]AgentRunResult, len(req.Issues))
	var mu sync.Mutex
	failureCount := 0
	abortedCount := 0
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
	// Skipped goroutines (e.g. blocked dependents) record their turn as
	// completed on return; advanceTurn consumes consecutive completed turns so
	// a later return never strands an earlier outstanding turn.
	var turnMu sync.Mutex
	var turnCond = sync.NewCond(&turnMu)
	servingTurn := 0
	completedTurns := make(map[int]struct{})

	for turn, num := range ordered {
		wg.Add(1)
		go func(idx, issueNum int, blockers []int, turn int) {
			defer wg.Done()
			defer close(completed[issueNum])

			issueCtx, issueCancel := context.WithCancel(ctx)
			o.registerIssueCancel(issueNum, issueCancel)
			defer o.unregisterIssueCancel(issueNum)
			defer issueCancel()

			advanceTurn := func() {
				if effectiveParallel != 1 {
					return
				}
				turnMu.Lock()
				completedTurns[turn] = struct{}{}
				for {
					if _, ok := completedTurns[servingTurn]; !ok {
						break
					}
					delete(completedTurns, servingTurn)
					servingTurn++
				}
				turnCond.Broadcast()
				turnMu.Unlock()
			}
			defer advanceTurn()

			runID := generateRunID(issueNum)

			if o.eventLog != nil && (len(blockers) > 0 || (effectiveParallel > 0 && effectiveParallel < len(req.Issues))) {
				_ = o.eventLog.Log(events.Event{
					Type:      "run.queued",
					Timestamp: time.Now(),
					RunID:     runID,
					Issue:     issueNum,
					IssueRef:  issueRef(issueNum),
					Payload:   map[string]any{"blocked_by": blockers},
				})
			}

			abortedBy := make([]int, 0, len(blockers))
			stillBlockedBy := make([]int, 0, len(blockers))
			for _, blocker := range blockers {
				if err := issueCtx.Err(); err != nil {
					<-completed[blocker]
				} else {
					select {
					case <-completed[blocker]:
					case <-issueCtx.Done():
						<-completed[blocker]
					}
				}
				mu.Lock()
				status := statuses[blocker]
				mu.Unlock()
				switch status {
				case "aborted":
					abortedBy = append(abortedBy, blocker)
				case "success":
				default:
					stillBlockedBy = append(stillBlockedBy, blocker)
				}
			}
			if len(abortedBy) > 0 {
				res := AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
				o.logAborted(issueNum, runID, abortedBy)

				mu.Lock()
				results[idx] = res
				statuses[issueNum] = res.Status
				abortedCount++
				mu.Unlock()
				return
			}
			if len(stillBlockedBy) > 0 {
				res := AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "blocked", Branch: req.Branches[issueNum]}
				o.logBlocked(issueNum, stillBlockedBy, runID)

				mu.Lock()
				results[idx] = res
				statuses[issueNum] = res.Status
				mu.Unlock()
				return
			}
			if err := issueCtx.Err(); err != nil {
				o.logAborted(issueNum, runID, nil)
				mu.Lock()
				results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
				statuses[issueNum] = "aborted"
				abortedCount++
				mu.Unlock()
				return
			}

			if effectiveParallel == 1 {
				turnMu.Lock()
				waiting := true
				for waiting {
					if err := issueCtx.Err(); err != nil {
						turnMu.Unlock()
						o.logAborted(issueNum, runID, nil)
						mu.Lock()
						results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
						statuses[issueNum] = "aborted"
						abortedCount++
						mu.Unlock()
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

			if err := startGate.Acquire(issueCtx); err != nil {
				o.logAborted(issueNum, runID, nil)
				mu.Lock()
				results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
				statuses[issueNum] = "aborted"
				abortedCount++
				mu.Unlock()
				return
			}

			renderCfg := req.PromptConfig
			if req.Continuation {
				if continuationPrompt, ok := req.ContinuePrompts[issueNum]; ok {
					renderCfg.ContinuePrompt = continuationPrompt
				}
			}
			issueBaseBranch := baseBranch
			if req.Continuation {
				if perIssueBaseBranch, ok := req.BaseBranches[issueNum]; ok && strings.TrimSpace(perIssueBaseBranch) != "" {
					issueBaseBranch = perIssueBaseBranch
				}
			}

			res, started := o.runSingle(issueCtx, issueNum, cfg, agentName, agentCfg, req.Continuation, req.PreviousRunIDs, resolveBatchGitIdentity, req.Branches, renderCfg, req.OutputWriter, activeRuns, &activeMu, policy.sandboxFactory, policy.containerAlloc, req.Force, issueBaseBranch, blockers, req.Blocked[issueNum], parallel, startDelay, retries, sandboxMode, containerCapacityForLog, req.ContainerCapacitySet, maxContainersForLog, req.MaxContainersSet, *dangerouslySkipPermissions)
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
			if res.Status == "aborted" {
				abortedCount++
			}
			mu.Unlock()
		}(inputIndex[num], num, dependencies[num], turn)
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

	if abortedCount > 0 {
		return &Result{Runs: results}, fmt.Errorf("%d of %d runs aborted: %w", abortedCount, len(req.Issues), ErrAborted)
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

	if req.RequireDockerfile {
		dockerfilePath := filepath.Join(".", ".sandman", "Dockerfile")
		if _, err := os.Stat(dockerfilePath); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf(".sandman/Dockerfile not found at %s; container mode requires a Dockerfile in the .sandman directory", dockerfilePath)
			}
			return nil, fmt.Errorf("check .sandman/Dockerfile: %w", err)
		}
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

	cleanup, err := PrepareContainerConfigMounts(".", req.RunDir, &startOpts)
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

func (o *Orchestrator) logBlocked(issueNum int, blockers []int, runID string) {
	if o.eventLog == nil {
		return
	}
	_ = o.eventLog.Log(events.Event{
		Type:      "run.blocked",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     issueNum,
		IssueRef:  issueRef(issueNum),
		Payload:   map[string]any{"blocked_by": blockers},
	})
}

func (o *Orchestrator) logAborted(issueNum int, runID string, abortedBy []int) {
	if o.eventLog == nil {
		return
	}
	payload := map[string]any{"status": "aborted"}
	if len(abortedBy) > 0 {
		payload["aborted_by"] = abortedBy
	}
	_ = o.eventLog.Log(events.Event{
		Type:      "run.aborted",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     issueNum,
		IssueRef:  issueRef(issueNum),
		Payload:   payload,
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

func (o *Orchestrator) runSingle(ctx context.Context, num int, cfg *config.Config, agentName string, agentCfg config.Agent, continuation bool, previousRunIDs map[int]string, resolveGitIdentity func() (gitIdentity, error), branches map[int]string, renderCfg prompt.RenderConfig, outputWriter io.Writer, activeRuns map[int]sandbox.Sandbox, activeMu *sync.Mutex, sbFactory SandboxFactory, containerAlloc containerAllocator, force bool, baseBranch string, blockers []int, externalBlockers []int, parallel int, startDelay time.Duration, retries int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool) (AgentRunResult, bool) {
	issue, err := o.githubClient.FetchIssue(num)
	if err != nil {
		fmt.Fprintf(o.errorLog, "error: fetch issue %d: %v\n", num, err)
		return AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "failure"}, false
	}

	branch := branches[num]
	if branch == "" {
		branch = BranchName(issue.Number, issue.Title)
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
	if setter, ok := wt.(interface{ SetForce(bool) }); ok {
		setter.SetForce(force)
	}
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
	runID := generateRunID(num)
	if len(blockedBy) > 0 {
		res := AgentRunResult{IssueNumber: num, Issue: issueRef(num), Status: "blocked", Branch: branch}
		o.logBlocked(num, blockedBy, runID)
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
			"branch":                 branch,
			"base_branch":            baseBranch,
			"prompt_source_type":     promptSourceType,
			"parallel":               parallel,
			"start_delay":            int(startDelay / time.Second),
			"retries":                retries,
			"sandbox":                sandboxMode,
			"container_capacity":     containerCapacity,
			"container_capacity_set": containerCapacitySet,
			"max_containers":         maxContainers,
			"max_containers_set":     maxContainersSet,
		}
		if continuation {
			payload["previous_run_id"] = previousRunIDs[num]
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
	logPath := filepath.Join(wt.WorkDir(), ".sandman", "logs", fmt.Sprintf("%d.log", num))
	for attempt := 0; attempt < attempts; attempt++ {
		attemptRenderCfg := renderCfg
		if attempt > 0 {
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
			if err := logRetryMarkerFn(logPath, attempt, retries); err != nil {
				if o.errorLog != nil {
					fmt.Fprintf(o.errorLog, "warning: write retry marker for issue %d: %v\n", num, err)
				}
			}
		} else if err := logRunMarkerFn(logPath, attempt, retries); err != nil {
			if o.errorLog != nil {
				fmt.Fprintf(o.errorLog, "warning: write run marker for issue %d: %v\n", num, err)
			}
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
			if !checkPRMerged(o.githubClient, branch) {
				result.Status = "failure"
				continue
			}
			result.Status = "success"
			break
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

func (o *Orchestrator) writeRetryMarker(workDir string, issueNum int, branch string, attempt, retries int) error {
	logDir := filepath.Join(workDir, ".sandman", "logs")
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

func (o *Orchestrator) runPromptOnly(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, resolveGitIdentity func() (gitIdentity, error), sbFactory SandboxFactory, containerAlloc containerAllocator, req Request, baseBranch string, startDelay time.Duration, parallel int, retries int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool) (*Result, error) {
	_ = startDelay
	_ = parallel
	branch := promptOnlyBranch(req.PromptConfig)
	result, started := o.runPromptOnlySingle(ctx, cfg, agentName, agentCfg, resolveGitIdentity, branch, req.PromptConfig, req.OutputWriter, sbFactory, containerAlloc, req.Force, baseBranch, startDelay, parallel, retries, sandboxMode, containerCapacity, containerCapacitySet, maxContainers, maxContainersSet, dangerouslySkipPermissions)
	if !started {
		return &Result{Runs: []AgentRunResult{result}}, fmt.Errorf("prompt-only run failed")
	}
	if result.Status == "aborted" {
		return &Result{Runs: []AgentRunResult{result}}, fmt.Errorf("prompt-only run aborted: %w", ErrAborted)
	}
	if result.Status != "success" {
		return &Result{Runs: []AgentRunResult{result}}, fmt.Errorf("prompt-only run failed")
	}
	return &Result{Runs: []AgentRunResult{result}}, nil
}

// runPromptOnlySingle executes a single prompt-only AgentRun.
func (o *Orchestrator) runPromptOnlySingle(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, resolveGitIdentity func() (gitIdentity, error), branch string, renderCfg prompt.RenderConfig, outputWriter io.Writer, sbFactory SandboxFactory, containerAlloc containerAllocator, force bool, baseBranch string, startDelay time.Duration, parallel int, retries int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool) (AgentRunResult, bool) {
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
	if setter, ok := wt.(interface{ SetForce(bool) }); ok {
		setter.SetForce(force)
	}
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
		payload := map[string]any{"branch": branch, "base_branch": baseBranch, "prompt_source_type": "prompt", "parallel": parallel, "start_delay": int(startDelay / time.Second), "retries": retries, "sandbox": sandboxMode, "container_capacity": containerCapacity, "container_capacity_set": containerCapacitySet, "max_containers": maxContainers, "max_containers_set": maxContainersSet}
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
			if err := o.writeRetryMarker(wt.WorkDir(), 0, branch, attempt, retries); err != nil {
				if o.errorLog != nil {
					fmt.Fprintf(o.errorLog, "warning: write retry marker for prompt-only run: %v\n", err)
				}
			}
		} else {
			logPath := filepath.Join(wt.WorkDir(), ".sandman", "logs", fmt.Sprintf("%s.log", strings.NewReplacer("/", "-", string(os.PathSeparator), "-", " ", "-").Replace(branch)))
			if err := logRunMarkerFn(logPath, attempt, retries); err != nil {
				if o.errorLog != nil {
					fmt.Fprintf(o.errorLog, "warning: write run marker for prompt-only run: %v\n", err)
				}
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
	slug := Slugify(source)
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
		eventType = "run.aborted"
		terminalStatus = "aborted"
	}
	return eventType, terminalStatus
}

func (o *Orchestrator) syncBaseBranch(repoPath, baseBranch string) error {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return nil
	}
	o.baseBranchSyncMu.Lock()
	defer o.baseBranchSyncMu.Unlock()
	syncFn := o.baseBranchSync
	if syncFn == nil {
		if o.sandboxFactory != nil {
			return nil
		}
		syncFn = sandbox.SyncBaseBranch
	}
	return syncFn(repoPath, baseBranch)
}

func Slugify(title string) string {
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

// BranchName returns the standard git branch name for an issue.
func BranchName(issueNumber int, title string) string {
	return fmt.Sprintf("sandman/%d-%s", issueNumber, Slugify(title))
}

// collectIssueBranches returns all unique branch names that should be cleaned
// for an issue. It reads the event log for previously recorded branches and
// combines them with the current title-derived branch.
func collectIssueBranches(issueNumber int, title string, recordedBranch string, eventLog events.EventLog) []string {
	seen := map[string]bool{}
	var branches []string
	add := func(b string) {
		if b != "" && !seen[b] {
			seen[b] = true
			branches = append(branches, b)
		}
	}

	// Read old branches from event log
	if eventLog != nil {
		events, err := eventLog.Read()
		if err == nil {
			for _, e := range events {
				if e.Issue == issueNumber {
					if b, ok := e.Payload["branch"].(string); ok {
						add(b)
					}
				}
			}
		}
	}

	add(recordedBranch)
	add(BranchName(issueNumber, title))
	return branches
}

// isMissingWorktreeError returns true when the git worktree remove error is
// caused by the worktree not existing — not a real failure.
func isMissingWorktreeError(err error, out []byte) bool {
	return err != nil && (bytes.Contains(out, []byte("is not a working tree")) ||
		bytes.Contains(out, []byte("could not open worktree")))
}

// isMissingBranchError returns true when the git branch -D error is caused
// by the branch not existing — not a real failure.
func isMissingBranchError(err error, out []byte) bool {
	return err != nil && bytes.Contains(out, []byte("not found"))
}

// ClearIssueArtifacts removes worktree, branch, logs, and event log entries
// for a given issue. It is idempotent — missing artifacts do not cause errors.
func ClearIssueArtifacts(issueNumber int, branch string, worktreeDir string, logDir string, eventLog events.EventLog, logWriter io.Writer) {
	wtPath := filepath.Join(worktreeDir, branch)

	// Remove worktree (may fail if already removed — idempotent)
	if out, err := exec.Command("git", "worktree", "remove", "--force", wtPath).CombinedOutput(); err != nil && !isMissingWorktreeError(err, out) {
		fmt.Fprintf(logWriter, "error: remove worktree %s for issue %d: %v: %s\n", wtPath, issueNumber, err, out)
	}
	if out, err := exec.Command("git", "worktree", "prune").CombinedOutput(); err != nil {
		fmt.Fprintf(logWriter, "error: prune worktrees for issue %d: %v: %s\n", issueNumber, err, out)
	}

	// Delete branch (may fail if already deleted — idempotent)
	if out, err := exec.Command("git", "branch", "-D", branch).CombinedOutput(); err != nil && !isMissingBranchError(err, out) {
		fmt.Fprintf(logWriter, "error: delete branch %s for issue %d: %v: %s\n", branch, issueNumber, err, out)
	}

	// Belt-and-suspenders: if the worktree directory still exists on disk
	// (e.g. a previous run crashed mid-`git worktree add` and left an orphan
	// dir that git never registered), remove it directly. Idempotent.
	if err := os.RemoveAll(wtPath); err != nil {
		fmt.Fprintf(logWriter, "error: remove worktree dir %s for issue %d: %v\n", wtPath, issueNumber, err)
	}

	// Remove log file
	if err := os.RemoveAll(filepath.Join(logDir, fmt.Sprintf("%d.log", issueNumber))); err != nil {
		fmt.Fprintf(logWriter, "error: remove log for issue %d: %v\n", issueNumber, err)
	}

	// Remove events for this issue
	if eventLog != nil {
		if err := eventLog.RemoveEventsByIssue(issueNumber); err != nil {
			fmt.Fprintf(logWriter, "error: remove events for issue %d: %v\n", issueNumber, err)
		}
	}
}

// Ensure Orchestrator implements Runner.
var _ Runner = (*Orchestrator)(nil)
