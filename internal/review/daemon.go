package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/runid"
)

// PollingInterval is the default interval at which the daemon scans open PRs
// for /sandman review comments. It is exported so tests and the CLI can
// reference the same constant.
const PollingInterval = 30 * time.Second

// pendingMaxCycles is the upper bound on consecutive `tick` cycles a
// pending review may stay in `pending` status before the daemon
// promotes it to `failure`. Three cycles is ~90s at the default
// 30s PollingInterval — large enough to tolerate GitHub API eventual
// consistency and the agent's startup latency, small enough that the
// daemon does not silently retry indefinitely when the agent never
// posts a review comment.
const pendingMaxCycles = 3

// Clock returns the current time. Inject a custom clock in tests to avoid
// time-based dependencies.
type Clock func() time.Time

// Trigger receives tick signals to drive the polling loop. The default
// implementation uses time.NewTicker; tests can inject a manual channel.
type Trigger <-chan struct{}

// GitHubClient is the subset of github.Client used by the review daemon.
// It is exposed as a small interface so tests can substitute a fake.
type GitHubClient interface {
	ListOpenPRs() ([]github.PR, error)
	ListPRComments(number int) ([]github.PRComment, error)
	FetchPR(number int) (*github.PR, error)
	RepoName() (string, error)
	AddCommentReaction(commentID, content string) (string, error)
	AddIssueReaction(issueNumber int, content string) (string, error)
	RemoveCommentReaction(commentID, reactionID string) error
	RemoveIssueReaction(issueNumber int, reactionID string) error
}

// BatchRunner is the subset of batch.Runner used by the review daemon.
type BatchRunner interface {
	RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error)
}

// Renderer renders review prompts.
type Renderer interface {
	RenderReview(cfg prompt.RenderConfig, data prompt.PRData) (string, error)
}

// pendingReviewEntry is the in-memory record the daemon keeps for a
// review that has been launched but whose agent-posted review comment
// has not yet been observed. The lazy-verify contract (issue #1482
// slice D) holds these in memory so a subsequent tick can resolve
// them without keeping `launchReview` on the critical path.
type pendingReviewEntry struct {
	commentID   string
	since       time.Time
	reviewState string // path to <runDir>/review-state.json for the launched run
	storeRef    *ReviewStateStore
	cycles      int
}

// Daemon polls the repo for /sandman review comments and launches review
// agents serially.
type Daemon struct {
	BaseDir              string
	GitHub               GitHubClient
	Prompts              Renderer
	Runner               BatchRunner
	Config               *config.Config
	Broadcaster          io.Writer
	Clock                Clock
	Trigger              Trigger
	PollInterval         time.Duration
	Sandbox              string
	ContainerCapacity    int
	ContainerCapacitySet bool
	MaxContainers        int
	MaxContainersSet     bool
	Agent                string
	Model                string
	Parallel             int
	ParallelSet          bool
	controlSocket        *daemon.ControlSocket
	busy                 chan struct{}
	promptOnce           sync.Once
	seenCache            map[int]map[string]bool
	seenCacheMu          sync.RWMutex
	slotTable            map[int]struct{}
	slotPool             chan struct{}
	slotMu               sync.Mutex
	pendingMu            sync.Mutex
	pendingReviews       map[int][]pendingReviewEntry
}

// New returns a Daemon configured with the project defaults for the
// polling interval and clock. The seen cache is hydrated eagerly from
// the on-disk batches index (issue #1480 slice A), and the in-memory
// pendingReviews map is rehydrated from the same index so an
// in-flight trigger survives a daemon restart (issue #1635). A
// missing or unreadable index yields empty caches; the rename-loser
// trade-off from ADR-0034 §3 means a stale skip is acceptable.
//
// parallel and parallelSet thread the CLI --parallel override through to
// the slot-pool sizing: when parallelSet is true and parallel > 0, the
// slot pool is sized to parallel regardless of cfg.DefaultReviewParallel.
// When parallelSet is false, the slot pool falls back to
// cfg.EffectiveReviewParallel() (the historical behavior).
func New(baseDir string, gh GitHubClient, prompts Renderer, runner BatchRunner, cfg *config.Config, broadcaster io.Writer, parallel int, parallelSet bool) *Daemon {
	parallelReviews := 1
	if parallelSet && parallel > 0 {
		parallelReviews = parallel
	} else if cfg != nil {
		parallelReviews = cfg.EffectiveReviewParallel()
		if parallelReviews < 1 {
			parallelReviews = 1
		}
	}
	d := &Daemon{
		BaseDir:        baseDir,
		GitHub:         gh,
		Prompts:        prompts,
		Runner:         runner,
		Config:         cfg,
		Broadcaster:    broadcaster,
		Clock:          time.Now,
		Trigger:        nil,
		PollInterval:   PollingInterval,
		Parallel:       parallel,
		ParallelSet:    parallelSet,
		busy:           make(chan struct{}, 1),
		seenCache:      map[int]map[string]bool{},
		slotTable:      map[int]struct{}{},
		slotPool:       make(chan struct{}, parallelReviews),
		pendingReviews: map[int][]pendingReviewEntry{},
	}
	if err := d.loadSeenCache(); err != nil {
		d.logf("load seen cache: %v", err)
	}
	if err := d.loadPendingReviews(); err != nil {
		d.logf("load pending reviews: %v", err)
	}
	return d
}

