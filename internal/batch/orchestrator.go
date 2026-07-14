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
// orchestrator ↔ daemon agreement: the cmd layer's `Request.RunDir`
// (e.g. cmd/review.go's one-shot path) is the authoritative path
// the daemon's `prepareReviewRun` will read `decision.md` from, so
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

func collectIssueBranchesFromIndex(issueNumber int, title string, recordedBranch string, index eventLogIndex) []string {
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
	add(BranchName(issueNumber, title))
	return branches
}

func (o *Orchestrator) validateBatchBranches(ctx context.Context, req Request) error {
	return o.validateBatchBranchesWithIndex(ctx, req, readEventLogIndex(o.eventLog))
}

func (o *Orchestrator) validateBatchBranchesWithIndex(ctx context.Context, req Request, index eventLogIndex) error {
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

		for _, branch := range collectIssueBranchesFromIndex(num, title, req.Branches[num], index) {
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
	heartbeatTickInterval time.Duration
	errorLog              io.Writer
	phaseMu               sync.Mutex
	phaseWriter           io.Writer
	firstSandboxStartOnce func()

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
	verifyPath VerifyPathFunc

	issueCancelsMu sync.Mutex
	issueCancels   map[int]context.CancelFunc

	// activeMu guards activeRuns and the supervisor done-channel
	// slice. activeRuns maps the issue number (or 0 for prompt-only
	// runs) to the sandbox currently running it. The supervisor slice
	// captures the done channels of the per-session superviseShutdown
	// goroutines so RunBatch can fan in on real process exit. Both
	// fields are shared by every execute and executePromptOnly call
	// and share a single mutex.
	activeMu               sync.Mutex
	activeRuns             map[int]sandbox.Sandbox
	shutdownSupervisorDone []<-chan struct{}

	badgeHooker BadgeHooker
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
}

func newBatchStartGate(parallel int, delay time.Duration) *batchStartGate {
	return &batchStartGate{parallel: parallel, delay: delay}
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
			baseBranchSyncMu: &sync.Mutex{},
		},
		issueCancels:           make(map[int]context.CancelFunc),
		activeRuns:             make(map[int]sandbox.Sandbox),
		shutdownSupervisorDone: nil,
		badgeHooker:            nopBadgeHooker{},
	}
	for _, opt := range opts {
		opt(o)
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

// NewBadgeHooker returns a BadgeHooker that suggests a Built with Sandman
// badge PR after a batch with merged sandman/* PRs. The hook is silent
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

// trackShutdownSupervisor records a done channel returned by
// superviseShutdown so RunBatch can fan in on the actual process exit.
func (o *Orchestrator) trackShutdownSupervisor(done <-chan struct{}) {
	o.activeMu.Lock()
	o.shutdownSupervisorDone = append(o.shutdownSupervisorDone, done)
	o.activeMu.Unlock()
}

// snapshotShutdownSupervisors returns a copy of the current supervisor
// done-channel list. RunBatch calls this when ctx fires so it can
// wait on a fixed snapshot of the in-flight runs.
func (o *Orchestrator) snapshotShutdownSupervisors() []<-chan struct{} {
	o.activeMu.Lock()
	defer o.activeMu.Unlock()
	if len(o.shutdownSupervisorDone) == 0 {
		return nil
	}
	out := make([]<-chan struct{}, len(o.shutdownSupervisorDone))
	copy(out, o.shutdownSupervisorDone)
	return out
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

// registerActiveRun stores sb under the given key in the shared
// activeRuns map. The shutdownSupervisorDone channel is captured by
// RunBatch's fan-in goroutine so the batch-wide wait completes only
// after every per-session supervisor has reported the real exit.
func (o *Orchestrator) registerActiveRun(key int, sb sandbox.Sandbox) {
	o.activeMu.Lock()
	if o.activeRuns == nil {
		o.activeRuns = make(map[int]sandbox.Sandbox)
	}
	o.activeRuns[key] = sb
	o.activeMu.Unlock()
}

func (o *Orchestrator) unregisterActiveRun(key int) {
	o.activeMu.Lock()
	if o.activeRuns != nil {
		delete(o.activeRuns, key)
	}
	o.activeMu.Unlock()
}

func writePhase(w io.Writer, name string, started time.Time) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "phase %s duration=%s\n", name, time.Since(started))
}

func (o *Orchestrator) currentPhaseWriter() io.Writer {
	o.phaseMu.Lock()
	defer o.phaseMu.Unlock()
	return o.phaseWriter
}

