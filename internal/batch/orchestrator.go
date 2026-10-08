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
	"time"
	"unicode"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/runid"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/scaffold"
	"github.com/rafaelromao/sandman/internal/shellenv"
)

// buildRunID returns the per-row RunID for an issue-driven AgentRun.
// Both the run.queued placeholder (emitted in RunBatch's goroutine launch)
// and the run.started / run.continued events emitted inside
// (*runSession).execute go through this helper so every per-row RunID
// shares the batch's (ts, shortid) prefix.
func buildRunID(num int, ts, shortid string) string {
	return runid.NewRunID(runid.KindIssue, fmt.Sprintf("%d", num), ts, shortid)
}

// BatchIDForIssue returns the per-batch directory name (the public BatchId)
// for an issue-driven session, derived from the batch's first issue number,
// total issue count, and the (ts, shortid) pair.
//
// The returned value is the canonical public BatchId:
//   - single issue (n==1): "<ts>-<sid>-<num>" (no +N suffix)
//   - multi-issue (n>=2):   "<ts>-<sid>-<firstIssue>+<additionalCount>"
//
// This delegates the +N/omission rule to runid.NewBatchID(KindIssue, n, ...).
func BatchIDForIssue(firstIssueNum, n int, ts, shortid string) string {
	if shortid == "" && ts == "" {
		return ""
	}
	return runid.NewBatchID(runid.KindIssue, n, fmt.Sprintf("%d", firstIssueNum), ts, shortid)
}

func issueBatchIDForRequest(req Request) string {
	if runDir := strings.TrimSpace(req.RunDir); runDir != "" {
		return filepath.Base(runDir)
	}
	if len(req.Issues) > 0 {
		return BatchIDForIssue(req.Issues[0], len(req.Issues), req.RunTS, req.RunShortID)
	}
	if req.RunShortID == "" && req.RunTS == "" {
		return ""
	}
	return req.RunTS + "-" + req.RunShortID
}

// batchIDForPromptOnly returns the per-row batch directory name for a
// prompt-only session. When runDir is non-empty, the batch ID is
// derived from the directory two levels above runDir (so
// `<batchesDir>/<batchID>/runs/<runID>` round-trips). This pins the
// orchestrator ↔ daemon agreement: the review daemon's `Request.RunDir`
// is the authoritative path the daemon's `prepareReviewRun` will read
// `decision.md` from, so
// the orchestrator must place `agentRun.runFolder` at the same
// path. Otherwise the reviewer bot writes `decision.md` to a path
// the daemon never reads and the review comment is silently dropped
// (issue discovered on PR #1875: per-row RunID
// `<ts>-<sid>-<linkedIssue>-PR<pr>` diverged from the legacy batch
// dir `<ts>-<sid>-PR<pr>` that `prepareReviewRun` mints).
//
// When runDir is empty, fall back to the user-provided runID; when
// that is also empty, route the (ts, shortid) pair through the
// shared identity engine so the output is `<ts>-<sid>-prompt`.
// Routing through `runid.NewBatchID` keeps the prompt-only BatchId
// shape aligned with every other prompt-only mint surface (cleanup
// of #1943, see issue #2042).
func batchIDForPromptOnly(ts, shortid, userRunID, runDir string) string {
	if runDir != "" {
		// runDir = <batchesDir>/<batchID>/runs/<runID>; the batchID
		// is two levels above runDir's last segment. Defensively
		// guard against a short path (no /runs/<runID>) by returning
		// the empty string so the caller falls back to the historical
		// derivation rather than panicking.
		parent := filepath.Dir(runDir)
		if filepath.Base(parent) != "runs" {
			return ""
		}
		return filepath.Base(filepath.Dir(parent))
	}
	if userRunID != "" {
		return userRunID
	}
	if shortid == "" && ts == "" {
		return ""
	}
	return runid.NewBatchID(runid.KindPromptOnly, 1, "", ts, shortid)
}

func issueRef(num int) *int {
	n := num
	return &n
}

var branchExists = sandbox.BranchExists
var branchValidationEnabled = true

const (
	usageLimitPollInterval = 10 * time.Minute
	usageLimitRetryWindow  = 5 * time.Hour
)

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

// resolveReviewTimeout picks the effective delegated review response budget in
// seconds. Precedence: explicit request override > repository config > default.
func resolveReviewTimeout(req Request, cfg *config.Config) (int, error) {
	value := config.DefaultReviewTimeout
	if req.ReviewTimeoutSet {
		value = req.ReviewTimeout
	} else if cfg != nil {
		value = cfg.EffectiveReviewTimeout()
	}
	if err := config.ValidateReviewTimeout(value); err != nil {
		return 0, err
	}
	return value, nil
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

func readRetryLogLines(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{}
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	lines := make([]string, 0, len(parts))
	for _, line := range parts {
		if !isRetryMarker(line) {
			lines = append(lines, line)
		}
	}
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

func isRetryMarker(line string) bool {
	const (
		prefix = "--- retry "
		suffix = " ---"
	)
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
		return false
	}
	attempt, maxAttempts, ok := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(line, prefix), suffix), "/")
	if !ok || attempt == "" || maxAttempts == "" {
		return false
	}
	for _, value := range []string{attempt, maxAttempts} {
		for _, r := range value {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
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

type eventLogIndex struct {
	priorRunByIssue map[int]bool
	branchesByIssue map[int][]string
}

func readEventLogIndex(eventLog events.EventLog) eventLogIndex {
	index := eventLogIndex{
		priorRunByIssue: map[int]bool{},
		branchesByIssue: map[int][]string{},
	}
	if eventLog == nil {
		return index
	}
	logs, err := eventLog.Read()
	if err != nil {
		return index
	}
	seenBranches := make(map[int]map[string]struct{})
	for _, e := range logs {
		if e.Issue == 0 {
			continue
		}
		if e.Type == "run.started" || e.Type == "run.continued" {
			index.priorRunByIssue[e.Issue] = true
		}
		branch, ok := e.Payload["branch"].(string)
		if !ok || branch == "" {
			continue
		}
		if seenBranches[e.Issue] == nil {
			seenBranches[e.Issue] = make(map[string]struct{})
		}
		if _, seen := seenBranches[e.Issue][branch]; seen {
			continue
		}
		seenBranches[e.Issue][branch] = struct{}{}
		index.branchesByIssue[e.Issue] = append(index.branchesByIssue[e.Issue], branch)
	}
	return index
}

func collectIssueBranchesFromIndex(issueNumber int, title string, recordedBranch string, sourceBranch string, index eventLogIndex) []string {
	seen := map[string]bool{}
	branches := make([]string, 0, len(index.branchesByIssue[issueNumber])+2)
	add := func(branch string) {
		if branch != "" && !seen[branch] {
			seen[branch] = true
			branches = append(branches, branch)
		}
	}
	for _, branch := range index.branchesByIssue[issueNumber] {
		add(branch)
	}
	add(recordedBranch)
	add(BranchName(issueNumber, title, sourceBranch))
	return branches
}

func (o *Orchestrator) validateBatchBranches(ctx context.Context, req Request, baseBranch string) error {
	return o.validateBatchBranchesWithIndex(ctx, req, baseBranch, readEventLogIndex(o.eventLog))
}

func (o *Orchestrator) validateBatchBranchesWithIndex(ctx context.Context, req Request, baseBranch string, index eventLogIndex) error {
	if !branchValidationEnabled || len(req.Issues) == 0 {
		return nil
	}

	repoRoot, err := gitTopLevel(".")
	if err != nil {
		return fmt.Errorf("resolve repo root for branch validation: %w", err)
	}

	priorRunByIssue := index.priorRunByIssue

	var conflicts []branchConflict
	seenConflict := make(map[string]struct{}, len(req.Issues))
	for _, num := range req.Issues {
		if req.IssueMode(num) != ModeFresh {
			continue
		}
		title, ok := req.IssueTitles[num]
		if !ok || title == "" {
			issue, err := o.githubClient.FetchIssue(ctx, num)
			if err != nil {
				if o.errorLog != nil {
					fmt.Fprintf(o.errorLog, "error: fetch issue %d for branch validation: %v\n", num, err)
				}
				return fmt.Errorf("fetch issue %d for branch validation: %w", num, err)
			}
			title = issue.Title
		}

		for _, branch := range collectIssueBranchesFromIndex(num, title, req.Branches[num], baseBranch, index) {
			if !branchExists(repoRoot, branch) {
				continue
			}
			key := fmt.Sprintf("#%d (%s)", num, branch)
			if _, ok := seenConflict[key]; ok {
				continue
			}
			seenConflict[key] = struct{}{}
			conflicts = append(conflicts, branchConflict{
				issueNum: num,
				branch:   branch,
				hasPrior: priorRunByIssue[num],
			})
		}
	}

	if len(conflicts) == 0 {
		return nil
	}

	conflictLabels := make([]string, 0, len(conflicts))
	remediations := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		conflictLabels = append(conflictLabels, fmt.Sprintf("#%d (%s)", c.issueNum, c.branch))
		if c.hasPrior {
			remediations = append(remediations, fmt.Sprintf("#%d: prior run exists — use --continue", c.issueNum))
		} else {
			remediations = append(remediations, fmt.Sprintf("#%d: no prior run — use --override", c.issueNum))
		}
	}

	//lint:ignore ST1005 Preserve the existing operator-facing error text.
	return fmt.Errorf(
		"refusing to start batch: branches already exist from previous runs: %s. %s. Delete the branch with `git branch -D <branch>` or use --override to restart from scratch.",
		strings.Join(conflictLabels, ", "),
		strings.Join(remediations, ". "),
	)
}

// branchConflict is one issue/branch pair that the pre-flight branch
// validator rejected, paired with whether the event log already records a
// prior run.started/run.continued for that issue. The prior-run flag drives
// the per-issue remediation hint in the returned error.
type branchConflict struct {
	issueNum int
	branch   string
	hasPrior bool
}

// Orchestrator coordinates parallel AgentRun execution.
type Orchestrator struct {
	githubClient            github.Client
	renderer                prompt.IssueRenderer
	configStore             config.Store
	eventLog                events.EventLog
	runnableFactory         RunnableFactory
	sandboxFactory          SandboxFactory
	containerRuntimeFactory ContainerRuntimeFactory
	// layout owns the on-disk paths the orchestrator writes to (logs, worktrees,
	// event log, archive, runs). It is resolved once in NewOrchestrator from the
	// current working directory so the orchestrator is independent of subsequent
	// directory changes.
	layout paths.Layout
	// heartbeatTickInterval overrides the default 30s heartbeat tick for tests.
	// Zero means use the default tick interval.
	heartbeatTickInterval    time.Duration
	closingGuardTickInterval time.Duration
	errorLog                 io.Writer

	// lookupGHToken resolves the host GitHub auth token for hydrating the
	// copied gh hosts.yml in container config snapshots. It runs once per
	// batch from resolveSandboxExecutionPolicy, *before* any runSession is
	// built, so the dependency lives on the Orchestrator (per-batch
	// lifetime) rather than on runSessionOptions (per-session lifetime).
	// NewOrchestrator initialises it to defaultLookupGHToken; tests in
	// this package assign a fake to drive token-resolution paths without
	// shelling out to `gh auth token`. Takes a context so the spawned
	// `gh auth token` invocation honours the caller's cancellation
	// (issue #1780).
	lookupGHToken func(ctx context.Context) (string, error)

	// runSessionOpts bundles the test-injection hooks consumed by
	// runSession (the function overrides and the test-tunable killTimeout)
	// together with the shared baseBranchSyncMu mutex that gates
	// syncBaseBranch. Production code leaves the function and timeout
	// fields at their zero values; NewOrchestrator initialises the mutex.
	// Tests in this package set fields on this struct directly to drive
	// injected behaviour.
	runSessionOpts runSessionOptions

	// verifyPath is the verify chain invoked by the alreadyResolved
	// short-circuit in runOnce. Production code leaves it nil; the
	// orchestrator builds a default chain (T2 / T4 / T1) when
	// verifyPath is unset. Tests inject a VerifyPathFunc to drive
	// outcomes without touching real git or GitHub.
	verifyPath     VerifyPathFunc
	coordinatorsMu sync.Mutex
	coordinators   map[*batchCoordinator]struct{}

	badgeHooker BadgeHooker
}

// batchCoordinator owns mutable state for exactly one RunBatch invocation.
// Its command server is run-scoped, so issue numbers need only be unique
// within this coordinator rather than across every concurrently running batch.
type batchCoordinator struct {
	mu                     sync.Mutex
	phaseWriter            io.Writer
	firstSandboxStartOnce  sync.Once
	activeRuns             map[int]sandbox.Sandbox
	issueCancels           map[int]context.CancelFunc
	commandServers         map[int]*daemon.CommandServer
	shutdownSupervisorDone []<-chan struct{}
}

func newBatchCoordinator(phaseWriter io.Writer) *batchCoordinator {
	return &batchCoordinator{
		phaseWriter:    phaseWriter,
		activeRuns:     make(map[int]sandbox.Sandbox),
		issueCancels:   make(map[int]context.CancelFunc),
		commandServers: make(map[int]*daemon.CommandServer),
	}
}

func (c *batchCoordinator) AbortIssue(issueNumber int) error {
	c.mu.Lock()
	cancel, ok := c.issueCancels[issueNumber]
	c.mu.Unlock()
	if !ok {
		return ErrNoSuchIssue
	}
	cancel()
	return nil
}

func (c *batchCoordinator) abortIfPresent(issueNumber int) bool {
	c.mu.Lock()
	cancel, ok := c.issueCancels[issueNumber]
	c.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

func (c *batchCoordinator) registerIssueCancel(issueNumber int, cancel context.CancelFunc) {
	c.mu.Lock()
	c.issueCancels[issueNumber] = cancel
	c.mu.Unlock()
}

func (c *batchCoordinator) unregisterIssueCancel(issueNumber int) {
	c.mu.Lock()
	delete(c.issueCancels, issueNumber)
	c.mu.Unlock()
}

func (c *batchCoordinator) registerActiveRun(key int, sb sandbox.Sandbox) {
	c.mu.Lock()
	c.activeRuns[key] = sb
	c.mu.Unlock()
}

func (c *batchCoordinator) unregisterActiveRun(key int) {
	c.mu.Lock()
	delete(c.activeRuns, key)
	c.mu.Unlock()
}

// startCommandServer keeps an awaited run's per-run endpoint alive across
// attempts, but creates it only after the session has passed its external gate.
func (c *batchCoordinator) startCommandServer(issueNumber int, dir string, commander daemon.IssueCommander) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.commandServers[issueNumber] != nil {
		return nil
	}
	server := daemon.NewCommandServerForIssue(dir, commander, issueNumber)
	if err := server.Start(); err != nil {
		return err
	}
	c.commandServers[issueNumber] = server
	return nil
}

func (c *batchCoordinator) stopCommandServer(issueNumber int) error {
	c.mu.Lock()
	server := c.commandServers[issueNumber]
	delete(c.commandServers, issueNumber)
	c.mu.Unlock()
	if server == nil {
		return nil
	}
	return server.Stop()
}

func (c *batchCoordinator) trackShutdownSupervisor(done <-chan struct{}) {
	c.mu.Lock()
	c.shutdownSupervisorDone = append(c.shutdownSupervisorDone, done)
	c.mu.Unlock()
}

func (c *batchCoordinator) snapshotShutdownSupervisors() []<-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]<-chan struct{}(nil), c.shutdownSupervisorDone...)
}

func (c *batchCoordinator) currentPhaseWriter() io.Writer { return c.phaseWriter }

func (c *batchCoordinator) firstSandboxStart(sandboxStarted time.Time) {
	c.firstSandboxStartOnce.Do(func() { writePhase(c.phaseWriter, "first-sandbox-start", sandboxStarted) })
}

// defaultLookupGHToken shells out to `gh auth token` and returns the
// trimmed token. An empty output is treated as an error so callers do not
// silently inject an empty oauth_token. The exec.ErrNotFound special-case
// for callers that want to skip token injection on minimal hosts lives
// in hydrateGHHostsFile, not here.
func defaultLookupGHToken(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", fmt.Errorf("gh auth token (context: %w): %w", cerr, err)
		}
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
	wake             chan struct{}
	priorityWaiters  []*batchStartWaiter
	normalWaiters    []*batchStartWaiter
	onWaiterQueued   func(bool)
	ordinaryStarts   uint64
	nextWaiterOrder  uint64
	now              func() time.Time
}

type batchStartWaiter struct {
	priority               bool
	ordinary               bool
	awaiting               bool
	lastChance             time.Time
	ordinaryStartsAtChance uint64
	order                  uint64
}

type awaitOpportunity struct {
	lastChance     time.Time
	ordinaryStarts uint64
}

const awaitingPriorityCooldown = 10 * time.Minute

// awaitedRowMayUsePriority applies the fairness rule at one gate observation.
// A recent waiting-row chance may bypass ordinary work only when no ordinary
// waiter remains or ordinary progress has occurred since that chance.
func awaitedRowMayUsePriority(lastChance time.Time, ordinaryStartsAtChance, ordinaryStarts uint64, ordinaryQueued bool, now time.Time) bool {
	if lastChance.IsZero() || now.Sub(lastChance) >= awaitingPriorityCooldown {
		return true
	}
	if !ordinaryQueued {
		return true
	}
	return ordinaryStarts > ordinaryStartsAtChance
}

func newBatchStartGate(parallel int, delay time.Duration, onWaiterQueued ...func(bool)) *batchStartGate {
	var callback func(bool)
	if len(onWaiterQueued) > 0 {
		callback = onWaiterQueued[0]
	}
	return &batchStartGate{
		parallel:       parallel,
		delay:          delay,
		wake:           make(chan struct{}),
		onWaiterQueued: callback,
		now:            time.Now,
	}
}

// effectiveParallelCap returns the effective parallel concurrency after applying
// the container pool capacity cap. In auto mode (maxContainers == 0) the pool
// creates containers on demand, so the cap never throttles below the requested
// parallel: each concurrent run gets its own container (each hosting up to
// containerCapacity AgentRuns). In explicit-cap mode the budget is
// containerCapacity * maxContainers, and parallel is capped to that total.
// The parallel == 0 (unlimited) semantics are preserved: an unlimited parallel
// request is never capped down to a finite number.
func effectiveParallelCap(parallel, containerCapacity, maxContainers int) int {
	if parallel == 0 {
		return 0
	}
	if containerCapacity <= 0 {
		return parallel
	}
	var totalSlots int
	if maxContainers == 0 {
		totalSlots = parallel * containerCapacity
	} else {
		totalSlots = containerCapacity * maxContainers
	}
	if totalSlots < parallel {
		return totalSlots
	}
	return parallel
}

// Acquire waits for an execution slot. A priority waiter is selected before
// ordinary waiters, but only after capacity and start delay permit a start.
// The optional argument keeps the existing ordinary-start call sites concise.
func (g *batchStartGate) Acquire(ctx context.Context, priority ...bool) error {
	isPriority := len(priority) > 0 && priority[0]
	_, err := g.acquire(ctx, &batchStartWaiter{priority: isPriority, ordinary: !isPriority})
	return err
}

func (g *batchStartGate) AcquireWithOpportunity(ctx context.Context, priority bool) (awaitOpportunity, error) {
	return g.acquire(ctx, &batchStartWaiter{priority: priority, ordinary: !priority})
}

// AcquireAwaiting re-enters a row that previously returned to external wait.
// Its returned opportunity is the timestamp and ordinary-start marker for the
// newly acquired chance.
func (g *batchStartGate) AcquireAwaiting(ctx context.Context, opportunity awaitOpportunity) (awaitOpportunity, error) {
	return g.acquire(ctx, &batchStartWaiter{
		awaiting:               true,
		lastChance:             opportunity.lastChance,
		ordinaryStartsAtChance: opportunity.ordinaryStarts,
	})
}