// acquirePRSlot reserves one of the parallel_reviews slots for
// prNumber. Returns true if reserved; false if either (a) the slot
// pool is saturated (cap = parallel_reviews: M and N are in-flight
// and O returns early) or (b) prNumber already has an in-flight slot
// held (a new trigger on the same PR is held for the next tick, not
// dropped). The pool is shared across PRs; the per-PR ownership is
// tracked by slotTable so a new trigger on PR N does not bump
// another free PR's reservation. Slice B's TryClaim guarantees that
// two processPR calls for the same PR never enter launchReview
// simultaneously, so the slot's 1-of-1 reservation is correct.
func (d *Daemon) acquirePRSlot(prNumber int) bool {
	d.slotMu.Lock()
	defer d.slotMu.Unlock()
	if _, held := d.slotTable[prNumber]; held {
		return false
	}
	select {
	case d.slotPool <- struct{}{}:
		d.slotTable[prNumber] = struct{}{}
		return true
	default:
		return false
	}
}

// releasePRSlot frees the slot held by prNumber, returning it to the
// pool. Idempotent: a no-op when prNumber has no held slot. Called
// from a defer in processPR after MarkSeen persists so the slot is
// freed only after terminal-seen state is on disk.
func (d *Daemon) releasePRSlot(prNumber int) {
	d.slotMu.Lock()
	defer d.slotMu.Unlock()
	if _, held := d.slotTable[prNumber]; !held {
		return
	}
	delete(d.slotTable, prNumber)
	select {
	case <-d.slotPool:
	default:
	}
}

// IsSlotHeld reports whether prNumber currently holds a slot.
// Exposed for slice-C regression tests (cross-PR concurrency and
// no-leak invariants). Production code does not branch on it; the
// slot is acquired by processPR itself.
func (d *Daemon) IsSlotHeld(prNumber int) bool {
	d.slotMu.Lock()
	defer d.slotMu.Unlock()
	_, held := d.slotTable[prNumber]
	return held
}

// slotHeldCount returns the number of currently-held slots. Used by
// the slice-C no-leak regression test. Returns 0 on an idle daemon.
func (d *Daemon) slotHeldCount() int {
	d.slotMu.Lock()
	defer d.slotMu.Unlock()
	return len(d.slotTable)
}

// IsTerminalSeen reports whether the daemon's seen cache currently
// records (prNumber, commentID) as terminal-seen. It is exposed for
// tests and for callers that need to check membership outside the
// per-tick short-circuit.
func (d *Daemon) IsTerminalSeen(prNumber int, commentID string) bool {
	d.seenCacheMu.RLock()
	defer d.seenCacheMu.RUnlock()
	if seen, ok := d.seenCache[prNumber]; ok {
		return seen[commentID]
	}
	return false
}

// MarkTerminalSeen records (prNumber, commentID) as terminal-seen in
// the daemon's seen cache. It is invoked by ReviewStateStore via the
// SeenCacheInvalidator seam after a successful MarkSeen whose status
// passes shouldSkipDedupStatus.
func (d *Daemon) MarkTerminalSeen(prNumber int, commentID string) {
	d.seenCacheMu.Lock()
	defer d.seenCacheMu.Unlock()
	if d.seenCache == nil {
		d.seenCache = map[int]map[string]bool{}
	}
	if _, ok := d.seenCache[prNumber]; !ok {
		d.seenCache[prNumber] = map[string]bool{}
	}
	d.seenCache[prNumber][commentID] = true
}

// Forget removes (prNumber, commentID) from the daemon's seen cache.
// It is invoked by ReviewStateStore via the SeenCacheInvalidator seam
// when a claim is released, so a previously cached comment is
// reprocessed on the next tick.
func (d *Daemon) Forget(prNumber int, commentID string) {
	d.seenCacheMu.Lock()
	defer d.seenCacheMu.Unlock()
	if seen, ok := d.seenCache[prNumber]; ok {
		delete(seen, commentID)
	}
}

// loadSeenCache rebuilds the seen cache from scratch by scanning the
// on-disk batches index and the canonical run folders for every
// review batch. Per ADR-0030 §Per-row RunID templates (issue #1551)
// review runs are first-class rows, so each batch's run.json lives
// under `<batch>/runs/<runID>/run.json` (see reviewRunIDFor for the
// exact per-row shape) and its review-state.json lives one folder up
// next to it. Existing entries are replaced.
func (d *Daemon) loadSeenCache() error {
	d.seenCacheMu.Lock()
	defer d.seenCacheMu.Unlock()
	d.seenCache = map[int]map[string]bool{}

	idx, err := seenCacheLoader(d.BaseDir)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}
	if idx == nil {
		return nil
	}
	for _, entry := range idx.Entries {
		if entry.Kind != batchindex.KindReview {
			continue
		}
		// Resolve the canonical row RunID for this batch. The
		// canonical rowID is the value persisted on the
		// batch's run.json by prepareReviewRun — by reading it
		// here we are version-independent of the exact
		// `<sid>-<ts>-<linkedIssue?>-PR<pr>` shape.
		rowID, err := readReviewRowID(filepath.Join(entry.Path, "runs"))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			d.logf("read review row id for %s: %v", entry.Path, err)
			continue
		}
		runDir := filepath.Join(entry.Path, "runs", rowID)
		state, err := seenStateReader(runDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			d.logf("read review state %s: %v", runDir, err)
			continue
		}
		for _, sc := range state.SeenComments {
			if !shouldSkipDedupStatus(sc.Status) {
				continue
			}
			if _, ok := d.seenCache[entry.PR]; !ok {
				d.seenCache[entry.PR] = map[string]bool{}
			}
			d.seenCache[entry.PR][sc.CommentID] = true
		}
	}
	return nil
}