// RunBatch executes the requested AgentRuns in parallel.
func (o *Orchestrator) RunBatch(ctx context.Context, req Request) (*Result, error) {
	cfg, err := o.configStore.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	o.phaseMu.Lock()
	o.phaseWriter = req.PhaseWriter
	o.phaseMu.Unlock()
	o.layout = paths.NewLayout(cfg, o.layout.RepoRoot)
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
	writePhase(o.currentPhaseWriter(), "sandbox-preflight", phaseStarted)
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

	for _, num := range req.Issues {
		if req.IssueMode(num) != ModeOverride {
			continue
		}
		issue, err := o.githubClient.FetchIssue(ctx, num)
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: fetch issue %d for force-clean: %v\n", num, err)
			continue
		}
		branches := collectIssueBranchesFromIndex(num, issue.Title, req.Branches[num], overrideIndex)
		for _, branch := range branches {
			ClearIssueArtifacts(num, branch, cfg.WorktreeDir, o.eventLog, o.errorLog, baseBranch, req.StrandedReconcile, o.layout.BatchesIndexPath)
		}
	}

	eventIndex := eventLogIndex{priorRunByIssue: map[int]bool{}, branchesByIssue: map[int][]string{}}
	if len(req.Issues) > 0 {
		eventIndex = readEventLogIndex(o.eventLog)
	}

	phaseStarted = time.Now()
	if err := o.validateBatchBranchesWithIndex(ctx, req, eventIndex); err != nil {
		return nil, err
	}
	writePhase(o.currentPhaseWriter(), "branch-validation", phaseStarted)

	dangerouslySkipPermissions := req.DangerouslySkipPermissions
	if dangerouslySkipPermissions == nil {
		dangerouslySkipPermissions = &isContainer
	}

	strandedReconcile := true
	if req.StrandedReconcile != nil {
		strandedReconcile = *req.StrandedReconcile
	}

	if len(req.Issues) == 0 && (req.PromptConfig.PromptFlag != "" || req.PromptConfig.TemplateFlag != "" || req.PromptConfig.TaskPrompt != "") {
		return o.runPromptOnly(ctx, cfg, agentName, agentCfg, newBatchIdentityResolver(o, "."), policy.sandboxFactory, policy.containerAlloc, req, baseBranch, startDelay, parallel, retries, sandboxMode, containerCapacityForLog, req.ContainerCapacitySet, maxContainersForLog, req.MaxContainersSet, *dangerouslySkipPermissions, strandedReconcile)
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

	batchIdentityResolver := newBatchIdentityResolver(o, ".")
	issueBatchID := issueBatchIDForRequest(req)

	// Reset the per-batch supervisor set so leftover entries from a
	// previous RunBatch call on the same orchestrator do not stall
	// this batch's fan-in.
	o.activeMu.Lock()
	o.shutdownSupervisorDone = nil
	o.activeMu.Unlock()

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

		for _, done := range o.snapshotShutdownSupervisors() {
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
		runID := buildRunID(num, req.RunTS, req.RunShortID)
		if o.eventLog != nil && (len(dependencies[num]) > 0 || (effectiveParallel > 0 && effectiveParallel < len(req.Issues))) {
			queuedPayload := map[string]any{"blocked_by": dependencies[num]}
			if title, ok := req.IssueTitles[num]; ok && title != "" {
				queuedPayload["issue_title"] = title
			} else if issue, err := o.githubClient.FetchIssue(ctx, num); err == nil && issue != nil {
				queuedPayload["issue_title"] = issue.Title
			}
			if issueBatchID != "" {
				queuedPayload["batch_id"] = issueBatchID
			}
			_ = o.eventLog.Log(events.Event{
				Type:      "run.queued",
				Timestamp: time.Now(),
				RunID:     runID,
				Issue:     num,
				IssueRef:  issueRef(num),
				Payload:   queuedPayload,
			})
		}
		go func(idx, issueNum int, blockers []int, turn int, runID string) {
			defer wg.Done()
			defer close(completed[issueNum])

			issueCtx, issueCancel := context.WithCancel(ctx)
			o.registerIssueCancel(issueNum, issueCancel)
			defer o.unregisterIssueCancel(issueNum)
			defer issueCancel()

			// parentCtx is the RunBatch ctx — it is only
			// cancelled by an external abort (e.g. parent ctx
			// cancellation), not by the per-issue abort or the
			// session's normal end. The supervisor in execute
			// uses parentCtx to decide whether to shut down the
			// process; a normal session end (no parent abort)
			// leaves the process alone.
			parentCtx := ctx

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
				blockerStatus := events.RunStatusFromPayload(status)
				switch {
				case blockerStatus.IsAborted():
					abortedBy = append(abortedBy, blocker)
				case blockerStatus.IsSuccess():
					issue, err := o.githubClient.FetchIssue(issueCtx, blocker)
					if err == nil && issue != nil && strings.EqualFold(issue.State, "open") {
						stillBlockedBy = append(stillBlockedBy, blocker)
					}
				case blockerStatus.IsTerminal() && !blockerStatus.IsSuccess():
					stillBlockedBy = append(stillBlockedBy, blocker)
				}
				// non-terminal statuses (running/queued/unknown/empty) intentionally
				// fall through so the dependent proceeds; this guards against a
				// blocker that wrote no status (e.g. panicked before writing).
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
				o.logBlocked(issueNum, stillBlockedBy, runID, issueBatchID)

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

			mode := req.IssueMode(issueNum)
			renderCfg := req.PromptConfig
			if mode == ModeContinue {
				if taskPrompt, ok := req.TaskPrompts[issueNum]; ok {
					renderCfg.TaskPrompt = taskPrompt
				}
			}
			issueBaseBranch := baseBranch
			if mode == ModeContinue {
				if perIssueBaseBranch, ok := req.BaseBranches[issueNum]; ok && strings.TrimSpace(perIssueBaseBranch) != "" {
					issueBaseBranch = perIssueBaseBranch
				}
			}

			res, started := o.runSingle(issueCtx, parentCtx, issueNum, cfg, agentName, agentCfg, mode == ModeContinue, req.PreviousRunIDs, batchIdentityResolver, req.Branches, renderCfg, req.OutputWriter, policy.sandboxFactory, policy.containerAlloc, mode == ModeOverride, issueBaseBranch, req.Blocked[issueNum], parallel, startDelay, retries, runIdleTimeout, sandboxMode, containerCapacityForLog, req.ContainerCapacitySet, maxContainersForLog, req.MaxContainersSet, *dangerouslySkipPermissions, strandedReconcile, req.RunTS, req.RunShortID, issueBatchID)
			if started {
				defer startGate.Release()
			} else {
				defer startGate.ReleaseWithoutDelay()
			}
			mu.Lock()
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
		}(inputIndex[num], num, dependencies[num], turn, runID)
	}

	wg.Wait()

	if policy.mode == "docker" || policy.mode == "podman" {
		for _, result := range results {
			if strings.TrimSpace(result.Branch) == "" {
				continue
			}
			worktreePath := filepath.Join(o.orchestratorWorktreeDir(cfg), result.Branch)
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

func (o *Orchestrator) logBlocked(issueNum int, blockers []int, runID string, batchID string) {
	if o.eventLog == nil {
		return
	}
	payload := map[string]any{"blocked_by": blockers}
	if batchID != "" {
		payload["batch_id"] = batchID
	}
	_ = o.eventLog.Log(events.Event{
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
// sandbox-timeout, kill-timeout, manual) is locked in ADR-0035 (slice 5
// of #1498) and must not be silently extended. If a future code path
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
func (o *Orchestrator) logRetry(runID, branch string, attempt, maxAttempts int, previousStatus, reason, logPath string, issueNumber int) {
	if o.eventLog == nil {
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
			"last_log_lines":  readTailLines(logPath, 3),
		},
	}
	if issueNumber > 0 {
		event.IssueRef = issueRef(issueNumber)
	}
	_ = o.eventLog.Log(event)
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
	mode                       IssueMode
	previousRunIDs             map[int]string
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
	review      bool
	prNumber    int
	reviewFocus string

	// opts carries the test-injection hooks copied from
	// Orchestrator.runSessionOpts at session construction. Zero-valued in
	// production; populated by tests to drive the per-session behaviour.
	opts runSessionOptions
}

// applyOverrideAndIdentity applies the session mode override and the resolved git identity to wt.
// On identity failure returns (_, false) so the caller can short-circuit.
// worktreeDir returns the resolved per-row worktree base
// directory. The layout (set by NewOrchestrator via
// paths.NewLayout) is the source of truth; sessions that don't
// have an initialised layout (tests that construct Orchestrator
// directly without calling NewOrchestrator) fall back to
// cfg.WorktreeDir.
func (s *runSession) worktreeDir() string {
	if s.o.layout.RepoRoot != "" {
		return s.o.layout.WorktreeDir
	}
	if s.cfg != nil {
		return strings.TrimSpace(s.cfg.WorktreeDir)
	}
	return ""
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
		return filepath.Join(s.o.layout.BatchesDir, "runs", runID)
	}
	return s.o.layout.RunFolder(batchID, runID)
}

// runLogPathFor returns the per-row run.log path. Mirrors runFolderFor
// so the path always routes through paths.Layout for a known batchID.
func (s *runSession) runLogPathFor(runID string) string {
	batchID := s.batchID
	if batchID == "" {
		batchID = batchIDFromRunID(runID)
	}
	if batchID == "" {
		return filepath.Join(s.o.layout.BatchesDir, "runs", runID, "run.log")
	}
	return s.o.layout.RunLogPath(batchID, runID)
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
// it with the continuation prompt. Used by ModeContinue (slice 9 B3) so
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

func (s *runSession) applyOverrideAndIdentity(wt sandbox.Sandbox, branch string) (AgentRunResult, bool) {
	o := s.o
	wt.SetOverride(s.mode == ModeOverride)
	wt.SetStrandedReconcile(s.strandedReconcile)
	wt.SetContinue(s.mode == ModeContinue)
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
func (s *runSession) withHeartbeat(ctx context.Context, runID string, attempt int, logPath string, wt sandbox.Sandbox, fn func() AgentRunResult) (AgentRunResult, bool) {
	o := s.o
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
	if abortedByHeartbeat && !events.RunStatusFromPayload(result.Status).IsSuccess() {
		result.Status = "aborted"
	}
	return result, abortedByHeartbeat
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
// `CONFLICTING`, the run is reclassified as `failure` and the terminal event
// payload carries `merge_conflict: true` and the PR number. This is
// defence-in-depth against skill regressions that forget to flag the DIRTY
// PR case; see issue #1684.
func (s *runSession) emitTerminal(ctx context.Context, runID string, result AgentRunResult, extras map[string]any) string {
	o := s.o
	if conflictExtras, ok := s.detectConflictingPR(result.Branch); ok {
		result.Status = "failure"
		if extras == nil {
			extras = map[string]any{}
		}
		for k, v := range conflictExtras {
			extras[k] = v
		}
	}
	terminalEventType, terminalStatus := terminalRunEvent(ctx, result.Status)
	s.updateRunManifestStatus(runID, batchindex.RunManifestStatus(terminalStatus))
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
		if s.issueNumber > 0 {
			event.Payload["issue_number"] = s.issueNumber
		}
	}
	for k, v := range extras {
		event.Payload[k] = v
	}
	_ = o.eventLog.Log(event)
	return terminalStatus
}

// emitEarlyFailure logs a terminal run.finished event (status "failure") for
// an early-return path in execute() that exits before run.started is emitted.
// Without this event, the portal keeps the run in its last projected state
// (typically "queued"), and dependent runs see a silently-set failure status
// with no corresponding event in the log. See issue #2136.
//
// The error detail is already written to errorLog (stderr) by the caller;
// this event makes the failure visible in the event log, the portal, and the
// RunState projection consumed by dependent issues' blocker-wait loops.
//
// This does NOT emit run.started — the run never started the agent — so
// ProjectRunStates folds run.finished directly, transitioning the run from
// "queued" to a terminal "failure" status. Safe to call when no run manifest
// exists yet (the manifest is written later in execute()).
//
// reason should be a short diagnostic string identifying the failure point
// (e.g. "fetch issue", "start sandbox"). The full error is already on stderr.
func (s *runSession) emitEarlyFailure(reason, branch string) {
	o := s.o
	if o.eventLog == nil {
		return
	}
	runID := buildRunID(s.issueNumber, s.runTS, s.runShortID)
	_ = o.eventLog.Log(events.Event{
		Type:      "run.finished",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     s.issueNumber,
		IssueRef:  issueRef(s.issueNumber),
		Payload: map[string]any{
			"status":        "failure",
			"branch":        branch,
			"base_branch":   s.baseBranch,
			"retries_total": s.retries,
			"retries_done":  0,
			"early_failure": true,
			"error":         reason,
		},
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
	o := s.o
	if strings.TrimSpace(branch) == "" {
		return nil, false
	}
	exists, prNumber, mergeable, err := LookupOpenPR(branch)
	if err != nil {
		fmt.Fprintf(o.errorLog, "warning: lookup open PR for branch %q: %v\n", branch, err)
		return nil, false
	}
	if !exists {
		return nil, false
	}
	if strings.EqualFold(mergeable, "CONFLICTING") {
		fmt.Fprintf(o.errorLog, "error: branch %q has CONFLICTING open PR #%d, overriding run status to failure\n", branch, prNumber)
		return map[string]any{"merge_conflict": true, "pr_number": prNumber}, true
	}
	return nil, false
}

// runVerifyPath is the seam the orchestrator uses to invoke the
// verify chain. Production code uses DefaultVerifyPath; tests
// inject a VerifyPathFunc literal to drive outcomes without touching
// real git or GitHub.
func (o *Orchestrator) runVerifyPath(ctx context.Context, in VerifyInput) (VerifyOutcome, []OracleCheck) {
	if o == nil {
		return RunVerifyPath(in)
	}
	if o.verifyPath != nil {
		return o.verifyPath(in)
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
func lookupPRForVerify(ctx context.Context, o *Orchestrator, branch string) *github.PR {
	if o == nil || o.githubClient == nil || strings.TrimSpace(branch) == "" {
		return nil
	}
	pr, err := o.githubClient.FindPRByBranch(ctx, branch)
	if err != nil {
		if o.errorLog != nil {
			fmt.Fprintf(o.errorLog, "warning: lookup PR for verify path on branch %q: %v\n", branch, err)
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
func hasBlockingOpenPR(o *Orchestrator, branch string) (map[string]any, bool) {
	if o == nil || strings.TrimSpace(branch) == "" {
		return nil, false
	}
	exists, prNumber, _, err := LookupOpenPR(branch)
	if err != nil {
		fmt.Fprintf(o.errorLog, "warning: lookup open PR for branch %q: %v\n", branch, err)
		return nil, false
	}
	if !exists {
		return nil, false
	}
	fmt.Fprintf(o.errorLog, "error: branch %q has open PR #%d blocking alreadyResolved short-circuit; overriding run status to failure\n", branch, prNumber)
	return map[string]any{"blocker": "open-pr-blocks-already-resolved", "pr_number": prNumber}, true
}

// updateRunManifestStatus rewrites the run.json snapshot with the terminal
// status. Failures are logged to errorLog and ignored; the event log remains
// authoritative.
func (s *runSession) updateRunManifestStatus(runID string, status batchindex.RunManifestStatus) {
	o := s.o
	batchDir := o.layout.BatchDir(s.batchID)
	if s.batchID == "" {
		batchDir = o.layout.BatchesDir
	}
	if err := daemon.UpdateRunManifestStatus(batchDir, runID, status); err != nil {
		fmt.Fprintf(o.errorLog, "error: update run manifest status for run %s: %v\n", runID, err)
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
	prepareAttempt func(attempt int) (prompt.RenderConfig, *AgentRunResult),
) (AgentRunResult, map[string]any, bool) {
	o := s.o
	if s.renderCfg.PromptFile == "" {
		s.renderCfg.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if s.renderCfg.RenderedPromptFile == "" {
		s.renderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "task.md")
	}

	attempts := s.retries + 1
	var result AgentRunResult
	var abortedByHeartbeat bool

	factory := o.runnableFactory
	if factory == nil {
		factory = defaultRunnableFactory{}
	}

	var terminalExtras map[string]any
	for attempt := 0; attempt < attempts; attempt++ {
		attemptRenderCfg, errResult := prepareAttempt(attempt)
		if errResult != nil {
			return *errResult, nil, events.RunStatusFromPayload(errResult.Status).IsSuccess()
		}

		if attempt > 0 {
			reason := mapRetryReason(result.Status, abortedByHeartbeat, s.parentCtx)
			o.logRetry(runID, branch, attempt+1, attempts, result.Status, reason, logPath, s.issueNumber)
		}

		runnable := factory.NewRunnable(issue, branch, wt)
		// Slice 9 B3: when launching a continuation, copy the original
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
					fmt.Fprintf(o.errorLog, "warning: snapshot task.md for continuation run %s: %v\n", runID, err)
				}
			}
		}
		if agentRun, ok := runnable.(*AgentRun); ok {
			agentRun.env = s.agentCfg.Env
			agentRun.preset = s.agentCfg.Preset
			agentRun.model = s.agentCfg.Model
			agentRun.modelProvider = s.agentCfg.ModelProvider
			agentRun.modelName = s.agentCfg.ModelName
			agentRun.opencodePermissionMode = s.agentCfg.OpencodePermissionMode
			agentRun.baseBranch = s.baseBranch
			agentRun.runID = runID
			agentRun.review = s.review
			agentRun.outputWriter = s.outputWriter
			agentRun.dangerouslySkipPermissions = &s.dangerouslySkipPermissions
			agentRun.sessionName = "Sandman " + runID + ": "
			agentRun.runFolder = s.runFolderFor(runID)
		}

		result, abortedByHeartbeat = s.withHeartbeat(ctx, runID, attempt, logPath, wt, func() AgentRunResult {
			return runnable.Run(ctx, o.renderer, s.agentCfg.Command, attemptRenderCfg)
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
		alreadyResolved := hasExactTaskStatus(taskContent, "## Status: already resolved")
		if mergeRequired {
			prMerged := checkPRMerged(ctx, o.githubClient, branch)
			if events.RunStatusFromPayload(result.Status).IsAborted() {
				continue
			}
			if prMerged || alreadyResolved {
				if ctx.Err() != nil {
					break
				}
				if alreadyResolved {
					pr := lookupPRForVerify(ctx, o, branch)
					outcome, checks := o.runVerifyPath(ctx, VerifyInput{Context: ctx, Issue: issue, Branch: branch, WorkDir: wt.WorkDir(), PR: pr})
					if outcome != VerifyNoSignal {
						terminalExtras = mergeVerificationExtras(terminalExtras, outcome, checks)
						if outcome == VerifyFailed {
							result.Status = "failure"
							break
						}
						// VerifyVerified: drop the conservative backstop;
						// the oracle proved the issue is already resolved.
						result.Status = "success"
						if issue != nil && !github.IsIssueClosed(issue) && o.githubClient != nil {
							if err := o.githubClient.CloseIssue(ctx, issue.Number, "Closed by sandman — issue already completed."); err != nil {
								fmt.Fprintf(o.errorLog, "error: close issue %d: %v\n", issue.Number, err)
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
					if extras, blocked := hasBlockingOpenPR(o, branch); blocked {
						terminalExtras = mergeBlockerExtras(terminalExtras, extras)
						result.Status = "failure"
						break
					}
				}
				result.Status = "success"
				if alreadyResolved && issue != nil && !github.IsIssueClosed(issue) && o.githubClient != nil {
					if err := o.githubClient.CloseIssue(ctx, issue.Number, "Closed by sandman — issue already completed."); err != nil {
						fmt.Fprintf(o.errorLog, "error: close issue %d: %v\n", issue.Number, err)
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
			if alreadyResolved && issue != nil && !github.IsIssueClosed(issue) && o.githubClient != nil {
				if err := o.githubClient.CloseIssue(ctx, issue.Number, "Closed by sandman — issue already completed."); err != nil {
					fmt.Fprintf(o.errorLog, "error: close issue %d: %v\n", issue.Number, err)
				}
			}
			if events.RunStatusFromPayload(result.Status).IsSuccess() || alreadyResolved {
				if issue != nil && o.githubClient != nil {
					prMerged := checkPRMerged(ctx, o.githubClient, branch)
					if prMerged || alreadyResolved {
						if alreadyResolved {
							pr := lookupPRForVerify(ctx, o, branch)
							outcome, checks := o.runVerifyPath(ctx, VerifyInput{Context: ctx, Issue: issue, Branch: branch, WorkDir: wt.WorkDir(), PR: pr})
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
							if extras, blocked := hasBlockingOpenPR(o, branch); blocked {
								terminalExtras = mergeBlockerExtras(terminalExtras, extras)
								result.Status = "failure"
								break
							}
							result.Status = "success"
						}
						break
					}
					result.Status = "failure"
				} else {
					break
				}
			}
		}
	}

	return result, terminalExtras, true
}

// runSingle runs a single issue-driven AgentRun. It builds a runSession and
// delegates to (*runSession).execute. parentCtx is the RunBatch ctx
// (the ctx that owns this whole batch); the supervisor uses it to
// distinguish external aborts from normal session end.
func (o *Orchestrator) runSingle(ctx context.Context, parentCtx context.Context, num int, cfg *config.Config, agentName string, agentCfg config.Agent, continuation bool, previousRunIDs map[int]string, identityResolver *gitIdentityResolver, branches map[int]string, renderCfg prompt.RenderConfig, outputWriter io.Writer, sbFactory SandboxFactory, containerAlloc containerAllocator, override bool, baseBranch string, externalBlockers []int, parallel int, startDelay time.Duration, retries int, runIdleTimeout int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool, strandedReconcile bool, runTS string, runShortID string, batchID ...string) (AgentRunResult, bool) {
	issueBatchID := ""
	if len(batchID) > 0 {
		issueBatchID = strings.TrimSpace(batchID[0])
	}
	if issueBatchID == "" && (runTS != "" || runShortID != "") {
		issueBatchID = batchIDFromRunID(buildRunID(num, runTS, runShortID))
	}
	s := &runSession{
		o:           o,
		issueNumber: num,
		cfg:         cfg,
		agentName:   agentName,
		agentCfg:    agentCfg,
		mode: func() IssueMode {
			if continuation {
				return ModeContinue
			}
			if override {
				return ModeOverride
			}
			return ModeFresh
		}(),
		previousRunIDs:             previousRunIDs,
		identityResolver:           identityResolver,
		branches:                   branches,
		renderCfg:                  renderCfg,
		outputWriter:               outputWriter,
		sbFactory:                  sbFactory,
		containerAlloc:             containerAlloc,
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
		strandedReconcile:          strandedReconcile,
		runTS:                      runTS,
		runShortID:                 runShortID,
		batchID:                    issueBatchID,
		parentCtx:                  parentCtx,
		opts:                       o.runSessionOpts,
	}
	return s.execute(ctx)
}

// execute runs the issue-driven AgentRun lifecycle owned by this session. It
// contains the body that previously lived in (*Orchestrator).runSingle.
func (s *runSession) execute(ctx context.Context) (AgentRunResult, bool) {
	o := s.o
	issue, err := o.githubClient.FetchIssue(ctx, s.issueNumber)
	if err != nil {
		fmt.Fprintf(o.errorLog, "error: fetch issue %d: %v\n", s.issueNumber, err)
		s.emitEarlyFailure("fetch issue", s.branches[s.issueNumber])
		return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure"}, false
	}

	branch := s.branches[s.issueNumber]
	if branch == "" {
		branch = BranchName(issue.Number, issue.Title)
	}
	if s.mode != ModeContinue {
		if err := o.syncBaseBranch(".", s.baseBranch); err != nil {
			fmt.Fprintf(o.errorLog, "error: sync base branch for issue %d: %v\n", s.issueNumber, err)
			s.emitEarlyFailure("sync base branch", branch)
			return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}, false
		}
	}
	sandboxStarted := time.Now()
	var container sandbox.Container
	if s.containerAlloc != nil {
		lease, err := s.containerAlloc.Acquire()
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: acquire container for issue %d: %v\n", s.issueNumber, err)
			s.emitEarlyFailure("acquire container", branch)
			return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}, false
		}
		container = lease.container
		defer lease.Release()
	}

	wt := s.sbFactory.NewSandbox(".", s.worktreeDir(), branch, s.baseBranch, container)
	if errResult, ok := s.applyOverrideAndIdentity(wt, branch); !ok {
		s.emitEarlyFailure("resolve git identity", branch)
		return errResult, false
	}
	if err := wt.Start(); err != nil {
		fmt.Fprintf(o.errorLog, "error: start sandbox for issue %d: %v\n", s.issueNumber, err)
		s.emitEarlyFailure("start sandbox", branch)
		return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}, false
	}
	sandboxStartOnce := func() {}
	sandboxStartOnce = func() { writePhase(o.currentPhaseWriter(), "first-sandbox-start", sandboxStarted) }
	o.phaseMu.Lock()
	if o.firstSandboxStartOnce == nil {
		o.firstSandboxStartOnce = func() { sandboxStartOnce() }
	}
	once := o.firstSandboxStartOnce
	o.phaseMu.Unlock()
	once()
	// Guaranteed cleanup: defer wt.RestoreHostPaths() so container
	// sandboxes normalize the preserved worktree's .git pointer back to
	// host paths on every exit path including panic, cancellation,
	// timeout, and normal completion. Worktree-only sandboxes no-op
	// this. The defer does NOT call Stop() — the worktree is preserved
	// on success for --continue reuse. Issue #2189.
	defer func() { _ = wt.RestoreHostPaths() }()

	blockedBy, err := o.recheckBlockedBy(ctx, s.externalBlockers)
	if err != nil {
		fmt.Fprintf(o.errorLog, "error: recheck blockers for issue %d: %v\n", s.issueNumber, err)
		_ = wt.Stop()
		s.emitEarlyFailure("recheck blockers", branch)
		return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}, false
	}
	runID := buildRunID(s.issueNumber, s.runTS, s.runShortID)
	if len(blockedBy) > 0 {
		res := AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "blocked", Branch: branch}
		o.logBlocked(s.issueNumber, blockedBy, runID, s.batchID)
		_ = wt.Stop()
		return res, false
	}

	o.registerActiveRun(s.issueNumber, wt)
	defer o.unregisterActiveRun(s.issueNumber)

	batchDir := s.o.layout.BatchDir(s.batchID)
	manifestBatchID := s.batchID
	if s.batchID == "" {
		batchDir = s.o.layout.BatchesDir
		manifestBatchID = batchIDFromRunID(runID)
	}
	runManifest := batchindex.RunManifest{
		RunID:        runID,
		BatchID:      manifestBatchID,
		Issue:        s.issueNumber,
		Branch:       branch,
		BaseBranch:   s.baseBranch,
		WorktreePath: wt.WorkDir(),
		Kind:         batchindex.KindIssue,
		CreatedAt:    time.Now(),
		Status:       batchindex.RunManifestStatusActive,
	}
	if err := daemon.WriteRunManifest(batchDir, runID, runManifest); err != nil {
		fmt.Fprintf(o.errorLog, "error: write run manifest for issue %d: %v\n", s.issueNumber, err)
		_ = wt.Stop()
		s.emitEarlyFailure("write run manifest", branch)
		return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}, false
	}
	cmdServer := daemon.NewCommandServer(daemon.RunFolder(batchDir, runID), s.o)
	if err := cmdServer.Start(); err != nil {
		fmt.Fprintf(o.errorLog, "error: start command server for issue %d: %v\n", s.issueNumber, err)
	} else {
		defer cmdServer.Stop()
	}

	// Pre-register the supervisor's done channel with the batch-wide
	// fan-in BEFORE spawning the supervisor, so a session that
	// started just before ctx fired cannot race the snapshot taken
	// by RunBatch's fan-in goroutine.
	supervisorDone := make(chan struct{})
	o.trackShutdownSupervisor(supervisorDone)
	sessionCtx, cancelSession := context.WithCancel(ctx)
	go func() {
		defer close(supervisorDone)
		<-sessionCtx.Done()
		if s.parentCtx != nil && s.parentCtx.Err() != nil {
			if proc := wt.Process(); proc != nil {
				done := superviseShutdown(ctx, proc, s.opts.killTimeout)
				<-done
			}
		}
	}()
	defer func() {
		cancelSession()
		<-supervisorDone
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
			"issue_title":            issue.Title,
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
		if s.mode == ModeContinue {
			payload["previous_run_id"] = s.previousRunIDs[s.issueNumber]
		}
		if promptSourceValue != "" && s.mode != ModeContinue {
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
		if s.batchID != "" {
			payload["batch_id"] = s.batchID
		}
		eventType := "run.started"
		if s.mode == ModeContinue {
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

	logPath := s.runLogPathFor(runID)
	result, terminalExtras, started := s.runOnce(ctx, issue, branch, wt, logPath, runID, s.mode != ModeContinue, func(attempt int) (prompt.RenderConfig, *AgentRunResult) {
		attemptRenderCfg := s.renderCfg
		if attempt > 0 {
			// Pre-retry guard: if the PR was merged between attempts (e.g. the
			// agent merged it on attempt 0 but exited non-zero due to a
			// transient error), short-circuit to success without launching
			// the agent again, resetting the branch, or re-rendering the
			// prompt. The merged PR is the sole success signal for
			// issue-driven runs (see #860). ModeContinue uses a different
			// `prepareAttempt` closure (the prompt-only one) that does not
			// contain this guard, so continuation replays are unaffected.
			if checkPRMerged(ctx, o.githubClient, branch) {
				return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "success", Branch: branch, RetriesTotal: attempt}
			}
			taskPath := filepath.Join(wt.WorkDir(), ".sandman", "task.md")
			openPR, prLookupErr := findOpenPRByBranch(ctx, o.githubClient, branch)
			// Always pass the task content verbatim (or empty template if
			// missing). The agent reads its next instruction from the task
			// document's ## Next Step field directly. The openPR value is only
			// used below to decide whether to reset the branch — the agent
			// receives the same task content regardless of open-PR state.
			taskContent, taskExists, err := ReadTaskContent(taskPath)
			if err != nil {
				fmt.Fprintf(o.errorLog, "error: read task for issue %d: %v\n", s.issueNumber, err)
				return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch, RetriesTotal: attempt}
			}
			attemptRenderCfg.TaskPrompt = prompt.ContinuationTaskPrompt(taskContent)
			attemptRenderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "task.md")
			if !taskExists && openPR == nil {
				if prLookupErr != nil {
					fmt.Fprintf(o.errorLog, "error: lookup PR for issue %d: %v\n", s.issueNumber, prLookupErr)
					return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch, RetriesTotal: attempt}
				}
				if err := o.resetRetryBranch(ctx, wt, branch, s.baseBranch); err != nil {
					fmt.Fprintf(o.errorLog, "error: reset retry branch for issue %d: %v\n", s.issueNumber, err)
					return attemptRenderCfg, &AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch, RetriesTotal: attempt}
				}
			}
			if err := logRetryMarkerFn(logPath, attempt, s.retries); err != nil {
				if o.errorLog != nil {
					fmt.Fprintf(o.errorLog, "warning: write retry marker for issue %d: %v\n", s.issueNumber, err)
				}
			}
		}
		return attemptRenderCfg, nil
	})
	if !started {
		return result, false
	}

	result.Status = s.emitTerminal(ctx, runID, result, terminalExtras)

	if events.RunStatusFromPayload(result.Status).IsSuccess() {
		s.reconcileWorktreeBranch(wt, branch)
	}

	return result, true
}

// reconcileWorktreeBranch returns the worktree's HEAD to the issue branch
// when it has drifted onto some other ref (e.g. after `gh pr merge --squash
// --delete-branch` leaves the worktree on the local base branch, or when
// the worktree ends up on a detached HEAD). The PR merge itself succeeded
// on GitHub, so any failure here is logged as a warning and the run still
// returns success.
func (s *runSession) reconcileWorktreeBranch(wt sandbox.Sandbox, branch string) {
	o := s.o
	workDir := wt.WorkDir()
	if workDir == "" {
		return
	}
	// Guard: if the worktree directory is not a valid git directory
	// (e.g. its .git file points to a stale/removed worktree
	// registration), skip the checkout to avoid the
	// "not a git repository: (null)" error. See #1189.
	if !sandbox.IsGitDir(workDir) {
		fmt.Fprintf(o.errorLog, "warning: reconcile worktree branch: worktree at %q is not a valid git directory; skipping checkout\n", workDir)
		return
	}
	expectedRef := "refs/heads/" + branch
	if currentRef, err := sandbox.CurrentBranchRef(workDir); err == nil && currentRef == expectedRef {
		return
	}
	if !sandbox.BranchExists(wt.RepoPath(), branch) {
		fmt.Fprintf(o.errorLog, "warning: reconcile worktree branch: branch %q was deleted; next run will recreate it\n", branch)
		return
	}
	cmd := exec.Command("git", "-C", workDir, "checkout", "-f", branch)
	cmd.Dir = wt.RepoPath()
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(o.errorLog, "warning: reconcile worktree branch: git checkout -f %s: %v\n%s\n", branch, err, out)
		return
	}
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

		issue, err := o.githubClient.FetchIssue(ctx, blocker)
		if err != nil {
			return nil, fmt.Errorf("fetch blocker issue %d: %w", blocker, err)
		}
		if !github.IsIssueClosed(issue) {
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
	command := fmt.Sprintf("git reset --hard && git checkout -f -B %s %s && git clean -fd", shellenv.Quote(branch), shellenv.Quote(baseBranch))
	if err := sb.Exec(ctx, command, &output, &output); err != nil {
		return fmt.Errorf("reset retry branch: %w\n%s", err, output.String())
	}
	return nil
}

func (o *Orchestrator) runPromptOnly(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, identityResolver *gitIdentityResolver, sbFactory SandboxFactory, containerAlloc containerAllocator, req Request, baseBranch string, startDelay time.Duration, parallel int, retries int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool, strandedReconcile bool) (*Result, error) {
	branch := promptOnlyBranch(req.PromptConfig)
	result, started := o.runPromptOnlySingle(ctx, cfg, agentName, agentCfg, identityResolver, branch, req.PromptConfig, req.OutputWriter, sbFactory, containerAlloc, req.IssueMode(0), baseBranch, startDelay, parallel, retries, sandboxMode, containerCapacity, containerCapacitySet, maxContainers, maxContainersSet, dangerouslySkipPermissions, strandedReconcile, req.Review, req.PRNumber, req.ReviewFocus, req.RunID, req.PreviousRunIDs, req.IssueNumber, req.BatchTS, req.BatchShortID, req.RunDir)
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

// runPromptOnlySingle runs a single prompt-only AgentRun. It builds a
// runSession and delegates to (*runSession).executePromptOnly.
func (o *Orchestrator) runPromptOnlySingle(ctx context.Context, cfg *config.Config, agentName string, agentCfg config.Agent, identityResolver *gitIdentityResolver, branch string, renderCfg prompt.RenderConfig, outputWriter io.Writer, sbFactory SandboxFactory, containerAlloc containerAllocator, mode IssueMode, baseBranch string, startDelay time.Duration, parallel int, retries int, sandboxMode string, containerCapacity int, containerCapacitySet bool, maxContainers int, maxContainersSet bool, dangerouslySkipPermissions bool, strandedReconcile bool, review bool, prNumber int, reviewFocus string, runID string, previousRunIDs map[int]string, reviewIssueNumber int, batchTS string, batchShortID string, runDir string) (AgentRunResult, bool) {
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
		mode:                       mode,
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
		strandedReconcile:          strandedReconcile,
		previousRunIDs:             previousRunIDs,
		review:                     review,
		prNumber:                   prNumber,
		reviewFocus:                reviewFocus,
		issueNumber:                reviewIssueNumber,
		runID:                      runID,
		batchTS:                    batchTS,
		batchShortID:               batchShortID,
		batchID:                    batchIDForPromptOnly(batchTS, batchShortID, runID, runDir),
		userProvidedRunID:          runID,
		parentCtx:                  ctx,
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
	if s.mode != ModeContinue {
		if err := o.syncBaseBranch(".", s.baseBranch); err != nil {
			fmt.Fprintf(o.errorLog, "error: sync base branch for prompt-only run: %v\n", err)
			return AgentRunResult{Status: "failure", Branch: branch, Review: s.review, RunID: s.runID}, false
		}
	}
	var container sandbox.Container
	if s.containerAlloc != nil {
		lease, err := s.containerAlloc.Acquire()
		if err != nil {
			fmt.Fprintf(o.errorLog, "error: acquire container for prompt-only run: %v\n", err)
			return AgentRunResult{Status: "failure", Branch: branch, Review: s.review, RunID: s.runID}, false
		}
		container = lease.container
		defer lease.Release()
	}

	wt := s.sbFactory.NewSandbox(".", s.worktreeDir(), branch, s.baseBranch, container)
	if errResult, ok := s.applyOverrideAndIdentity(wt, branch); !ok {
		return errResult, false
	}
	if err := wt.Start(); err != nil {
		fmt.Fprintf(o.errorLog, "error: start sandbox for prompt-only run: %v\n", err)
		return AgentRunResult{Status: "failure", Branch: branch, Review: s.review, RunID: s.runID}, false
	}
	// Guaranteed cleanup: defer wt.RestoreHostPaths() so container
	// sandboxes normalize the preserved worktree's .git pointer back to
	// host paths on every exit path. Worktree-only sandboxes no-op.
	// executePromptOnly has no end-of-success cleanup otherwise; this
	// defer is the only end-of-success cleanup here. Issue #2189.
	defer func() { _ = wt.RestoreHostPaths() }()

	o.registerActiveRun(0, wt)
	defer o.unregisterActiveRun(0)

	// Pre-register the supervisor's done channel with the batch-wide
	// fan-in BEFORE spawning the supervisor, so a session that
	// started just before ctx fired cannot race the snapshot taken
	// by RunBatch's fan-in goroutine. sessionCtx is cancelled when
	// this executePromptOnly call returns, regardless of how it
	// returns, so the supervisor can exit promptly.
	supervisorDone := make(chan struct{})
	o.trackShutdownSupervisor(supervisorDone)
	sessionCtx, cancelSession := context.WithCancel(ctx)
	go func() {
		defer close(supervisorDone)
		<-sessionCtx.Done()
		if s.parentCtx != nil && s.parentCtx.Err() != nil {
			if proc := wt.Process(); proc != nil {
				done := superviseShutdown(ctx, proc, s.opts.killTimeout)
				<-done
			}
		}
	}()
	defer func() {
		cancelSession()
		<-supervisorDone
	}()

	runID := s.runID
	if s.batchTS != "" && s.batchShortID != "" {
		// runid.NewRunID(KindPromptOnly, …) hard-codes the `prompt`
		// segment (issue #1920 slice 4 of #1916), so passing the
		// user-supplied --run-id as `subject` (or "" for the no-userid
		// case) produces the canonical per-row RunID that doubles as
		// the public BatchId.
		runID = runid.NewRunID(runid.KindPromptOnly, s.userProvidedRunID, s.batchTS, s.batchShortID)
	} else if runID == "" {
		runID = fmt.Sprintf("run-0-%d", time.Now().UnixNano())
	}
	// For prompt-only batches the public BatchId equals the per-row
	// RunID (issue #1920 slice 4). The cmd layer pre-seeds s.batchID
	// from the same runid.NewBatchID call, and the one-shot review
	// path sets s.batchID by walking runDir; if neither path
	// populated it (legacy callers), the legacy batchIDFromRunID
	// fallback below returns the `<ts>-<sid>` prefix — which is the
	// historical contract that the on-disk dir resolver already
	// understood, so the manifest writes still land at a coherent
	// (if legacy-shaped) path. The cmd and review paths pre-seed
	// s.batchID today, so this fallback is best-effort.
	if s.batchID == "" {
		s.batchID = batchIDFromRunID(runID)
	}

	batchDir := s.o.layout.BatchDir(s.batchID)
	manifestBatchID := s.batchID
	var runKind batchindex.Kind
	if s.review {
		runKind = batchindex.KindReview
	} else {
		runKind = batchindex.KindPromptOnly
	}
	runManifest := batchindex.RunManifest{
		RunID:        runID,
		BatchID:      manifestBatchID,
		Issue:        s.issueNumber,
		Branch:       branch,
		BaseBranch:   s.baseBranch,
		WorktreePath: wt.WorkDir(),
		Kind:         runKind,
		CreatedAt:    time.Now(),
		PR:           s.prNumber,
		Status:       batchindex.RunManifestStatusActive,
	}
	if err := daemon.WriteRunManifest(batchDir, runID, runManifest); err != nil {
		fmt.Fprintf(o.errorLog, "error: write run manifest for prompt-only run: %v\n", err)
		_ = wt.Stop()
		return AgentRunResult{Status: "failure", Branch: branch, Review: s.review, RunID: runID}, false
	}
	cmdServer := daemon.NewCommandServer(daemon.RunFolder(batchDir, runID), s.o)
	if err := cmdServer.Start(); err != nil {
		fmt.Fprintf(o.errorLog, "error: start command server for prompt-only run: %v\n", err)
	} else {
		defer cmdServer.Stop()
	}

	if o.eventLog != nil {
		promptSourceType := "current"
		payload := map[string]any{"branch": branch, "base_branch": s.baseBranch, "prompt_source_type": "prompt", "parallel": s.parallel, "start_delay": int(s.startDelay / time.Second), "retries": s.retries, "sandbox": s.sandboxMode, "container_capacity": s.containerCapacity, "container_capacity_set": s.containerCapacitySet, "max_containers": s.maxContainers, "max_containers_set": s.maxContainersSet}
		if s.renderCfg.PromptFlag != "" {
			promptSourceType = "prompt"
		} else if s.renderCfg.TemplateFlag != "" {
			promptSourceType = "template"
		}
		payload["prompt_source_type"] = promptSourceType
		if s.mode == ModeContinue {
			payload["previous_run_id"] = s.previousRunIDs[0]
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
		if s.batchID != "" {
			payload["batch_id"] = s.batchID
		}
		if s.review {
			payload["review"] = true
			payload["pr_number"] = s.prNumber
			payload["review_focus"] = s.reviewFocus
			if s.issueNumber > 0 {
				payload["issue_number"] = s.issueNumber
			}
		}
		eventType := "run.started"
		if s.mode == ModeContinue {
			eventType = "run.continued"
		}
		_ = o.eventLog.Log(events.Event{Type: eventType, Timestamp: time.Now(), RunID: runID, Issue: 0, IssueRef: nil, Payload: payload})
	}

	logPath := s.runLogPathFor(runID)
	result, terminalExtras, started := s.runOnce(ctx, nil, branch, wt, logPath, runID, false, func(attempt int) (prompt.RenderConfig, *AgentRunResult) {
		if attempt > 0 {
			if err := o.resetRetryBranch(ctx, wt, branch, s.baseBranch); err != nil {
				fmt.Fprintf(o.errorLog, "error: reset retry branch for prompt-only run: %v\n", err)
				return prompt.RenderConfig{}, &AgentRunResult{Status: "failure", Branch: branch, RetriesTotal: attempt, Review: s.review, RunID: s.runID}
			}
			if err := logRetryMarkerFn(logPath, attempt, s.retries); err != nil {
				if o.errorLog != nil {
					fmt.Fprintf(o.errorLog, "warning: write retry marker for prompt-only run: %v\n", err)
				}
			}
		}
		return s.renderCfg, nil
	})
	result.Review = s.review
	result.RunID = s.runID
	if !started {
		return result, false
	}

	result.Status = s.emitTerminal(ctx, runID, result, terminalExtras)

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
// ADR-0027 (stranded-worktree first, then base-branch checkout).
//
// When `strandedReconcile` is nil, today's belt-and-suspenders behaviour
// is preserved (the failure is logged and the function continues);
// false is the explicit opt-out (`--no-reconcile-stranded`).
//
// `baseBranch` is the branch the function falls back to when no stranded
// worktree is found; when empty, the fallback path is skipped.
func ClearIssueArtifacts(issueNumber int, branch string, worktreeDir string, eventLog events.EventLog, logWriter io.Writer, baseBranch string, strandedReconcile *bool, batchesIndexPath string) {
	wtPath := filepath.Join(worktreeDir, branch)

	// Remove worktree (may fail if already removed — idempotent)
	if out, err := exec.Command("git", "worktree", "remove", "--force", wtPath).CombinedOutput(); err != nil && !isMissingWorktreeError(err, out) {
		if isPrunableWorktreeError(err, out) && strandedReconcile != nil && *strandedReconcile {
			if rmErr := os.RemoveAll(wtPath); rmErr != nil {
				fmt.Fprintf(logWriter, "error: remove worktree dir %s for issue %d: %v\n", wtPath, issueNumber, rmErr)
			}
		} else {
			fmt.Fprintf(logWriter, "error: remove worktree %s for issue %d: %v: %s\n", wtPath, issueNumber, err, out)
		}
	}
	if out, err := exec.Command("git", "worktree", "prune").CombinedOutput(); err != nil {
		fmt.Fprintf(logWriter, "error: prune worktrees for issue %d: %v: %s\n", issueNumber, err, out)
	}

	// Delete branch (may fail if already deleted — idempotent)
	if out, err := exec.Command("git", "branch", "-D", branch).CombinedOutput(); err != nil && !isMissingBranchError(err, out) {
		if strandedReconcile != nil && *strandedReconcile {
			recoverBranchDeleteFromMainRepo(logWriter, branch, worktreeDir, baseBranch)
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

	// Remove events for this issue
	if eventLog != nil {
		if err := eventLog.RemoveEventsByIssue(issueNumber); err != nil {
			fmt.Fprintf(logWriter, "error: remove events for issue %d: %v\n", issueNumber, err)
		}
	}
}

// recoverBranchDeleteFromMainRepo attempts to unstick `git branch -D <branch>`
// when the main repo is itself checked out on `branch`. It mirrors the
// recovery strategy from WorktreeSandbox.Start (issue #937):
//  1. Detect a stranded worktree at <worktreeBase>/<branch>; if present,
//     delete the branch from inside that worktree's cwd (which bypasses
//     the main-repo guard).
//  2. Otherwise, if `baseBranch` is set, `git checkout -f <baseBranch>` in
//     the main repo so the branch can be deleted.
//
// On failure at any step, a warning is logged. The caller retries the
// delete after this returns.
func recoverBranchDeleteFromMainRepo(logWriter io.Writer, branch, worktreeBase, baseBranch string) {
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
	if strings.TrimSpace(baseBranch) == "" {
		fmt.Fprintf(logWriter, "warning: cannot recover branch %s — main repo is checked out on it, no stranded worktree found, and no base branch configured\n", branch)
		return
	}
	if out, err := exec.Command("git", "checkout", "-f", baseBranch).CombinedOutput(); err != nil {
		fmt.Fprintf(logWriter, "warning: git checkout -f %s to recover branch %s: %v: %s\n", baseBranch, branch, err, out)
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
//	// later, from (*runSession).applyOverrideAndIdentity:
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