func (g *batchStartGate) acquire(ctx context.Context, waiter *batchStartWaiter) (awaitOpportunity, error) {
	return g.acquireObserved(ctx, waiter, nil, 0)
}

// Keep the same queued waiter while observing lifecycle changes; observations
// never claim execution capacity and do not change fairness opportunities.
func (g *batchStartGate) acquireObserved(ctx context.Context, waiter *batchStartWaiter, observe func() error, interval time.Duration) (awaitOpportunity, error) {
	acquired := false
	defer func() {
		if !acquired {
			g.cancelWaiter(waiter)
		}
	}()
	for {
		if observe != nil {
			if err := observe(); err != nil {
				return awaitOpportunity{}, err
			}
		}
		wake, wait, opportunity, ok, err := g.tryAcquire(ctx, waiter)
		if err != nil {
			return awaitOpportunity{}, err
		}
		if ok {
			acquired = true
			return opportunity, nil
		}
		if observe != nil && (wait <= 0 || wait > interval) {
			wait = interval
		}
		if err := waitForStartGate(ctx, wake, wait); err != nil {
			return awaitOpportunity{}, err
		}
	}
}

func (g *batchStartGate) tryAcquire(ctx context.Context, waiter *batchStartWaiter) (chan struct{}, time.Duration, awaitOpportunity, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, awaitOpportunity{}, false, err
	}
	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return nil, 0, awaitOpportunity{}, false, err
	}
	now := g.currentTime()
	g.refreshAwaitingWaitersLocked(now)
	if waiter.awaiting && !g.isQueuedLocked(waiter) {
		waiter.priority = awaitedRowMayUsePriority(
			waiter.lastChance,
			waiter.ordinaryStartsAtChance,
			g.ordinaryStarts,
			g.hasOrdinaryWaiterLocked(),
			now,
		)
	}
	if g.canAcquireLocked(waiter, now) {
		opportunity := g.grantLocked(waiter, now)
		g.mu.Unlock()
		return nil, 0, opportunity, true, nil
	}
	queued := g.enqueueWaiterLocked(waiter)
	g.refreshAwaitingWaitersLocked(now)
	if g.canAcquireLocked(waiter, now) {
		opportunity := g.grantLocked(waiter, now)
		g.mu.Unlock()
		if queued && g.onWaiterQueued != nil {
			g.onWaiterQueued(waiter.priority)
		}
		return nil, 0, opportunity, true, nil
	}
	wake := g.wake
	wait := time.Duration(0)
	if g.delay > 0 && now.Before(g.nextAllowedStart) {
		wait = g.nextAllowedStart.Sub(now)
	}
	if waiter.awaiting && !waiter.priority {
		cooldownWait := waiter.lastChance.Add(awaitingPriorityCooldown).Sub(now)
		if cooldownWait > wait {
			wait = cooldownWait
		}
	}
	g.mu.Unlock()
	if queued && g.onWaiterQueued != nil {
		g.onWaiterQueued(waiter.priority)
	}
	return wake, wait, awaitOpportunity{}, false, nil
}

func (g *batchStartGate) grantLocked(waiter *batchStartWaiter, now time.Time) awaitOpportunity {
	if g.parallel > 0 {
		g.active++
	}
	if waiter.ordinary {
		g.ordinaryStarts++
	}
	opportunity := awaitOpportunity{lastChance: now, ordinaryStarts: g.ordinaryStarts}
	g.removeWaiterLocked(waiter)
	g.signalLocked()
	return opportunity
}

func (g *batchStartGate) enqueueWaiterLocked(waiter *batchStartWaiter) bool {
	if g.isQueuedLocked(waiter) {
		return false
	}
	if waiter.order == 0 {
		g.nextWaiterOrder++
		waiter.order = g.nextWaiterOrder
	}
	if waiter.priority {
		g.insertPriorityWaiterLocked(waiter)
	} else {
		g.insertNormalWaiterLocked(waiter)
	}
	return true
}

func (g *batchStartGate) insertPriorityWaiterLocked(waiter *batchStartWaiter) {
	index := len(g.priorityWaiters)
	for i, candidate := range g.priorityWaiters {
		if waiter.order < candidate.order {
			index = i
			break
		}
	}
	g.priorityWaiters = append(g.priorityWaiters, nil)
	copy(g.priorityWaiters[index+1:], g.priorityWaiters[index:])
	g.priorityWaiters[index] = waiter
}

func (g *batchStartGate) insertNormalWaiterLocked(waiter *batchStartWaiter) {
	if !waiter.ordinary {
		g.normalWaiters = append(g.normalWaiters, waiter)
		return
	}
	index := len(g.normalWaiters)
	for i, candidate := range g.normalWaiters {
		if !candidate.ordinary {
			index = i
			break
		}
	}
	g.normalWaiters = append(g.normalWaiters, nil)
	copy(g.normalWaiters[index+1:], g.normalWaiters[index:])
	g.normalWaiters[index] = waiter
}

func waitForStartGate(ctx context.Context, wake <-chan struct{}, wait time.Duration) error {
	if wait <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
			return nil
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	case <-timer.C:
		return nil
	}
}

func (g *batchStartGate) currentTime() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *batchStartGate) hasOrdinaryWaiterLocked() bool {
	for _, waiter := range g.normalWaiters {
		if waiter.ordinary {
			return true
		}
	}
	return false
}

func (g *batchStartGate) refreshAwaitingWaitersLocked(now time.Time) {
	for index := 0; index < len(g.priorityWaiters); {
		waiter := g.priorityWaiters[index]
		if !waiter.awaiting || awaitedRowMayUsePriority(
			waiter.lastChance,
			waiter.ordinaryStartsAtChance,
			g.ordinaryStarts,
			g.hasOrdinaryWaiterLocked(),
			now,
		) {
			index++
			continue
		}
		g.priorityWaiters = append(g.priorityWaiters[:index], g.priorityWaiters[index+1:]...)
		waiter.priority = false
		g.insertNormalWaiterLocked(waiter)
	}
	for index := 0; index < len(g.normalWaiters); {
		waiter := g.normalWaiters[index]
		if !waiter.awaiting || !awaitedRowMayUsePriority(
			waiter.lastChance,
			waiter.ordinaryStartsAtChance,
			g.ordinaryStarts,
			g.hasOrdinaryWaiterLocked(),
			now,
		) {
			index++
			continue
		}
		g.normalWaiters = append(g.normalWaiters[:index], g.normalWaiters[index+1:]...)
		waiter.priority = true
		g.insertPriorityWaiterLocked(waiter)
	}
}

func (g *batchStartGate) canAcquireLocked(waiter *batchStartWaiter, now time.Time) bool {
	if g.parallel > 0 && g.active >= g.parallel {
		return false
	}
	if g.delay > 0 && now.Before(g.nextAllowedStart) {
		return false
	}
	if len(g.priorityWaiters) > 0 {
		return g.priorityWaiters[0] == waiter
	}
	if len(g.normalWaiters) > 0 {
		return g.normalWaiters[0] == waiter
	}
	return true
}

func (g *batchStartGate) isQueuedLocked(waiter *batchStartWaiter) bool {
	for _, candidate := range g.priorityWaiters {
		if candidate == waiter {
			return true
		}
	}
	for _, candidate := range g.normalWaiters {
		if candidate == waiter {
			return true
		}
	}
	return false
}

func (g *batchStartGate) removeWaiterLocked(waiter *batchStartWaiter) {
	for list := range [][]*batchStartWaiter{g.priorityWaiters, g.normalWaiters} {
		var waiters []*batchStartWaiter
		if list == 0 {
			waiters = g.priorityWaiters
		} else {
			waiters = g.normalWaiters
		}
		for index, candidate := range waiters {
			if candidate != waiter {
				continue
			}
			waiters = append(waiters[:index], waiters[index+1:]...)
			if list == 0 {
				g.priorityWaiters = waiters
			} else {
				g.normalWaiters = waiters
			}
			return
		}
	}
}

func (g *batchStartGate) cancelWaiter(waiter *batchStartWaiter) {
	g.mu.Lock()
	if g.isQueuedLocked(waiter) {
		g.removeWaiterLocked(waiter)
		g.refreshAwaitingWaitersLocked(g.currentTime())
		g.signalLocked()
	}
	g.mu.Unlock()
}

func (g *batchStartGate) signalLocked() {
	close(g.wake)
	g.wake = make(chan struct{})
}

func waitForAwaitPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
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
		g.signalLocked()
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
	g.signalLocked()
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

		if p.hasPendingCapacityLocked() {
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

func (p *containerPool) hasPendingCapacityLocked() bool {
	for _, entry := range p.shared {
		if !entry.ready && !entry.dead && entry.startErr == nil && (p.capacity <= 0 || entry.active < p.capacity) {
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
// By default, BadgeHooker is a nop implementation. Pass WithBadgeHooker to
// use the real badge suggestion hook.
func NewOrchestrator(githubClient github.Client, renderer prompt.IssueRenderer, configStore config.Store, eventLog events.EventLog, opts ...OrchestratorOpt) *Orchestrator {
	root, err := filepath.Abs(".")
	if err != nil {
		root = "."
	}
	o := &Orchestrator{
		githubClient:  githubClient,
		renderer:      renderer,
		configStore:   configStore,
		eventLog:      eventLog,
		errorLog:      os.Stderr,
		layout:        paths.NewLayout(&config.Config{}, root),
		lookupGHToken: defaultLookupGHToken,
		runSessionOpts: runSessionOptions{
			baseBranchSyncMu:     &sync.Mutex{},
			taskWriter:           atomicfs.WriteAtomic,
			foregroundLifecycle:  false,
			releaseAwaitCapacity: true,
		},
		badgeHooker:  nopBadgeHooker{},
		coordinators: make(map[*batchCoordinator]struct{}),
	}
	for _, opt := range opts {
		opt(o)
	}
	// Ensure the baseBranchSyncMu mutex survives option application —
	// WithRunSessionOpts replaces the whole runSessionOptions struct, so
	// the per-Orchestrator mutex must be re-established afterwards to
	// keep syncBaseBranch's lock serialisation intact (wayfinder #2231).
	if o.runSessionOpts.baseBranchSyncMu == nil {
		o.runSessionOpts.baseBranchSyncMu = &sync.Mutex{}
	}
	return o
}

type OrchestratorOpt func(*Orchestrator)

// WithBadgeHooker sets the BadgeHooker for the Orchestrator. Use this in
// production wiring; tests should leave BadgeHooker as nopBadgeHooker{}.
func WithBadgeHooker(h BadgeHooker) OrchestratorOpt {
	return func(o *Orchestrator) {
		o.badgeHooker = h
	}
}

// Test-injection options (wayfinder #2231). These are the public seam that
// replaces the historical pattern of post-construction assignment to
// Orchestrator private fields (`o.runnableFactory = ...`) and struct-literal
// construction (`&Orchestrator{runnableFactory: ...}`). They feed NewOrchestrator
// at construction time so tests stop reaching into unexported state.
//
// Every option below corresponds to a field the catalog (#2230) classified
// as load-bearing (tests drive meaningful behaviour through it) or test-tunable
// (tests adjust a production default). The 3 not-injected fields (lookupGHToken,
// phaseWriter, firstSandboxStartOnce) intentionally have no options.
//
// NewOrchestrator restores o.runSessionOpts.baseBranchSyncMu after applying
// options, so WithRunSessionOpts can safely replace the whole struct without
// losing the per-Orchestrator serialisation mutex.

func WithRunnableFactory(f RunnableFactory) OrchestratorOpt {
	return func(o *Orchestrator) { o.runnableFactory = f }
}

func WithSandboxFactory(f SandboxFactory) OrchestratorOpt {
	return func(o *Orchestrator) { o.sandboxFactory = f }
}

func WithContainerRuntimeFactory(f ContainerRuntimeFactory) OrchestratorOpt {
	return func(o *Orchestrator) { o.containerRuntimeFactory = f }
}

func WithErrorLog(w io.Writer) OrchestratorOpt {
	return func(o *Orchestrator) { o.errorLog = w }
}

// WithRunSessionOpts sets the whole runSessionOptions struct on the Orchestrator.
// Note: NewOrchestrator restores the per-Orchestrator baseBranchSyncMu mutex
// after this option is applied, so passing a fresh runSessionOptions{} is safe.
func WithRunSessionOpts(opts runSessionOptions) OrchestratorOpt {
	return func(o *Orchestrator) { o.runSessionOpts = opts }
}

// WithContextRolloverLiteralAdditions overrides the additive literal catalog
// for one orchestrator without mutating the process-wide default.
func WithContextRolloverLiteralAdditions(values []string) OrchestratorOpt {
	return func(o *Orchestrator) {
		o.runSessionOpts.contextRolloverLiterals = append([]string(nil), values...)
		o.runSessionOpts.contextRolloverLiteralsSet = true
	}
}

func WithHeartbeatTickInterval(d time.Duration) OrchestratorOpt {
	return func(o *Orchestrator) { o.heartbeatTickInterval = d }
}

func WithClosingGuardTickInterval(d time.Duration) OrchestratorOpt {
	return func(o *Orchestrator) { o.closingGuardTickInterval = d }
}

func WithVerifyPath(v VerifyPathFunc) OrchestratorOpt {
	return func(o *Orchestrator) { o.verifyPath = v }
}

// NewBadgeHooker returns a BadgeHooker that suggests a Built with Sandman
// badge PR after a batch with merged Sandman-managed PRs. The hook is silent
// to the operator — it writes nothing to any user-visible stream and
// only persists a marker under <sandmanDir>/state/ to gate future
// batches (see issue #2195). It returns a nopBadgeHooker if the sandman
// binary cannot be resolved.
func NewBadgeHooker() BadgeHooker {
	sandmanRunner, err := newDefaultSandmanRunner()
	if err != nil {
		return nopBadgeHooker{}
	}
	root, err := filepath.Abs(".")
	if err != nil {
		root = "."
	}
	layout := paths.NewLayout(nil, root)
	return newDefaultBadgeHooker(
		&defaultPRLister{gh: realGhCommander{}},
		&defaultBadgeControlFileReader{layout: layout},
		&defaultBadgeControlFileWriter{layout: layout},
		sandmanRunner,
	)
}

// NewBadgeHookerWith returns a BadgeHooker that uses the provided
// SandmanRunner, PRLister, BadgeControlFileReader, and
// BadgeControlFileWriter implementations. It exists so e2e tests can
// drive the production defaultBadgeHooker end-to-end without shelling out
// to the sandman binary resolved from os.Executable() (which inside a test
// binary resolves to the test binary itself and is unusable as sandman).
//
// Production wiring continues to use NewBadgeHooker — its nop fallback
// when the sandman binary is unresolved is preserved.
func NewBadgeHookerWith(runner SandmanRunner, lister PRLister, controlReader BadgeControlFileReader, controlWriter BadgeControlFileWriter) BadgeHooker {
	return newDefaultBadgeHooker(lister, controlReader, controlWriter, runner)
}

// AbortIssue cancels a uniquely identified in-flight issue. The per-run command
// server uses its batch coordinator directly; this compatibility method returns
// ErrNoSuchIssue when concurrent batches make an issue number ambiguous.
func (o *Orchestrator) AbortIssue(issueNumber int) error {
	o.coordinatorsMu.Lock()
	coordinators := make([]*batchCoordinator, 0, len(o.coordinators))
	for coord := range o.coordinators {
		coordinators = append(coordinators, coord)
	}
	o.coordinatorsMu.Unlock()
	var target *batchCoordinator
	for _, coord := range coordinators {
		coord.mu.Lock()
		_, active := coord.issueCancels[issueNumber]
		coord.mu.Unlock()
		if !active {
			continue
		}
		if target != nil {
			return ErrNoSuchIssue
		}
		target = coord
	}
	if target != nil && target.abortIfPresent(issueNumber) {
		return nil
	}
	return ErrNoSuchIssue
}

func writePhase(w io.Writer, name string, started time.Time) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "phase %s duration=%s\n", name, time.Since(started))
}

// RunBatch executes the requested AgentRuns in parallel.
func (o *Orchestrator) RunBatch(ctx context.Context, req Request) (*Result, error) {
	cfg, err := o.configStore.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	coord := newBatchCoordinator(req.PhaseWriter)
	o.coordinatorsMu.Lock()
	o.coordinators[coord] = struct{}{}
	o.coordinatorsMu.Unlock()
	defer func() {
		o.coordinatorsMu.Lock()
		delete(o.coordinators, coord)
		o.coordinatorsMu.Unlock()
	}()
	layout := paths.NewLayout(cfg, o.layout.RepoRoot)
	retries := resolveRetries(req, cfg)
	runIdleTimeout := resolveRunIdleTimeout(req, cfg)
	reviewTimeout, err := resolveReviewTimeout(req, cfg)
	if err != nil {
		return nil, err
	}
	req.PromptConfig.ReviewTimeout = reviewTimeout

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
	variant := strings.TrimSpace(req.Variant)
	if !req.VariantSet && strings.TrimSpace(req.Variant) == "" {
		variant = strings.TrimSpace(cfg.Variant)
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

	phaseStarted := time.Now()
	policy, err := o.resolveSandboxExecutionPolicy(ctx, cfg, agentCfg, req, sandboxMode)
	if err != nil {
		return nil, err
	}
	writePhase(coord.currentPhaseWriter(), "sandbox-preflight", phaseStarted)
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

	allContinue := len(req.Issues) > 0 && len(req.Mode) > 0
	for _, num := range req.Issues {
		if req.IssueMode(num) != ModeContinue {
			allContinue = false
			break
		}
	}
	if !allContinue && req.PromptConfig.PromptFile == "" {
		req.PromptConfig.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if req.PromptConfig.RenderedPromptFile == "" {
		req.PromptConfig.RenderedPromptFile = filepath.Join(".", ".sandman", "task.md")
	}
	if !allContinue {
		if err := prompt.MaterializePromptFile(req.PromptConfig); err != nil {
			return nil, fmt.Errorf("materialize prompt template: %w", err)
		}
	}

	overrideIndex := eventLogIndex{priorRunByIssue: map[int]bool{}, branchesByIssue: map[int][]string{}}
	hasOverride := false
	for _, num := range req.Issues {
		if req.IssueMode(num) == ModeOverride {
			hasOverride = true
			break
		}
	}
	if hasOverride {
		overrideIndex = readEventLogIndex(o.eventLog)
	}

	var overridePRCloseErrors []string
	var overrideBranches []struct {
		issueNumber int
		branch      string
	}
	for _, num := range req.Issues {
		if req.IssueMode(num) != ModeOverride {
			continue
		}
		issue, err := o.githubClient.FetchIssue(ctx, num)
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: override: failed to inspect issue %d before retiring pull requests: %v; worktree and branch preserved\n", num, err)
			overridePRCloseErrors = append(overridePRCloseErrors, fmt.Sprintf("issue %d: %v", num, err))
			continue
		}
		branches := collectIssueBranchesFromIndex(num, issue.Title, req.Branches[num], baseBranch, overrideIndex)
		for _, branch := range branches {
			overrideBranches = append(overrideBranches, struct {
				issueNumber int
				branch      string
			}{issueNumber: num, branch: branch})
			if err := closeOpenPRForBranch(ctx, branch, o.githubClient); err != nil {
				fmt.Fprintf(o.errorLog, "error: override: failed to retire open PR for branch %s (issue %d): %v; worktree and branch preserved\n", branch, num, err)
				overridePRCloseErrors = append(overridePRCloseErrors, fmt.Sprintf("branch %s (issue %d): %v", branch, num, err))
			}
		}
	}
	if len(overridePRCloseErrors) > 0 {
		return nil, fmt.Errorf("override: failed to close open PR(s) before replacing branch(es): %s; worktree and branch preserved", strings.Join(overridePRCloseErrors, "; "))
	}
	for _, entry := range overrideBranches {
		ClearIssueArtifacts(entry.issueNumber, entry.branch, layout.WorktreeDir, o.eventLog, o.errorLog, baseBranch, req.StrandedReconcile, layout.BatchesIndexPath)
	}

	eventIndex := eventLogIndex{priorRunByIssue: map[int]bool{}, branchesByIssue: map[int][]string{}}
	if len(req.Issues) > 0 {
		eventIndex = readEventLogIndex(o.eventLog)
	}

	phaseStarted = time.Now()
	if err := o.validateBatchBranchesWithIndex(ctx, req, baseBranch, eventIndex); err != nil {
		return nil, err
	}
	writePhase(coord.currentPhaseWriter(), "branch-validation", phaseStarted)

	dangerouslySkipPermissions := req.DangerouslySkipPermissions
	if dangerouslySkipPermissions == nil {
		dangerouslySkipPermissions = &isContainer
	}

	strandedReconcile := true
	if req.StrandedReconcile != nil {
		strandedReconcile = *req.StrandedReconcile
	}

	if len(req.Issues) == 0 && (req.PromptConfig.PromptFlag != "" || req.PromptConfig.TemplateFlag != "" || req.PromptConfig.TaskPrompt != "") {
		return o.runPromptOnly(ctx, cfg, agentName, agentCfg, newBatchIdentityResolver(o, "."), policy.sandboxFactory, policy.containerAlloc, req, baseBranch, startDelay, parallel, retries, runIdleTimeout, sandboxMode, containerCapacityForLog, req.ContainerCapacitySet, maxContainersForLog, req.MaxContainersSet, *dangerouslySkipPermissions, strandedReconcile, coord, layout)
	}

	startGate := newBatchStartGate(effectiveParallel, startDelay, o.runSessionOpts.startWaiterQueued)
	var wg sync.WaitGroup
	results := make([]AgentRunResult, len(req.Issues))
	var mu sync.Mutex
	// usageLimitGate pauses admission of not-yet-started rows once any row
	// reports provider usage-limit exhaustion. Quota is global per preset, so
	// launching another agent against the same exhausted quota only burns
	// retries. Paused rows emit run.capacity_queued and stay non-terminal
	// until resume via normal admission. RunBatch-local only.
	quotaGate := newBatchQuotaGate()
	failureCount := 0
	abortedCount := 0
	statuses := make(map[int]string, len(req.Issues))
	completed := make(map[int]chan struct{}, len(req.Issues))
	yielded := make(map[int]chan struct{}, len(req.Issues))
	for _, num := range req.Issues {
		completed[num] = make(chan struct{})
		yielded[num] = make(chan struct{})
	}

	batchIdentityResolver := newBatchIdentityResolver(o, ".")
	issueBatchID := issueBatchIDForRequest(req)
	claims := make(map[int]*daemon.RunClaim, len(ordered))
	for _, num := range ordered {
		id := strings.TrimSpace(req.RunIDs[num])
		if id == "" {
			id = buildRunID(num, req.RunTS, req.RunShortID)
		}
		claim, err := daemon.ClaimRun(layout.SandmanDir, id)
		if err != nil {
			for _, held := range claims {
				_ = held.Close()
			}
			return nil, fmt.Errorf("claim run %s: %w", id, err)
		}
		claims[num] = claim
	}
	defer func() {
		for _, claim := range claims {
			_ = claim.Close()
		}
	}()
	claimedStates := map[string]events.RunState{}
	if o.eventLog != nil {
		var err error
		claimedStates, err = events.ReadRunStates(o.eventLog)
		if err != nil {
			return nil, fmt.Errorf("read lifecycle under run claims: %w", err)
		}
	}
	recoveryRejected := map[int]bool{}
	var waitOwnersMu sync.Mutex
	var waitOwners []*waitOwner
	trackWaitOwner := func(owner *waitOwner) {
		waitOwnersMu.Lock()
		waitOwners = append(waitOwners, owner)
		waitOwnersMu.Unlock()
	}
	ownerPulse := func(id string) <-chan time.Time {
		if o.runSessionOpts.waitOwnerPulse != nil {
			return o.runSessionOpts.waitOwnerPulse(id)
		}
		return nil
	}
	defer func() {
		for _, owner := range waitOwners {
			owner.close()
		}
	}()
	recoveryNow := func() time.Time {
		if o.runSessionOpts.now != nil {
			return o.runSessionOpts.now().UTC()
		}
		return time.Now().UTC()
	}
	for issue, recovery := range req.RecoveryWaits {
		if claimedStates[recovery.RunID].IsTerminal() {
			continue
		}
		state := claimedStates[recovery.RunID]
		if !state.IsActive() || state.HasStarted() && !state.IsAwaiting() && !state.IsCapacityQueued() {
			recoveryRejected[issue] = true
			continue
		}
		current, err := daemon.ReadRunWait(layout.BatchDir(recovery.BatchID), recovery.RunID)
		if os.IsNotExist(err) && !recovery.RecoveryEventAt.IsZero() {
			if legacy, valid := legacyRecoveryWait(claimedStates[recovery.RunID]); valid && legacy.RecoveryEventAt.Equal(recovery.RecoveryEventAt) && legacy.RecoverableAt(recoveryNow()) {
				current = legacy
				err = daemon.RenewRunWait(layout.BatchDir(current.BatchID), current, recoveryNow())
			}
		}
		if err != nil || current.Issue != issue {
			recoveryRejected[issue] = true
			continue
		}
		reconciled, changed, valid := reconcileRecoveryWait(current, claimedStates[recovery.RunID])
		if !valid || !reconciled.RecoverableAt(recoveryNow()) {
			recoveryRejected[issue] = true
			continue
		}
		if changed {
			if err := daemon.RenewRunWait(layout.BatchDir(current.BatchID), reconciled, recoveryNow()); err != nil {
				recoveryRejected[issue] = true
				continue
			}
			current = reconciled
		}
		newBatchID := issueBatchID
		if newBatchID == "" {
			newBatchID = batchIDFromRunID(current.RunID)
			if newBatchID == "" {
				newBatchID = current.RunID
			}
		}
		current, err = daemon.TransferRunWait(layout.BatchDir(current.BatchID), layout.BatchDir(newBatchID), current.RunID, recoveryNow())
		if err != nil {
			recoveryRejected[issue] = true
			continue
		}
		req.RecoveryWaits[issue] = current
		if !current.InitialAdmission {
			if req.ReadyContinuations == nil {
				req.ReadyContinuations = map[int]bool{}
			}
			req.ReadyContinuations[issue] = current.Ready
		}
		if current.UsageLimitProbe && !claimedStates[current.RunID].IsTerminal() {
			quotaGate.report(issue, AgentRunResult{Status: "await", UsageLimitReached: true}, true)
		}
	}

	// Graceful shutdown: each per-session supervisor (spawned in
	// execute / executePromptOnly) owns the signal/kill of its own
	// process. This batch-wide goroutine only fans in: once ctx
	// fires, it waits for every supervisor's done channel to close,
	// so RunBatch returns as soon as every process is actually gone
	// instead of after a wall-clock sleep.
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)

	go func() {
		select {
		case <-ctx.Done():
		case <-shutdownDone:
			return
		}

		for _, done := range coord.snapshotShutdownSupervisors() {
			<-done
		}
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
		runID := strings.TrimSpace(req.RunIDs[num])
		if runID == "" {
			runID = buildRunID(num, req.RunTS, req.RunShortID)
		}
		recoveredWait, recovering := req.RecoveryWaits[num]
		queueWriteFailed := false
		if o.eventLog != nil && !recoveryRejected[num] && !claimedStates[runID].IsTerminal() && !(recovering && !recoveredWait.InitialAdmission && !recoveredWait.Ready) && (recovering && recoveredWait.InitialAdmission || req.ReadyContinuations[num] || len(dependencies[num]) > 0 || (effectiveParallel > 0 && effectiveParallel < len(req.Issues))) {
			queuedPayload := map[string]any{"blocked_by": dependencies[num]}
			if title, ok := req.IssueTitles[num]; ok && title != "" {
				queuedPayload["issue_title"] = title
			} else if issue, err := o.githubClient.FetchIssue(ctx, num); err == nil && issue != nil {
				queuedPayload["issue_title"] = issue.Title
			}
			if issueBatchID != "" {
				queuedPayload["batch_id"] = issueBatchID
			}
			queuedType := "run.queued"
			queuedPayload["initial_admission"] = true
			if req.ReadyContinuations[num] {
				queuedType = "run.capacity_queued"
				queuedPayload["ready_continuation"] = true
				queuedPayload["branch"] = req.Branches[num]
				queuedPayload["base_branch"] = req.BaseBranches[num]
				queuedPayload["previous_run_id"] = req.PreviousRunIDs[num]
				queuedPayload["previous_run_batch_id"] = req.PreviousRunBatchIDs[num]
			}
			if err := o.eventLog.Log(events.Event{
				Type:      queuedType,
				Timestamp: time.Now(),
				RunID:     runID,
				Issue:     num,
				IssueRef:  issueRef(num),
				Payload:   queuedPayload,
			}); err != nil {
				queueWriteFailed = true
				fmt.Fprintf(o.errorLog, "persist admission ownership for run %s: %v\n", runID, err)
			}
		}
		go func(idx, issueNum int, blockers []int, turn int, runID string, queueWriteFailed bool) {
			defer wg.Done()
			defer close(completed[issueNum])
			defer func() {
				mu.Lock()
				terminal := events.RunStatusFromPayload(results[idx].Status).IsTerminal()
				mu.Unlock()
				// Returning an await leaves logical ownership alive. Only a
				// terminal owner or explicit batch cancellation releases its pause.
				if terminal || ctx.Err() != nil {
					quotaGate.retire(issueNum)
				}
			}()
			_, recoveringRow := req.RecoveryWaits[issueNum]
			if claimedStates[runID].IsTerminal() {
				state := claimedStates[runID]
				mu.Lock()
				results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: state.Status(), Branch: state.Branch()}
				statuses[issueNum] = state.Status()
				mu.Unlock()
				return
			}
			if recoveryRejected[issueNum] || queueWriteFailed {
				o.logAborted(issueNum, runID, nil)
				mu.Lock()
				results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted"}
				statuses[issueNum] = "aborted"
				abortedCount++
				mu.Unlock()
				return
			}
			yieldedCapacity := false

			issueCtx, issueCancel := context.WithCancel(ctx)
			coord.registerIssueCancel(issueNum, issueCancel)
			defer coord.unregisterIssueCancel(issueNum)
			defer issueCancel()
			leaseFailure := func(err error) {
				fmt.Fprintf(o.errorLog, "renew ownership for run %s: %v; aborting owned intent\n", runID, err)
				issueCancel()
				mu.Lock()
				defer mu.Unlock()
				// Active execution consumes cancellation through its ordinary abort
				// path. Returned unfinished rows still belong to this batch.
				if results[idx].Status != "" && !events.RunStatusFromPayload(results[idx].Status).IsTerminal() {
					o.logAborted(issueNum, runID, nil)
					results[idx].Status, statuses[issueNum] = "aborted", "aborted"
					abortedCount++
				}
			}
			rowTerminal := func() bool {
				mu.Lock()
				defer mu.Unlock()
				return events.RunStatusFromPayload(results[idx].Status).IsTerminal()
			}

			// parentCtx is the RunBatch ctx — it is only
			// cancelled by an external abort (e.g. parent ctx
			// cancellation), not by the per-issue abort or the
			// session's normal end. The supervisor in execute
			// uses parentCtx to decide whether to shut down the
			// process; a normal session end (no parent abort)
			// leaves the process alone.
			parentCtx := ctx

			turnAdvanced := false
			advanceTurn := func() {
				if effectiveParallel != 1 {
					return
				}
				turnMu.Lock()
				if turnAdvanced {
					turnMu.Unlock()
					return
				}
				turnAdvanced = true
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
			if req.ReadyContinuations[issueNum] || recoveringRow && !req.RecoveryWaits[issueNum].InitialAdmission {
				// Observation is slot-free. Started waiting rows compete through
				// the start gate only when their selected action needs execution.
				advanceTurn()
			}
			var initialOwner *waitOwner
			if !claimedStates[runID].HasStarted() {
				initialBatchID := issueBatchID
				if initialBatchID == "" {
					initialBatchID = batchIDFromRunID(runID)
					if initialBatchID == "" {
						initialBatchID = runID
					}
				}
				initialBaseBranch := req.BaseBranches[issueNum]
				if initialBaseBranch == "" {
					initialBaseBranch = baseBranch
				}
				record := daemon.RunWait{Protocol: "run-wait/v1", RunID: runID, BatchID: initialBatchID, Issue: issueNum, Branch: req.Branches[issueNum], BaseBranch: initialBaseBranch, InitialAdmission: true, AdmissionMode: int(req.IssueMode(issueNum)), Dependencies: append([]int(nil), blockers...), ReuseSession: req.ReuseSession[issueNum], Ready: true, OperationID: "admission", PreviousRunID: req.PreviousRunIDs[issueNum], PreviousBatchID: req.PreviousRunBatchIDs[issueNum]}
				var err error
				initialOwner, err = newWaitOwnerWithFailure(layout.BatchDir(initialBatchID), record, recoveryNow, o.eventLog, ownerPulse(runID), leaseFailure)
				if err != nil {
					o.logAborted(issueNum, runID, nil)
					mu.Lock()
					results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted"}
					statuses[issueNum] = "aborted"
					abortedCount++
					mu.Unlock()
					return
				}
				trackWaitOwner(initialOwner)
				defer func() {
					if initialOwner != nil && rowTerminal() {
						initialOwner.close()
					}
				}()
			}

			abortedBy := make([]int, 0, len(blockers))
			stillBlockedBy := make([]int, 0, len(blockers))
			pendingBy := make([]int, 0, len(blockers))
			for _, blocker := range blockers {
				if err := issueCtx.Err(); err != nil {
					if ctx.Err() == nil {
						break
					}
					<-completed[blocker]
				} else {
					select {
					case <-completed[blocker]:
					case <-yielded[blocker]:
						// Keep the dependent registered until its prerequisite's
						// terminal transition. Only its serial turn is released;
						// yielding must not return the dependent as queued.
						advanceTurn()
						select {
						case <-completed[blocker]:
						case <-issueCtx.Done():
						}
					case <-issueCtx.Done():
					}
				}
				if issueCtx.Err() != nil {
					if ctx.Err() == nil {
						break
					}
					// Whole-batch abort also stops the prerequisite. Preserve its
					// settled abort identity for the dependent's cascade evidence.
					<-completed[blocker]
				}
				mu.Lock()
				status := statuses[blocker]
				mu.Unlock()
				blockerStatus := events.RunStatusFromPayload(status)
				switch {
				case blockerStatus.IsAborted():
					abortedBy = append(abortedBy, blocker)
				case blockerStatus.IsSuccess():
					state, err := fetchIssueState(issueCtx, o.githubClient, blocker)
					if err != nil || !strings.EqualFold(state, "closed") {
						stillBlockedBy = append(stillBlockedBy, blocker)
					}
				case blockerStatus.IsTerminal() && !blockerStatus.IsSuccess():
					stillBlockedBy = append(stillBlockedBy, blocker)
				default:
					// Unfinished or unknown prerequisite evidence cannot authorize
					// execution. Keep this initial admission and its durable edges.
					pendingBy = append(pendingBy, blocker)
				}
			}
			if issueCtx.Err() != nil {
				o.logAborted(issueNum, runID, abortedBy)
				mu.Lock()
				results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
				statuses[issueNum] = "aborted"
				abortedCount++
				mu.Unlock()
				return
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
				logBlocked(o.eventLog, issueNum, stillBlockedBy, runID, issueBatchID)

				mu.Lock()
				results[idx] = res
				statuses[issueNum] = res.Status
				mu.Unlock()
				return
			}
			if len(pendingBy) > 0 {
				mu.Lock()
				results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), RunID: runID, Status: "queued", Branch: req.Branches[issueNum]}
				statuses[issueNum] = "queued"
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

			if effectiveParallel == 1 && !turnAdvanced {
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

			mode := req.IssueMode(issueNum)
			renderCfg := req.PromptConfig
			if mode == ModeContinue {
				if taskPrompt, ok := req.TaskPrompts[issueNum]; ok {
					renderCfg.TaskPrompt = taskPrompt
				}
			}
			issueBaseBranch := baseBranch
			if mode == ModeContinue || recoveringRow {
				if perIssueBaseBranch, ok := req.BaseBranches[issueNum]; ok && strings.TrimSpace(perIssueBaseBranch) != "" {
					issueBaseBranch = perIssueBaseBranch
				}
			}

			row := RowSpec{
				IssueNumber:         issueNum,
				Mode:                mode,
				Branches:            req.Branches,
				PreviousRunIDs:      req.PreviousRunIDs,
				PreviousRunBatchIDs: req.PreviousRunBatchIDs,
				RunID:               runID,
				ReuseSession:        req.ReuseSession[issueNum],
				BaseBranch:          issueBaseBranch,
				ExternalBlockers:    req.Blocked[issueNum],
				RenderCfg:           renderCfg,
				OutputWriter:        req.OutputWriter,
				RunTS:               req.RunTS,
				RunShortID:          req.RunShortID,
				BatchID:             issueBatchID,
				QualityRulesFile:    req.QualityRulesFile,
			}
			recovery, recovering := req.RecoveryWaits[issueNum]
			if recovering && !recovery.InitialAdmission {
				row.UsageLimitProbe = recovery.UsageLimitProbe
				if recovery.UsageLimitProbe {
					evidence := quotaPollingEvidence(claimedStates[runID])
					if waited, ok := quotaPollingWaited(evidence["usage_limit_waited_seconds"]); ok {
						row.UsageLimitWaited = waited
					} else {
						row.UsageLimitWaited = usageLimitRetryWindow
					}
					if seconds, ok := lifecycleDeadlineSeconds(evidence["usage_limit_deadline_unix_seconds"]); ok {
						row.UsageLimitDeadline = time.Unix(seconds, 0)
					}
				}
				if !recovery.Ready && claimedStates[runID].AwaitEvent != nil && o.eventLog != nil {
					payload := cloneLifecycleExtras(claimedStates[runID].AwaitEvent.Payload)
					payload["batch_id"], payload["recovered"] = issueBatchID, true
					if err := o.eventLog.Log(events.Event{Type: "run.await", Timestamp: time.Now().UTC(), RunID: runID, Issue: issueNum, IssueRef: issueRef(issueNum), Payload: payload}); err != nil {
						fmt.Fprintf(o.errorLog, "persist recovered wait ownership for run %s: %v\n", runID, err)
						o.logAborted(issueNum, runID, nil)
						mu.Lock()
						results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted"}
						statuses[issueNum] = "aborted"
						abortedCount++
						mu.Unlock()
						return
					}
				}
			}
			bc := BatchConfig{
				Cfg:                        cfg,
				AgentName:                  agentName,
				AgentCfg:                   agentCfg,
				Variant:                    variant,
				IdentityResolver:           batchIdentityResolver,
				Parallel:                   parallel,
				StartDelay:                 startDelay,
				Retries:                    retries,
				RunIdleTimeout:             runIdleTimeout,
				ContextRolloverLiterals:    cfg.ContextErrorPhrases,
				SandboxMode:                sandboxMode,
				ContainerCapacity:          containerCapacityForLog,
				ContainerCapacitySet:       req.ContainerCapacitySet,
				MaxContainers:              maxContainersForLog,
				MaxContainersSet:           req.MaxContainersSet,
				DangerouslySkipPermissions: *dangerouslySkipPermissions,
				StrandedReconcile:          strandedReconcile,
			}
			clock := func() time.Time {
				if o.runSessionOpts.now != nil {
					return o.runSessionOpts.now().UTC()
				}
				return time.Now().UTC()
			}
			waitBatchID := issueBatchID
			if waitBatchID == "" {
				waitBatchID = batchIDFromRunID(runID)
				if waitBatchID == "" {
					waitBatchID = runID
				}
			}
			waitBranch := row.Branches[issueNum]
			if waitBranch == "" {
				waitBranch = claimedStates[runID].Branch()
			}
			ownerRecord := daemon.RunWait{
				Protocol: "run-wait/v1", RunID: runID, BatchID: waitBatchID, Issue: issueNum,
				Branch: waitBranch, BaseBranch: issueBaseBranch, InitialAdmission: !claimedStates[runID].HasStarted(), AdmissionMode: int(row.Mode), OperationID: "admission", Ready: true,
				PreviousRunID: row.PreviousRunIDs[issueNum], PreviousBatchID: row.PreviousRunBatchIDs[issueNum],
				Dependencies: append([]int(nil), blockers...),
				ReuseSession: row.ReuseSession,
			}
			if recovering {
				ownerRecord = recovery
				ownerRecord.BatchID = waitBatchID
			}
			owner, ownerErr := newWaitOwnerWithFailure(layout.BatchDir(waitBatchID), ownerRecord, clock, o.eventLog, ownerPulse(runID), leaseFailure)
			if ownerErr != nil {
				o.logAborted(issueNum, runID, nil)
				mu.Lock()
				results[idx] = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted"}
				statuses[issueNum] = "aborted"
				abortedCount++
				mu.Unlock()
				return
			}
			trackWaitOwner(owner)
			defer func() {
				if rowTerminal() {
					owner.close()
				}
			}()
			if initialOwner != nil {
				initialOwner.close()
				initialOwner = nil
			}
			var res AgentRunResult
			var started bool
			awaitPoll := 0
			usageLimitWaited := row.UsageLimitWaited
			defer func() {
				if err := coord.stopCommandServer(issueNum); err != nil {
					fmt.Fprintf(o.errorLog, "error: stop command server for issue %d: %v\n", issueNum, err)
				}
			}()
			executor := o.newRunExecutorWith(parentCtx, bc, policy.sandboxFactory, policy.containerAlloc, coord, coord, layout)
			executor.deps.quotaProgress = quotaGate.modelProgress
			executor.deps.quotaLimit = quotaGate.limit
			awaiting := req.ReadyContinuations[issueNum] || recovering && !recovery.InitialAdmission
			readyContinuation := req.ReadyContinuations[issueNum]
			var opportunity awaitOpportunity
			markQuotaProbeReady := func() error {
				row.UsageLimitProbe = true
				row.UsageLimitWaited = min(usageLimitWaited, usageLimitRetryWindow)
				row.UsageLimitDeadline = clock().Add(max(0, usageLimitRetryWindow-row.UsageLimitWaited))
				extras := map[string]any{
					"gate": "usage-limit", "reason": "quota-probe-ready", "next_action": "recheck provider availability and continue through configured ordinary retries when polling is consumed",
					"usage_limit_probe": true, "usage_limit_waited_seconds": int(row.UsageLimitWaited / time.Second), "usage_limit_deadline_unix_seconds": row.UsageLimitDeadline.Unix(),
				}
				if err := logCapacityQueuedContinuationAt(o.eventLog, clock(), runID, issueNum, issueBatchID, row, extras, req.IssueTitles[issueNum]); err != nil {
					// Failed accounting cannot renew a quota allowance or veto
					// the already available ordinary recovery attempts.
					usageLimitWaited, row.UsageLimitWaited = usageLimitRetryWindow, usageLimitRetryWindow
					row.ReuseSession = false
					if o.errorLog != nil {
						fmt.Fprintf(o.errorLog, "warning: persist completed quota poll for run %s; continuing without further quota waits: %v\n", runID, err)
					}
				}
				awaiting, readyContinuation = true, true
				return owner.checkpoint(row, true, 0)
			}
			waitForObservation := func() error {
				interval := awaitPollInterval(o.runSessionOpts, awaitPoll)
				if recovering && recovery.NextPollAt.After(clock()) {
					interval = recovery.NextPollAt.Sub(clock())
					recovering = false
				}
				if err := owner.checkpoint(row, false, interval); err != nil {
					return err
				}
				awaitPoll++
				awaitWait := o.runSessionOpts.awaitWait
				if awaitWait == nil {
					awaitWait = waitForAwaitPoll
				}
				return awaitWait(issueCtx, interval)
			}
			for {
				if row.UsageLimitProbe {
					status, extras, handled := executor.observeLifecycle(issueCtx, row)
					if handled && (status == "success" || extras["reason"] == "PULL_REQUEST_CLOSED" || extras["completion"] != nil) {
						res = executor.finishObserved(issueCtx, row, status, extras)
						quotaGate.report(issueNum, res, true)
						break
					}
					if recovering && recovery.NextPollAt.After(clock()) {
						interval := recovery.NextPollAt.Sub(clock())
						wait := o.runSessionOpts.awaitWait
						if wait == nil {
							wait = waitForAwaitPoll
						}
						if err := wait(issueCtx, interval); err != nil {
							o.logAborted(issueNum, runID, nil)
							res.Status = "aborted"
							break
						}
						usageLimitWaited += interval
						if err := markQuotaProbeReady(); err != nil {
							o.logAborted(issueNum, runID, nil)
							res.Status = "aborted"
							break
						}
						recovering = false
					}
				}
				if awaiting && !readyContinuation && !row.UsageLimitProbe {
					status, extras, handled := executor.observeLifecycle(issueCtx, row)
					if issueCtx.Err() != nil {
						o.logAborted(issueNum, runID, nil)
						res = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
						break
					}
					if handled && status == "await" {
						if err := waitForObservation(); err != nil {
							o.logAborted(issueNum, runID, nil)
							res = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
							break
						}
						continue
					}
					if !handled {
						status = "failure"
						extras = lifecycleGateFailureEvidence(idleGateReason, idleGateNextAction, lifecycleGateNone, nil, "")
					}
					if status != "resume" {
						res = executor.finishObserved(issueCtx, row, status, extras)
						break
					}
					if o.eventLog != nil {
						if err := logCapacityQueuedContinuationAt(o.eventLog, executor.deps.runSessionOpts.runtimeNow(), runID, issueNum, issueBatchID, row, extras, req.IssueTitles[issueNum]); err != nil {
							if o.errorLog != nil {
								fmt.Fprintf(o.errorLog, "warning: persist ready continuation for issue %d: %v; keeping the run in its external wait\n", issueNum, err)
							}
							if waitErr := waitForObservation(); waitErr != nil {
								o.logAborted(issueNum, runID, nil)
								res = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
								break
							}
							continue
						}
					}
					readyContinuation = true
					if err := owner.checkpoint(row, true, 0); err != nil {
						o.logAborted(issueNum, runID, nil)
						res.Status = "aborted"
						break
					}
				}
				if !row.UsageLimitProbe && quotaGate.paused() {
					extras := map[string]any{
						"gate":        "usage-limit",
						"reason":      "usage-limit-paused",
						"next_action": "resume after provider usage limit resets; Sandman did not start another run while suspended",
					}
					if o.eventLog != nil && (!awaiting || readyContinuation) {
						if err := logCapacityQueuedContinuationAt(o.eventLog, executor.deps.runSessionOpts.runtimeNow(), runID, issueNum, issueBatchID, row, extras, req.IssueTitles[issueNum]); err != nil {
							if o.errorLog != nil {
								fmt.Fprintf(o.errorLog, "warning: persist usage-limit pause for issue %d: %v\n", issueNum, err)
							}
						}
					}
					advanceTurn()
					if err := owner.checkpoint(row, !awaiting || readyContinuation, 0); err != nil {
						o.logAborted(issueNum, runID, nil)
						res.Status = "aborted"
						break
					}
					if !o.runSessionOpts.releaseAwaitCapacity {
						// Yielding executors return unfinished intent to their
						// caller; an active quota owner still blocks new launches.
						status := "queued"
						if awaiting {
							status = "await"
						}
						res = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: status, Branch: row.Branches[issueNum]}
						break
					}
					var quotaTerminalStatus string
					var quotaTerminalExtras map[string]any
					quotaTerminal := errors.New("terminal observation during quota admission")
					var quotaObserve func() error
					if awaiting {
						quotaObserve = func() error {
							status, extras, handled := executor.observeLifecycle(issueCtx, row)
							if issueCtx.Err() != nil {
								return issueCtx.Err()
							}
							if handled && status == "await" && readyContinuation {
								if err := executor.persistObservedAwait(issueCtx, row, extras); err != nil {
									return err
								}
								readyContinuation = false
								if err := owner.checkpoint(row, false, awaitPollInterval(o.runSessionOpts, awaitPoll)); err != nil {
									return err
								}
							}
							if handled && status != "resume" && status != "await" {
								quotaTerminalStatus, quotaTerminalExtras = status, extras
								return quotaTerminal
							}
							return nil
						}
					}
					if err := quotaGate.waitObserved(issueCtx, quotaObserve, awaitPollInterval(o.runSessionOpts, awaitPoll)); err != nil {
						if errors.Is(err, quotaTerminal) {
							res = executor.finishObserved(issueCtx, row, quotaTerminalStatus, quotaTerminalExtras)
							break
						}
						if issueCtx.Err() != nil {
							o.logAborted(issueNum, runID, nil)
							res = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
						} else {
							res = executor.finishObserved(issueCtx, row, "failure", map[string]any{"reason": "AGENT_USAGE_LIMIT", "next_action": "resume with available provider quota after the exhausted recovery window"})
						}
						break
					}
					continue
				}
				var err error
				var admissionStatus string
				var admissionExtras map[string]any
				admissionChanged := errors.New("lifecycle changed during execution admission")
				if awaiting && readyContinuation && !row.UsageLimitProbe {
					observe := func() error {
						status, extras, handled := executor.observeLifecycle(issueCtx, row)
						if issueCtx.Err() != nil {
							return issueCtx.Err()
						}
						if !handled {
							status = "failure"
							extras = lifecycleGateFailureEvidence(idleGateReason, idleGateNextAction, lifecycleGateNone, nil, "")
						}
						if status != "resume" {
							admissionStatus, admissionExtras = status, extras
							return admissionChanged
						}
						return nil
					}
					opportunity, err = startGate.acquireObserved(issueCtx, &batchStartWaiter{awaiting: true, lastChance: opportunity.lastChance, ordinaryStartsAtChance: opportunity.ordinaryStarts}, observe, awaitPollInterval(o.runSessionOpts, awaitPoll))
				} else if awaiting {
					opportunity, err = startGate.AcquireAwaiting(issueCtx, opportunity)
				} else {
					opportunity, err = startGate.AcquireWithOpportunity(issueCtx, false)
				}
				if errors.Is(err, admissionChanged) {
					if admissionStatus == "await" {
						if err := executor.persistObservedAwait(issueCtx, row, admissionExtras); err != nil {
							o.logAborted(issueNum, runID, nil)
							res.Status = "aborted"
							break
						}
						readyContinuation = false
						if err := waitForObservation(); err != nil {
							o.logAborted(issueNum, runID, nil)
							res.Status = "aborted"
							break
						}
						continue
					}
					res = executor.finishObserved(issueCtx, row, admissionStatus, admissionExtras)
					break
				}
				if err != nil {
					o.logAborted(issueNum, runID, nil)
					res = AgentRunResult{IssueNumber: issueNum, Issue: issueRef(issueNum), Status: "aborted", Branch: req.Branches[issueNum]}
					break
				}
				// Quota can close while an ordinary row is blocked inside Acquire.
				// Revalidate at the actual launch boundary, without pacing a start
				// that never happened.
				if !row.UsageLimitProbe && quotaGate.paused() {
					startGate.ReleaseWithoutDelay()
					continue
				}
				res, started = executor.Execute(issueCtx, row)
				if res.Branch != "" {
					row.Branches = map[int]string{issueNum: res.Branch}
				}
				quotaGate.report(issueNum, res, row.UsageLimitProbe)
				if !res.UsageLimitReached {
					row.UsageLimitProbe = false
					row.UsageLimitDeadline = time.Time{}
					usageLimitWaited, row.UsageLimitWaited = 0, 0
				}
				if started {
					startGate.Release()
				} else {
					startGate.ReleaseWithoutDelay()
				}
				if res.Status == "await" && !o.runSessionOpts.releaseAwaitCapacity {
					interval := awaitPollInterval(o.runSessionOpts, awaitPoll)
					if res.UsageLimitReached {
						row.UsageLimitProbe, row.UsageLimitDeadline = true, res.UsageLimitDeadline
						interval = usageLimitPollInterval
					}
					if err := owner.checkpoint(row, false, interval); err != nil {
						o.logAborted(issueNum, runID, nil)
						res.Status = "aborted"
					}
				}
				if res.Status != "await" || !o.runSessionOpts.releaseAwaitCapacity {
					break
				}
				if !yieldedCapacity {
					close(yielded[issueNum])
					yieldedCapacity = true
				}
				advanceTurn()
				if res.UsageLimitReached {
					interval := usageLimitPollInterval
					row.UsageLimitDeadline = res.UsageLimitDeadline
					row.UsageLimitProbe = true
					if err := owner.checkpoint(row, false, interval); err != nil {
						o.logAborted(issueNum, runID, nil)
						res.Status = "aborted"
						break
					}
					awaitWait := o.runSessionOpts.awaitWait
					if awaitWait == nil {
						awaitWait = waitForAwaitPoll
					}
					if err := awaitWait(issueCtx, interval); err != nil {
						o.logAborted(issueNum, runID, nil)
						res.Status = "aborted"
						break
					}
					usageLimitWaited += interval
					row.Mode = ModeContinue
					row.PreviousRunIDs = map[int]string{issueNum: runID}
					row.PreviousRunBatchIDs = map[int]string{issueNum: issueBatchID}
					row.ReuseSession = true
					row.UsageLimitProbe = true
					row.UsageLimitDeadline = res.UsageLimitDeadline
					row.UsageLimitWaited = usageLimitWaited
					if err := markQuotaProbeReady(); err != nil {
						o.logAborted(issueNum, runID, nil)
						res.Status = "aborted"
						break
					}
					awaiting = true
					continue
				}
				if err := waitForObservation(); err != nil {
					o.logAborted(issueNum, runID, nil)
					res.Status = "aborted"
					break
				}
				row.Mode = ModeContinue
				row.PreviousRunIDs = map[int]string{issueNum: runID}
				row.PreviousRunBatchIDs = map[int]string{issueNum: issueBatchID}
				row.ReuseSession = true
				awaiting = true
				readyContinuation = false
				continue
			}
			mu.Lock()
			if issueCtx.Err() != nil && !events.RunStatusFromPayload(res.Status).IsTerminal() {
				o.logAborted(issueNum, runID, nil)
				res.Status = "aborted"
			}
			results[idx] = res
			statuses[issueNum] = res.Status
			resStatus := events.RunStatusFromPayload(res.Status)
			if resStatus.IsFailure() {
				failureCount++
			}
			if resStatus.IsAborted() {
				abortedCount++
			}
			mu.Unlock()
		}(inputIndex[num], num, dependencies[num], turn, runID, queueWriteFailed)
	}

	wg.Wait()
	if ctx.Err() != nil {
		// The batch retains RunID claims through this final cancellation fence.
		// A returned await/initial queue is still unfinished owned intent;
		// preserve terminal siblings and prevent later grace-based reclamation.
		for i := range results {
			if events.RunStatusFromPayload(results[i].Status).IsTerminal() {
				continue
			}
			issue := req.Issues[i]
			id := strings.TrimSpace(req.RunIDs[issue])
			if id == "" {
				id = buildRunID(issue, req.RunTS, req.RunShortID)
			}
			o.logAborted(issue, id, nil)
			results[i].Status = "aborted"
			statuses[issue] = "aborted"
			abortedCount++
		}
	}

	if policy.mode == "docker" || policy.mode == "podman" {
		for _, result := range results {
			if strings.TrimSpace(result.Branch) == "" {
				continue
			}
			worktreePath := filepath.Join(layout.WorktreeDir, result.Branch)
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

	o.badgeHooker.MaybeSuggestBadge(ctx, results)

	if abortedCount > 0 {
		return &Result{Runs: results}, fmt.Errorf("%d of %d runs aborted: %w", abortedCount, len(req.Issues), ErrAborted)
	}
	if failureCount > 0 {
		return &Result{Runs: results}, fmt.Errorf("%d of %d runs failed", failureCount, len(req.Issues))
	}

	return &Result{Runs: results}, nil
}

func (o *Orchestrator) resolveSandboxExecutionPolicy(ctx context.Context, cfg *config.Config, agentCfg config.Agent, req Request, sandboxMode string) (*sandboxExecutionPolicy, error) {
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
	expectedDefaultAgent, requiredAgents := dockerfileAgentExpectations(defaultAgent, req.Agent, agentCfg)
	if err := scaffold.ValidateDockerfileMetadata(".", cfg.BuildTools, expectedDefaultAgent, requiredAgents); err != nil {
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

	cleanup, err := PrepareContainerConfigMounts(ctx, ".", req.RunDir, &startOpts, o.lookupGHToken)
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

func logBlocked(eventLog events.EventLog, issueNum int, blockers []int, runID string, batchID string) {
	if eventLog == nil {
		return
	}
	payload := map[string]any{"blocked_by": blockers}
	if batchID != "" {
		payload["batch_id"] = batchID
	}
	_ = eventLog.Log(events.Event{
		Type:      "run.blocked",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     issueNum,
		IssueRef:  issueRef(issueNum),
		Payload:   payload,
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
	if err := o.eventLog.Log(events.Event{
		Type:      "run.aborted",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     issueNum,
		IssueRef:  issueRef(issueNum),
		Payload:   payload,
	}); err != nil {
		fmt.Fprintf(o.errorLog, "event log write failed: run.aborted (issue=%d run=%s): %v\n", issueNum, runID, err)
	}
}

// mapRetryReason picks the closed-vocabulary reason for a run.retry emit
// from the previous attempt's status, the heartbeat-trips signal, and the
// parent context. The vocabulary (agent-stalled, agent-failed,
// sandbox-timeout, kill-timeout, manual, context-exhausted) is locked in
// ADR-0030 and the context-rollover contract
// and must not be silently extended. If a future code path
// surfaces a status that does not map to a known arm, the function
// panics so the new condition is added to the ADR and the mapping
// explicitly, rather than collapsing to an empty string that violates
// the slice-3 contract "never null, never empty" (#1501 acceptance #3).
func mapRetryReason(previousStatus string, abortedByHeartbeat bool, parentCtx context.Context) string {
	switch previousStatus {
	case "failure":
		return "agent-failed"
	case "aborted":
		if abortedByHeartbeat {
			return "agent-stalled"
		}
		if parentCtx != nil && parentCtx.Err() != nil {
			return "kill-timeout"
		}
	}
	panic(fmt.Sprintf("mapRetryReason: unmapped previous_status=%q abortedByHeartbeat=%v; add a vocabulary arm via ADR-0035", previousStatus, abortedByHeartbeat))
}

// logRetry writes a run.retry event at the top of a retry iteration. It is
// called from runOnce for both the issue-driven and prompt-only loops, with
// attempt (1-indexed, the about-to-start attempt), maxAttempts, and the
// status of the previous iteration passed through verbatim. branch is the
// run's branch; logPath is the per-run log file the heartbeat and the retry
// event both tail. issueNumber == 0 denotes a prompt-only run, matching the
// existing prompt-only convention (issue: 0 in the JSON payload). No-op when
// the orchestrator has no event log.
func logRetry(eventLog events.EventLog, runID, branch string, attempt, maxAttempts int, previousStatus, reason, logPath string, issueNumber int) {
	if eventLog == nil {
		return
	}
	event := events.Event{
		Type:      "run.retry",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     issueNumber,
		Payload: map[string]any{
			"attempt":         attempt,
			"max_attempts":    maxAttempts,
			"previous_status": previousStatus,
			"reason":          reason,
			"branch":          branch,
			"last_log_lines":  readRetryLogLines(logPath, 3),
		},
	}
	if issueNumber > 0 {
		event.IssueRef = issueRef(issueNumber)
	}
	_ = eventLog.Log(event)
}

// dockerfileAgentExpectations returns what a container run requires of the
// Dockerfile metadata. The image must install the run agent's preset. The
// default-agent header is compared with config only when the run uses the
// config default agent: a run that selects another agent with --agent does not
// depend on the default agent at all. Preset-less custom providers add no
// install requirement because Sandman cannot know which binary they run.
func dockerfileAgentExpectations(defaultAgent, runAgent string, runAgentCfg config.Agent) (string, []string) {
	var required []string
	if runAgentCfg.Preset != "" {
		required = []string{runAgentCfg.Preset}
	}
	if runAgent = strings.TrimSpace(runAgent); runAgent != "" && runAgent != defaultAgent {
		return "", required
	}
	return defaultAgent, required
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
// (including the context detector literals and Task writer), together with the
// shared baseBranchSyncMu mutex that gates syncBaseBranch. The function and
// timeout fields exist so tests in this package can override behaviour that
// would otherwise touch the network, run real git, or sleep for the production
// timeout. The baseBranchSyncMu pointer is initialised once per Orchestrator
// in NewOrchestrator and shared across all sessions via this struct's value
// copy at construction time, so that concurrent calls to syncBaseBranch
// serialise on the same mutex. If baseBranchSyncMu is ever converted from a
// pointer to a value type, update runSingle / runPromptOnlySingle to share it
// explicitly — otherwise serialisation will silently break.
type runSessionOptions struct {
	waitOwnerPulse             func(string) <-chan time.Time
	now                        func() time.Time
	baseBranchSync             func(repoPath, sourceBranch string) error
	baseBranchSyncMu           *sync.Mutex
	contextRolloverLiterals    []string
	contextRolloverLiteralsSet bool
	taskWriter                 func(string, []byte, os.FileMode) error
	retryReset                 func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error
	killTimeout                time.Duration
	currentHead                func(workDir string) (string, error)
	// lifecyclePollPlan and lifecycleWait keep foreground lifecycle observation
	// deterministic in tests. Production uses the implementation review plan
	// and a context-aware timer when these hooks are unset.
	lifecyclePollPlan    []time.Duration
	lifecycleWait        func(context.Context, time.Duration) error
	awaitWait            func(context.Context, time.Duration) error
	startWaiterQueued    func(bool)
	foregroundLifecycle  bool
	releaseAwaitCapacity bool
	// awaitResumeMax bounds in-session agent relaunches triggered by a
	// resume-worthy PR gate (ready-to-merge / actionable-feedback) within
	// one session. Zero uses the default (3); when the cap is exhausted the
	// run fails with a remediation-budget diagnostic rather than waiting on
	// already-resolved or implementor-owned work.
	// Re-invocation starts a fresh session, so the cap resets per session.
	awaitResumeMax int
	// Review registration seams keep the completion boundary deterministic in
	// batch tests while production uses the file-backed store and wall clock.
	reviewRegistrationStore reviewRegistrationStore
	reviewRegistrationNow   func() time.Time
}

// runSession owns the per-AgentRun state and lifecycle for a single issue
// (or prompt-only) execution. It is private to the orchestrator package and
// is built by runSingle / runPromptOnlySingle. A session is short-lived: it
// lives for one execute call and is discarded on return.
type runSession struct {
	deps      runDeps
	coord     runCoordination
	commander daemon.IssueCommander

	// Inputs captured from the runSingle / runPromptOnlySingle call site.
	issueNumber                int
	issueState                 string
	cfg                        *config.Config
	agentName                  string
	agentCfg                   config.Agent
	variant                    string
	mode                       IssueMode
	previousRunIDs             map[int]string
	previousRunBatchIDs        map[int]string
	reuseSession               bool
	usageLimitProbe            bool
	usageLimitWaited           time.Duration
	usageLimitDeadline         time.Time
	usageLimitRestoreErr       error
	identityResolver           *gitIdentityResolver
	branches                   map[int]string
	renderCfg                  prompt.RenderConfig
	outputWriter               io.Writer
	sbFactory                  SandboxFactory
	containerAlloc             containerAllocator
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
	strandedReconcile          bool
	// parentCtx is the RunBatch ctx. The supervisor in execute
	// uses it to decide whether the session is being externally
	// aborted (parent ctx fired) versus ending normally (parent
	// ctx alive). ctx (the parameter to execute) is the per-issue
	// ctx, which is cancelled in both cases — by external abort
	// AND by the deferred issueCancel on normal return — so it
	// cannot be used to distinguish the two.
	parentCtx context.Context

	// runID is an optional batch-level identifier for prompt-only runs.
	// When non-empty, it is used as the run directory name and as the
	// RunID in run.started events instead of an auto-generated fallback.
	runID string

	// batchID is the per-batch directory name used to scope the run folder
	// under <batchesDir>/<batchID>/runs/<runID>. For issue-driven runs it is
	// supplied by RunBatch from the selected issue set; for prompt-only runs it
	// comes from (batchTS, batchShortID). Empty is treated as a guard failure by
	// the execute path (returns AgentRunResult{Status: "failure", ...}).
	batchID string

	// batchTS and batchShortID are the timestamp and short-id components
	// of the auto-generated batch id for prompt-only runs. Used to
	// construct the per-row RunID in run.started events when runID is
	// empty.
	batchTS      string
	batchShortID string

	// runTS and runShortID are the timestamp and short-id components of
	// the auto-generated batch id for issue-driven runs. Populated from
	// batch.Request.RunTS / RunShortID by runSingle; consumed by
	// buildRunID in execute to produce the per-row RunID for
	// run.started / run.continued events.
	runTS      string
	runShortID string

	// userProvidedRunID is the original user-provided --run-id value
	// (empty if not provided). Used to construct the subject for the
	// per-row RunID in run.started events.
	userProvidedRunID string

	// review, prNumber and reviewFocus mark the session as a review-agent
	// run. They are sourced from batch.Request and propagated into the
	// run.started and run.finished event payloads so the event log and
	// portal can distinguish review runs from implementation runs. They
	// are only set on prompt-only sessions; issue-driven sessions always
	// leave them at zero values.
	review       bool
	prNumber     int
	reviewFocus  string
	portalHidden bool
	// qualityRulesFile is the host-absolute path of the
	// `.sandman/reviews/quality-rules.md` file the daemon has just
	// materialised. The session copies the file into the per-row
	// worktree at `.sandman/reviews/quality-rules.md` after the sandbox
	// starts, so the relative path the review prompt points at resolves
	// inside the agent's CWD. Empty for non-review runs.
	qualityRulesFile string

	// opts carries the test-injection hooks copied from
	// Orchestrator.runSessionOpts at session construction. Zero-valued in
	// production; populated by tests to drive the per-session behaviour.
	opts runSessionOptions

	reviewRegistrationStore     reviewRegistrationStore
	reviewRegistrationNow       func() time.Time
	reviewAttemptStartedAt      time.Time
	reviewRegistrationAttempted bool
	reviewRegistrationObserved  bool

	// resumeCount counts in-session agent relaunches triggered by a
	// resume-worthy PR gate within this session (bounded by
	// runSessionOptions.awaitResumeMax). It is the per-session cap state;
	// re-invocation starts a fresh session and a fresh counter.
	resumeCount int

	modelProgress func()
	usageLimit    func()

	// lifecyclePRSnapshot carries the last live PR observation made by the
	// closing-reference guard into the authoritative lifecycle decision. This
	// avoids a second lookup that could observe a different lifecycle state.
	lifecyclePRSnapshot      *github.PR
	lifecycleAlreadyResolved bool
	lifecycleTerminal        bool
}

func (s *runSession) worktreeDir() string {
	if s.deps.layout.RepoRoot != "" {
		return s.deps.layout.WorktreeDir
	}
	if s.cfg != nil {
		return strings.TrimSpace(s.cfg.WorktreeDir)
	}
	return ""
}

// copyQualityRulesIntoWorktree copies the host-materialised
// `.sandman/reviews/quality-rules.md` (the daemon's authoritative copy) into
// the per-row review worktree so the agent can read it at the relative
// path the review prompt points at. Called after wt.Start succeeds, only
// for review runs.
//
// The file copy is best-effort: a missing source is logged and skipped so
// the agent can still render the canonical "Quality rules unavailable in
// this repository" verdict. Write errors are also non-fatal — the prompt
// already documents the absent-file fallback.
func (s *runSession) copyQualityRulesIntoWorktree(branch string) error {
	if !s.review {
		return nil
	}
	src := strings.TrimSpace(s.qualityRulesFile)
	if src == "" {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", src, err)
	}
	wtPath := filepath.Join(s.worktreeDir(), branch)
	targetDir := filepath.Join(wtPath, ".sandman", "reviews")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", targetDir, err)
	}
	target := filepath.Join(targetDir, "quality-rules.md")
	if err := atomicfs.WriteAtomic(target, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

// orchestratorWorktreeDir resolves the worktree base directory
// for orchestrator-level code (outside a runSession) like the
// post-run cleanup walker.
func (o *Orchestrator) orchestratorWorktreeDir(cfg *config.Config) string {
	if o.layout.RepoRoot != "" {
		return o.layout.WorktreeDir
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.WorktreeDir)
	}
	return ""
}

// runFolderFor returns the per-run folder path under the batch for the
// given per-row runID. Replaces the legacy join that collapsed to
// <batchesDir>/runs/<runID> when s.runID was empty (issue-driven runs).
// When s.batchID is empty (e.g. legacy callers that did not pass
// runTS/runShortID), it falls back to deriving a batchID from runID so
// the path is still scoped under a per-batch directory rather than
// dropping the batchID segment entirely.
func (s *runSession) runFolderFor(runID string) string {
	batchID := s.batchID
	if batchID == "" {
		batchID = batchIDFromRunID(runID)
	}
	if batchID == "" {
		return filepath.Join(s.deps.layout.BatchesDir, "runs", runID)
	}
	return s.deps.layout.RunFolder(batchID, runID)
}

// runLogPathFor returns the per-row run.log path. Mirrors runFolderFor
// so the path always routes through paths.Layout for a known batchID.
func (s *runSession) runLogPathFor(runID string) string {
	batchID := s.batchID
	if batchID == "" {
		batchID = batchIDFromRunID(runID)
	}
	if batchID == "" {
		return filepath.Join(s.deps.layout.BatchesDir, "runs", runID, "run.log")
	}
	return s.deps.layout.RunLogPath(batchID, runID)
}

// batchIDFromRunID derives a stable batch directory name from a
// per-row runID. The new runID format is `<ts>-<shortid>-<subject>`
// (runid.NewRunID); we strip the subject suffix to recover the batch
// prefix. For legacy runIDs without that prefix, the whole runID is
// returned so the path still has a batchID segment.
func batchIDFromRunID(runID string) string {
	if runID == "" {
		return ""
	}
	dashCount := 0
	for i := 0; i < len(runID); i++ {
		if runID[i] == '-' {
			dashCount++
			if dashCount == 2 {
				return runID[:i]
			}
		}
	}
	return runID
}

// snapshotOriginalTask copies the worktree's live task.md into the new
// per-row run folder as a historical snapshot before the agent overwrites
// it with the continuation prompt. Used by ModeContinue so
// the prior task wording survives in <runFolder>/task.md for the
// operator to revisit later. The function is best-effort: a missing
// source file (already warned about upstream) is treated as a no-op
// rather than a fatal error.
func snapshotOriginalTask(worktreeDir, runFolder string) error {
	if strings.TrimSpace(worktreeDir) == "" || strings.TrimSpace(runFolder) == "" {
		return fmt.Errorf("worktree dir or run folder is empty")
	}
	src := filepath.Join(worktreeDir, ".sandman", "task.md")
	content, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read source task.md: %w", err)
	}
	if err := os.MkdirAll(runFolder, 0o755); err != nil {
		return fmt.Errorf("create run folder: %w", err)
	}
	dst := filepath.Join(runFolder, "task.md")
	if err := os.WriteFile(dst, content, 0o644); err != nil {
		return fmt.Errorf("write snapshot task.md: %w", err)
	}
	return nil
}

func hasExactTaskStatus(taskContent, status string) bool {
	for _, line := range strings.Split(taskContent, "\n") {
		if strings.TrimSpace(line) == status {
			return true
		}
	}
	return false
}

// startOptsFor builds the SandboxStart value for one row's Start(opts),
// resolving the row-specific git identity and packaging it with the
// per-row mode flags plus the batch-constant stranded-reconcile.
//
// On identity-resolution failure, returns (SandboxStart{}, AgentRunResult{
// Status:"failure", Branch: branch, [IssueNumber/Issue if applicable]}, false).
// The lifecycle handles the !ok branch before calling wt.Start(opts),
// preserving the existing mode-specific failure
// semantics (was orchestrator.go:1965-1974 in the applyOverrideAndIdentity
// era; byte-identical in the characterization net).
func (s *runSession) startOptsFor(branch string) (sandbox.SandboxStart, AgentRunResult, bool) {
	identity, err := s.identityResolver.resolve()
	if err != nil {
		fmt.Fprintf(s.deps.errorLog, "error: resolve git identity for issue %d: %v\n", s.issueNumber, err)
		result := AgentRunResult{Status: "failure", Branch: branch}
		if s.issueNumber > 0 {
			result.IssueNumber = s.issueNumber
			result.Issue = issueRef(s.issueNumber)
		}
		return sandbox.SandboxStart{}, result, false
	}
	return sandbox.SandboxStart{
		Override:          s.mode == ModeOverride,
		Continue:          s.mode == ModeContinue,
		StrandedReconcile: s.strandedReconcile,
		Identity:          sandbox.SandboxIdentity{Name: identity.Name, Email: identity.Email},
	}, AgentRunResult{}, true
}

// withHeartbeat runs fn under the run-idle-timeout watchdog when
// s.runIdleTimeout > 0; any non-success result is rewritten to "aborted".
func (s *runSession) withHeartbeat(ctx context.Context, runID string, attempt int, logPath string, wt sandbox.Sandbox, fn func() AgentRunResult) (AgentRunResult, bool) {
	if s.runIdleTimeout <= 0 {
		return fn(), false
	}
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	heartbeatDone := make(chan struct{})
	var abortedByHeartbeat bool

	heartbeat := &Heartbeat{
		LogPath:      logPath,
		IdleTimeout:  time.Duration(s.runIdleTimeout) * time.Second,
		TickInterval: s.deps.heartbeatTickInterval,
	}
	heartbeat.OnIdle = func(idle time.Duration) {
		abortedByHeartbeat = true
		if s.deps.eventLog != nil {
			_ = s.deps.eventLog.Log(events.Event{
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
	if abortedByHeartbeat && !events.RunStatusFromPayload(result.Status).IsSuccess() {
		result.Status = "aborted"
	}
	return result, abortedByHeartbeat
}

// withClosingReferenceGuard repairs an open PR as soon as it becomes visible
// during an issue run. This keeps an agent from merging a body that only uses
// a non-closing reference such as "Refs #42". A merged PR is never edited:
// at that point GitHub cannot apply its auto-close behavior retroactively.
func (s *runSession) withClosingReferenceGuard(ctx context.Context, branch string, fn func() AgentRunResult) AgentRunResult {
	if s.issueNumber <= 0 || s.deps.githubClient == nil || strings.TrimSpace(branch) == "" {
		return fn()
	}

	interval := s.deps.closingGuardTickInterval
	if interval <= 0 {
		interval = time.Second
	}
	guardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait := interval
		for {
			outcome, observedPR := repairOpenPRClosingReferenceWithSnapshot(guardCtx, s.deps.githubClient, branch, s.issueNumber, s.deps.errorLog)
			if observedPR != nil {
				s.lifecyclePRSnapshot = observedPR
			}
			if outcome == closingGuardProtected || outcome == closingGuardTerminal {
				return
			}
			// A PR cannot be repaired before it exists. Back off while it is
			// absent, but preserve the configured cadence for lookup failures.
			if (outcome == closingGuardAbsent || outcome == closingGuardRetry) && wait < 30*time.Second {
				wait *= 2
				if wait > 30*time.Second {
					wait = 30 * time.Second
				}
			}
			timer := time.NewTimer(wait)
			select {
			case <-guardCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()

	result := fn()
	cancel()
	<-done
	return result
}

type closingGuardOutcome int

const (
	closingGuardRetry closingGuardOutcome = iota
	closingGuardAbsent
	closingGuardProtected
	closingGuardTerminal
)

// repairOpenPRClosingReference repairs closing intent only while the PR is
// open. It rechecks after the edit because an edit that races a merge cannot
// make GitHub auto-close the issue retroactively.
func repairOpenPRClosingReference(ctx context.Context, client github.Client, branch string, issueNumber int, errorLog io.Writer) closingGuardOutcome {
	outcome, _ := repairOpenPRClosingReferenceWithSnapshot(ctx, client, branch, issueNumber, errorLog)
	return outcome
}

func repairOpenPRClosingReferenceWithSnapshot(ctx context.Context, client github.Client, branch string, issueNumber int, errorLog io.Writer) (closingGuardOutcome, *github.PR) {
	pr, err := client.FindPRByBranch(ctx, branch)
	if err != nil {
		if github.IsRateLimited(err) {
			return closingGuardTerminal, nil
		}
		return closingGuardRetry, nil
	}
	if pr == nil {
		return closingGuardAbsent, nil
	}
	if !strings.EqualFold(pr.State, "open") || pr.Merged {
		return closingGuardTerminal, pr
	}
	body, changed := github.EnsureClosingReference(pr.Body, issueNumber)
	if !changed {
		return closingGuardProtected, pr
	}
	if err := client.EditPRBody(ctx, pr.Number, body); err != nil {
		if errorLog != nil {
			fmt.Fprintf(errorLog, "error: repair closing reference for PR #%d and issue %d: %v\n", pr.Number, issueNumber, err)
		}
		return closingGuardRetry, pr
	}
	updated, err := client.FindPRByBranch(ctx, branch)
	if err != nil {
		return closingGuardRetry, pr
	}
	if updated == nil {
		return closingGuardAbsent, pr
	}
	if updated.Merged || !strings.EqualFold(updated.State, "open") {
		if errorLog != nil {
			fmt.Fprintf(errorLog, "error: PR #%d merged while repairing closing reference for issue %d\n", pr.Number, issueNumber)
		}
		return closingGuardTerminal, updated
	}
	if updated.ClosesIssue(issueNumber) {
		return closingGuardProtected, updated
	}
	return closingGuardRetry, updated
}

// emitAwait writes a non-terminal run.await event and returns the
// await status. Unlike emitTerminal, it does not mark the run as
// finished — the run stays active and can be resumed later.
func (s *runSession) emitAwait(ctx context.Context, runID string, result AgentRunResult, extras map[string]any) string {
	if s.deps.eventLog == nil {
		return "await"
	}
	event := events.Event{
		Type:      "run.await",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     s.issueNumber,
		Payload: map[string]any{
			"await":         true,
			"branch":        result.Branch,
			"base_branch":   s.baseBranch,
			"retries_total": s.retries,
		},
	}
	if s.issueNumber > 0 {
		event.IssueRef = issueRef(s.issueNumber)
	}
	if s.portalHidden {
		event.Payload["portal_hidden"] = true
	}
	for k, v := range extras {
		event.Payload[k] = v
	}
	// The await reason mirrors the gate reason when the caller did not
	// provide an explicit await_reason, so RunState.AwaitReason() and
	// the run.await payload always agree.
	if _, ok := event.Payload["await_reason"]; !ok {
		if gate, ok := event.Payload["gate"].(string); ok && gate != "" {
			event.Payload["await_reason"] = gate
		}
	}
	_ = s.deps.eventLog.Log(event)
	return "await"
}

// emitTerminal writes the terminal run event (run.finished or run.aborted),
// rewrites the on-disk run.json snapshot so its status matches the terminal
// event, and returns the normalised status so the caller can use it without
// recomputing. Errors updating the snapshot are logged but do not change the
// run outcome. The event-log write is skipped when the orchestrator has no
// event log.
//
// extras carries extra run.finished payload keys that the caller wants to
// merge into the event (e.g. "blocker", "pr_number", "merge_conflict" — see
// issue #1684). Passing nil is fine; the standard payload keys
// ("status", "branch", "base_branch", "retries_total", etc.) are always set
// by this function.
//
// Before normalising the terminal event, emitTerminal performs a defensive
// post-check: if the agent's branch has an open PR whose mergeable state is
// `CONFLICTING`, the terminal event payload carries `merge_conflict: true`
// and the PR number. The result is reclassified as a lifecycle failure.
func (s *runSession) normalizeTerminalResult(result AgentRunResult, extras map[string]any) (AgentRunResult, map[string]any) {
	if conflictExtras, ok := s.detectConflictingPR(result.Branch); ok {
		result.Status = "failure"
		if extras == nil {
			extras = map[string]any{}
		}
		for k, v := range conflictExtras {
			extras[k] = v
		}
	}
	return result, extras
}

// emitTerminal writes a terminal event without changing the sandbox. Callers
// that own a sandbox should use finishTerminal so the event reflects cleanup.
func (s *runSession) emitTerminal(ctx context.Context, runID string, result AgentRunResult, extras map[string]any) string {
	result, extras = s.normalizeTerminalResult(result, extras)
	return s.emitNormalizedTerminal(ctx, runID, result, extras)
}

func (s *runSession) emitNormalizedTerminal(ctx context.Context, runID string, result AgentRunResult, extras map[string]any) string {
	terminalEventType, terminalStatus := terminalRunEvent(ctx, result.Status)
	s.updateRunManifestStatus(runID, batchindex.RunManifestStatus(terminalStatus))
	if s.deps.eventLog == nil {
		return terminalStatus
	}
	retriesDone := result.RetriesTotal - 1
	if retriesDone < 0 {
		retriesDone = 0
	}
	worktreeState := "preserved"
	if state, ok := extras["worktree_state"].(string); ok && state != "" {
		worktreeState = state
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
			"worktree_state": worktreeState,
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
		if s.issueNumber > 0 {
			event.Payload["issue_number"] = s.issueNumber
		}
	}
	if s.portalHidden {
		event.Payload["portal_hidden"] = true
	}
	for k, v := range extras {
		// Await/resume evidence may reach a terminal adapter after observation
		// or a legacy session cap. Keep the cause as diagnostics, not readiness.
		if k == "await" || k == "await_reason" {
			continue
		}
		if k == "gate" {
			if _, present := extras["external_gate"]; !present {
				event.Payload["external_gate"] = v
			}
			continue
		}
		event.Payload[k] = v
	}
	_ = s.deps.eventLog.Log(event)
	return terminalStatus
}

// finishTerminal cleans successful runs before recording their terminal event
// so worktree_state describes the actual on-disk result.
func (s *runSession) finishTerminal(ctx context.Context, runID string, result AgentRunResult, extras map[string]any, wt sandbox.Sandbox, branch string) string {
	result, extras = s.normalizeTerminalResult(result, extras)
	return s.finishDecidedTerminal(ctx, runID, result, extras, wt, branch)
}

func (s *runSession) finishDecidedTerminal(ctx context.Context, runID string, result AgentRunResult, extras map[string]any, wt sandbox.Sandbox, branch string) string {
	_, terminalStatus := terminalRunEvent(ctx, result.Status)
	worktreeState := "preserved"
	if terminalStatus == "success" && !s.review && (s.cfg == nil || s.cfg.EffectiveCleanupWorktrees()) {
		restoreErr := wt.RestoreHostPaths()
		if restoreErr != nil && s.deps.errorLog != nil {
			fmt.Fprintf(s.deps.errorLog, "warning: restore host paths for succeeded run %d: %v\n", s.issueNumber, restoreErr)
		}
		stopErr := wt.Stop()
		if stopErr != nil && s.deps.errorLog != nil {
			fmt.Fprintf(s.deps.errorLog, "warning: auto-clean worktree %s for succeeded run %d: %v\n", branch, s.issueNumber, stopErr)
		}
		worktreeRemoved := false
		if workDir := wt.WorkDir(); workDir != "" {
			_, statErr := os.Stat(workDir)
			worktreeRemoved = os.IsNotExist(statErr)
		}
		if (restoreErr == nil && stopErr == nil) || worktreeRemoved {
			worktreeState = "cleaned"
		}
		if cleanupErr := errors.Join(restoreErr, stopErr); cleanupErr != nil {
			if extras == nil {
				extras = make(map[string]any)
			}
			extras["cleanup_error"] = cleanupErr.Error()
		}
	}
	if extras == nil {
		extras = make(map[string]any)
	}
	extras["worktree_state"] = worktreeState
	return s.emitNormalizedTerminal(ctx, runID, result, extras)
}

// emitEarlyFailure logs a terminal run.finished event (status "failure") for
// an early-return path in execute() that exits before run.started is emitted.
// Without this event, the portal keeps the run in its last projected state
// (typically "queued"), and dependent runs see a silently-set failure status
// with no corresponding event in the log. See issue #2136.
//
// The error detail is also written to errorLog (stderr) by the caller; this
// event additionally persists the underlying error in the payload under
// `error_message` so the operator can read it back from .sandman/events.jsonl
// after the fact. Without this, the only place the underlying message
// survives is the live stderr at run time — a regression that bit run
// 260721104202-24a8-2316 (and its sibling rows 2317/2318/2319 in batch
// 260721104202-24a8-2318+11), where `wt.Start` failed mid-batch and the
// operator had no on-disk breadcrumb to diagnose why.
//
// This does NOT emit run.started — the run never started the agent — so
// ProjectRunStates folds run.finished directly, transitioning the run from
// "queued" to a terminal "failure" status. Safe to call when no run manifest
// exists yet (the manifest is written later in execute()).
//
// reason should be a short diagnostic string identifying the failure point
// (e.g. "fetch issue", "start sandbox"). underlyingErr, when non-nil, is
// preserved verbatim under `error_message` so the operator can recover the
// actionable diagnostic from the persisted event log.
func (s *runSession) emitEarlyFailure(reason, branch string, underlyingErr error) {
	if s.deps.eventLog == nil {
		return
	}
	runID := s.issueRunID()
	payload := map[string]any{
		"status":        "failure",
		"branch":        branch,
		"base_branch":   s.baseBranch,
		"retries_total": s.retries,
		"retries_done":  0,
		"early_failure": true,
		"error":         reason,
	}
	if underlyingErr != nil {
		payload["error_message"] = underlyingErr.Error()
	}
	_ = s.deps.eventLog.Log(events.Event{
		Type:      "run.finished",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     s.issueNumber,
		IssueRef:  issueRef(s.issueNumber),
		Payload:   payload,
	})
}

// detectConflictingPR inspects the branch's open PR and, when its mergeable
// state is `CONFLICTING`, returns a payload extras map with
// `merge_conflict: true` and `pr_number` set, plus a `true` ok flag.
//
// Errors from the underlying `gh pr list` lookup are logged to `errorLog`
// but treated as a soft pass-through: a transient gh failure must not
// silently flip a real success into a fake failure. See issue #1684.
func (s *runSession) detectConflictingPR(branch string) (map[string]any, bool) {
	if strings.TrimSpace(branch) == "" {
		return nil, false
	}
	exists, prNumber, mergeable, err := LookupOpenPR(branch)
	if err != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: lookup open PR for branch %q: %v\n", branch, err)
		return nil, false
	}
	if !exists {
		return nil, false
	}
	if strings.EqualFold(mergeable, "CONFLICTING") {
		fmt.Fprintf(s.deps.errorLog, "error: branch %q has CONFLICTING open PR #%d\n", branch, prNumber)
		return map[string]any{"merge_conflict": true, "pr_number": prNumber}, true
	}
	return nil, false
}

// runVerifyPath is the seam the orchestrator uses to invoke the
// verify chain. Production code uses DefaultVerifyPath; tests
// inject a VerifyPathFunc literal to drive outcomes without touching
// real git or GitHub.
func runVerifyPath(verifyPath VerifyPathFunc, in VerifyInput) (VerifyOutcome, []OracleCheck) {
	if verifyPath != nil {
		return verifyPath(in)
	}
	return DefaultVerifyPath()(in)
}

// lookupPRForVerify fetches the branch's open PR via the orchestrator's
// GitHub client so the T4 cheap gate has a snapshot to read. The
// fetched PR carries the slice-1 review fields (ReviewDecision,
// MergeStateStatus, StatusCheckRollup). Failures are soft: a transient
// `gh` error returns nil so the verify path falls through to T1 with
// `T4 == abstain`, matching the "transient errors must not block
// the run" contract used elsewhere in the orchestrator.
func lookupPRForVerify(ctx context.Context, githubClient github.Client, errorLog io.Writer, branch string) *github.PR {
	if githubClient == nil || strings.TrimSpace(branch) == "" {
		return nil
	}
	pr, err := githubClient.FindPRByBranch(ctx, branch)
	if err != nil {
		if errorLog != nil {
			fmt.Fprintf(errorLog, "warning: lookup PR for verify path on branch %q: %v\n", branch, err)
		}
		return nil
	}
	return pr
}

// mergeVerificationExtras folds a verify outcome into the terminal
// event payload under the `verification` key. The function only ever
// writes `verification.outcome` and `verification.checks`; it does
// not touch the `blocker` key (the conservative backstop writes
// `blocker` directly into the same map, so the two layers compose
// without overwriting each other). When called twice — once with
// partial oracle checks and again with the conservative backstop —
// the resulting payload carries both `verification` and `blocker`.
func mergeVerificationExtras(existing map[string]any, outcome VerifyOutcome, checks []OracleCheck) map[string]any {
	if len(checks) == 0 {
		return existing
	}
	out := map[string]any{}
	for k, v := range existing {
		out[k] = v
	}
	outcomeStr := "NoSignal"
	switch outcome {
	case VerifyVerified:
		outcomeStr = "Verified"
	case VerifyFailed:
		outcomeStr = "Failed"
	}
	verification := map[string]any{
		"outcome": outcomeStr,
		"checks":  oracleChecksToAny(checks),
	}
	out["verification"] = verification
	return out
}

func mergeCompletionFailureExtras(existing map[string]any, issueNumber int) map[string]any {
	if existing == nil {
		existing = make(map[string]any)
	}
	existing["completion"] = map[string]any{
		"reason":         "merged-pr-missing-closing-reference",
		"expected_issue": issueNumber,
	}
	return existing
}

// mergeBlockerExtras folds the conservative-backstop blocker payload
// into the terminal event map. It is a small wrapper that allocates
// a fresh map only when the caller hasn't yet, so it composes cleanly
// with `mergeVerificationExtras` when both layers run.
func mergeBlockerExtras(existing, blocker map[string]any) map[string]any {
	if len(blocker) == 0 {
		return existing
	}
	out := existing
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range blocker {
		out[k] = v
	}
	return out
}

func oracleChecksToAny(checks []OracleCheck) []any {
	out := make([]any, 0, len(checks))
	for _, c := range checks {
		entry := map[string]any{
			"name": c.Name,
		}
		for k, v := range c.Details {
			entry[k] = v
		}
		out = append(out, entry)
	}
	return out
}

// hasBlockingOpenPR returns true when the branch currently has an open PR
// AND the run is being short-circuited to success via the `alreadyResolved`
// marker. On hit, it returns a payload extras map with
// `blocker: "open-pr-blocks-already-resolved"` and the PR number. See
// issue #1684.
//
// Errors from `gh pr list` are logged but treated as a soft pass: a
// transient failure should not flip a real success into a fake failure.
func hasBlockingOpenPR(errorLog io.Writer, branch string) (map[string]any, bool) {
	if strings.TrimSpace(branch) == "" {
		return nil, false
	}
	exists, prNumber, _, err := LookupOpenPR(branch)
	if err != nil {
		fmt.Fprintf(errorLog, "warning: lookup open PR for branch %q: %v\n", branch, err)
		return nil, false
	}
	if !exists {
		return nil, false
	}
	fmt.Fprintf(errorLog, "error: branch %q has open PR #%d blocking alreadyResolved short-circuit; overriding run status to failure\n", branch, prNumber)
	return map[string]any{"blocker": "open-pr-blocks-already-resolved", "pr_number": prNumber}, true
}

// updateRunManifestStatus rewrites the run.json snapshot with the terminal
// status. Failures are logged to errorLog and ignored; the event log remains
// authoritative.
func (s *runSession) updateRunManifestStatus(runID string, status batchindex.RunManifestStatus) {
	batchDir := s.deps.layout.BatchDir(s.batchID)
	if s.batchID == "" {
		batchDir = s.deps.layout.BatchesDir
	}
	if err := daemon.UpdateRunManifestStatus(batchDir, runID, status); err != nil {
		fmt.Fprintf(s.deps.errorLog, "error: update run manifest status for run %s: %v\n", runID, err)
	}
}

// runOnce runs the retry loop for a session. mergeRequired gates the
// issue-driven flavour's checkPRMerged check (the sole success signal);
// prepareAttempt returns (_, &errResult) to short-circuit. A short-circuit
// result with Status="success" (e.g. the pre-retry guard on a merged PR)
// propagates as a started run so the terminal success event is emitted;
// any other short-circuit status propagates as a non-started failure.
//
// The returned `terminalExtras` carries extra payload keys the terminal
// event should merge in (e.g. "blocker" / "pr_number" when an open PR
// blocks the run from being declared success — see issue #1684). It may
// be nil. Started mirrors the second return value of the original
// signature.
func (s *runSession) runOnce(
	ctx context.Context,
	issue *github.Issue,
	branch string,
	wt sandbox.Sandbox,
	logPath string,
	runID string,
	mergeRequired bool,
	prepareAttempt func(attempt int, previous AgentRunResult) (prompt.RenderConfig, *AgentRunResult),
) (AgentRunResult, map[string]any, bool) {
	if s.renderCfg.PromptFile == "" {
		s.renderCfg.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if s.renderCfg.RenderedPromptFile == "" {
		s.renderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "task.md")
	}

	attempts := s.retries + 1
	var result AgentRunResult
	var abortedByHeartbeat bool
	s.lifecycleTerminal = false

	factory := s.deps.runnableFactory
	if factory == nil {
		factory = defaultRunnableFactory{}
	}

	var terminalExtras map[string]any
loop:
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			result.Status = "aborted"
			result.ContextExhausted = false
			break loop
		}
		if attempt > 0 {
			terminalExtras = nil
			// Session reuse is a launch choice, not retry state. Retries and
			// context-rollover recovery always start a fresh conversation.
			s.reuseSession = false
			// An operator cancellation must win before recovery can replace the
			// Task or start a fresh session.
			if result.ContextExhausted && ctx.Err() != nil {
				result.ContextExhausted = false
				break loop
			}
			if result.ContextExhausted {
				// Container sandboxes leave worktree metadata addressed inside the
				// container after Exec. Restore it before the recovery Task is
				// written; ContainerSandbox.Exec reapplies container paths for the
				// next command.
				if err := wt.RestoreHostPaths(); err != nil {
					fmt.Fprintf(s.deps.errorLog, "error: restore host paths before context recovery: %v\n", err)
					result.Status = "failure"
					break loop
				}
			}
		}
		attemptRenderCfg, errResult := prepareAttempt(attempt, result)
		if ctx.Err() != nil {
			result.Status = "aborted"
			result.ContextExhausted = false
			break loop
		}
		if errResult != nil {
			return *errResult, nil, events.RunStatusFromPayload(errResult.Status).IsSuccess()
		}
		// prepareAttempt builds a recovery Task from the preserved Task. Check
		// again after that work so cancellation cannot launch its replacement.
		if result.ContextExhausted && ctx.Err() != nil {
			result.ContextExhausted = false
			break loop
		}

		if attempt > 0 {
			reason := mapRetryReason(result.Status, abortedByHeartbeat, s.parentCtx)
			if result.ContextExhausted {
				reason = contextExhaustedRetryReason
			}
			logRetry(s.deps.eventLog, runID, branch, attempt+1, attempts, result.Status, reason, logPath, s.issueNumber)
		}

		var runnable Runnable
		// When launching a continuation, copy the original
		// task.md that lives in the worktree into the new per-row run
		// folder as a sibling of run.json / run.log. The worktree file
		// is about to be overwritten by the continuation prompt; this
		// snapshot preserves the prior wording as a historical artifact
		// for the operator to revisit. The copy is best-effort: if the
		// worktree's task.md is missing (already warned about upstream)
		// we silently skip the snapshot — the operator still has the
		// live run.log and event log to reconstruct state. The
		// runFolder is the same path the AgentRun (or any other
		// Runnable implementation that respects the per-row folder)
		// would write to, so the snapshot lands alongside run.json /
		// run.log regardless of which Runnable factory is in use.
		if s.mode == ModeContinue {
			runFolder := s.runFolderFor(runID)
			if runFolder != "" {
				if err := snapshotOriginalTask(wt.WorkDir(), runFolder); err != nil {
					fmt.Fprintf(s.deps.errorLog, "warning: snapshot task.md for continuation run %s: %v\n", runID, err)
				}
			}
		}
		var alreadyResolved bool
	relaunch:
		// In-session resume loop (issue #2595): the agent may be relaunched
		// within the same attempt when the PR gate turns resume-worthy
		// (ready-to-merge / actionable-feedback) after a clean completion.
		// A resume relaunch reuses the attempt index — no run.retry, no
		// retry branch reset, no snapshot — and carries the request-scoped
		// review evidence in the prompt. The per-session resume cap
		// (awaitResumeMax) bounds the loop; exhaustion is terminal failure
		// on the same gate instead of a synthetic await.
		for {
			runnable = factory.NewRunnable(issue, branch, wt)
			if agentRun, ok := runnable.(*AgentRun); ok {
				agentRun.env = s.agentCfg.Env
				agentRun.preset = s.agentCfg.Preset
				agentRun.contextRolloverLiterals = append([]string(nil), s.opts.contextRolloverLiterals...)
				if s.opts.taskWriter != nil {
					agentRun.taskWriter = s.opts.taskWriter
				}
				agentRun.model = s.agentCfg.Model
				agentRun.modelProvider = s.agentCfg.ModelProvider
				agentRun.modelName = s.agentCfg.ModelName
				agentRun.variant = s.variant
				agentRun.opencodePermissionMode = s.agentCfg.OpencodePermissionMode
				agentRun.baseBranch = s.baseBranch
				agentRun.runID = runID
				agentRun.review = s.review
				agentRun.outputWriter = s.outputWriter
				agentRun.dangerouslySkipPermissions = &s.dangerouslySkipPermissions
				agentRun.sessionName = "Sandman " + runID + ": "
				agentRun.runFolder = s.runFolderFor(runID)
				agentRun.batchID = s.batchID
				agentRun.previousRunID = s.previousRunIDs[s.issueNumber]
				agentRun.previousBatchID = s.previousRunBatchIDs[s.issueNumber]
				agentRun.reuseSession = s.reuseSession
				agentRun.sessionWarning = s.deps.errorLog
			}
			if reporter, ok := runnable.(interface{ setQuotaSignals(func(), func()) }); ok {
				reporter.setQuotaSignals(s.modelProgress, s.usageLimit)
			}

			s.reviewRegistrationAttempted = false
			s.reviewRegistrationObserved = false
			s.reviewAttemptStartedAt = s.reviewNow()
			s.lifecyclePRSnapshot = nil
			result, abortedByHeartbeat = s.withHeartbeat(ctx, runID, attempt, logPath, wt, func() AgentRunResult {
				return s.withClosingReferenceGuard(ctx, branch, func() AgentRunResult {
					return runnable.Run(ctx, s.deps.renderer, s.agentCfg.Command, attemptRenderCfg)
				})
			})
			if result.Issue == nil && s.issueNumber > 0 {
				result.Issue = issueRef(s.issueNumber)
			}
			if result.IssueNumber == 0 && s.issueNumber > 0 {
				result.IssueNumber = s.issueNumber
			}
			result.RetriesTotal = attempt + 1

			taskPath := filepath.Join(wt.WorkDir(), ".sandman", "task.md")
			taskContent, _, _ := ReadTaskContent(taskPath)
			alreadyResolved = hasExactTaskStatus(taskContent, "## Status: already resolved")
			s.lifecycleAlreadyResolved = alreadyResolved
			if s.isIssueDriven() && ctx.Err() == nil {
				hostPathsReady := s.restoreHostPathsBeforeExternalGate(wt)
				if gateStatus, extras, handled := s.handleLifecycleDecisionForAttempt(ctx, wt.WorkDir(), branch, logPath, runID, hostPathsReady, result.Status); handled {
					if isImplementorOwnedGateFailure(extras) {
						// A clean but incomplete handoff is owned work, not an
						// external await or a separate lifecycle-resume budget.
						// Use the configured ordinary retry budget to perform it.
						result.Status = "failure"
						terminalExtras = cloneLifecycleExtras(extras)
						delete(terminalExtras, "await")
						delete(terminalExtras, "gate")
						continue loop
					}
					if gateStatus == "success" || gateStatus == "failure" || gateStatus == "aborted" {
						// A terminal lifecycle decision is authoritative. Do not
						// let the legacy post-decision PR arbitration replace it.
						s.lifecycleTerminal = true
						result.Status = gateStatus
						terminalExtras = mergeBlockerExtras(terminalExtras, extras)
						break loop
					}
					observe := gateStatus == "await"
					if observe {
						if !s.opts.foregroundLifecycle {
							s.emitAwait(ctx, runID, result, extras)
							result.Status = gateStatus
							break loop
						}
						s.emitAwait(ctx, runID, result, extras)
						gateStatus, extras, _ = s.observeLifecycle(ctx, wt.WorkDir(), branch, logPath, runID, result, extras, hostPathsReady)
					}
					if gateStatus == "resume" {
						if resumePrompt, resume := s.resumePromptFromGate(ctx, wt, branch, runID, extras); resume {
							s.reuseSession = true
							s.previousRunIDs = map[int]string{s.issueNumber: runID}
							s.previousRunBatchIDs = map[int]string{s.issueNumber: s.batchID}
							attemptRenderCfg.TaskPrompt = resumePrompt
							continue relaunch
						}
					}
					if gateStatus == "resume" {
						// An exhausted in-session resume budget ends the
						// session for every gate, including CI remediation:
						// a budget with no remaining relaunch cannot keep
						// resolving, so it must not prolong a wait
						// (issue #2743).
						gate, _ := extras["gate"].(string)
						gateStatus = "failure"
						extras = remediationBudgetFailureEvidence(gate, extras,
							"inspect the current pull-request remediation evidence and continue in a fresh session")
					}
					s.lifecycleTerminal = gateStatus == "success" || gateStatus == "failure" || gateStatus == "aborted"
					result.Status = gateStatus
					terminalExtras = mergeBlockerExtras(terminalExtras, extras)
					break loop
				}
			}
			break relaunch
		}
		if mergeRequired {
			prMerged := checkPRMergedForIssue(ctx, s.deps.githubClient, branch, s.issueNumber)
			if events.RunStatusFromPayload(result.Status).IsAborted() {
				continue
			}
			if events.RunStatusFromPayload(result.Status).IsSuccess() && mergedPRMissingClosingReference(ctx, s.deps.githubClient, branch, s.issueNumber) {
				terminalExtras = mergeCompletionFailureExtras(terminalExtras, s.issueNumber)
				s.lifecycleTerminal = true
				result.Status = "failure"
				break
			}
			if prMerged || alreadyResolved {
				if ctx.Err() != nil {
					break
				}
				if alreadyResolved {
					pr := lookupPRForVerify(ctx, s.deps.githubClient, s.deps.errorLog, branch)
					outcome, checks := runVerifyPath(s.deps.verifyPath, VerifyInput{Context: ctx, Issue: issue, Branch: branch, WorkDir: wt.WorkDir(), PR: pr})
					if outcome != VerifyNoSignal {
						terminalExtras = mergeVerificationExtras(terminalExtras, outcome, checks)
						if outcome == VerifyFailed {
							result.Status = "failure"
							break
						}
						// VerifyVerified: drop the conservative backstop;
						// the oracle proved the issue is already resolved.
						result.Status = "success"
						if issue != nil && !github.IsIssueClosed(issue) && s.deps.githubClient != nil {
							if err := s.deps.githubClient.CloseIssue(ctx, issue.Number, "Closed by sandman — issue already completed."); err != nil {
								fmt.Fprintf(s.deps.errorLog, "error: close issue %d: %v\n", issue.Number, err)
							}
						}
						break
					}
					// VerifyNoSignal: record the chain's checks (if any)
					// so the operator can see why we abstained, then
					// fall through to the conservative backstop. The
					// blocker payload and pr_number fields are
					// preserved verbatim.
					if len(checks) > 0 {
						terminalExtras = mergeVerificationExtras(terminalExtras, outcome, checks)
					}
					if extras, blocked := hasBlockingOpenPR(s.deps.errorLog, branch); blocked {
						terminalExtras = mergeBlockerExtras(terminalExtras, extras)
						result.Status = "failure"
						break
					}
				}
				result.Status = "success"
				if alreadyResolved && issue != nil && !github.IsIssueClosed(issue) && s.deps.githubClient != nil {
					if err := s.deps.githubClient.CloseIssue(ctx, issue.Number, "Closed by sandman — issue already completed."); err != nil {
						fmt.Fprintf(s.deps.errorLog, "error: close issue %d: %v\n", issue.Number, err)
					}
				}
				break
			}
			if github.IsIssueClosed(issue) {
				if events.RunStatusFromPayload(result.Status).IsSuccess() {
					break
				}
			}
			result.Status = "failure"
		} else {
			if alreadyResolved && issue != nil && !github.IsIssueClosed(issue) && s.deps.githubClient != nil {
				if err := s.deps.githubClient.CloseIssue(ctx, issue.Number, "Closed by sandman — issue already completed."); err != nil {
					fmt.Fprintf(s.deps.errorLog, "error: close issue %d: %v\n", issue.Number, err)
				}
			}
			if events.RunStatusFromPayload(result.Status).IsSuccess() || alreadyResolved {
				if issue != nil && s.deps.githubClient != nil {
					prMerged := checkPRMergedForIssue(ctx, s.deps.githubClient, branch, s.issueNumber)
					if events.RunStatusFromPayload(result.Status).IsSuccess() && mergedPRMissingClosingReference(ctx, s.deps.githubClient, branch, s.issueNumber) {
						terminalExtras = mergeCompletionFailureExtras(terminalExtras, s.issueNumber)
						s.lifecycleTerminal = true
						result.Status = "failure"
						break
					}
					if prMerged || alreadyResolved {
						if alreadyResolved {
							pr := lookupPRForVerify(ctx, s.deps.githubClient, s.deps.errorLog, branch)
							outcome, checks := runVerifyPath(s.deps.verifyPath, VerifyInput{Context: ctx, Issue: issue, Branch: branch, WorkDir: wt.WorkDir(), PR: pr})
							if outcome != VerifyNoSignal {
								terminalExtras = mergeVerificationExtras(terminalExtras, outcome, checks)
								if outcome == VerifyFailed {
									result.Status = "failure"
									break
								}
								result.Status = "success"
								break
							}
							if len(checks) > 0 {
								terminalExtras = mergeVerificationExtras(terminalExtras, outcome, checks)
							}
							if extras, blocked := hasBlockingOpenPR(s.deps.errorLog, branch); blocked {
								terminalExtras = mergeBlockerExtras(terminalExtras, extras)
								result.Status = "failure"
								break
							}
							result.Status = "success"
						}
						break
					}
					if events.RunStatusFromPayload(result.Status).IsSuccess() && ctx.Err() == nil {
						hostPathsReady := s.restoreHostPathsBeforeExternalGate(wt)
						if gateStatus, extras, handled := s.handleLifecycleDecisionAfterAgent(ctx, wt.WorkDir(), branch, logPath, runID, hostPathsReady); handled {
							if gateStatus == "resume" {
								// This legacy verification fallback is past the
								// in-session resume point. Never turn a resolved
								// gate into an await here; fail with the owned
								// action and let the next session resume it.
								gateStatus = "failure"
								extras = lifecycleGateFailureEvidence("IMPLEMENTOR_ACTION_REQUIRED", "resume the implementation to complete current pull-request feedback or merge work", lifecycleGateNone, nil, "")
							}
							s.lifecycleTerminal = gateStatus == "success" || gateStatus == "failure" || gateStatus == "aborted"
							result.Status = gateStatus
							terminalExtras = mergeBlockerExtras(terminalExtras, extras)
							break loop
						}
					}
					result.Status = "failure"
				} else {
					break
				}
			}
		}
		if s.shouldAwaitUsageLimit(result) {
			// An admitted quota wait preserves ordinary retries. Once the
			// polling allowance is consumed, those fresh attempts remain usable.
			break loop
		}
	}
	if result.UsageLimitReached && !result.ContextExhausted &&
		!events.RunStatusFromPayload(result.Status).IsSuccess() {
		terminalExtras = mergeLifecycleDiagnostics(terminalExtras, map[string]any{
			"reason":      "AGENT_USAGE_LIMIT",
			"next_action": "retry with an agent provider that has available capacity or resume after its usage limit resets",
		})
	}

	if result.ContextExhausted {
		if terminalExtras == nil {
			terminalExtras = make(map[string]any)
		}
		terminalExtras["context_exhausted"] = true
	}
	return result, terminalExtras, true
}

func (s *runSession) shouldAwaitUsageLimit(result AgentRunResult) bool {
	return s.isIssueDriven() &&
		strategyFor(s.agentCfg.Preset, s.agentCfg.Command).AwaitsUsageLimit() &&
		result.UsageLimitReached &&
		!result.ContextExhausted &&
		!events.RunStatusFromPayload(result.Status).IsSuccess() &&
		s.usageLimitWaited >= 0 && s.usageLimitWaited < usageLimitRetryWindow
}

func (s *runSession) restoreHostPathsBeforeExternalGate(wt sandbox.Sandbox) bool {
	if err := wt.RestoreHostPaths(); err != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: restore host paths before external gate: %v\n", err)
		return false
	}
	return true
}

// runSingleRow is the elevated seam for one issue-driven AgentRun. It builds a
// runExecutor (the E2-decoupled constructor/test seam) and delegates to
// Execute, which discriminates on IssueNumber>0 and runs the issue-driven
// lifecycle. parentCtx is the RunBatch ctx (the ctx that owns this whole
// batch); the supervisor uses it to distinguish external abort from normal
// session end. Retained as a thin wrapper so the test call sites that still
// cross the positional shim (#2231) keep compiling unchanged.
func (o *Orchestrator) runSingleRow(ctx context.Context, parentCtx context.Context, row RowSpec, bc BatchConfig, sbFactory SandboxFactory, containerAlloc containerAllocator) (AgentRunResult, bool) {
	coord := newBatchCoordinator(nil)
	return o.newRunExecutorWith(parentCtx, bc, sbFactory, containerAlloc, coord, coord, o.layout).Execute(ctx, row)
}

// prepareIssueAttempt preserves issue-specific merged-PR checks, Task recovery,
// and retry reset policy; execution and terminal persistence belong to execute.
func (s *runSession) prepareIssueAttempt(ctx context.Context, branch string, wt sandbox.Sandbox, logPath string, attempt int, previous AgentRunResult) (prompt.RenderConfig, *AgentRunResult) {
	attemptRenderCfg := s.renderCfg
	if attempt > 0 {
		// Pre-retry guard: if the PR was merged between attempts (e.g. the
		// agent merged it on attempt 0 but exited non-zero due to a
		// transient error), short-circuit to success without launching
		// the agent again, resetting the branch, or re-rendering the
		// prompt. The merged PR is the sole success signal for
		// issue-driven runs (see #860). Prompt-only runs use a separate
		// retry preparation helper without this guard.
		if checkPRMergedForIssue(ctx, s.deps.githubClient, branch, s.issueNumber) {
			// Preserve the historical empty-task artifact when a managed PR
			// resolves the retry before the replacement session launches.
			taskPath := filepath.Join(wt.WorkDir(), ".sandman", "task.md")
			if _, err := os.Stat(taskPath); os.IsNotExist(err) {
				if err := atomicfs.WriteAtomic(taskPath, []byte(EmptyTaskTemplate), 0o644); err != nil {
					fmt.Fprintf(s.deps.errorLog, "warning: write empty task for resolved issue %d: %v\n", s.issueNumber, err)
				}
			}
			return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "success", Branch: branch, RetriesTotal: attempt}
		}
		taskPath := filepath.Join(wt.WorkDir(), ".sandman", "task.md")
		openPR, prLookupErr := findOpenPRByBranch(ctx, s.deps.githubClient, branch)
		// Preserve the task content (or use the empty template if missing)
		// and place the continuation freshness guard after persisted state. The agent
		// revalidates the task document's ## Next Step against live state. The openPR value is only
		// used below to decide whether to reset the branch — the agent
		// receives the same task content regardless of open-PR state.
		taskContent, taskExists, err := ReadTaskContent(taskPath)
		if err != nil {
			fmt.Fprintf(s.deps.errorLog, "error: read task for issue %d: %v\n", s.issueNumber, err)
			return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch, RetriesTotal: attempt}
		}
		if previous.ContextExhausted {
			attemptRenderCfg.TaskPrompt = prompt.ContextRecoveryTaskPrompt(taskContent, s.renderCfg.ReviewTimeout)
			attemptRenderCfg.ContextRecovery = true
		} else {
			attemptRenderCfg.TaskPrompt = prompt.ContinuationTaskPromptWithReviewTimeout(taskContent, s.renderCfg.ReviewTimeout)
		}
		attemptRenderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "task.md")
		if !taskExists && openPR == nil && !previous.ContextExhausted {
			if prLookupErr != nil {
				fmt.Fprintf(s.deps.errorLog, "error: lookup PR for issue %d: %v\n", s.issueNumber, prLookupErr)

				return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch, RetriesTotal: attempt}
			}
			if err := resetRetryBranch(s.deps.runSessionOpts, ctx, wt, branch, s.baseBranch); err != nil {
				fmt.Fprintf(s.deps.errorLog, "error: reset retry branch for issue %d: %v\n", s.issueNumber, err)
				return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch, RetriesTotal: attempt}
			}
		}
		if err := logRetryMarkerFn(logPath, attempt, s.retries); err != nil {
			if s.deps.errorLog != nil {
				fmt.Fprintf(s.deps.errorLog, "warning: write retry marker for issue %d: %v\n", s.issueNumber, err)
			}
		}
	}
	return attemptRenderCfg, nil

}

// reconcileWorktreeBranch returns the worktree's HEAD to the issue branch
// when it has drifted onto some other ref (e.g. after `gh pr merge --squash
// --delete-branch` leaves the worktree on the local base branch, or when
// the worktree ends up on a detached HEAD). The PR merge itself succeeded
// on GitHub, so any failure here is logged as a warning and the run still
// returns success.
func (s *runSession) reconcileWorktreeBranch(wt sandbox.Sandbox, branch string) {
	workDir := wt.WorkDir()
	if workDir == "" {
		return
	}
	// Guard: if the worktree directory is not a valid git directory
	// (e.g. its .git file points to a stale/removed worktree
	// registration), skip the checkout to avoid the
	// "not a git repository: (null)" error. See #1189.
	if !sandbox.IsGitDir(workDir) {
		fmt.Fprintf(s.deps.errorLog, "warning: reconcile worktree branch: worktree at %q is not a valid git directory; skipping checkout\n", workDir)
		return
	}
	expectedRef := "refs/heads/" + branch
	if currentRef, err := sandbox.CurrentBranchRef(workDir); err == nil && currentRef == expectedRef {
		return
	}
	if !sandbox.BranchExists(wt.RepoPath(), branch) {
		fmt.Fprintf(s.deps.errorLog, "warning: reconcile worktree branch: branch %q was deleted; next run will recreate it\n", branch)
		return
	}
	cmd := exec.Command("git", "-C", workDir, "checkout", "-f", branch)
	cmd.Dir = wt.RepoPath()
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: reconcile worktree branch: git checkout -f %s: %v\n%s\n", branch, err, out)
		return
	}
}

// verifyNoRemainingProcesses checks that no process whose command line
// contains the given runID is still alive. This is a safety-net
// verification (issue #2605 acceptance criterion #2) that runs after the
// primary cleanup in waitCmd and after the terminal event is appended.
// Failures are logged as warnings; the run outcome is not changed.
func (s *runSession) verifyNoRemainingProcesses(runID string) {
	if runID == "" {
		return
	}
	out, err := exec.Command("ps", "ax").CombinedOutput()
	if err != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: verify no remaining processes: ps failed: %v\n", err)
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, runID) {
			fmt.Fprintf(s.deps.errorLog, "warning: verify no remaining processes: found lingering process for run %s: %s\n", runID, strings.TrimSpace(line))
		}
	}
}

func recheckBlockedBy(ctx context.Context, githubClient github.Client, blockers []int) ([]int, error) {
	blockers = uniqueIssues(blockers)
	if len(blockers) == 0 {
		return nil, nil
	}

	blockedBy := make([]int, 0, len(blockers))
	for _, blocker := range blockers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		state, err := fetchIssueState(ctx, githubClient, blocker)
		if err != nil {
			return nil, fmt.Errorf("fetch blocker issue %d: %w", blocker, err)
		}
		if !strings.EqualFold(state, "closed") {
			blockedBy = append(blockedBy, blocker)
		}
	}

	return blockedBy, nil
}