// readReviewRowID returns the row RunID for a review batch's runs
// directory. It consults the first run.json under runs/ — review
// batches always launch a single row, so there is exactly one
// run.json — and returns its `RunID` field. The folder name matches
// the canonical per-row RunID from ADR-0030 §Per-row RunID templates
// (see reviewRunIDFor for the exact shape). The legacy
// `runs/review/run.json` is intentionally NOT consulted: the daemon
// must not read the literal "review" alias as a run folder name.
func readReviewRowID(runsDir string) (string, error) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return "", err
	}
	var pick string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "review" {
			continue
		}
		pick = e.Name()
		break
	}
	if pick == "" {
		return "", os.ErrNotExist
	}
	manifestPath := filepath.Join(runsDir, pick, "run.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	var manifest batchindex.RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("decode run manifest: %w", err)
	}
	return manifest.RunID, nil
}

// InvalidateSeenCache forces a rebuild of the seen cache by re-running
// the on-disk scan. Callers use this when an out-of-band change to
// .sandman/batches.json or a review-state.json is observed (e.g. via
// fsnotify) or as a slow-tick recovery path.
func (d *Daemon) InvalidateSeenCache() error {
	return d.loadSeenCache()
}

// loadPendingReviews rehydrates the in-memory pendingReviews map from
// the on-disk review-state.json files referenced by .sandman/batches.json.
//
// The lazy-verify contract (issue #1482 slice D) records each launched
// trigger as status `pending` in the per-run review-state.json and keeps
// the matching entry in the in-memory pendingReviews map. The seen-cache
// hydration at construction deliberately excludes `pending` entries
// (see shouldSkipDedupStatus), so without this rehydration a daemon
// restart between launchReview and the first post-launch
// promotePendingReviews tick would orphan the in-flight trigger and
// the next instance would re-launch the review. Issue #1635.
//
// The rehydration is read-only: it walks the same index the seen cache
// uses and registers a pendingReviewEntry for every SeenComment whose
// status is "pending". The since timestamp is the entry's recorded
// Timestamp (so a fresh promote tick can detect reviewer replies
// posted at or after the original launch window). Stale entries
// (rows the bounded-retry escape would have promoted already) are
// still bounded by the existing pendingMaxCycles cap on the new
// instance — at most 3 promote ticks escape them to "failure" + the
// seen cache, matching the in-memory behavior.
func (d *Daemon) loadPendingReviews() error {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	d.pendingReviews = map[int][]pendingReviewEntry{}

	idx, err := seenCacheLoader(d.BaseDir)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}
	if idx == nil {
		return nil
	}
	for _, entry := range idx.Entries {
		if entry.Kind != batchindex.KindReview {
			continue
		}
		rowID, err := readReviewRowID(filepath.Join(entry.Path, "runs"))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			d.logf("read review row id for %s: %v", entry.Path, err)
			continue
		}
		runDir := filepath.Join(entry.Path, "runs", rowID)
		state, err := seenStateReader(runDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			d.logf("read review state %s: %v", runDir, err)
			continue
		}
		reviewStatePath := filepath.Join(runDir, "review-state.json")
		for _, sc := range state.SeenComments {
			if sc.Status != "pending" {
				continue
			}
			// Drop zero-timestamp entries: a missing Timestamp
			// means we cannot bound the promote window safely, and
			// falling back to wall-clock at rehydration time would
			// hide reviewer replies that landed before the restart.
			// The bounded-retry escape on the new instance will
			// still clear the row after pendingMaxCycles ticks.
			if sc.Timestamp.IsZero() {
				d.logf("skip rehydrate of pending %s (zero timestamp on disk)", sc.CommentID)
				continue
			}
			d.pendingReviews[entry.PR] = append(d.pendingReviews[entry.PR], pendingReviewEntry{
				commentID:   sc.CommentID,
				since:       sc.Timestamp,
				reviewState: reviewStatePath,
			})
		}
	}
	return nil
}

// InvalidatePendingReviews forces a rebuild of the in-memory
// pendingReviews map by re-running the on-disk scan. Symmetric with
// InvalidateSeenCache.
func (d *Daemon) InvalidatePendingReviews() error {
	return d.loadPendingReviews()
}

// SocketPath returns the absolute path of the daemon's control socket.
// The socket lives under .sandman/reviews/ alongside the shared prompt
// template, so the daemon's on-disk footprint is just two files plus
// run folders under .sandman/batches/.
func (d *Daemon) SocketPath() string {
	return filepath.Join(d.BaseDir, "reviews", "review.sock")
}

// effectiveAgent returns the review agent name to use for this run.
// Precedence: the CLI override (Daemon.Agent, when non-empty after
// trimming) wins; otherwise d.Config.EffectiveReviewAgent(). Returns
// the empty string when both sources are unset.
func (d *Daemon) effectiveAgent() string {
	if v := strings.TrimSpace(d.Agent); v != "" {
		return v
	}
	if d.Config == nil {
		return ""
	}
	return d.Config.EffectiveReviewAgent()
}

// effectiveModel returns the review model name to use for this run.
// Precedence matches effectiveAgent: the CLI override (Daemon.Model)
// wins, otherwise d.Config.EffectiveReviewModel(). Returns the empty
// string when both sources are unset.
func (d *Daemon) effectiveModel() string {
	if v := strings.TrimSpace(d.Model); v != "" {
		return v
	}
	if d.Config == nil {
		return ""
	}
	return d.Config.EffectiveReviewModel()
}

