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

// resolveRunIdleTimeout picks the effective idle timeout in seconds for a
// batch. Precedence: explicit request flag > config value. A value of 0
// disables the heartbeat watchdog.
func resolveRunIdleTimeout(req Request, cfg *config.Config) int {
	if req.RunIdleTimeoutSet {
		return req.RunIdleTimeout
	}
	if cfg != nil {
		return cfg.RunIdleTimeout
	}
	return 0
}

func readTailLines(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{}
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return []string{}
	}
	if len(parts) <= n {
		return parts
	}
	return parts[len(parts)-n:]
}

// agentLogPath returns the canonical absolute log path for the given filename
// under <repoRoot>/.sandman/logs/. The repo root is resolved from the current
// working directory via filepath.Abs.
func agentLogPath(filename string) string {
	root, err := filepath.Abs(".")
	if err != nil {
		panic("agentLogPath: " + err.Error())
	}
	return filepath.Join(root, ".sandman", "logs", filename)
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
	containerRuntimeFactory ContainerRuntimeFactory
	// heartbeatTickInterval overrides the default 30s heartbeat tick for tests.
	// Zero means use the default tick interval.
	heartbeatTickInterval time.Duration
	errorLog              io.Writer

	// lookupGHToken resolves the host GitHub auth token for hydrating the
	// copied gh hosts.yml in container config snapshots. It runs once per
	// batch from resolveSandboxExecutionPolicy, *before* any runSession is
	// built, so the dependency lives on the Orchestrator (per-batch
	// lifetime) rather than on runSessionOptions (per-session lifetime).
	// NewOrchestrator initialises it to defaultLookupGHToken; tests in
	// this package assign a fake to drive token-resolution paths without
	// shelling out to `gh auth token`.
	lookupGHToken func() (string, error)

	// runSessionOpts bundles the test-injection hooks consumed by
	// runSession (the function overrides and the test-tunable killTimeout)
	// together with the shared baseBranchSyncMu mutex that gates
	// syncBaseBranch. Production code leaves the function and timeout
	// fields at their zero values; NewOrchestrator initialises the mutex.
	// Tests in this package set fields on this struct directly to drive
	// injected behaviour.
	runSessionOpts runSessionOptions

	issueCancelsMu sync.Mutex
	issueCancels   map[int]context.CancelFunc
}

// defaultLookupGHToken shells out to `gh auth token` and returns the
// trimmed token. An empty output is treated as an error so callers do not
// silently inject an empty oauth_token. The exec.ErrNotFound special-case
// for callers that want to skip token injection on minimal hosts lives
// in hydrateGHHostsFile, not here.
func defaultLookupGHToken() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("gh auth token returned empty token")
	}
	return token, nil
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
		githubClient:  githubClient,
		renderer:      renderer,
		configStore:   configStore,
		eventLog:      eventLog,
		errorLog:      os.Stderr,
		lookupGHToken: defaultLookupGHToken,
		runSessionOpts: runSessionOptions{
			baseBranchSyncMu: &sync.Mutex{},
		},
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
	// The issue may unregister between lookup and cancel; cancel is still safe.
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
	runIdleTimeout := resolveRunIdleTimeout(req, cfg)

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
			req.PromptConfig.RenderedPromptFile = filepath.Join(".", ".sandman", "handoff-prompt.md")
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
		return o.runPromptOnly(ctx, cfg, agentName, agentCfg, newBatchIdentityResolver(o, "."), policy.sandboxFactory, policy.containerAlloc, req, baseBranch, startDelay, parallel, retries, sandboxMode, containerCapacityForLog, req.ContainerCapacitySet, maxContainersForLog, req.MaxContainersSet, *dangerouslySkipPermissions)
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
	batchIdentityResolver := newBatchIdentityResolver(o, ".")

	// Graceful shutdown: on context cancel, SIGTERM all processes, wait 10s, then SIGKILL.
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)

	go func() {
		select {
		case <-ctx.Done():
		case <-shutdownDone:
			return
		}

		timeout := o.runSessionOpts.killTimeout
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
		runID := generateRunID(num)
		if o.eventLog != nil && (len(dependencies[num]) > 0 || (effectiveParallel > 0 && effectiveParallel < len(req.Issues))) {
			_ = o.eventLog.Log(events.Event{
				Type:      "run.queued",
				Timestamp: time.Now(),
				RunID:     runID,
				Issue:     num,
				IssueRef:  issueRef(num),
				Payload:   map[string]any{"blocked_by": dependencies[num]},
			})
		}
		go func(idx, issueNum int, blockers []int, turn int, runID string) {
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
				if handoffPrompt, ok := req.HandoffPrompts[issueNum]; ok {
					renderCfg.HandoffPrompt = handoffPrompt
				}
			}
			issueBaseBranch := baseBranch
			if req.Continuation {
				if perIssueBaseBranch, ok := req.BaseBranches[issueNum]; ok && strings.TrimSpace(perIssueBaseBranch) != "" {
					issueBaseBranch = perIssueBaseBranch
				}
			}

			res, started := o.runSingle(issueCtx, issueNum, cfg, agentName, agentCfg, req.Continuation, req.PreviousRunIDs, batchIdentityResolver, req.Branches, renderCfg, req.OutputWriter, activeRuns, &activeMu, policy.sandboxFactory, policy.containerAlloc, req.Force, issueBaseBranch, req.Blocked[issueNum], parallel, startDelay, retries, runIdleTimeout, sandboxMode, containerCapacityForLog, req.ContainerCapacitySet, maxContainersForLog, req.MaxContainersSet, *dangerouslySkipPermissions)
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
		}(inputIndex[num], num, dependencies[num], turn, runID)
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

	cleanup, err := PrepareContainerConfigMounts(".", req.RunDir, &startOpts, o.lookupGHToken)
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

	if preset, ok := config.BuiltInAgentPresets[agentCfg.Preset]; ok {
		expanded, err := expandPaths(preset.SnapshotExcludes, "snapshot exclude")
		if err != nil {
			return sandbox.StartOptions{}, err
		}
		opts.AgentConfigExcludes = append(opts.AgentConfigExcludes, expanded...)

		expanded, err = expandPaths(preset.LiveMounts, "live mount")
		if err != nil {
			return sandbox.StartOptions{}, err
		}
		opts.LiveMounts = append(opts.LiveMounts, expanded...)
	}

	return opts, nil
}