func fetchIssueContent(ctx context.Context, client github.Client, number int) (*github.Issue, error) {
	if contentClient, ok := client.(github.IssueContentFetcher); ok {
		return contentClient.FetchIssueContent(ctx, number)
	}
	return client.FetchIssue(ctx, number)
}

func fetchIssueState(ctx context.Context, client github.Client, number int) (string, error) {
	if stateClient, ok := client.(github.IssueStateFetcher); ok {
		return stateClient.FetchIssueState(ctx, number)
	}
	issue, err := client.FetchIssue(ctx, number)
	if err != nil {
		return "", err
	}
	if issue == nil {
		return "", nil
	}
	return issue.State, nil
}

func resetRetryBranch(opts runSessionOptions, ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
	if opts.retryReset != nil {
		return opts.retryReset(ctx, sb, branch, baseBranch)
	}

	var output bytes.Buffer
	command := fmt.Sprintf("git reset --hard && git checkout -f -B %s %s && git clean -fd", shellenv.Quote(branch), shellenv.Quote(baseBranch))
	if err := sb.Exec(ctx, command, &output, &output); err != nil {
		return fmt.Errorf("reset retry branch: %w\n%s", err, output.String())
	}
	return nil
}

func (o *Orchestrator) runPromptOnly(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, identityResolver *gitIdentityResolver, sbFactory SandboxFactory, containerAlloc containerAllocator, req Request, baseBranch string, startDelay time.Duration, parallel int, retries int, runIdleTimeout int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool, strandedReconcile bool, coord runCoordination, layout paths.Layout) (*Result, error) {
	branch := promptOnlyBranch(req.PromptConfig)
	variant := strings.TrimSpace(req.Variant)
	if !req.VariantSet && variant == "" {
		if req.Review {
			variant = cfg.EffectiveReviewVariant()
		} else {
			variant = strings.TrimSpace(cfg.Variant)
		}
	}
	row := RowSpec{
		IssueNumber:         req.IssueNumber,
		Mode:                req.IssueMode(0),
		Branches:            map[int]string{0: branch},
		PreviousRunIDs:      req.PreviousRunIDs,
		PreviousRunBatchIDs: req.PreviousRunBatchIDs,
		ReuseSession:        req.ReuseSession[0],
		BaseBranch:          baseBranch,
		RenderCfg:           req.PromptConfig,
		OutputWriter:        req.OutputWriter,
		BatchID:             batchIDForPromptOnly(req.BatchTS, req.BatchShortID, req.RunID, req.RunDir),
		BatchTS:             req.BatchTS,
		BatchShortID:        req.BatchShortID,
		RunID:               req.RunID,
		UserProvidedRunID:   req.RunID,
		Review:              req.Review,
		PRNumber:            req.PRNumber,
		ReviewFocus:         req.ReviewFocus,
		PortalHidden:        req.PortalHidden,
		QualityRulesFile:    req.QualityRulesFile,
	}
	bc := BatchConfig{
		Cfg:                        cfg,
		AgentName:                  agentName,
		AgentCfg:                   agentCfg,
		Variant:                    variant,
		IdentityResolver:           identityResolver,
		Parallel:                   parallel,
		StartDelay:                 startDelay,
		Retries:                    retries,
		RunIdleTimeout:             runIdleTimeout,
		SandboxMode:                sandboxMode,
		ContainerCapacity:          containerCapacity,
		ContainerCapacitySet:       containerCapacitySet,
		MaxContainers:              maxContainers,
		MaxContainersSet:           maxContainersSet,
		DangerouslySkipPermissions: dangerouslySkipPermissions,
		StrandedReconcile:          strandedReconcile,
		ContextRolloverLiterals:    cfg.ContextErrorPhrases,
	}
	commander, ok := coord.(daemon.IssueCommander)
	if !ok {
		return nil, fmt.Errorf("batch coordination does not support commands")
	}
	result, started := o.newRunExecutorWith(ctx, bc, sbFactory, containerAlloc, coord, commander, layout).Execute(ctx, row)
	if !started {
		return &Result{Runs: []AgentRunResult{result}}, fmt.Errorf("prompt-only run failed")
	}
	resultStatus := events.RunStatusFromPayload(result.Status)
	if resultStatus.IsAborted() {
		return &Result{Runs: []AgentRunResult{result}}, fmt.Errorf("prompt-only run aborted: %w", ErrAborted)
	}
	if !resultStatus.IsSuccess() {
		return &Result{Runs: []AgentRunResult{result}}, fmt.Errorf("prompt-only run failed")
	}
	return &Result{Runs: []AgentRunResult{result}}, nil
}