// effectiveParallel returns the parallel value to use for this run.
// Precedence matches effectiveAgent/effectiveModel: the CLI override
// wins when positive (Daemon.ParallelSet && Daemon.Parallel > 0);
// otherwise d.Config.EffectiveReviewParallel(). Parallel <= 0 falls
// through because parallel=0 means "unlimited" in the orchestrator and
// must not be treated as an explicit override.
func (d *Daemon) effectiveParallel() int {
	if d.ParallelSet && d.Parallel > 0 {
		return d.Parallel
	}
	if d.Config == nil {
		return 1
	}
	return d.Config.EffectiveReviewParallel()
}

// PromptTemplatePath returns the absolute path of the shared review
// prompt template. The file is a static, PR-agnostic copy of the
// built-in template; PR context flows only through the per-run
// batch request.
//
// Acceptance criterion #2 from issue #1224: ".sandman/reviews/
// contains only review.sock and review-prompt.md".
func (d *Daemon) PromptTemplatePath() string {
	return filepath.Join(d.BaseDir, "reviews", "review-prompt.md")
}

// initPromptTemplate writes the static, PR-agnostic review prompt
// template to PromptTemplatePath() exactly once. It is safe to call
// from multiple goroutines and from both StartSocket and launchReview.
func (d *Daemon) initPromptTemplate() error {
	var err error
	d.promptOnce.Do(func() {
		dir := filepath.Dir(d.PromptTemplatePath())
		if err = os.MkdirAll(dir, 0755); err != nil {
			return
		}
		tmp := d.PromptTemplatePath() + ".tmp"
		if err = os.WriteFile(tmp, []byte(prompt.DefaultPRReviewPrompt()), 0644); err != nil {
			return
		}
		err = os.Rename(tmp, d.PromptTemplatePath())
	})
	return err
}

// ReviewStatePath returns the on-disk path of the per-run review-state
// file for a given run folder.
//
// Per ADR-0030 §Per-row RunID templates (issue #1551) review runs are
// first-class rows, so the review-state file lives next to its row's
// run.json under the canonical per-row folder:
// `<batch>/runs/<runID>/review-state.json` (see reviewRunIDFor). The
// folder name is NOT the legacy `runs/review` alias. Callers pass the
// run folder path in; this helper joins the state filename.
func (d *Daemon) ReviewStatePath(runDir string) string {
	return filepath.Join(runDir, "review-state.json")
}

// SetSocket stores a pre-built ControlSocket on the daemon. The cmd layer
// uses this to share a Broadcaster-driven socket with attach; tests can
// also inject a custom socket.
func (d *Daemon) SetSocket(s *daemon.ControlSocket) {
	d.controlSocket = s
}

// StartSocket ensures the .sandman/reviews dir exists, writes the
// static shared prompt template, and starts the control socket. Safe
// to call multiple times.
func (d *Daemon) StartSocket() error {
	if err := os.MkdirAll(filepath.Dir(d.SocketPath()), 0755); err != nil {
		return fmt.Errorf("create reviews dir: %w", err)
	}
	if err := d.initPromptTemplate(); err != nil {
		return fmt.Errorf("init review prompt template: %w", err)
	}
	if d.controlSocket == nil {
		d.controlSocket = daemon.NewControlSocketWithName(filepath.Dir(d.SocketPath()), "review.sock", daemon.NewBroadcaster())
	}
	return d.controlSocket.Start()
}

// Stop closes the control socket. Idempotent.
func (d *Daemon) Stop() error {
	if d.controlSocket == nil {
		return nil
	}
	return d.controlSocket.Stop()
}

// Run drives the polling loop. It blocks until ctx is cancelled, then
// closes the control socket and returns. ctx cancellation also cancels
// any in-flight RunBatch call. When a Trigger channel is wired, the
// initial scan is skipped so tests can drive ticks explicitly.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.StartSocket(); err != nil {
		return err
	}
	defer d.Stop()

	if d.Config != nil {
		effectiveAgent := d.effectiveAgent()
		if _, err := d.Config.ResolveAgentProvider(effectiveAgent); err != nil {
			d.logf("review agent validation failed: %v", err)
			return err
		}
		if strings.TrimSpace(d.effectiveModel()) == "" {
			return fmt.Errorf("review model is not set; configure review_model or model in sandman config")
		}
	}

	if d.Trigger == nil {
		if err := d.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			d.logf("initial scan: %v", err)
		}
	}

	interval := d.PollInterval
	if interval <= 0 {
		interval = PollingInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		var ch <-chan time.Time
		if d.Trigger == nil {
			ch = ticker.C
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
			if err := d.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				d.logf("scan: %v", err)
			}
		case <-d.Trigger:
			if err := d.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				d.logf("scan: %v", err)
			}
		}
	}
}