func expandPaths(paths []string, label string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		expanded, err := expandPath(p)
		if err != nil {
			return nil, fmt.Errorf("expand %s %q: %w", label, p, err)
		}
		if expanded != "" {
			out = append(out, expanded)
		}
	}
	return out, nil
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

// runSessionOptions bundles the test-injection hooks consumed by runSession
// (the function overrides and the test-tunable killTimeout) together with
// the shared baseBranchSyncMu mutex that gates syncBaseBranch. The function
// and timeout fields exist only so tests in this package can override
// behaviour that would otherwise touch the network, run real git, or sleep
// for the production timeout; production code leaves them at their zero
// values. The baseBranchSyncMu pointer is initialised once per Orchestrator
// in NewOrchestrator and shared across all sessions via this struct's value
// copy at construction time, so that concurrent calls to syncBaseBranch
// serialise on the same mutex. If baseBranchSyncMu is ever converted from a
// pointer to a value type, update runSingle / runPromptOnlySingle to share
// it explicitly — otherwise serialisation will silently break.
type runSessionOptions struct {
	baseBranchSync   func(repoPath, sourceBranch string) error
	baseBranchSyncMu *sync.Mutex
	retryReset       func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error
	killTimeout      time.Duration
}

// runSession owns the per-AgentRun state and lifecycle for a single issue
// (or prompt-only) execution. It is private to the orchestrator package and
// is built by runSingle / runPromptOnlySingle. A session is short-lived: it
// lives for one execute call and is discarded on return.
type runSession struct {
	o *Orchestrator

	// Inputs captured from the runSingle / runPromptOnlySingle call site.
	issueNumber                int
	cfg                        *config.Config
	agentName                  string
	agentCfg                   config.Agent
	continuation               bool
	previousRunIDs             map[int]string
	identityResolver           *gitIdentityResolver
	branches                   map[int]string
	renderCfg                  prompt.RenderConfig
	outputWriter               io.Writer
	activeRuns                 map[int]sandbox.Sandbox
	activeMu                   *sync.Mutex
	sbFactory                  SandboxFactory
	containerAlloc             containerAllocator
	force                      bool
	baseBranch                 string
	externalBlockers           []int
	parallel                   int
	startDelay                 time.Duration
	retries                    int
	runIdleTimeout             int
	sandboxMode                string
	containerCapacity          int
	containerCapacitySet       bool
	maxContainers              int
	maxContainersSet           bool
	dangerouslySkipPermissions bool

	// review, prNumber and reviewFocus mark the session as a review-agent
	// run. They are sourced from batch.Request and propagated into the
	// run.started and run.finished event payloads so the event log and
	// portal can distinguish review runs from implementation runs. They
	// are only set on prompt-only sessions; issue-driven sessions always
	// leave them at zero values.
	review      bool
	prNumber    int
	reviewFocus string

	// opts carries the test-injection hooks copied from
	// Orchestrator.runSessionOpts at session construction. Zero-valued in
	// production; populated by tests to drive the per-session behaviour.
	opts runSessionOptions
}

// applyForceAndIdentity applies s.force and the resolved git identity to wt.
// On identity failure returns (_, false) so the caller can short-circuit.
func (s *runSession) applyForceAndIdentity(wt sandbox.Sandbox, branch string) (AgentRunResult, bool) {
	o := s.o
	wt.SetForce(s.force)
	identity, err := s.identityResolver.resolve()
	if err != nil {
		fmt.Fprintf(o.errorLog, "error: resolve git identity for issue %d: %v\n", s.issueNumber, err)
		result := AgentRunResult{Status: "failure", Branch: branch}
		if s.issueNumber > 0 {
			result.IssueNumber = s.issueNumber
			result.Issue = issueRef(s.issueNumber)
		}
		return result, false
	}
	wt.SetGitIdentity(identity.Name, identity.Email)
	return AgentRunResult{}, true
}