// runPromptOnlyRow is the elevated seam for one prompt-only AgentRun. It builds
// a runExecutor (the E2-decoupled constructor/test seam) and delegates to
// Execute. parentCtx is ctx itself for prompt-only runs (they share the
// RunBatch ctx — there is no per-issue fan-out), matching the prior
// newRunSession(..., ctx) wiring. Retained as a thin wrapper so the test call
// sites that still cross the positional shim (#2231) keep compiling unchanged.
func (o *Orchestrator) runPromptOnlyRow(ctx context.Context, row RowSpec, bc BatchConfig, sbFactory SandboxFactory, containerAlloc containerAllocator) (AgentRunResult, bool) {
	coord := newBatchCoordinator(nil)
	return o.newRunExecutorWith(ctx, bc, sbFactory, containerAlloc, coord, coord, o.layout).Execute(ctx, row)
}

// preparePromptAttempt keeps prompt-only/review reset and continuation policy
// separate from the common resource lifecycle.
func (s *runSession) preparePromptAttempt(ctx context.Context, branch string, wt sandbox.Sandbox, logPath string, attempt int, previous AgentRunResult) (prompt.RenderConfig, *AgentRunResult) {
	if attempt > 0 {
		if !previous.ContextExhausted {
			if err := resetRetryBranch(s.deps.runSessionOpts, ctx, wt, branch, s.baseBranch); err != nil {
				fmt.Fprintf(s.deps.errorLog, "error: reset retry branch for prompt-only run: %v\n", err)
				return prompt.RenderConfig{}, &AgentRunResult{Status: "failure", Branch: branch, RetriesTotal: attempt, Review: s.review, RunID: s.runID}
			}
		}
		if err := logRetryMarkerFn(logPath, attempt, s.retries); err != nil {
			if s.deps.errorLog != nil {
				fmt.Fprintf(s.deps.errorLog, "warning: write retry marker for prompt-only run: %v\n", err)
			}
		}
	}
	attemptCfg := s.renderCfg
	if s.mode == ModeContinue || attempt > 0 {
		taskPath := filepath.Join(wt.WorkDir(), ".sandman", "task.md")
		taskContent, taskExists, err := ReadTaskContent(taskPath)
		if err != nil {
			fmt.Fprintf(s.deps.errorLog, "error: read prompt-only task for continuation: %v\n", err)
		} else if taskExists || taskContent != "" {
			if taskContent == "" {
				taskContent = EmptyTaskTemplate
			}
			if previous.ContextExhausted {
				attemptCfg.TaskPrompt = prompt.ContextRecoveryTaskPrompt(taskContent, s.renderCfg.ReviewTimeout)
				attemptCfg.ContextRecovery = true
			} else {
				attemptCfg.TaskPrompt = prompt.ContinuationTaskPromptWithReviewTimeout(taskContent, s.renderCfg.ReviewTimeout)
			}
			attemptCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "task.md")
		}
	}
	return attemptCfg, nil
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
	return fmt.Sprintf("%s-%d", slug, time.Now().UnixNano())
}