// tick performs one full scan over open PRs. It serializes via a single
// semaphore so concurrent trigger signals are dropped while a scan runs.
func (d *Daemon) tick(ctx context.Context) error {
	select {
	case d.busy <- struct{}{}:
		defer func() { <-d.busy }()
	default:
		d.logf("scan: previous tick still running, skipping")
		return nil
	}

	// Lazy verify (issue #1482 slice D): before scanning open PRs for
	// new triggers, the daemon promotes or rejects any pending
	// verification carried over from previous launches. This keeps
	// launchReview on the critical path (RunBatch only) while still
	// detecting agent-posted review comments on the next tick.
	if err := d.promotePendingReviews(ctx); err != nil {
		d.logf("promote pending reviews: %v", err)
	}

	prs, err := d.GitHub.ListOpenPRs()
	if err != nil {
		return fmt.Errorf("list open PRs: %w", err)
	}

	var wg sync.WaitGroup
	for _, pr := range prs {
		pr := pr
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.processPR(ctx, pr.Number); err != nil {
				d.logf("process PR #%d: %v", pr.Number, err)
			}
		}()
	}
	wg.Wait()
	return nil
}

// processPR scans one PR's comments and launches a review agent for the
// newest unseen /sandman review trigger. The per-PR slot (issue #1481
// slice C — see acquirePRSlot / releasePRSlot) preserves the trigger
// when the slot pool is full or a stale review is in flight; the deferred
// release runs after MarkSeen persists.
//
// The dedup state lives in the run folder's `review-state.json`, managed
// by ReviewStateStore. No per-PR directory is created under `.sandman/reviews/`.
//
// Acceptance criteria #1 and #3 from issue #1224:
//   - No code path creates `.sandman/reviews/<PR>/`
//   - `review-state.json` lives at `<batch>/runs/<run>/review-state.json`
func (d *Daemon) processPR(ctx context.Context, prNumber int) error {
	comments, err := d.GitHub.ListPRComments(prNumber)
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	if len(comments) == 0 {
		return nil
	}

	sort.Slice(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})

	type unseenTrigger struct {
		comment github.PRComment
		focus   string
	}
	var triggers []unseenTrigger
	for _, comment := range comments {
		focus, ok := ParseTrigger(comment.Body)
		if !ok {
			continue
		}
		triggers = append(triggers, unseenTrigger{comment: comment, focus: focus})
	}
	if len(triggers) == 0 {
		return nil
	}

	// Cross-run dedup: read terminal-seen membership from the
	// per-process in-memory cache populated at construction
	// (issue #1480 slice A). ADR-0034 §3 accepts the rename-loser
	// trade-off; the cache only short-circuits what is already
	// persisted on disk. The read lock must be held across the
	// per-trigger lookup because the inner map is shared with
	// MarkTerminalSeen / Forget on sibling PRs in the same tick
	// (slice-A PR review, race-detector finding).
	d.seenCacheMu.RLock()
	var unprocessed []unseenTrigger
	for _, t := range triggers {
		if d.seenCache[prNumber][t.comment.ID] {
			d.logf("comment %s already terminal-seen, skipping", t.comment.ID)
			continue
		}
		unprocessed = append(unprocessed, t)
	}
	d.seenCacheMu.RUnlock()
	if len(unprocessed) == 0 {
		return nil
	}

	// Lazy verify (issue #1482 slice D): drop triggers that are
	// already registered as pending in this daemon. The next tick's
	// promotePendingReviews step will observe the agent's review
	// comment and promote them to success/failure; launching a second
	// review for the same trigger would double-process the comment.
	d.pendingMu.Lock()
	pendingSet := map[string]bool{}
	for _, e := range d.pendingReviews[prNumber] {
		pendingSet[e.commentID] = true
	}
	d.pendingMu.Unlock()
	if len(pendingSet) > 0 {
		var filtered []unseenTrigger
		for _, t := range unprocessed {
			if pendingSet[t.comment.ID] {
				continue
			}
			filtered = append(filtered, t)
		}
		if len(filtered) == 0 {
			return nil
		}
		unprocessed = filtered
	}

	newest := unprocessed[0]
	for i := 1; i < len(unprocessed); i++ {
		if unprocessed[i].comment.CreatedAt.After(newest.comment.CreatedAt) {
			newest = unprocessed[i]
		}
	}

	comment := newest.comment
	focus := newest.focus

	// Acquire a per-PR slot before reactions / launch so a stale
	// in-flight review (or the parallel_reviews cross-PR cap) makes
	// the trigger wait for the next tick instead of dropping it.
	if !d.acquirePRSlot(prNumber) {
		return nil
	}
	defer d.releasePRSlot(prNumber)

	reviewRunFolder, perRowRunID, rs, state, prepErr := d.prepareReviewRun(prNumber, comment.ID)
	if prepErr != nil {
		d.logf("prepare review run for PR #%d comment %s: %v", prNumber, comment.ID, prepErr)
		return nil
	}

	if !state.TryClaim(comment.ID) {
		d.logf("comment %s already claimed or terminal-seen, skipping", comment.ID)
		_ = rs.Close()
		return nil
	}

	commentReactionID, commentErr := d.GitHub.AddCommentReaction(comment.ID, "eyes")
	if commentErr != nil {
		d.logf("add reaction to comment %s: %v", comment.ID, commentErr)
	}
	prReactionID, prErr := d.GitHub.AddIssueReaction(prNumber, "eyes")
	if prErr != nil {
		d.logf("add reaction to PR #%d: %v", prNumber, prErr)
	}

	launchErr := d.launchReview(ctx, prNumber, focus, comment.ID, commentReactionID, prReactionID, reviewRunFolder, perRowRunID, rs)
	if launchErr != nil {
		d.logf("launch review for PR #%d comment %s: %v", prNumber, comment.ID, launchErr)
	}

	statePath := d.ReviewStatePath(reviewRunFolder)
	persisted, _ := os.Stat(statePath)
	for _, t := range unprocessed {
		if t.comment.ID == comment.ID {
			continue
		}
		if state.IsSeen(t.comment.ID) {
			continue
		}
		if err := state.MarkSeen(t.comment.ID, "superseded"); err != nil {
			d.logf("mark superseded comment %s: %v", t.comment.ID, err)
		} else {
			d.logf("skipping stale trigger comment %s (newer %s exists)", t.comment.ID, comment.ID)
		}
	}
	if launchErr == nil {
		// Lazy verify (issue #1482 slice D): record the trigger as
		// pending so the next tick can promote it to success/failure.
		// The state handle here is the same one used above for the
		// superseded writes, so all writes land in the same file.
		if err := state.MarkSeen(comment.ID, "pending"); err != nil {
			d.logf("mark comment %s pending: %v", comment.ID, err)
		}
		d.registerPendingReview(prNumber, comment.ID, d.now(), statePath, state)
	} else {
		if persisted == nil {
			state.Release(comment.ID)
		}
		if err := state.MarkSeen(comment.ID, "failure"); err != nil {
			d.logf("mark comment %s failure: %v", comment.ID, err)
		}
	}
	return nil
}