// withHeartbeat runs fn under the run-idle-timeout watchdog when
// s.runIdleTimeout > 0; any non-success result is rewritten to "aborted".
func (s *runSession) withHeartbeat(ctx context.Context, runID string, attempt int, logPath string, wt sandbox.Sandbox, fn func() AgentRunResult) AgentRunResult {
	o := s.o
	if s.runIdleTimeout <= 0 {
		return fn()
	}
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	heartbeatDone := make(chan struct{})
	var abortedByHeartbeat bool

	heartbeat := &Heartbeat{
		LogPath:      logPath,
		IdleTimeout:  time.Duration(s.runIdleTimeout) * time.Second,
		TickInterval: o.heartbeatTickInterval,
	}
	heartbeat.OnIdle = func(idle time.Duration) {
		abortedByHeartbeat = true
		if o.eventLog != nil {
			_ = o.eventLog.Log(events.Event{
				Type:      "run.idle_timeout",
				Timestamp: time.Now(),
				RunID:     runID,
				Issue:     s.issueNumber,
				IssueRef:  issueRef(s.issueNumber),
				Payload: map[string]any{
					"issue":                s.issueNumber,
					"idle_seconds":         idle.Seconds(),
					"idle_timeout_seconds": s.runIdleTimeout,
					"attempt":              attempt + 1,
					"reason":               "run_idle_timeout",
					"last_log_lines":       readTailLines(logPath, 3),
				},
			})
		}
	}
	go func() {
		defer close(heartbeatDone)
		_ = heartbeat.Run(heartbeatCtx, func() error {
			if p := wt.Process(); p != nil {
				return p.Kill()
			}
			return nil
		})
	}()

	result := fn()
	cancelHeartbeat()
	<-heartbeatDone
	if abortedByHeartbeat && result.Status != "success" {
		result.Status = "aborted"
	}
	return result
}

// emitTerminal writes the terminal run event (run.finished or run.aborted) and
// returns the normalised status so the caller can use it without recomputing.
// No-op when the orchestrator has no event log.
func (s *runSession) emitTerminal(ctx context.Context, runID string, result AgentRunResult) string {
	o := s.o
	terminalEventType, terminalStatus := terminalRunEvent(ctx, result.Status)
	if o.eventLog == nil {
		return terminalStatus
	}
	retriesDone := result.RetriesTotal - 1
	if retriesDone < 0 {
		retriesDone = 0
	}
	event := events.Event{
		Type:      terminalEventType,
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     s.issueNumber,
		Payload: map[string]any{
			"status":         terminalStatus,
			"branch":         result.Branch,
			"base_branch":    s.baseBranch,
			"worktree_state": "preserved",
			"retries_total":  s.retries,
			"retries_done":   retriesDone,
		},
	}
	if s.issueNumber > 0 {
		event.IssueRef = issueRef(s.issueNumber)
	}
	if s.review {
		event.Payload["review"] = true
		event.Payload["pr_number"] = s.prNumber
		event.Payload["review_focus"] = s.reviewFocus
	}
	_ = o.eventLog.Log(event)
	return terminalStatus
}

// runOnce runs the retry loop for a session. mergeRequired gates the
// issue-driven flavour's extra parseLogForCompletion + checkPRMerged checks;
// prepareAttempt returns (_, &errResult) to short-circuit.
func (s *runSession) runOnce(
	ctx context.Context,
	issue *github.Issue,
	branch string,
	wt sandbox.Sandbox,
	logPath string,
	runID string,
	mergeRequired bool,
	prepareAttempt func(attempt int) (prompt.RenderConfig, *AgentRunResult),
) (AgentRunResult, bool) {
	o := s.o
	if s.renderCfg.PromptFile == "" {
		s.renderCfg.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if s.renderCfg.RenderedPromptFile == "" {
		s.renderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "rendered-prompt.md")
	}

	attempts := s.retries + 1
	var result AgentRunResult

	factory := o.runnableFactory
	if factory == nil {
		factory = defaultRunnableFactory{}
	}

	for attempt := 0; attempt < attempts; attempt++ {
		attemptRenderCfg, errResult := prepareAttempt(attempt)
		if errResult != nil {
			return *errResult, false
		}

		runnable := factory.NewRunnable(issue, branch, wt)
		if agentRun, ok := runnable.(*AgentRun); ok {
			agentRun.env = s.agentCfg.Env
			agentRun.preset = s.agentCfg.Preset
			agentRun.model = s.agentCfg.Model
			agentRun.modelProvider = s.agentCfg.ModelProvider
			agentRun.modelName = s.agentCfg.ModelName
			agentRun.opencodePermissionMode = s.agentCfg.OpencodePermissionMode
			agentRun.baseBranch = s.baseBranch
			agentRun.outputWriter = s.outputWriter
			agentRun.dangerouslySkipPermissions = &s.dangerouslySkipPermissions
		}

		result = s.withHeartbeat(ctx, runID, attempt, logPath, wt, func() AgentRunResult {
			return runnable.Run(ctx, o.renderer, s.agentCfg.Command, attemptRenderCfg)
		})
		if result.Issue == nil && s.issueNumber > 0 {
			result.Issue = issueRef(s.issueNumber)
		}
		if result.IssueNumber == 0 && s.issueNumber > 0 {
			result.IssueNumber = s.issueNumber
		}
		result.RetriesTotal = attempt + 1

		success := result.Status == "success"
		if mergeRequired {
			success = success || parseLogForCompletion(logPath)
		}
		if success {
			if mergeRequired && !checkPRMerged(o.githubClient, branch) {
				result.Status = "failure"
				continue
			}
			result.Status = "success"
			break
		}
	}

	return result, true
}