func terminalRunEvent(ctx context.Context, status string) (string, string) {
	eventType := "run.finished"
	terminalStatus := status
	terminalCode := events.RunStatusFromPayload(terminalStatus)
	if terminalCode.String() == "" {
		terminalStatus = "failure"
		terminalCode = events.RunStatusFailure
	}
	if ctx.Err() != nil && !terminalCode.IsSuccess() {
		eventType = "run.aborted"
		terminalStatus = "aborted"
	}
	return eventType, terminalStatus
}

func syncBaseBranch(opts runSessionOptions, sandboxFactory SandboxFactory, repoPath, baseBranch string) error {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return nil
	}
	mu := opts.baseBranchSyncMu
	if mu == nil {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	syncFn := opts.baseBranchSync
	if syncFn == nil {
		if sandboxFactory != nil {
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
//
// The branch name is always the issue-driven default shape
// "<n>-<slug>". The sourceBranch argument is accepted for
// future extensibility (e.g. namespaced issue branches), but the
// current convention keeps the branch name simple regardless of the
// configured base — git's ref-namespace model forbids encoding a
// feature branch (e.g. "feat/release-pipeline-2026q3") as a prefix
// of another branch (e.g. "feat/release-pipeline-2026q3/955-...")
// when the feature branch itself exists. The source branch is the
// orchestrator's `--base-branch` value, threaded through for that
// future extension; see ADR-0040 for the rationale.
func BranchName(issueNumber int, title string, sourceBranch string) string {
	_ = sourceBranch
	return fmt.Sprintf("%d-%s", issueNumber, Slugify(title))
}

// isMissingWorktreeError returns true when the git worktree remove error is
// caused by the worktree not existing — not a real failure.
func isMissingWorktreeError(err error, out []byte) bool {
	if err == nil {
		return false
	}
	return bytes.Contains(out, []byte("is not a working tree")) ||
		bytes.Contains(out, []byte("could not open worktree"))
}

// isPrunableWorktreeError returns true when the git worktree remove error is
// caused by a prunable worktree — a worktree whose .git gitlink points to a
// non-existent directory.
func isPrunableWorktreeError(err error, out []byte) bool {
	if err == nil {
		return false
	}
	return bytes.Contains(out, []byte("is not a .git file"))
}

// isMissingBranchError returns true when the git branch -D error is caused
// by the branch not existing — not a real failure.
func isMissingBranchError(err error, out []byte) bool {
	return err != nil && bytes.Contains(out, []byte("not found"))
}

// ClearIssueArtifacts removes worktree, branch, and event log entries
// for a given issue. It is idempotent — missing artifacts do not cause errors.
//
// When `git branch -D <branch>` from the main-repo cwd fails because the
// branch is currently checked out somewhere, and `strandedReconcile` is
// non-nil and true, the function runs the auto-recovery flow described in
// ADR-0023: (1) delete from inside a stranded worktree at the canonical
// path if present, (2) detach HEAD in a foreign worktree at a
// non-canonical path if present, (3) detach HEAD in the parent repo and
// `git branch -D` the branch only when the parent is the verified
// holder. The fallback branch is no longer used; the helper refuses
// to delete a held branch instead of force-checking out an arbitrary
// base branch underneath the operator.
//
// When `strandedReconcile` is nil, today's belt-and-suspenders behaviour
// is preserved (the failure is logged and the function continues);
// false is the explicit opt-out (`--no-reconcile-stranded`).
//
// closeOpenPRForBranch retires an open PR for branch before destructive
// override cleanup. It finds the PR via FindPRByBranch, closes it without
// merge if open, then refetches to verify it is no longer open. A nil
// github client, missing PR, or non-open state is a no-op success. A close
// error or a verification that still reports open (or a verification fetch
// error) is returned so the caller can preserve the worktree and branch.
func closeOpenPRForBranch(ctx context.Context, branch string, ghClient github.Client) error {
	if ghClient == nil {
		return nil
	}
	pr, err := ghClient.FindPRByBranch(ctx, branch)
	if err != nil {
		return fmt.Errorf("find PR for branch %s: %w", branch, err)
	}
	if pr == nil || !strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		return nil
	}
	if err := ghClient.ClosePR(ctx, pr.Number); err != nil {
		return fmt.Errorf("close PR %d for branch %s: %w", pr.Number, branch, err)
	}
	verified, err := ghClient.FindPRByBranch(ctx, branch)
	if err != nil {
		return fmt.Errorf("verify PR closure for branch %s (pr %d): %w", branch, pr.Number, err)
	}
	if verified != nil && strings.EqualFold(strings.TrimSpace(verified.State), "open") {
		return fmt.Errorf("PR %d for branch %s still open after close", pr.Number, branch)
	}
	return nil
}

// `baseBranch` is accepted for API compatibility with prior call sites
// but is no longer used by the recovery flow.
func ClearIssueArtifacts(issueNumber int, branch string, worktreeDir string, eventLog events.EventLog, logWriter io.Writer, baseBranch string, strandedReconcile *bool, batchesIndexPath string) {
	wtPath := filepath.Join(worktreeDir, branch)

	// Remove worktree (may fail if already removed — idempotent)
	if out, err := exec.Command("git", "worktree", "remove", "--force", wtPath).CombinedOutput(); err != nil {
		if (isPrunableWorktreeError(err, out) || isMissingWorktreeError(err, out)) && strandedReconcile != nil && *strandedReconcile {
			if rmErr := os.RemoveAll(wtPath); rmErr != nil {
				fmt.Fprintf(logWriter, "error: remove worktree dir %s for issue %d: %v\n", wtPath, issueNumber, rmErr)
			}
			if rmErr := sandbox.RemoveWorktreeRegistration(".", wtPath); rmErr != nil {
				fmt.Fprintf(logWriter, "error: remove worktree registration %s for issue %d: %v\n", wtPath, issueNumber, rmErr)
			}
		} else if !isMissingWorktreeError(err, out) {
			fmt.Fprintf(logWriter, "error: remove worktree %s for issue %d: %v: %s\n", wtPath, issueNumber, err, out)
		}
	}
	// Delete branch (may fail if already deleted — idempotent)
	if out, err := exec.Command("git", "branch", "-D", branch).CombinedOutput(); err != nil && !isMissingBranchError(err, out) {
		if strandedReconcile != nil && *strandedReconcile {
			recoverBranchDeleteFromMainRepo(logWriter, branch, worktreeDir)
			// Retry the delete from the main repo. If the recovery
			// succeeded the branch is already gone (and `git branch -D`
			// will report "not found", suppressed by
			// isMissingBranchError); if recovery failed, surface the
			// original failure.
			if retryOut, retryErr := exec.Command("git", "branch", "-D", branch).CombinedOutput(); retryErr != nil && !isMissingBranchError(retryErr, retryOut) {
				fmt.Fprintf(logWriter, "error: delete branch %s for issue %d: %v: %s\n", branch, issueNumber, retryErr, retryOut)
			}
		} else {
			fmt.Fprintf(logWriter, "error: delete branch %s for issue %d: %v: %s\n", branch, issueNumber, err, out)
		}
	}

	// Belt-and-suspenders: if the worktree directory still exists on disk
	// (e.g. a previous run crashed mid-`git worktree add` and left an orphan
	// dir that git never registered), remove it directly. Idempotent.
	// Skip in override mode — worktrees persist until sandman clean.
	if strandedReconcile == nil || !*strandedReconcile {
		if err := os.RemoveAll(wtPath); err != nil {
			fmt.Fprintf(logWriter, "error: remove worktree dir %s for issue %d: %v\n", wtPath, issueNumber, err)
		}
	}

	// Override preserves prior AgentRun history: events are append-only
	// and batch/run artifacts (batches.json, batch.json, run.log) are
	// retained. Only the worktree and branch are replaced for the new
	// attempt so the earlier blocked event remains inspectable.
	_ = eventLog
	_ = batchesIndexPath
}

// recoverBranchDeleteFromMainRepo attempts to unstick `git branch -D <branch>`
// when the branch is checked out somewhere that blocks the delete. It
// mirrors the recovery strategy from WorktreeSandbox.Start (issue #937):
//  1. Detect a stranded worktree at <worktreeBase>/<branch>; if present,
//     delete the branch from inside that worktree's cwd (which bypasses
//     the main-repo guard).
//  2. Otherwise, detect a foreign live worktree at a non-canonical path
//     holding the branch; if found, detach its HEAD via
//     `git -C <path> checkout --detach`. This is the only step that
//     preserves a foreign worktree's directory, `.git` gitlink, and
//     `.git/worktrees/<dir>` registration — the foreign worktree is left
//     in detached HEAD state at the same commit (no data loss).
//  3. Otherwise, if the main repo itself holds the branch (its HEAD is
//     `refs/heads/<branch>`), detach HEAD in the main repo (working tree
//     untouched) and delete the branch via `git branch -D`. The
//     `git branch -D` form (rather than raw `git update-ref -d`) re-checks
//     worktree holders atomically with the ref-drop, closing the TOCTOU
//     window where a sibling worktree could check out the branch between
//     the parent's detach and the ref-drop. This keeps the operator's
//     working-tree contents intact instead of force-checking out the base
//     branch underneath them.
//
// The guard in step 3 is critical: without verifying that the parent HEAD
// actually points at the branch, a foreign-worktree holder race (or a
// `git branch -D` failure caused by something other than a checked-out
// branch) would silently detach the parent's HEAD even when the parent
// is on a different branch. The ref-drop is also refused if the parent
// HEAD is already detached — we cannot tell whether the parent or a
// foreign worktree holds the branch in that state.
//
// On failure at any step, a warning is logged. The caller retries the
// delete after this returns.
func recoverBranchDeleteFromMainRepo(logWriter io.Writer, branch, worktreeBase string) {
	absBase, err := filepath.Abs(worktreeBase)
	if err != nil {
		fmt.Fprintf(logWriter, "warning: resolve worktree base for stranded recovery: %v\n", err)
		return
	}
	if info, stranded := sandbox.StrandedWorktree(".", absBase, branch); stranded {
		delCmd := exec.Command("git", "branch", "-D", branch)
		delCmd.Dir = info.Path
		if out, err := delCmd.CombinedOutput(); err != nil {
			fmt.Fprintf(logWriter, "warning: delete branch %s from stranded worktree %s: %v: %s\n", branch, info.Path, err, out)
		}
		return
	}
	// Strategy 2: foreign worktree (non-canonical, non-main-repo path).
	// ForeignStrandedWorktree may also return the main repo as a
	// "foreign" entry when the main repo is on the branch and its
	// path differs from the canonical worktree path; in that case we
	// fall through to strategy 3 below.
	if info, foreign := sandbox.ForeignStrandedWorktree(".", absBase, branch); foreign && !sandbox.SamePath(info.Path, ".") {
		if err := sandbox.ReleaseBranchInWorktree(info.Path); err != nil {
			fmt.Fprintf(logWriter, "warning: detach HEAD in foreign worktree %s: %v\n", info.Path, err)
		}
		return
	}
	// Strategy 3: main repo on branch. Detach HEAD and delete the branch
	// via `git branch -D`. The `git branch -D` form re-checks worktree
	// holders atomically with the ref-drop, closing the TOCTOU window
	// where a sibling worktree could check out the branch between the
	// parent's detach and the delete.
	headRef, headErr := sandbox.CurrentBranchRef(".")
	if headErr != nil {
		fmt.Fprintf(logWriter, "warning: drop refs/heads/%s: main repo HEAD is not a symbolic ref — refusing to detach (foreign worktree holder race?): %v\n", branch, headErr)
		return
	}
	if headRef != "refs/heads/"+branch {
		fmt.Fprintf(logWriter, "warning: drop refs/heads/%s: main repo HEAD is %q, not the target branch — refusing to detach (foreign worktree holder race?)\n", branch, headRef)
		return
	}
	if out, err := exec.Command("git", "checkout", "--detach").CombinedOutput(); err != nil {
		fmt.Fprintf(logWriter, "warning: detach HEAD in main repo: %v: %s\n", err, out)
		return
	}
	if out, err := exec.Command("git", "branch", "-D", branch).CombinedOutput(); err != nil {
		fmt.Fprintf(logWriter, "warning: drop branch refs/heads/%s in main repo via 'git branch -D': %v: %s\n", branch, err, out)
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
//	// later, from (*runSession).startOptsFor (renamed from applyOverrideAndIdentity in #2264):
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