// loadGlobalSeenForPR was removed in issue #1480 slice A: cross-run
// dedup now reads from the daemon's seenCache (hydrated at
// construction), so the per-tick on-disk scan no longer exists. The
// on-disk source-of-truth construction lives in loadSeenCache.

// shouldSkipDedupStatus reports whether a recorded comment status means
// the comment should be skipped during global dedup.
//
// This intentionally deviates from PRD #1218's terminal run-status set
// {success, failure, aborted}:
//   - failure is retryable (per #1333)
//   - aborted is retryable (the run was interrupted before publishing a
//     review, so the trigger should be retried)
//   - superseded is treated as terminal (obsolete trigger, not in PRD set)
//   - success is terminal (the review comment was published)
//   - pending is retryable (issue #1482 slice D): the lazy-verify
//     promotion step walks pending comments on every tick and either
//     promotes them to success (review comment observed) or to
//     failure (bounded retry escape). The seen-cache must therefore
//     NOT short-circuit pending entries, otherwise a follow-up tick
//     would never see them and the promotion step would never run.
func shouldSkipDedupStatus(status string) bool {
	return status == "success" || status == "superseded"
}

// prepareReviewRun creates the run folder and state store for a new review
// run. It is called by processPR before TryClaim so that the run folder,
// run session, and state store exist before TryClaim is called. The returned
// *daemon.RunSession must be passed to launchReview; processPR does not
// close it.
//
// The PR is fetched once (via the GitHub client) so the linked issue number
// can fold into the per-row RunID. The per-row RunID is minted by
// reviewRunIDFor below per ADR-0030 §Per-row RunID templates; the run
// folder is named after that per-row RunID (not the legacy `runs/review`
// alias). This replaces the legacy literal `RunID: "review"` alias —
// issue #1551 makes the review run a first-class row like every other
// run kind.
func (d *Daemon) prepareReviewRun(prNumber int, commentID string) (string, string, *daemon.RunSession, *ReviewStateStore, error) {
	ts, shortid, err := runid.NewBatch()
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("generate batch ID: %w", err)
	}
	batchDirName := runid.NewBatchID(runid.KindReview, 1, fmt.Sprintf("%d", prNumber), ts, shortid)

	var linkedIssue int
	if pr, fetchErr := d.GitHub.FetchPR(prNumber); fetchErr == nil && pr != nil {
		linkedIssue = pr.LinkedIssueNumber()
	} else if fetchErr != nil {
		// Non-fatal: a transient GitHub API failure must not block
		// the launch path. The one-shot `cmd/review.go` calls
		// FetchPR anyway; if it failed once, it would fail again.
		// We surface the failure in the daemon log instead.
		d.logf("fetch PR #%d for linked issue resolution: %v", prNumber, fetchErr)
	}

	perRowRunID := reviewRunIDFor(prNumber, linkedIssue, ts, shortid)

	rs := daemon.NewRunSession(d.BaseDir, batchDirName)
	manifest := daemon.BatchManifest{BatchId: batchDirName, CreatedAt: time.Now(), RunKind: "review", Issues: []int{}, PR: &prNumber}
	if err := rs.Prepare(manifest); err != nil {
		_ = rs.Close()
		return "", "", nil, nil, fmt.Errorf("prepare review run session: %w", err)
	}

	runDir := rs.RunDir()
	reviewRunFolder := daemon.RunFolder(runDir, perRowRunID)
	if err := os.MkdirAll(reviewRunFolder, 0755); err != nil {
		_ = rs.Close()
		return "", "", nil, nil, fmt.Errorf("create review run folder: %w", err)
	}

	runManifest := batchindex.RunManifest{
		RunID:     perRowRunID,
		BatchID:   batchDirName,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := daemon.WriteRunManifest(runDir, perRowRunID, runManifest); err != nil {
		_ = rs.Close()
		return "", "", nil, nil, fmt.Errorf("write run manifest: %w", err)
	}

	state, err := NewReviewStateStore(d.ReviewStatePath(reviewRunFolder), prNumber, d)
	if err != nil {
		_ = rs.Close()
		return "", "", nil, nil, fmt.Errorf("open review state: %w", err)
	}

	return reviewRunFolder, perRowRunID, rs, state, nil
}