// runSingle runs a single issue-driven AgentRun. It builds a runSession and
// delegates to (*runSession).execute.
func (o *Orchestrator) runSingle(ctx context.Context, num int, cfg *config.Config, agentName string, agentCfg config.Agent, continuation bool, previousRunIDs map[int]string, identityResolver *gitIdentityResolver, branches map[int]string, renderCfg prompt.RenderConfig, outputWriter io.Writer, activeRuns map[int]sandbox.Sandbox, activeMu *sync.Mutex, sbFactory SandboxFactory, containerAlloc containerAllocator, force bool, baseBranch string, externalBlockers []int, parallel int, startDelay time.Duration, retries int, runIdleTimeout int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool) (AgentRunResult, bool) {
	s := &runSession{
		o:                          o,
		issueNumber:                num,
		cfg:                        cfg,
		agentName:                  agentName,
		agentCfg:                   agentCfg,
		continuation:               continuation,
		previousRunIDs:             previousRunIDs,
		identityResolver:           identityResolver,
		branches:                   branches,
		renderCfg:                  renderCfg,
		outputWriter:               outputWriter,
		activeRuns:                 activeRuns,
		activeMu:                   activeMu,
		sbFactory:                  sbFactory,
		containerAlloc:             containerAlloc,
		force:                      force,
		baseBranch:                 baseBranch,
		externalBlockers:           externalBlockers,
		parallel:                   parallel,
		startDelay:                 startDelay,
		retries:                    retries,
		runIdleTimeout:             runIdleTimeout,
		sandboxMode:                sandboxMode,
		containerCapacity:          containerCapacity,
		containerCapacitySet:       containerCapacitySet,
		maxContainers:              maxContainers,
		maxContainersSet:           maxContainersSet,
		dangerouslySkipPermissions: dangerouslySkipPermissions,
		opts:                       o.runSessionOpts,
	}
	return s.execute(ctx)
}