// reviewBranchName returns the git branch name the review daemon uses for
// a given (PR, commentID) pair. Centralized so launchReview, its batch
// request, and the cleanup defer all derive the same string and tests can
// reference it without re-encoding the format.
func reviewBranchName(prNumber int, commentID string) string {
	return fmt.Sprintf("sandman/review-%d-%s", prNumber, commentID)
}

// logWriterFor returns the io.Writer ClearReviewArtifacts should write
// cleanup logs to. Falls back to the daemon's Broadcaster when set,
// otherwise to stderr (matching d.logf's fallback). Using Broadcaster
// directly rather than d.logf lets ClearReviewArtifacts stay a free
// function that does not depend on *Daemon.
func logWriterFor(d *Daemon) io.Writer {
	if d != nil && d.Broadcaster != nil {
		return d.Broadcaster
	}
	return os.Stderr
}

// launchReview renders the review prompt and runs the batch. The PR
// metadata is re-fetched via the GitHub client so the prompt reflects
// the current title and body. The rendered prompt is passed to the
// agent through the per-run batch request; the shared
// .sandman/reviews/review-prompt.md file stays a static, PR-agnostic
// template. The run folder is pre-created by prepareReviewRun; this
// function skips folder creation and uses the provided reviewRunFolder
// and perRowRunID. The provided *daemon.RunSession is closed by this
// function's defer before returning. An error means the review was not
// launched but the run folder may have been created and must be cleaned
// up by the portal's stale recovery (issue #1024).
//
// On success this function records the trigger comment as `pending` in
// the per-run review-state.json and registers the entry in the
// daemon's pending set (issue #1482 slice D). The next tick's
// promotePendingReviews step will then promote the comment to success
// when the agent's review comment arrives, or to failure after
// pendingMaxCycles ticks.
func (d *Daemon) launchReview(ctx context.Context, prNumber int, focus, commentID, commentReactionID, prReactionID, reviewRunFolder, perRowRunID string, rs *daemon.RunSession) error {
	// We compute the review branch name up-front so the cleanup defer
	// has it available on every exit path, including early errors
	// before RunBatch runs. The same value is reused in the
	// batch.Request.PromptConfig.Branch below so cleanup and creation
	// always target the same branch.
	reviewBranch := reviewBranchName(prNumber, commentID)
	defer func() {
		if rs != nil {
			_ = rs.Close()
		}
		if commentReactionID != "" {
			if err := d.GitHub.RemoveCommentReaction(commentID, commentReactionID); err != nil {
				d.logf("remove reaction from comment %s: %v", commentID, err)
			}
		}
		if prReactionID != "" {
			if err := d.GitHub.RemoveIssueReaction(prNumber, prReactionID); err != nil {
				d.logf("remove reaction from PR #%d: %v", prNumber, err)
			}
		}
		// Best-effort cleanup of the review worktree + branch runs last
		// so any in-flight reaction removal or socket close above is not
		// racing a half-removed worktree directory.
		if d.Config != nil {
			ClearReviewArtifacts(reviewBranch, d.Config.WorktreeDir, logWriterFor(d))
		}
	}()

	pr, err := d.GitHub.FetchPR(prNumber)
	if err != nil {
		return fmt.Errorf("fetch PR: %w", err)
	}

	rendered, err := d.Prompts.RenderReview(prompt.RenderConfig{}, prompt.PRData{
		Number:      pr.Number,
		Title:       pr.Title,
		Body:        pr.Body,
		ReviewFocus: focus,
	})
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}

	if err := d.initPromptTemplate(); err != nil {
		return fmt.Errorf("init review prompt template: %w", err)
	}

	agentName := ""
	modelName := ""
	sandboxMode := d.Sandbox
	if sandboxMode == "" && d.Config != nil {
		sandboxMode = d.Config.Sandbox
	}
	if sandboxMode == "" {
		sandboxMode = config.DefaultSandbox
	}
	if d.Config != nil {
		agentName = d.effectiveAgent()
		modelName = d.effectiveModel()
	}
	if agentName == "" {
		return fmt.Errorf("review agent is not set; configure review_agent or agent in sandman config")
	}
	if modelName == "" {
		return fmt.Errorf("review model is not set; configure review_model or model in sandman config")
	}

	repoName, err := d.GitHub.RepoName()
	if err != nil {
		return fmt.Errorf("get repo name: %w", err)
	}
	d.logf("repo=%s agent=%s model=%s pr=%d", repoName, agentName, modelName, prNumber)

	req := batch.Request{
		Agent:                agentName,
		Model:                modelName,
		Mode:                 map[int]batch.IssueMode{0: batch.ModeOverride},
		Sandbox:              sandboxMode,
		Parallel:             d.effectiveParallel(),
		ContainerCapacity:    d.ContainerCapacity,
		ContainerCapacitySet: d.ContainerCapacitySet,
		MaxContainers:        d.MaxContainers,
		MaxContainersSet:     d.MaxContainersSet,
		PromptConfig: prompt.RenderConfig{
			PromptFlag: rendered,
			Branch:     reviewBranch,
		},
		OutputWriter: rs.Broadcaster(),
		Review:       true,
		PRNumber:     prNumber,
		IssueNumber:  0,
		ReviewFocus:  focus,
		RunID:        perRowRunID,
		RunDir:       reviewRunFolder,
	}
	if _, err := d.Runner.RunBatch(ctx, req); err != nil {
		return fmt.Errorf("run batch: %w", err)
	}
	return nil
}

// otherwise falls back to time.Now. Tests inject a custom clock.
func (d *Daemon) now() time.Time {
	if d.Clock != nil {
		return d.Clock()
	}
	return time.Now()
}

// promotePendingComment resolves a single pending review: the daemon
// has launched an agent for a trigger comment and recorded the trigger
// as `pending`. The next tick calls this method for the entry; it
// asks GitHub for the PR comments and returns the new status to
// apply:
//
//   - "success" when a non-trigger comment has been posted at or after
//     `since` (the agent posted a review comment).
//   - ("pending", error) when no review comment has been observed
//     yet. The error lets the caller decide whether to increment the
//     cycle counter or promote to failure after pendingMaxCycles.
//
// The caller is responsible for writing the new status back into the
// per-run ReviewStateStore and updating the in-memory pending entry.
// Issue #1482 slice D.
func (d *Daemon) promotePendingComment(ctx context.Context, prNumber int, excludeCommentID string, since time.Time) (string, error) {
	comments, err := d.GitHub.ListPRComments(prNumber)
	if err != nil {
		return "", fmt.Errorf("list PR comments: %w", err)
	}
	for _, c := range comments {
		if c.ID == excludeCommentID {
			continue
		}
		if c.CreatedAt.After(since) || c.CreatedAt.Equal(since) {
			if c.Body == "" {
				continue
			}
			d.logf("PR #%d: review comment verified (ID %s, posted at %v)", prNumber, c.ID, c.CreatedAt)
			return "success", nil
		}
	}
	return "pending", fmt.Errorf("no review comment found on PR #%d after %v", prNumber, since)
}

// promotePendingReviews is the tick-level walker that runs at the
// start of every tick (after busy is acquired and before ListOpenPRs)
// to advance any pending lazy-verify entries toward a terminal status.
// For each pending entry:
//
//   - Call promotePendingComment against GitHub.
//   - If success: MarkSeen("success") on the per-run store and drop
//     the entry. The MarkSeen fires the seen-cache hook on success
//     per slice A, so the next tick skips the comment via the seen
//     cache.
//   - If pending: increment the cycle counter; once it reaches
//     pendingMaxCycles the daemon calls MarkSeen("failure") on the
//     per-run store and drops the entry. Bounded-retry failure is
//     added to the seen cache directly via MarkTerminalSeen so the
//     next tick does NOT re-launch the review (the failure escape has
//     already fired); this is a slice-D-only path and does not affect
//     the slice-A "RunBatch-error failure is retryable" contract
//     because that path lives in processPR, not here.
//
// Errors from ListPRComments are logged and the entry is kept — the
// next tick will retry. This is conservative: a temporary GitHub
// outage does not silently promote an in-flight review to failure.
// Issue #1482 slice D.
func (d *Daemon) promotePendingReviews(ctx context.Context) error {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	if len(d.pendingReviews) == 0 {
		return nil
	}

	for prNumber, entries := range d.pendingReviews {
		if len(entries) == 0 {
			delete(d.pendingReviews, prNumber)
			continue
		}
		kept := make([]pendingReviewEntry, 0, len(entries))
		for _, e := range entries {
			// Open the per-run store lazily if we did not cache it.
			store := e.storeRef
			if store == nil {
				s, err := NewReviewStateStore(e.reviewState, prNumber, d)
				if err != nil {
					d.logf("PR #%d: reopen review-state for pending %s: %v", prNumber, e.commentID, err)
					kept = append(kept, e)
					continue
				}
				store = s
			}

			status, err := d.promotePendingComment(ctx, prNumber, e.commentID, e.since)
			if err == nil && status == "success" {
				if markErr := store.MarkSeen(e.commentID, "success"); markErr != nil {
					d.logf("PR #%d: promote pending %s to success: %v", prNumber, e.commentID, markErr)
				}
				continue
			}
			// err != nil covers both "no review comment yet" (the
			// usual path) and a transient ListPRComments failure
			// (kept by the next tick). The status field is always
			// "pending" in this branch — promotePendingComment only
			// returns (success, nil) or (pending, err) — so we can
			// safely increment the cycle counter and bail on the
			// bounded-retry escape.
			e.cycles++
			if e.cycles >= pendingMaxCycles {
				if markErr := store.MarkSeen(e.commentID, "failure"); markErr != nil {
					d.logf("PR #%d: promote pending %s to failure: %v", prNumber, e.commentID, markErr)
				}
				// Bounded-retry escape: cache the pair so the
				// next tick's processPR skips the trigger. This
				// is a slice-D-only path (processPR's RunBatch-
				// error failure path does NOT fire this).
				d.MarkTerminalSeen(prNumber, e.commentID)
				continue
			}
			e.storeRef = store
			kept = append(kept, e)
		}
		if len(kept) > 0 {
			d.pendingReviews[prNumber] = kept
		} else {
			delete(d.pendingReviews, prNumber)
		}
	}
	return nil
}

// registerPendingReview records a new pending review entry after
// launchReview returned successfully. processPR calls this once the
// per-run ReviewStateStore has been written with status=pending.
func (d *Daemon) registerPendingReview(prNumber int, commentID string, since time.Time, statePath string, store *ReviewStateStore) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	d.pendingReviews[prNumber] = append(d.pendingReviews[prNumber], pendingReviewEntry{
		commentID:   commentID,
		since:       since,
		reviewState: statePath,
		storeRef:    store,
	})
}

// logf writes a line to the broadcaster (or stderr when none is wired).
// The format is a single timestamped line.
func (d *Daemon) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if d.Broadcaster == nil {
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	fmt.Fprintln(d.Broadcaster, msg)
}