// execute runs the issue-driven AgentRun lifecycle owned by this session. It
// contains the body that previously lived in (*Orchestrator).runSingle.
func (s *runSession) execute(ctx context.Context) (AgentRunResult, bool) {
	o := s.o
	issue, err := o.githubClient.FetchIssue(s.issueNumber)
	if err != nil {
		fmt.Fprintf(o.errorLog, "error: fetch issue %d: %v\n", s.issueNumber, err)
		return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure"}, false
	}

	branch := s.branches[s.issueNumber]
	if branch == "" {
		branch = BranchName(issue.Number, issue.Title)
	}
	if !s.continuation {
		if err := o.syncBaseBranch(".", s.baseBranch); err != nil {
			fmt.Fprintf(o.errorLog, "error: sync base branch for issue %d: %v\n", s.issueNumber, err)
			return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}, false
		}
	}
	var container sandbox.Container
	if s.containerAlloc != nil {
		lease, err := s.containerAlloc.Acquire()
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: acquire container for issue %d: %v\n", s.issueNumber, err)
			return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}, false
		}
		container = lease.container
		defer lease.Release()
	}

	wt := s.sbFactory.NewSandbox(".", s.cfg.WorktreeDir, branch, s.baseBranch, container)
	if errResult, ok := s.applyForceAndIdentity(wt, branch); !ok {
		return errResult, false
	}
	if err := wt.Start(); err != nil {
		fmt.Fprintf(o.errorLog, "error: start sandbox for issue %d: %v\n", s.issueNumber, err)
		return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}, false
	}

	blockedBy, err := o.recheckBlockedBy(ctx, s.externalBlockers)
	if err != nil {
		fmt.Fprintf(o.errorLog, "error: recheck blockers for issue %d: %v\n", s.issueNumber, err)
		_ = wt.Stop()
		return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}, false
	}
	runID := generateRunID(s.issueNumber)
	if len(blockedBy) > 0 {
		res := AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "blocked", Branch: branch}
		o.logBlocked(s.issueNumber, blockedBy, runID)
		_ = wt.Stop()
		return res, false
	}

	s.activeMu.Lock()
	s.activeRuns[s.issueNumber] = wt
	s.activeMu.Unlock()
	defer func() {
		s.activeMu.Lock()
		delete(s.activeRuns, s.issueNumber)
		s.activeMu.Unlock()
	}()

	issueShutdownDone := make(chan struct{})
	defer close(issueShutdownDone)
	go func() {
		select {
		case <-ctx.Done():
		case <-issueShutdownDone:
			return
		}

		timeout := s.opts.killTimeout
		if timeout == 0 {
			timeout = 10 * time.Second
		}

		if p := wt.Process(); p != nil {
			p.Signal(syscall.SIGINT)
		}

		time.Sleep(timeout)

		if p := wt.Process(); p != nil {
			p.Kill()
		}
	}()

	if o.eventLog != nil {
		promptSourceType := "current"
		promptSourceValue := ""
		switch {
		case s.renderCfg.PromptFlag != "":
			promptSourceType = "prompt"
			promptSourceValue = s.renderCfg.PromptFlag
		case s.renderCfg.TemplateFlag != "":
			promptSourceType = "template"
			promptSourceValue = s.renderCfg.TemplateFlag
		}

		payload := map[string]any{
			"branch":                 branch,
			"base_branch":            s.baseBranch,
			"prompt_source_type":     promptSourceType,
			"parallel":               s.parallel,
			"start_delay":            int(s.startDelay / time.Second),
			"retries":                s.retries,
			"sandbox":                s.sandboxMode,
			"container_capacity":     s.containerCapacity,
			"container_capacity_set": s.containerCapacitySet,
			"max_containers":         s.maxContainers,
			"max_containers_set":     s.maxContainersSet,
		}
		if s.continuation {
			payload["previous_run_id"] = s.previousRunIDs[s.issueNumber]
		}
		if promptSourceValue != "" && !s.continuation {
			payload["prompt_source_value"] = promptSourceValue
		}
		if len(s.renderCfg.PromptArgs) > 0 {
			payload["prompt_args"] = s.renderCfg.PromptArgs
		}
		if s.renderCfg.ReviewCommandSet {
			payload["review_command"] = s.renderCfg.ReviewCommand
		}
		if s.agentName != "" {
			payload["agent"] = s.agentName
		}
		if model := strings.TrimSpace(s.agentCfg.Model); model != "" {
			payload["model"] = model
		}
		eventType := "run.started"
		if s.continuation {
			eventType = "run.continued"
		}
		_ = o.eventLog.Log(events.Event{
			Type:      eventType,
			Timestamp: time.Now(),
			RunID:     runID,
			Issue:     s.issueNumber,
			IssueRef:  issueRef(s.issueNumber),
			Payload:   payload,
		})
	}

	logPath := agentLogPath(fmt.Sprintf("%d.log", s.issueNumber))
	result, started := s.runOnce(ctx, issue, branch, wt, logPath, runID, true, func(attempt int) (prompt.RenderConfig, *AgentRunResult) {
		attemptRenderCfg := s.renderCfg
		if attempt > 0 {
			openPR, prLookupErr := findOpenPRByBranch(o.githubClient, branch)
			contCtxPath := filepath.Join(wt.WorkDir(), ".sandman", "handoff.md")
			if content, err := os.ReadFile(contCtxPath); err == nil {
				if openPR != nil {
					attemptRenderCfg.HandoffPrompt = buildPRReviewHandoffPrompt(string(content))
					attemptRenderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "handoff-prompt.md")
				} else {
					stage, contextWithoutStage := parseStage(string(content))
					if stage != "" {
						priorContext := strings.TrimSpace(stripHandoffHeader(contextWithoutStage))
						attemptRenderCfg.HandoffPrompt = buildHandoffPrompt(priorContext, stage)
					} else {
						attemptRenderCfg.HandoffPrompt = buildRetryHandoffPrompt(string(content))
					}
					attemptRenderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "handoff-prompt.md")
				}
			} else {
				prFound := false
				if openPR != nil {
					attemptRenderCfg.HandoffPrompt = "Continue with sandman-pr-review until the PR is merged"
					attemptRenderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "handoff-prompt.md")
					prFound = true
				}
				if !prFound {
					if prLookupErr != nil {
						fmt.Fprintf(o.errorLog, "error: lookup PR for issue %d: %v\n", s.issueNumber, prLookupErr)
						return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch, RetriesTotal: attempt}
					}
					if err := o.resetRetryBranch(ctx, wt, branch, s.baseBranch); err != nil {
						fmt.Fprintf(o.errorLog, "error: reset retry branch for issue %d: %v\n", s.issueNumber, err)
						return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch, RetriesTotal: attempt}
					}
				}
			}
			if err := logRetryMarkerFn(logPath, attempt, s.retries); err != nil {
				if o.errorLog != nil {
					fmt.Fprintf(o.errorLog, "warning: write retry marker for issue %d: %v\n", s.issueNumber, err)
				}
			}
		} else if err := logRunMarkerFn(logPath, attempt, s.retries); err != nil {
			if o.errorLog != nil {
				fmt.Fprintf(o.errorLog, "warning: write run marker for issue %d: %v\n", s.issueNumber, err)
			}
		}
		return attemptRenderCfg, nil
	})
	if !started {
		return result, false
	}

	result.Status = s.emitTerminal(ctx, runID, result)

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
	if o.runSessionOpts.retryReset != nil {
		return o.runSessionOpts.retryReset(ctx, sb, branch, baseBranch)
	}

	var output bytes.Buffer
	command := fmt.Sprintf("git reset --hard && git checkout -f -B %s %s && git clean -fd", shellQuote(branch), shellQuote(baseBranch))
	if err := sb.Exec(ctx, command, &output, &output); err != nil {
		return fmt.Errorf("reset retry branch: %w\n%s", err, output.String())
	}
	return nil
}

func (o *Orchestrator) runPromptOnly(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, identityResolver *gitIdentityResolver, sbFactory SandboxFactory, containerAlloc containerAllocator, req Request, baseBranch string, startDelay time.Duration, parallel int, retries int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool) (*Result, error) {
	branch := promptOnlyBranch(req.PromptConfig)
	result, started := o.runPromptOnlySingle(ctx, cfg, agentName, agentCfg, identityResolver, branch, req.PromptConfig, req.OutputWriter, sbFactory, containerAlloc, req.Force, baseBranch, startDelay, parallel, retries, sandboxMode, containerCapacity, containerCapacitySet, maxContainers, maxContainersSet, dangerouslySkipPermissions, req.Review, req.PRNumber, req.ReviewFocus)
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

// runPromptOnlySingle runs a single prompt-only AgentRun. It builds a
// runSession and delegates to (*runSession).executePromptOnly.
func (o *Orchestrator) runPromptOnlySingle(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, identityResolver *gitIdentityResolver, branch string, renderCfg prompt.RenderConfig, outputWriter io.Writer, sbFactory SandboxFactory, containerAlloc containerAllocator, force bool, baseBranch string, startDelay time.Duration, parallel int, retries int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool, review bool, prNumber int, reviewFocus string) (AgentRunResult, bool) {
	s := &runSession{
		o:                          o,
		cfg:                        cfg,
		agentName:                  agentName,
		agentCfg:                   agentCfg,
		identityResolver:           identityResolver,
		branches:                   map[int]string{0: branch},
		renderCfg:                  renderCfg,
		outputWriter:               outputWriter,
		sbFactory:                  sbFactory,
		containerAlloc:             containerAlloc,
		force:                      force,
		baseBranch:                 baseBranch,
		parallel:                   parallel,
		startDelay:                 startDelay,
		retries:                    retries,
		sandboxMode:                sandboxMode,
		containerCapacity:          containerCapacity,
		containerCapacitySet:       containerCapacitySet,
		maxContainers:              maxContainers,
		maxContainersSet:           maxContainersSet,
		dangerouslySkipPermissions: dangerouslySkipPermissions,
		review:                     review,
		prNumber:                   prNumber,
		reviewFocus:                reviewFocus,
		opts:                       o.runSessionOpts,
	}
	return s.executePromptOnly(ctx)
}

// executePromptOnly runs the prompt-only AgentRun lifecycle owned by this
// session. It contains the body that previously lived in
// (*Orchestrator).runPromptOnlySingle. Unlike execute, the prompt-only
// flavor owns its own local active-runs map because it does not share the
// per-issue active-run bookkeeping that RunBatch uses.
func (s *runSession) executePromptOnly(ctx context.Context) (AgentRunResult, bool) {
	o := s.o
	branch := s.branches[0]
	if err := o.syncBaseBranch(".", s.baseBranch); err != nil {
		fmt.Fprintf(o.errorLog, "error: sync base branch for prompt-only run: %v\n", err)
		return AgentRunResult{Status: "failure", Branch: branch}, false
	}
	var container sandbox.Container
	if s.containerAlloc != nil {
		lease, err := s.containerAlloc.Acquire()
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: acquire container for prompt-only run: %v\n", err)
			return AgentRunResult{Status: "failure", Branch: branch}, false
		}
		container = lease.container
		defer lease.Release()
	}

	wt := s.sbFactory.NewSandbox(".", s.cfg.WorktreeDir, branch, s.baseBranch, container)
	if errResult, ok := s.applyForceAndIdentity(wt, branch); !ok {
		return errResult, false
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

	runID := generateRunID(0)
	if o.eventLog != nil {
		promptSourceType := "current"
		payload := map[string]any{"branch": branch, "base_branch": s.baseBranch, "prompt_source_type": "prompt", "parallel": s.parallel, "start_delay": int(s.startDelay / time.Second), "retries": s.retries, "sandbox": s.sandboxMode, "container_capacity": s.containerCapacity, "container_capacity_set": s.containerCapacitySet, "max_containers": s.maxContainers, "max_containers_set": s.maxContainersSet}
		if s.renderCfg.PromptFlag != "" {
			promptSourceType = "prompt"
			payload["prompt_source_value"] = s.renderCfg.PromptFlag
		} else if s.renderCfg.TemplateFlag != "" {
			promptSourceType = "template"
			payload["prompt_source_value"] = s.renderCfg.TemplateFlag
		}
		payload["prompt_source_type"] = promptSourceType
		if len(s.renderCfg.PromptArgs) > 0 {
			payload["prompt_args"] = s.renderCfg.PromptArgs
		}
		if s.renderCfg.ReviewCommandSet {
			payload["review_command"] = s.renderCfg.ReviewCommand
		}
		if s.agentName != "" {
			payload["agent"] = s.agentName
		}
		if model := strings.TrimSpace(s.agentCfg.Model); model != "" {
			payload["model"] = model
		}
		if s.review {
			payload["review"] = true
			payload["pr_number"] = s.prNumber
			payload["review_focus"] = s.reviewFocus
		}
		_ = o.eventLog.Log(events.Event{Type: "run.started", Timestamp: time.Now(), RunID: runID, Issue: 0, IssueRef: nil, Payload: payload})
	}

	logPath := agentLogPath(fmt.Sprintf("%s.log", strings.NewReplacer("/", "-", string(os.PathSeparator), "-", " ", "-").Replace(branch)))
	result, started := s.runOnce(ctx, nil, branch, wt, logPath, runID, false, func(attempt int) (prompt.RenderConfig, *AgentRunResult) {
		if attempt > 0 {
			if err := o.resetRetryBranch(ctx, wt, branch, s.baseBranch); err != nil {
				fmt.Fprintf(o.errorLog, "error: reset retry branch for prompt-only run: %v\n", err)
				return prompt.RenderConfig{}, &AgentRunResult{Status: "failure", Branch: branch, RetriesTotal: attempt}
			}
			if err := logRetryMarkerFn(logPath, attempt, s.retries); err != nil {
				if o.errorLog != nil {
					fmt.Fprintf(o.errorLog, "warning: write retry marker for prompt-only run: %v\n", err)
				}
			}
		} else if err := logRunMarkerFn(logPath, attempt, s.retries); err != nil {
			if o.errorLog != nil {
				fmt.Fprintf(o.errorLog, "warning: write run marker for prompt-only run: %v\n", err)
			}
		}
		return s.renderCfg, nil
	})
	if !started {
		return result, false
	}

	result.Status = s.emitTerminal(ctx, runID, result)

	return result, true
}

func promptOnlyBranch(cfg prompt.RenderConfig) string {
	if branch := strings.TrimSpace(cfg.Branch); branch != "" {
		return branch
	}
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
	mu := o.runSessionOpts.baseBranchSyncMu
	if mu == nil {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	syncFn := o.runSessionOpts.baseBranchSync
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

// gitIdentityResolver resolves the host git identity exactly once and caches
// the result for all callers. It is created per batch by RunBatch (one
// resolver shared across every runSession in the batch) and per prompt-only
// invocation by runPromptOnly, and is owned by the session via the
// identityResolver field. A single resolver preserves the sync.Once
// semantics that the previous resolveBatchGitIdentity closure provided: the
// first concurrent caller pays the cost of running `git config`; every
// subsequent caller reads the cached value or error.
//
// Production usage:
//
//	resolver := newBatchIdentityResolver(o, ".")
//	// later, from (*runSession).applyForceAndIdentity:
//	identity, err := s.identityResolver.resolve()
//
// Test usage (no real git invocation, no worktree-config side effect):
//
//	resolver := noopIdentityResolver()
type gitIdentityResolver struct {
	repoPath    string
	once        sync.Once
	resolved    gitIdentity
	err         error
	skipResolve bool
}

// newBatchIdentityResolver returns a resolver that performs a real
// resolution unless the orchestrator is running in a mode that handles git
// identity itself (sandbox / runnable / container-runtime factories set), in
// which case the resolver is a no-op. The repoPath is the path passed to
// `git -C` for every config read and write.
func newBatchIdentityResolver(o *Orchestrator, repoPath string) *gitIdentityResolver {
	if o.sandboxFactory != nil || o.runnableFactory != nil || o.containerRuntimeFactory != nil {
		return &gitIdentityResolver{skipResolve: true}
	}
	return &gitIdentityResolver{repoPath: repoPath}
}

// newPromptOnlyIdentityResolver returns a resolver that always performs a
// real resolution. Prompt-only runs do not share the orchestrator's
// sandbox-factory short-circuit, so they always need an identity.
func newPromptOnlyIdentityResolver(repoPath string) *gitIdentityResolver {
	return &gitIdentityResolver{repoPath: repoPath}
}

// noopIdentityResolver returns a resolver that returns a zero-value
// identity with no error and never runs `git config`. Used by tests that
// inject identity resolution into runSingle / runPromptOnlySingle.
func noopIdentityResolver() *gitIdentityResolver {
	return &gitIdentityResolver{skipResolve: true}
}

// resolve returns the cached git identity, computing it on the first call
// and on every subsequent call returning the same value (or error). Safe
// for concurrent use. When skipResolve is true, returns a zero identity and
// nil error without running any external command.
func (r *gitIdentityResolver) resolve() (gitIdentity, error) {
	if r.skipResolve {
		return gitIdentity{}, nil
	}
	r.once.Do(func() {
		r.resolved, r.err = r.loadIdentity()
		if r.err != nil {
			return
		}
		if err := r.setWorktreeConfig(); err != nil {
			r.err = fmt.Errorf("enable worktree git config: %w", err)
		}
	})
	return r.resolved, r.err
}

// loadIdentity runs the full resolution cascade: home ~/.gitconfig, then
// XDG_CONFIG_HOME/git/config, then repo-local .git/config. Returns a
// descriptive error listing whichever keys are still missing.
func (r *gitIdentityResolver) loadIdentity() (gitIdentity, error) {
	home, err := os.UserHomeDir()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return gitIdentity{}, fmt.Errorf("resolve home dir for git identity: %w", err)
	}

	identity := gitIdentity{}
	if home != "" {
		identity.Name, err = r.resolveGitIdentityValue(filepath.Join(home, ".gitconfig"), "user.name")
		if err != nil {
			return gitIdentity{}, err
		}
		identity.Email, err = r.resolveGitIdentityValue(filepath.Join(home, ".gitconfig"), "user.email")
		if err != nil {
			return gitIdentity{}, err
		}

		gitConfigDir := r.hostGitConfigDir(home)
		if strings.TrimSpace(identity.Name) == "" {
			identity.Name, err = r.resolveGitIdentityValue(filepath.Join(gitConfigDir, "config"), "user.name")
			if err != nil {
				return gitIdentity{}, err
			}
		}
		if strings.TrimSpace(identity.Email) == "" {
			identity.Email, err = r.resolveGitIdentityValue(filepath.Join(gitConfigDir, "config"), "user.email")
			if err != nil {
				return gitIdentity{}, err
			}
		}
	}

	if strings.TrimSpace(identity.Name) == "" {
		identity.Name, err = r.gitConfigValue("--includes", "--local", "--get", "user.name")
		if err != nil {
			return gitIdentity{}, err
		}
	}
	if strings.TrimSpace(identity.Email) == "" {
		identity.Email, err = r.gitConfigValue("--includes", "--local", "--get", "user.email")
		if err != nil {
			return gitIdentity{}, err
		}
	}

	missing := make([]string, 0, 2)
	if strings.TrimSpace(identity.Name) == "" {
		missing = append(missing, "user.name")
	}
	if strings.TrimSpace(identity.Email) == "" {
		missing = append(missing, "user.email")
	}
	if len(missing) > 0 {
		return gitIdentity{}, fmt.Errorf("resolve git identity: missing %s; set them in ~/.gitconfig, %s, or repo-local .git/config", strings.Join(missing, " and "), filepath.Join(r.hostGitConfigDir("~"), "config"))
	}

	return identity, nil
}

// setWorktreeConfig enables extensions.worktreeConfig in the repo so the
// worktree can carry its own git config.
func (r *gitIdentityResolver) setWorktreeConfig() error {
	return r.setGitConfigValue("extensions.worktreeConfig", "true")
}

// resolveGitIdentityValue reads a single key from the given git config file.
// Returns an empty string (no error) if the file does not exist.
func (r *gitIdentityResolver) resolveGitIdentityValue(configPath, key string) (string, error) {
	if _, err := os.Stat(configPath); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat git config %q: %w", configPath, err)
	}
	return r.gitConfigValue("--includes", "--file", configPath, "--get", key)
}

// gitConfigValue runs `git -C <repoPath> config <args...>` and returns the
// trimmed stdout. Exit code 1 (key not set) is treated as an empty string
// with no error so callers can fall through to the next source.
func (r *gitIdentityResolver) gitConfigValue(args ...string) (string, error) {
	cmdArgs := append([]string{"-C", r.repoPath, "config"}, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(cmdArgs, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// setGitConfigValue runs `git -C <repoPath> config <args...>` and discards
// stdout. Any non-zero exit is returned as an error.
func (r *gitIdentityResolver) setGitConfigValue(args ...string) error {
	cmdArgs := append([]string{"-C", r.repoPath, "config"}, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(cmdArgs, " "), err, out)
	}
	return nil
}

// hostGitConfigDir returns the host git config directory: $XDG_CONFIG_HOME/git
// when set, otherwise $HOME/.config/git.
func (r *gitIdentityResolver) hostGitConfigDir(home string) string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "git")
	}
	return filepath.Join(home, ".config", "git")
}
