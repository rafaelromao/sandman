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

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/runid"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// PollingInterval is the default interval at which the daemon scans open PRs
// for /sandman review comments. It is exported so tests and the CLI can
// reference the same constant.
const PollingInterval = 30 * time.Second

// PostStepMaxAttempts caps the number of gh pr comment invocations
// postDecision makes before falling back to the rehydrate path. A
// transient gh failure (network blip, rate limit, auth refresh) is
// retried in-process; only a sustained failure reaches the
// pending-rehydrate escape (issue #1847 S4).
const PostStepMaxAttempts = 5

// postStepBackoffs is the per-attempt sleep schedule postWithRetry
// honours between transient PostComment failures. Total worst case
// 1+2+4+8+16 = 31s. The slot is held for that window but the busy
// semaphore has already been released (tick returned when the
// goroutine launched), so the next tick runs unaffected. The
// per-PR slot table (issue #1481 slice C) keeps the trigger from
// being re-launched while the post retries are in flight.
var postStepBackoffs = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
}

// failureBackoffBase is the floor of the launch-failure backoff
// schedule. Doubled each attempt, capped at failureBackoffCap. The
// schedule is exponential so a transient upstream failure (e.g. the
// opencode SDK returning HTTP 500) does not drive a tight
// re-launch loop that mints a new RunID every tick. Issue #2210.
const failureBackoffBase = 10 * time.Second

// failureBackoffCap is the ceiling of the launch-failure backoff
// schedule. Once an attempt's computed backoff exceeds the cap, the
// cap is used for that attempt and every subsequent attempt. Issue
// #2210.
const failureBackoffCap = 60 * time.Second

// nextFailureBackoff returns the backoff duration to sleep before
// the next launch attempt for a commentID that has failed `attempts`
// times in this daemon lifetime. Schedule:
//
//	attempt 1 → 10s
//	attempt 2 → 20s
//	attempt 3 → 40s
//	attempt 4 → 60s (cap reached)
//	attempt 5+ → 60s
//
// attempts < 1 returns 0 so callers can pass raw counters without
// pre-checking. The bit-shift overflow check (d <= 0) closes the
// failure mode where an extreme attempt count would wrap the
// int64-shifted duration to a non-positive value. Issue #2210.
func nextFailureBackoff(attempts int) time.Duration {
	if attempts < 1 {
		return 0
	}
	d := failureBackoffBase << (attempts - 1)
	if d <= 0 || d > failureBackoffCap {
		return failureBackoffCap
	}
	return d
}

// Clock returns the current time. Inject a custom clock in tests to avoid
// time-based dependencies.
type Clock func() time.Time

// Trigger receives tick signals to drive the polling loop. The default
// implementation uses time.NewTicker; tests can inject a manual channel.
type Trigger <-chan struct{}

// GitHubClient is the subset of github.Client used by the review daemon.
// It is exposed as a small interface so tests can substitute a fake.
// Every method takes a context.Context so a hung gh invocation can be
// cancelled by the daemon's loop or by an operator signal (issue #1780).
type GitHubClient interface {
	ListOpenPRs(ctx context.Context) ([]github.PR, error)
	ListPRComments(ctx context.Context, number int) ([]github.PRComment, error)
	AuthenticatedLogin(ctx context.Context) (string, error)
	FetchPR(ctx context.Context, number int) (*github.PR, error)
	RepoName(ctx context.Context) (string, error)
	AddCommentReaction(ctx context.Context, commentID, content string) (string, error)
	AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error)
	RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error
	RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error
}

// BatchRunner is the subset of batch.Runner used by the review daemon.
type BatchRunner interface {
	RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error)
}

// Renderer renders review prompts.
type Renderer interface {
	RenderReview(cfg prompt.RenderConfig, data prompt.PRData) (string, error)
}

// pendingPostEntry is the in-memory record the daemon keeps for a
// review whose agent-run has produced <runDir>/decision.md but
// whose daemon-side post step did not complete (e.g. the daemon
// was cancelled mid-post). The rehydrate-on-startup path (issue
// #1847 S4) walks every review-kind batch at construction; for
// every `pending` review-state.json whose row folder has a
// decision.md on disk, it registers one of these entries keyed by
// (prNumber, commentID).
//
// runDir and reviewState are absolute paths so the processPR
// rehydrate branch can read decision.md and open the per-run
// ReviewStateStore without further resolution. The rehydrate
// branch drops the entry on a successful post (MarkSeen("success")
// is the new terminal-seen status) and retains it on a failed post
// (the next tick retries). When decision.md is missing at tick
// time the entry is treated as stale and the daemon falls through
// to the existing launch path.
//
// since carries the original review-state.json Timestamp so future
// observability surfaces (operator queries, logs) can answer "how
// long has this rehydrate been waiting?" without re-reading the
// on-disk JSON.
type pendingPostEntry struct {
	commentID   string
	runDir      string    // absolute path to <batch>/runs/<rowID>
	reviewState string    // absolute path to <runDir>/review-state.json
	since       time.Time // when the trigger entered `pending` on disk
}

// Daemon polls the repo for /sandman review comments and launches review
// agents up to the configured parallel_reviews limit.
type Daemon struct {
	BaseDir              string
	Layout               paths.Layout
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
	CommentPoster        CommentPoster
	controlSocket        *daemon.ControlSocket
	busy                 chan struct{}
	promptOnce           sync.Once
	seenCache            map[int]map[string]bool
	seenCacheMu          sync.RWMutex
	// nextAttempt is the per-(prNumber, commentID) retry-budget
	// stamp persisted in review-state.json (issue #2211). The
	// processPR dedup loop consults it on every tick and skips
	// triggers whose stamp is in the future — so a launch failure
	// gates re-launches without holding the per-PR slot. Hydrated
	// at construction by loadSeenCache alongside the seen cache;
	// updated by recordLaunchFailure (write) and by MarkSeen
	// "success" via the success-clears-stamp branch.
	nextAttempt   map[int]map[string]time.Time
	nextAttemptMu sync.RWMutex
	slotTable     map[int]struct{}
	slotPool      chan struct{}
	slotMu        sync.Mutex
	// pendingPost is the rehydrate-on-startup map (issue #1847 S4).
	// Outer key is PR number; inner key is the trigger comment ID.
	// Each entry remembers the absolute path of the per-row folder
	// (so the daemon can read <runDir>/decision.md at tick time)
	// and the absolute path of the per-run review-state.json (so
	// the rehydrate post can MarkSeen on the right store). Entries
	// are written by loadPendingPosts at construction, read and
	// dropped by processPR's rehydrate branch on a successful
	// post, and read-and-retained on a failed post.
	//
	// The map is locked by a dedicated mutex (pendingPostMu) to
	// match the existing one-mutex-per-data-structure convention
	// (seenCacheMu guards seenCache). Sharing pendingPostMu between
	// this map and any future map would create ambiguous ownership
	// for future readers.
	pendingPostMu sync.Mutex
	pendingPost   map[int]map[string]pendingPostEntry
	inFlight      sync.WaitGroup
	// postBackoffs is the per-attempt sleep schedule used by
	// postWithRetry. It defaults to the package-level
	// postStepBackoffs when nil; tests inject a zero-length slice
	// (or all-zero durations) so the retry loop completes in
	// milliseconds instead of sleeping through the real 31s
	// budget.
	postBackoffs       []time.Duration
	authenticatedLogin string
	// launchBackoff is the per-attempts sleep schedule used by
	// sleepLaunchFailureBackoff. It defaults to nextFailureBackoff
	// when nil; tests inject a zero-cost function
	// (func(int) time.Duration { return 0 }) so the launch
	// goroutine returns immediately after recording a failure
	// instead of sleeping through the real 10–60s budget. Issue
	// #2210.
	launchBackoff func(attempts int) time.Duration
}

// effectiveLaunchBackoff returns the launch-failure backoff for
// `attempts` recorded failures. When d.launchBackoff is set (by
// tests), it wins; otherwise the package-level nextFailureBackoff
// is used. Issue #2210.
func (d *Daemon) effectiveLaunchBackoff(attempts int) time.Duration {
	if d.launchBackoff != nil {
		return d.launchBackoff(attempts)
	}
	return nextFailureBackoff(attempts)
}

// New returns a Daemon configured with the project defaults for the
// polling interval and clock. The seen cache is hydrated eagerly from
// the on-disk batches index (issue #1480 slice A), and the in-memory
// pendingPost map (issue #1847 S4) is rehydrated from the same index
// so an in-flight rehydrate post survives a daemon restart. A
// missing or unreadable index yields empty caches; the rename-loser
// trade-off from ADR-0034 §3 means a stale skip is acceptable.
//
// parallel and parallelSet thread the CLI --parallel override through to
// the slot-pool sizing: when parallelSet is true and parallel > 0, the
// slot pool is sized to parallel regardless of cfg.DefaultReviewParallel.
// When parallelSet is false, the slot pool falls back to
// cfg.EffectiveReviewParallel() (the historical behavior).
func New(baseDir string, gh GitHubClient, prompts Renderer, runner BatchRunner, cfg *config.Config, broadcaster io.Writer, parallel int, parallelSet bool, poster CommentPoster) *Daemon {
	// poster is required in production. The nil-to-nop fallback
	// exists only so the dozens of pre-#1846 test fixtures
	// (daemon_test.go, daemon_canonical_test.go, etc.) keep
	// compiling without each one adding an explicit CommentPoster
	// argument. The cmd layer (cmd/review.go) always wires a real
	// GH-backed poster; tests that exercise the post step
	// (daemon_sliceS3_test.go) inject a fake at the seam.
	if poster == nil {
		poster = nopCommentPoster{}
	}
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
		BaseDir:       baseDir,
		GitHub:        gh,
		Prompts:       prompts,
		Runner:        runner,
		Config:        cfg,
		Broadcaster:   broadcaster,
		Clock:         time.Now,
		Trigger:       nil,
		PollInterval:  PollingInterval,
		Parallel:      parallel,
		ParallelSet:   parallelSet,
		CommentPoster: poster,
		busy:          make(chan struct{}, 1),
		seenCache:     map[int]map[string]bool{},
		slotTable:     map[int]struct{}{},
		slotPool:      make(chan struct{}, parallelReviews),
		// S4 (issue #1847): initialise the rehydrate-on-startup
		// map; loadPendingPosts (Slice B) populates it from the
		// on-disk review-state.json files at construction.
		pendingPost: map[int]map[string]pendingPostEntry{},
	}
	if err := d.loadSeenCache(); err != nil {
		d.logf("load seen cache: %v", err)
	}
	// Issue #1847 (S4): rehydrate-on-startup. Walk every
	// review-kind batch and register one pendingPost entry per
	// `pending` review-state.json whose row folder has
	// decision.md on disk. The next tick's processPR consults
	// this map before the launch path so the daemon posts the
	// existing body instead of re-running the agent. See
	// loadPendingPosts for the full filter.
	if err := d.loadPendingPosts(); err != nil {
		d.logf("load pending posts: %v", err)
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

// WaitForIdle blocks until all in-flight review goroutines have completed
// (slotHeldCount returns 0) or ctx is cancelled. It is intended for tests
// that need to wait for background reviews to settle after tick returns.
// It does NOT gate on the S4 rehydrate map — successful posts drop their
// entries inline in processPR; failed posts retain entries and are retried
// on the next tick (separate behaviour from in-flight goroutines).
func (d *Daemon) WaitForIdle(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if d.slotHeldCount() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
// passes shouldSkipDedupStatus. The per-trigger retry-budget stamp
// (issue #2211) is also cleared so the gate in processPR does not
// block a comment that has reached its terminal state.
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
	d.SetNextAttemptAt(prNumber, commentID, time.Time{})
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

// NextAttemptAt reports the persisted retry-budget stamp for
// (prNumber, commentID). Returns the zero time.Time when no gate is
// set — callers compare against time.Now() and treat zero as
// "launch immediately". Issue #2211.
func (d *Daemon) NextAttemptAt(prNumber int, commentID string) time.Time {
	d.nextAttemptMu.RLock()
	defer d.nextAttemptMu.RUnlock()
	if m, ok := d.nextAttempt[prNumber]; ok {
		return m[commentID]
	}
	return time.Time{}
}

// SetNextAttemptAt records the retry-budget stamp for
// (prNumber, commentID) in the daemon's in-memory map. Pass zero
// time to clear the stamp. The on-disk source of truth lives in
// review-state.json; this map is the per-process short-circuit the
// processPR dedup loop consults on every tick. Issue #2211.
func (d *Daemon) SetNextAttemptAt(prNumber int, commentID string, stamp time.Time) {
	d.nextAttemptMu.Lock()
	defer d.nextAttemptMu.Unlock()
	if d.nextAttempt == nil {
		d.nextAttempt = map[int]map[string]time.Time{}
	}
	if _, ok := d.nextAttempt[prNumber]; !ok {
		d.nextAttempt[prNumber] = map[string]time.Time{}
	}
	if stamp.IsZero() {
		delete(d.nextAttempt[prNumber], commentID)
		return
	}
	d.nextAttempt[prNumber][commentID] = stamp
}

// peekPendingPost reports the rehydrate post entry for
// (prNumber, commentID), if any. The slice-A/B/C/D/E regression
// tests use this to observe the in-memory map without depending
// on processPR's launch/rehydrate internal wiring. Returns
// (entry, true) on a hit, (zero, false) on a miss. Reads the
// pendingPostMu lock under which the walker (Slice B) writes.
func (d *Daemon) peekPendingPost(prNumber int, commentID string) (pendingPostEntry, bool) {
	d.pendingPostMu.Lock()
	defer d.pendingPostMu.Unlock()
	if m, ok := d.pendingPost[prNumber]; ok {
		if entry, ok := m[commentID]; ok {
			return entry, true
		}
	}
	return pendingPostEntry{}, false
}

// loadSeenCache rebuilds the seen cache from scratch by scanning the
// on-disk batches index and the canonical run folders for every
// review batch. Per ADR-0030 §Per-row RunID templates (issue #1551)
// review runs are first-class rows, so each batch's run.json lives
// under `<batch>/runs/<runID>/run.json` (see ReviewRunIDFor for the
// exact per-row shape) and its review-state.json lives one folder up
// next to it. Existing entries are replaced.
func (d *Daemon) loadSeenCache() error {
	d.seenCacheMu.Lock()
	defer d.seenCacheMu.Unlock()
	d.seenCache = map[int]map[string]bool{}

	d.nextAttemptMu.Lock()
	defer d.nextAttemptMu.Unlock()
	d.nextAttempt = map[int]map[string]time.Time{}

	idx, err := seenCacheLoader(d.BaseDir)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}
	if idx == nil {
		return nil
	}
	for _, entry := range idx.Batches {
		if entry.Kind != batchindex.KindReview {
			continue
		}
		// Resolve the canonical row RunID for this batch. The
		// canonical rowID is the value persisted on the
		// batch's run.json by prepareReviewRun — by reading it
		// here we are version-independent of the exact
		// `<ts>-<sid>-<linkedIssue?>-PR<pr>` shape.
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
			if shouldSkipDedupStatus(sc.Status) {
				if _, ok := d.seenCache[entry.PR]; !ok {
					d.seenCache[entry.PR] = map[string]bool{}
				}
				d.seenCache[entry.PR][sc.CommentID] = true
			}
			if sc.NextAttemptAt != nil && !sc.NextAttemptAt.IsZero() {
				if _, ok := d.nextAttempt[entry.PR]; !ok {
					d.nextAttempt[entry.PR] = map[string]time.Time{}
				}
				d.nextAttempt[entry.PR][sc.CommentID] = *sc.NextAttemptAt
			}
		}
	}
	return nil
}

// readReviewRowID returns the row RunID for a review batch's runs
// directory. It consults the first run.json under runs/ — review
// batches always launch a single row, so there is exactly one
// run.json — and returns its `RunID` field. The folder name matches
// the canonical per-row RunID from ADR-0030 §Per-row RunID templates
// (see ReviewRunIDFor for the exact shape). The legacy
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

// loadPendingPosts rehydrates the in-memory pendingPost map from the
// on-disk review-state.json files referenced by .sandman/batches.json.
//
// Issue #1847 (S4): a daemon restart between the agent-run finishing
// its write of <runDir>/decision.md and the daemon completing the
// post step (e.g. process killed during `gh pr comment`, ctx cancelled
// mid-post, network blip) leaves a review in the state "review is on
// disk but not on the PR". Rather than re-launch the agent on the
// next tick — which would produce a duplicate review the bot has no
// memory of writing — the next daemon reads the existing
// decision.md, redacts it via RedactBody, and posts it as part of
// the normal tick. The pendingPost map is the in-memory index of
// "review files waiting for the daemon to post".
//
// The walker is read-only and reuses the seenCacheLoader +
// readReviewRowID + seenStateReader seams so it observes the same
// on-disk shape the existing loadSeenCache walker already reads
// from. Per-row RunID resolution (ADR-0030 §Per-row RunID
// templates, issue #1551) is preserved.
//
// The walker's three filters are all required for an entry to be
// registered:
//
//  1. entry.Kind == batchindex.KindReview (so non-review batches
//     such as changes cannot register a pendingPost);
//  2. sc.Status == "pending" (matches the S3 lazy-verify contract:
//     terminal-seen statuses do not get reposted);
//  3. <runDir>/decision.md exists on disk (the source-of-truth gate:
//     a missing file means the bot never finished, so rehydrate has
//     nothing to post — the daemon launches a fresh agent instead).
//
// Issue #1849 (S6): the lazy-verify walker that previously
// coexisted with this one is gone. `pendingPost` is now the SOLE
// rehydrate mechanism. A row with decision.md on disk takes the
// rehydrate path; a row without decision.md (or with the rehydrate
// drop on a stale entry) falls through to the launch path which
// re-runs the agent.
//
// Existing entries are replaced (consistent with loadSeenCache).
// Best-effort on partial-failure: a single unreadable
// review-state.json is logged and skipped, never fatal, because the
// rename-loser trade-off from ADR-0034 §3 accepts a stale skip over
// a daemon-start failure.
func (d *Daemon) loadPendingPosts() error {
	d.pendingPostMu.Lock()
	defer d.pendingPostMu.Unlock()
	d.pendingPost = map[int]map[string]pendingPostEntry{}

	idx, err := seenCacheLoader(d.BaseDir)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}
	if idx == nil {
		return nil
	}
	for _, entry := range idx.Batches {
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
		reviewState := filepath.Join(runDir, "review-state.json")
		for _, sc := range state.SeenComments {
			if sc.Status != "pending" {
				continue
			}
			// Source-of-truth gate: a row is rehydrate-eligible
			// only when decision.md actually exists on disk as a
			// regular file (issue #1949: a directory at that path
			// is treated as missing so the next tick falls through
			// to the launch path which clears the worktree via
			// ClearReviewArtifacts).
			// Issue #1953: decision.md lives in the per-row
			// worktree (the agent's CWD), not the run folder.
			// The worktree path is derived from (prNumber,
			// commentID) so the walker can compute it without
			// coordination with the orchestrator.
			worktreePath := d.reviewWorktreePath(entry.PR, sc.CommentID)
			decisionPath := filepath.Join(worktreePath, "decision.md")
			info, statErr := os.Stat(decisionPath)
			if statErr != nil {
				if !os.IsNotExist(statErr) {
					d.logf("stat %s: %v", decisionPath, statErr)
				}
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if _, ok := d.pendingPost[entry.PR]; !ok {
				d.pendingPost[entry.PR] = map[string]pendingPostEntry{}
			}
			d.pendingPost[entry.PR][sc.CommentID] = pendingPostEntry{
				commentID:   sc.CommentID,
				runDir:      worktreePath,
				reviewState: reviewState,
				since:       sc.Timestamp,
			}
		}
	}
	return nil
}

// InvalidatePendingPosts forces a rebuild of the in-memory
// pendingPost map by re-running the on-disk scan. Symmetric with
// InvalidateSeenCache.
func (d *Daemon) InvalidatePendingPosts() error {
	return d.loadPendingPosts()
}

// SocketPath returns the absolute path of the daemon's control socket.
// The socket lives under .sandman/reviews/ alongside the shared prompt
// template, so the daemon's on-disk footprint is just two files plus
// run folders under .sandman/batches/. When the daemon has a layout
// field set, the layout's ReviewSocketPath is the single source of
// truth; otherwise the legacy BaseDir-derived path is returned for
// callers that pre-date the layout migration.
func (d *Daemon) SocketPath() string {
	if d.Layout.RepoRoot != "" {
		return d.Layout.ReviewSocketPath()
	}
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
// contains only review.sock, review-prompt.md, and the quality
// rules file materialised alongside it".
func (d *Daemon) PromptTemplatePath() string {
	if d.Layout.RepoRoot != "" {
		return d.Layout.ReviewPromptPath()
	}
	return filepath.Join(d.BaseDir, "reviews", "review-prompt.md")
}

// QualityRulesPath returns the absolute path of the quality rules file
// that the reviewer reads at runtime. It lives next to the review
// prompt so the two are materialised and edited together.
func (d *Daemon) QualityRulesPath() string {
	if d.Layout.RepoRoot != "" {
		return d.Layout.QualityRulesPath()
	}
	return filepath.Join(d.BaseDir, "reviews", "quality-rules.md")
}

// initPromptTemplate writes the static, PR-agnostic review prompt
// template to PromptTemplatePath() and the quality rules to
// .sandman/reviews/quality-rules.md, but only when they are missing.
// Files materialised by `sandman init` (or by a previous daemon start)
// are left untouched so user edits survive restarts. It is safe to call
// from multiple goroutines and from both StartSocket and launchReview.
//
// Both writes go through atomicfs.WriteAtomic, which uses a unique
// temp-file suffix (".tmp.<random>") rather than a fixed "<path>.tmp",
// so two concurrent callers (across processes or goroutines) cannot
// collide on the same temp name. The pre-check os.Stat + os.IsNotExist
// guard is preserved so user-edited files are not clobbered.
func (d *Daemon) initPromptTemplate() error {
	var err error
	d.promptOnce.Do(func() {
		if _, statErr := os.Stat(d.PromptTemplatePath()); os.IsNotExist(statErr) {
			if err = atomicfs.WriteAtomic(d.PromptTemplatePath(), []byte(prompt.DefaultPRReviewPrompt()), 0644); err != nil {
				return
			}
		}
		qualityRulesPath := d.QualityRulesPath()
		if _, statErr := os.Stat(qualityRulesPath); os.IsNotExist(statErr) {
			if err = atomicfs.WriteAtomic(qualityRulesPath, []byte(prompt.DefaultQualityRules()), 0644); err != nil {
				return
			}
		}
	})
	return err
}

// ReviewStatePath returns the on-disk path of the per-run review-state
// file for a given run folder.
//
// Per ADR-0030 §Per-row RunID templates (issue #1551) review runs are
// first-class rows, so the review-state file lives next to its row's
// run.json under the canonical per-row folder:
// `<batch>/runs/<runID>/review-state.json` (see ReviewRunIDFor). The
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
	login, err := d.GitHub.AuthenticatedLogin(ctx)
	if err != nil {
		return fmt.Errorf("authenticated GitHub login: %w", err)
	}
	d.authenticatedLogin = strings.TrimSpace(login)
	if d.authenticatedLogin == "" {
		return fmt.Errorf("authenticated GitHub login is empty")
	}

	if err := d.StartSocket(); err != nil {
		return err
	}
	defer d.Stop()
	defer d.inFlight.Wait()

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

	prs, err := d.GitHub.ListOpenPRs(ctx)
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
	comments, err := d.GitHub.ListPRComments(ctx, prNumber)
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	if len(comments) == 0 {
		return nil
	}

	sort.Slice(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})

	// priorReviewExists is computed from the raw comment list (before
	// any trigger / bot-shaped filter) and feeds the {{PRIOR_REVIEW_EXISTS}}
	// substitution in the review prompt (issue #1892). The flag tells
	// the review agent whether to render the `## Previous review
	// progress` section at all. IsReviewRequest excludes implementor
	// triggers (`/sandman review …`, `@bot /sandman review …`) so they
	// are NOT counted as prior reviews. Bot self-posts (filtered out of
	// the trigger list by LooksLikeBotReviewBody below) ARE counted as
	// prior reviews — they are reviews of the PR, just not fresh
	// triggers. The two filters answer different questions and coexist.
	priorReviewExists := false
	for _, c := range comments {
		if !IsReviewRequest(c.Body) {
			priorReviewExists = true
			break
		}
	}

	type unseenTrigger struct {
		comment github.PRComment
		focus   string
	}
	var triggers []unseenTrigger
	for _, comment := range comments {
		if d.authenticatedLogin != "" && (strings.TrimSpace(comment.AuthorLogin) == "" || !strings.EqualFold(comment.AuthorLogin, d.authenticatedLogin)) {
			continue
		}
		// Self-post filter (issue #1648, ordering fixed by #1702):
		// runs BEFORE ParseTrigger so a comment whose body matches
		// a hash the bot has previously posted is dropped before its
		// body is parsed for a trigger. This protects the daemon
		// from re-triggering a review on the bot's own review-body,
		// which contains the literal `/sandman review` substring in
		// its `## Previous review progress` section. The
		// Structural self-defence (issues #1821 / #1848): a body
		// that carries the `## Previous review progress` heading
		// AND the literal `/sandman review` trigger substring is
		// overwhelmingly likely to be a previous bot review
		// body, not a fresh implementor trigger. Post-#1848 the
		// `SelfPostStore` is gone and this sniff is the SOLE
		// gate that drops bot-shaped bodies before
		// `ParseTrigger`. Daemon-side redaction (issue #1845)
		// makes this a defence-in-depth measure — the bot body
		// never contains the trigger substring in the normal
		// case — but it remains the structural last line of
		// defence against a previous bot review re-matching the
		// trigger regex. The asymmetric contract — bot bodies
		// are flagged, bare implementor triggers are not — is
		// pinned by `TestLooksLikeBotReviewBody` in
		// `trigger_test.go`.
		if LooksLikeBotReviewBody(comment.Body) {
			d.logf("PR #%d: comment %s structurally matches a bot review body; dropping before ParseTrigger (issue #1821 self-defence)", prNumber, comment.ID)
			continue
		}
		focus, ok := ParseTrigger(comment.Body)
		if ok {
			triggers = append(triggers, unseenTrigger{comment: comment, focus: focus})
			continue
		}
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
		// Issue #2211: per-trigger retry-budget gate. Skips
		// triggers whose persisted NextAttemptAt is in the
		// future, without acquiring a slot and without log spam
		// — the next tick after the stamp elapses will retry
		// the launch. The stamp is per-(prNumber, commentID)
		// so a fresh trigger on the same PR is not blocked by
		// an older comment's stamp.
		if stamp := d.NextAttemptAt(prNumber, t.comment.ID); !stamp.IsZero() && stamp.After(d.now()) {
			continue
		}
		unprocessed = append(unprocessed, t)
	}
	d.seenCacheMu.RUnlock()
	if len(unprocessed) == 0 {
		return nil
	}

	// Rehydrate-on-startup (issue #1847 S4): consult pendingPost for
	// each remaining trigger. Issue #1849 (S6): the lazy-verify
	// map is gone, so this is the SOLE rehydrate gate. TryRehydratePost
	// returns true when the entry was handled (success, failure,
	// ctx-cancel, or kept-for-retry) — the trigger is dropped from
	// unprocessed. Returns false when the entry is stale (decision.md
	// missing at tick time) — the trigger falls through to the launch
	// path.
	d.pendingPostMu.Lock()
	hasPostEntry := map[string]bool{}
	for cid := range d.pendingPost[prNumber] {
		hasPostEntry[cid] = true
	}
	d.pendingPostMu.Unlock()
	if len(hasPostEntry) > 0 {
		var filtered []unseenTrigger
		for _, t := range unprocessed {
			if hasPostEntry[t.comment.ID] {
				if d.tryRehydratePost(ctx, prNumber, t.comment) {
					continue
				}
			}
			filtered = append(filtered, t)
		}
		if len(filtered) == 0 {
			return nil
		}
		unprocessed = filtered
	}

	// Lazy verify (issue #1482 slice D) is gone (issue #1849 S6);
	// no in-memory pendingSet filter is needed — the seen-cache
	// short-circuit (driven by MarkSeen on success / failure) is
	// the sole deduplication gate.

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

	reviewRunFolder, perRowRunID, rs, state, prepErr := d.prepareReviewRun(ctx, prNumber, comment.ID)
	if prepErr != nil {
		d.logf("prepare review run for PR #%d comment %s: %v", prNumber, comment.ID, prepErr)
		d.releasePRSlot(prNumber)
		return nil
	}

	if !state.TryClaim(comment.ID) {
		d.logf("comment %s already claimed or terminal-seen, skipping", comment.ID)
		_ = rs.Close()
		d.releasePRSlot(prNumber)
		return nil
	}

	commentReactionID, commentErr := d.GitHub.AddCommentReaction(ctx, comment.ID, "eyes")
	if commentErr != nil {
		d.logf("add reaction to comment %s: %v", comment.ID, commentErr)
	}
	prReactionID, prErr := d.GitHub.AddIssueReaction(ctx, prNumber, "eyes")
	if prErr != nil {
		d.logf("add reaction to PR #%d: %v", prNumber, prErr)
	}

	// persisted must be captured before any MarkSeen call so it
	// reflects whether the state file pre-existed the launch path.
	statePath := d.ReviewStatePath(reviewRunFolder)
	persisted, _ := os.Stat(statePath)

	// Superseded marking is independent of RunBatch and stays
	// synchronous so the goroutine and the sync preamble never
	// touch the ReviewStateStore concurrently.
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

	// Launch the review asynchronously so tick returns immediately.
	// The slot is held by the goroutine and released after MarkSeen
	// persists terminal-seen state on disk — this lets the slot pool
	// fill across ticks as ADR-0034 §Per-PR slot table intended.
	//
	// Issue #1846 (S3) and #1849 (S6): launchReview is the SOLE
	// writer to MarkSeen on the launch path, and the lazy-verify
	// multi-cycle walker is gone. The goroutine's only failure
	// surface is ctx-cancel between RunBatch returning and the post
	// step recording terminal-seen state; in that case no MarkSeen
	// was recorded, so the goroutine releases the claim and the
	// next tick's processPR re-launches the trigger. All other
	// errors (post-step failures, pre-batch errors) are already
	// terminal-seen by the time the goroutine sees them, so the
	// seen-cache short-circuit keeps the trigger from re-launching.
	d.inFlight.Add(1)
	go func() {
		defer d.inFlight.Done()
		defer d.releasePRSlot(prNumber)

		launchErr := d.launchReview(ctx, prNumber, focus, comment.ID, commentReactionID, prReactionID, reviewRunFolder, perRowRunID, rs, state, priorReviewExists)
		if launchErr != nil {
			d.logf("launch review for PR #%d comment %s: %v", prNumber, comment.ID, launchErr)
			// Ctx-cancel between RunBatch and the post step:
			// no MarkSeen was recorded; release the claim so
			// the next tick's processPR can re-launch.
			if errors.Is(launchErr, context.Canceled) || errors.Is(launchErr, context.DeadlineExceeded) {
				if persisted == nil {
					state.Release(comment.ID)
				}
				return
			}
			// All other errors (post-step or pre-batch):
			// recordLaunchFailure / postDecision recorded
			// MarkSeen("failure") with the incremented
			// attempts counter AND the NextAttemptAt stamp
			// (issue #2211). The per-PR slot is released
			// immediately; the gate in processPR consults
			// the stamp on the next tick and skips the
			// trigger until the budget elapses. The seen
			// cache was NOT marked terminal — the S6
			// retryable contract holds. Issue #2210's
			// sleep-in-goroutine is no longer required
			// because the gate carries the timing forward.
			return
		}
	}()

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
//   - failure is treated as terminal-seen in the in-memory seen
//     cache (issue #1849 S6): the lazy-verify bounded-retry walker
//     is gone, so the bounded-retry contract is now expressed by
//     postDecision calling MarkTerminalSeen immediately after
//     MarkSeen("failure"). The on-disk status remains "failure"
//     (so operator-driven retry via re-post still works), but the
//     processPR loop drops the trigger before launch.
//   - aborted is retryable (the run was interrupted before publishing a
//     review, so the trigger should be retried)
//   - superseded is treated as terminal (obsolete trigger, not in the terminal-status set PRD #1218 specified)
//   - success is terminal (the review comment was published)
//   - pending is retryable: the S4 rehydrate walker (issue #1847) is
//     the only mechanism that observes pending entries from disk;
//     no daemon code path writes "pending" anymore.
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
// ReviewRunIDFor below per ADR-0030 §Per-row RunID templates; the run
// folder is named after that per-row RunID (not the legacy `runs/review`
// alias). This replaces the legacy literal `RunID: "review"` alias —
// issue #1551 makes the review run a first-class row like every other
// run kind.
func (d *Daemon) prepareReviewRun(ctx context.Context, prNumber int, commentID string) (string, string, *daemon.RunSession, *ReviewStateStore, error) {
	ts, shortid, err := runid.NewBatch()
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("generate batch ID: %w", err)
	}

	var linkedIssue int
	if pr, fetchErr := d.GitHub.FetchPR(ctx, prNumber); fetchErr == nil && pr != nil {
		linkedIssue = pr.LinkedIssueNumber()
	} else if fetchErr != nil {
		// Non-fatal: a transient GitHub API failure must not block
		// the launch path. The one-shot `cmd/review.go` calls
		// FetchPR anyway; if it failed once, it would fail again.
		// We surface the failure in the daemon log instead.
		d.logf("fetch PR #%d for linked issue resolution: %v", prNumber, fetchErr)
	}

	perRowRunID := ReviewRunIDFor(prNumber, linkedIssue, ts, shortid)

	rs := daemon.NewRunSession(d.BaseDir, perRowRunID)
	// Issue #1919 slice 3: the on-disk batch directory name and the
	// per-row RunID MUST agree for both orphan and linked reviews.
	// For orphan reviews both are `<ts>-<sid>-PR<pr>`; for linked
	// reviews both are `<ts>-<sid>-<linkedIssue>-PR<pr>`. ADR-0030
	// pins the same invariant on batch.json.batchId, run.json.BatchID,
	// and the run.started payload's batch_id field.
	manifest := daemon.BatchManifest{BatchId: perRowRunID, CreatedAt: time.Now(), RunKind: "review", RunTS: ts, RunShortID: shortid, Issues: []int{}, PR: &prNumber}
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
		RunID:   perRowRunID,
		BatchID: perRowRunID,
		PR:      prNumber,
		Kind:    batchindex.KindReview,
		// Issue #2220 slice 2: WorktreePath must be stamped onto the
		// review manifest so the portal's verdict reader
		// (reviewVerdictFromDecisionFile) can locate decision.md at the
		// per-row worktree, which is the canonical artifact path since
		// #1953 made the worktree the agent's CWD. Without this field,
		// the verdict reader falls back to <runDir>/decision.md, which
		// no longer exists, and every review row surfaces as "Unclear"
		// in the portal even when the agent wrote a parseable verdict.
		// Implementation-run manifests (orchestrator.go:2557) already
		// carry WorktreePath; this brings review manifests into parity.
		WorktreePath: d.reviewWorktreePath(prNumber, commentID),
		CreatedAt:    time.Now(),
		Status:       batchindex.RunManifestStatusActive,
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

// reviewWorktreeBase returns the host-absolute path of the
// per-row review worktree base directory. The worktree itself is
// <reviewWorktreeBase>/<reviewBranchName(pr, commentID)>.
//
// In production (cmd/review.go) the daemon's Layout is set by
// paths.NewLayout(cfg, repoRoot), which already resolves a
// relative cfg.WorktreeDir against repoRoot. We use that resolved
// path as the source of truth so the daemon and the orchestrator
// (which creates the worktree via NewSandbox) agree on the exact
// location. For the test fixture path (newDaemonForTest) where
// Layout is the zero value, we fall back to resolving
// cfg.WorktreeDir against d.BaseDir to keep the existing tests
// working.
func (d *Daemon) reviewWorktreeBase() string {
	if d.Layout.RepoRoot != "" {
		return d.Layout.WorktreeDir
	}
	worktreeDir := ""
	if d.Config != nil {
		worktreeDir = strings.TrimSpace(d.Config.WorktreeDir)
	}
	if worktreeDir != "" && !filepath.IsAbs(worktreeDir) {
		worktreeDir = filepath.Join(d.BaseDir, worktreeDir)
	}
	return worktreeDir
}

// reviewWorktreePath returns the host-absolute path of the per-row
// review worktree (issue #1953). The worktree layout is fixed:
// <reviewWorktreeBase>/<reviewBranchName(pr, commentID)>. The
// sandbox creation path in the container runtime creates the
// directory at this exact location; both the daemon and the agent
// can therefore compute the same path without coordination.
func (d *Daemon) reviewWorktreePath(prNumber int, commentID string) string {
	return filepath.Join(d.reviewWorktreeBase(), reviewBranchName(prNumber, commentID))
}

// reviewDecisionPath returns the host-absolute path of the
// decision.md file the agent writes. The worktree IS the canonical
// location for review artifacts (issue #1953); the run folder
// keeps run.json, run.log, and the per-row state files but
// decision.md belongs to the worktree.
func (d *Daemon) reviewDecisionPath(prNumber int, commentID string) string {
	return filepath.Join(d.reviewWorktreePath(prNumber, commentID), "decision.md")
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
// On success this function records the trigger comment as `success`
// (or `failure`, on post-step errors) in the per-run review-state.json
// via the post step (issue #1846 S3). The seen-cache hook fires on
// `success`, short-circuiting subsequent ticks. On `failure`, the post
// step additionally calls MarkTerminalSeen so the next tick's processPR
// drops the trigger before launch (issue #1849 S6 — the lazy-verify
// bounded-retry walker is gone; the bounded-retry contract is now
// expressed as a single-shot at launch-end via the seen-cache).
func (d *Daemon) launchReview(ctx context.Context, prNumber int, focus, commentID, commentReactionID, prReactionID, reviewRunFolder, perRowRunID string, rs *daemon.RunSession, state *ReviewStateStore, priorReviewExists bool) error {
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
			if err := d.GitHub.RemoveCommentReaction(ctx, commentID, commentReactionID); err != nil {
				d.logf("remove reaction from comment %s: %v", commentID, err)
			}
		}
		if prReactionID != "" {
			if err := d.GitHub.RemoveIssueReaction(ctx, prNumber, prReactionID); err != nil {
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

	pr, err := d.GitHub.FetchPR(ctx, prNumber)
	if err != nil {
		return fmt.Errorf("fetch PR: %w", err)
	}

	// sandboxMode stays the effective resolution the rest of this
	// function relies on. Computed here (rather than at the original
	// site further down) so the prompt's {{RUN_DIR}} substitution below
	// can rebase reviewRunFolder onto the container mount when the
	// sandbox is container-style. The value is reused verbatim in
	// req.Sandbox a few dozen lines below; the only behaviour change
	// vs. the previous ordering is that sandboxMode is available here.
	sandboxMode := d.Sandbox
	if sandboxMode == "" && d.Config != nil {
		sandboxMode = d.Config.Sandbox
	}
	if sandboxMode == "" {
		sandboxMode = config.DefaultSandbox
	}

	// Issue #1902 + issue #1953: the prompt's {{RUN_DIR}} resolves
	// to the per-row worktree path (the agent's CWD), not the run
	// folder. The worktree path is deterministic from
	// (prNumber, commentID, d.Config.WorktreeDir) so the daemon
	// knows where the agent wrote decision.md before the orchestrator
	// reports back. Translate to the container-visible form so the
	// agent inside a podman/docker sandbox sees the same path the
	// daemon writes from on the host.
	//
	// The production wiring in cmd/review.go passes
	// filepath.Join(repoRoot, ".sandman") as the Daemon's BaseDir,
	// so filepath.Dir(d.BaseDir) is the host repo root; the
	// ".sandman"-suffix guard keeps the translation a no-op for
	// tests (newDaemonForTest passes a tmp dir directly, no
	// .sandman layout).
	repoRoot := ""
	if filepath.Base(d.BaseDir) == ".sandman" {
		repoRoot = filepath.Dir(d.BaseDir)
	}
	agentRunDir := sandbox.ContainerVisiblePath(d.reviewWorktreePath(prNumber, commentID), repoRoot, sandboxMode)

	rendered, err := d.Prompts.RenderReview(prompt.RenderConfig{}, prompt.PRData{
		Number:            pr.Number,
		Title:             pr.Title,
		Body:              pr.Body,
		ReviewFocus:       focus,
		RunDir:            agentRunDir,
		PriorReviewExists: priorReviewExists,
	})
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}

	if err := d.initPromptTemplate(); err != nil {
		return fmt.Errorf("init review prompt template: %w", err)
	}

	agentName := ""
	modelName := ""
	if d.Config != nil {
		agentName = d.effectiveAgent()
		modelName = d.effectiveModel()
	}
	if agentName == "" {
		return errors.New("review agent is not set; configure review_agent or agent in sandman config")
	}
	if modelName == "" {
		return errors.New("review model is not set; configure review_model or model in sandman config")
	}

	repoName, err := d.GitHub.RepoName(ctx)
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
		WorktreeDir:  d.reviewWorktreeBase(),
	}
	if _, err := d.Runner.RunBatch(ctx, req); err != nil {
		return d.recordLaunchFailure(ctx, commentID, state, fmt.Errorf("run batch: %w", err))
	}

	// S3 post step (issue #1846): the agent writes
	// <runDir>/decision.md; the daemon reads it, redacts it, and
	// posts via d.CommentPoster. The post step is the terminal
	// action of launchReview, so this function owns the
	// MarkSeen("success"/"failure") recording rather than the
	// launch goroutine doing it. The goroutine therefore no
	// longer makes MarkSeen calls on the launch path — its
	// `else` branch only Releases the claim so the bounded-retry
	// escape can re-process the comment if launchReview returned
	// an error before any decision.md existed.
	return d.postDecision(ctx, prNumber, commentID, reviewRunFolder, state)
}

// postDecision implements the S3 post step (issue #1846):
//
//   - If <worktree>/decision.md is missing: MarkSeen("pending") and
//     register a pendingPost entry (issue #1949) so the next tick's
//     rehydrate walker drops the stale entry (decision.md still
//     missing) and falls through to the launch path. The trigger is
//     NOT marked terminal-seen — failure here means "the agent did
//     not write a review", which the daemon treats as retryable:
//     the next tick re-launches the agent and re-runs the post step.
//   - If <worktree>/decision.md is a directory: same contract as the
//     missing branch (issue #1949).
//   - If present: read it, run RedactBody, call
//     d.CommentPoster.PostComment(ctx, prNumber, redacted).
//   - On successful post: MarkSeen("success"). The SeenCacheInvalidator
//     hook fires MarkTerminalSeen, short-circuiting subsequent ticks.
//   - On post error or ctx.Err() != nil while preparing/posting:
//     leave status untouched (no MarkSeen call so the trigger stays
//     in the previous on-disk state, which is absent at this point —
//     no daemon code path writes `pending` anymore per issue #1849
//     S6); return the error so the goroutine takes its claim-Release
//     path and the next tick's processPR re-launches the trigger.
//
// Issue #1953: decision.md lives in the per-row worktree (the
// agent's CWD), not the run folder. The run folder keeps run.json,
// run.log, run.sock, and the per-row state files. The worktree
// path is deterministic from (prNumber, commentID,
// d.Config.WorktreeDir), so the daemon computes it without waiting
// for the orchestrator to report back.
func (d *Daemon) postDecision(ctx context.Context, prNumber int, commentID, reviewRunFolder string, state *ReviewStateStore) error {
	decisionPath := d.reviewDecisionPath(prNumber, commentID)
	info, err := os.Stat(decisionPath)
	if err != nil {
		if os.IsNotExist(err) {
			d.logf("PR #%d: missing %s after RunBatch; marking pending for retry (issue #1949)", prNumber, decisionPath)
			if state != nil {
				if markErr := state.MarkSeen(commentID, "pending"); markErr != nil {
					d.logf("PR #%d: mark %s pending: %v", prNumber, commentID, markErr)
				}
			}
			// Issue #1949: the missing-decision.md outcome is
			// retryable, not terminal. The next tick's rehydrate
			// walker drops the stale pendingPost entry (the
			// source-of-truth gate at daemon.go rehydrateStale
			// fails because decision.md is still missing) and
			// falls through to the launch path. The seen cache
			// is NOT marked terminal-seen so the trigger stays
			// visible to subsequent ticks.
			d.registerPendingPost(prNumber, commentID, d.reviewWorktreePath(prNumber, commentID), reviewRunFolder)
			return fmt.Errorf("missing %s: %w", decisionPath, err)
		}
		return fmt.Errorf("stat %s: %w", decisionPath, err)
	}
	if info.IsDir() {
		d.logf("PR #%d: %s is a directory, not a file; marking pending for retry (issue #1949)", prNumber, decisionPath)
		if state != nil {
			if markErr := state.MarkSeen(commentID, "pending"); markErr != nil {
				d.logf("PR #%d: mark %s pending: %v", prNumber, commentID, markErr)
			}
		}
		d.registerPendingPost(prNumber, commentID, d.reviewWorktreePath(prNumber, commentID), reviewRunFolder)
		return fmt.Errorf("%s is a directory", decisionPath)
	}

	body, err := os.ReadFile(decisionPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", decisionPath, err)
	}

	// Issue #2224 slice 3a: persist decision.md to the run folder
	// before the launchReview defer fires ClearReviewArtifacts and
	// removes the worktree (which deletes the only copy of
	// decision.md). The run folder copy is what the portal's verdict
	// reader falls back to after the worktree is gone (slice 3b).
	// Use atomicfs.WriteAtomic so the portal never observes a
	// partially written file. If the write fails we log and
	// continue — the post step is the critical path, and the
	// worktree copy still exists at this point.
	if reviewRunFolder != "" {
		runDecisionPath := filepath.Join(reviewRunFolder, "decision.md")
		if writeErr := atomicfs.WriteAtomic(runDecisionPath, body, 0644); writeErr != nil {
			d.logf("PR #%d: persist decision.md to %s failed: %v (portal will still try the worktree copy until cleanup)", prNumber, runDecisionPath, writeErr)
		}
	}

	// Honour ctx cancellation observed between RunBatch returning
	// and the post step: do NOT call MarkSeen so the trigger
	// stays in the prior on-disk state (the bounded-retry escape
	// engages on a subsequent tick).
	if cerr := ctx.Err(); cerr != nil {
		d.logf("PR #%d: ctx cancelled before post; leaving status untouched (issue #1846)", prNumber)
		return cerr
	}

	redacted := RedactBody(string(body))
	postErr := postWithRetry(ctx, d, prNumber, redacted)
	if postErr != nil {
		if cerr := ctx.Err(); cerr != nil {
			d.logf("PR #%d: ctx cancelled during post; leaving status untouched (issue #1846)", prNumber)
			return cerr
		}
		// Post failed after the retry budget. Fall back to the
		// rehydrate path: write `pending` on disk and register a
		// pendingPost entry so the same-process next tick (or
		// the S4 rehydrate walker after a daemon restart) picks
		// the trigger up and re-attempts the post. The trigger is
		// NOT marked terminal-seen — failure here means "post did
		// not land", not "review is permanently lost". This
		// supersedes the S6 single-shot escape (issue #1849) for
		// the post-failure branch only; the missing-decision.md
		// and directory branches above still apply
		// MarkTerminalSeen because those represent "the agent did
		// not produce a review", not "the post could not land".
		d.logf("PR #%d: post failed after %d attempts (last err: %v); registering as pending for rehydrate (issue #1891)", prNumber, PostStepMaxAttempts, postErr)
		if state != nil {
			if markErr := state.MarkSeen(commentID, "pending"); markErr != nil {
				d.logf("PR #%d: mark %s pending: %v", prNumber, commentID, markErr)
			}
		}
		d.registerPendingPost(prNumber, commentID, d.reviewWorktreePath(prNumber, commentID), reviewRunFolder)
		return fmt.Errorf("post decision: %w", postErr)
	}

	if state != nil {
		if err := state.MarkSeen(commentID, "success"); err != nil {
			return fmt.Errorf("mark %s success: %w", commentID, err)
		}
	}
	return nil
}

// postWithRetry calls CommentPoster.PostComment up to
// PostStepMaxAttempts times. Between attempts it sleeps
// postStepBackoffs[attempt-1]. Returns the last error from
// PostComment on exhaustion, or ctx.Err() if the context is
// cancelled at any point. Issue #1891.
func postWithRetry(ctx context.Context, d *Daemon, prNumber int, body string) error {
	var lastErr error
	for attempt := 1; attempt <= PostStepMaxAttempts; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err := d.CommentPoster.PostComment(ctx, prNumber, body); err != nil {
			lastErr = err
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if attempt < PostStepMaxAttempts {
				backoff := d.effectivePostBackoffs()[attempt-1]
				d.logf("PR #%d: post attempt %d/%d failed: %v; retrying in %v", prNumber, attempt, PostStepMaxAttempts, err, backoff)
				select {
				case <-time.After(backoff):
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			continue
		}
		if attempt > 1 {
			d.logf("PR #%d: post succeeded on attempt %d/%d", prNumber, attempt, PostStepMaxAttempts)
		}
		return nil
	}
	return lastErr
}

// effectivePostBackoffs returns the backoff schedule used by
// postWithRetry. When d.postBackoffs is set (by tests), it wins;
// otherwise the package-level postStepBackoffs is used (production).
// Tests that want zero-cost retries must set a non-nil, non-empty
// slice (e.g. []time.Duration{0,0,0,0,0}) — a nil or empty slice
// falls back to the real 31s schedule.
func (d *Daemon) effectivePostBackoffs() []time.Duration {
	if len(d.postBackoffs) > 0 {
		return d.postBackoffs
	}
	return postStepBackoffs
}

// registerPendingPost registers (prNumber, commentID) in the
// in-memory pendingPost map so the same-process next tick re-runs
// tryRehydratePost (issue #1847 S4). The on-disk review-state.json
// must already carry status="pending" — postDecision writes it via
// state.MarkSeen just before calling this helper. Locking matches
// registerPendingPost records a rehydrate-eligible trigger under
// (prNumber, commentID). The worktree path is where decision.md
// lives (canonical artifact path, issue #1953); the run folder
// path is where review-state.json lives. Both must be recorded so
// the next tick's processPR can pick the trigger up after a
// daemon restart via loadPendingPosts, which only knows the
// review-state.json's PR number — the worktree path is derived
// deterministically from (prNumber, commentID) at read time.
func (d *Daemon) registerPendingPost(prNumber int, commentID, worktreePath, reviewRunFolder string) {
	d.pendingPostMu.Lock()
	defer d.pendingPostMu.Unlock()
	if _, ok := d.pendingPost[prNumber]; !ok {
		d.pendingPost[prNumber] = map[string]pendingPostEntry{}
	}
	d.pendingPost[prNumber][commentID] = pendingPostEntry{
		commentID:   commentID,
		runDir:      worktreePath,
		reviewState: d.ReviewStatePath(reviewRunFolder),
		since:       d.now(),
	}
}

// tryRehydratePost handles a rehydrate-eligible trigger discovered
// in pendingPost by processPR (issue #1847 S4). It returns true
// when the entry was registered AND decision.md is still on disk,
// signalling that processPR should NOT proceed to the launch path.
//
// Three branches:
//
//   - decision.md on disk, post succeeds: MarkSeen("success") is
//     persisted on the per-run ReviewStateStore (which fires the
//     seen-cache hook so the next tick's processPR short-circuits
//     the trigger), the pendingPost entry is dropped, and the
//     function returns true. No new agent run is launched.
//
//   - decision.md on disk, post fails (PostComment returns an
//     error): the entry is kept in pendingPost so the next tick
//     retries; MarkSeen is left untouched so the trigger stays
//     `pending` on disk. No new agent run is launched. Returns
//     true so processPR does not fall through to launch.
//
//   - decision.md missing at tick time (stale entry, e.g. an
//     operator removed the file or it was never written): the
//     entry is dropped from pendingPost and the function returns
//     false so processPR falls through to the existing launch
//     path. The launch path's prepareReviewRun / TryClaim /
//     launchReview cycle handles the trigger from scratch — the
//     rehydrate-on-startup walker has nothing to recover.
//
// tryRehydratePost holds the per-run ReviewStateStore open only
// for the MarkSeen call and never spawns a goroutine, so it can
// run inline in processPR.
func (d *Daemon) tryRehydratePost(ctx context.Context, prNumber int, comment github.PRComment) bool {
	d.pendingPostMu.Lock()
	m, ok := d.pendingPost[prNumber]
	if !ok {
		d.pendingPostMu.Unlock()
		return false
	}
	entry, ok := m[comment.ID]
	if !ok {
		d.pendingPostMu.Unlock()
		return false
	}
	d.pendingPostMu.Unlock()

	decisionPath := filepath.Join(entry.runDir, "decision.md")
	info, statErr := os.Stat(decisionPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			// Stale entry: drop and fall through to launch.
			d.logf("PR #%d comment %s: rehydrate entry stale, decision.md missing at tick time, falling through to launch (issue #1847)", prNumber, comment.ID)
			d.pendingPostMu.Lock()
			delete(d.pendingPost[prNumber], comment.ID)
			if len(d.pendingPost[prNumber]) == 0 {
				delete(d.pendingPost, prNumber)
			}
			d.pendingPostMu.Unlock()
			return false
		}
		// Read failed for some other reason (perm denied, IO).
		// Keep the entry; the next tick retries. Do NOT proceed
		// to launch because the existing post is still better
		// than re-running the agent.
		d.logf("PR #%d comment %s: rehydrate post stat %s failed: %v; keeping entry for retry (issue #1847)", prNumber, comment.ID, decisionPath, statErr)
		return true
	}
	if !info.Mode().IsRegular() {
		// Issue #1949: a directory at decision.md is treated as
		// missing. The launch path's ClearReviewArtifacts defer
		// will remove the worktree directory and the next
		// postDecision observes a clean slate. Keep the entry
		// drop-and-fallthrough to launch, mirroring the missing
		// branch.
		d.logf("PR #%d comment %s: rehydrate entry stale, decision.md is not a regular file at tick time, falling through to launch (issue #1949)", prNumber, comment.ID)
		d.pendingPostMu.Lock()
		delete(d.pendingPost[prNumber], comment.ID)
		if len(d.pendingPost[prNumber]) == 0 {
			delete(d.pendingPost, prNumber)
		}
		d.pendingPostMu.Unlock()
		return false
	}
	body, err := os.ReadFile(decisionPath)
	if err != nil {
		// Read failed for some other reason (perm denied, IO).
		// Keep the entry; the next tick retries. Do NOT proceed
		// to launch because the existing post is still better
		// than re-running the agent.
		d.logf("PR #%d comment %s: rehydrate post read %s failed: %v; keeping entry for retry (issue #1847)", prNumber, comment.ID, decisionPath, err)
		return true
	}

	// Honour ctx cancellation observed between ReadFile and the
	// post step: do NOT call MarkSeen so the trigger stays in
	// the prior on-disk state (the next daemon's rehydrate walker
	// picks the entry up again on restart).
	if cerr := ctx.Err(); cerr != nil {
		d.logf("PR #%d comment %s: ctx cancelled before rehydrate post; leaving entry untouched (issue #1847)", prNumber, comment.ID)
		return true
	}

	redacted := RedactBody(string(body))
	if err := d.CommentPoster.PostComment(ctx, prNumber, redacted); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			// Ctx cancelled DURING post: leave entry; the next
			// daemon's rehydrate walker re-attempts.
			d.logf("PR #%d comment %s: ctx cancelled during rehydrate post; leaving entry untouched (issue #1847)", prNumber, comment.ID)
			return true
		}
		// Post failed for a non-ctx reason: log, keep the entry
		// so the next tick retries. Do NOT MarkSeen; the
		// rehydrate entry is the source of truth and the
		// next tick re-attempts the post.
		d.logf("PR #%d comment %s: rehydrate post failed: %v; keeping entry for next-tick retry (issue #1847)", prNumber, comment.ID, err)
		return true
	}

	// Successful post. MarkSeen("success") on the per-run store
	// fires the SeenCacheInvalidator seam (production: d), which
	// MarkTerminalSeen's the (prNumber, commentID) pair in the
	// in-memory seenCache. The on-disk review-state.json gets
	// the success status updated atomically by ReviewStateStore.
	store, storeErr := NewReviewStateStore(entry.reviewState, prNumber, d)
	if storeErr != nil {
		// State store could not be opened: log and keep the entry
		// so the next tick retries — but DO call MarkTerminalSeen
		// on the seen cache so the trigger is not reprocessed as
		// a fresh launch in the interim (the entry acts as the
		// source of truth until the store can be opened).
		d.logf("PR #%d comment %s: open review-state for MarkSeen failed: %v; keeping entry and seen-cache untouched (issue #1847)", prNumber, comment.ID, storeErr)
		return true
	}
	if err := store.MarkSeen(comment.ID, "success"); err != nil {
		d.logf("PR #%d comment %s: MarkSeen(success) failed in rehydrate branch: %v; keeping entry (issue #1847)", prNumber, comment.ID, err)
		return true
	}

	// Drop the entry from pendingPost under the dedicated mutex.
	d.pendingPostMu.Lock()
	delete(d.pendingPost[prNumber], comment.ID)
	if len(d.pendingPost[prNumber]) == 0 {
		delete(d.pendingPost, prNumber)
	}
	d.pendingPostMu.Unlock()
	return true
}

// otherwise falls back to time.Now. Tests inject a custom clock.
func (d *Daemon) now() time.Time {
	if d.Clock != nil {
		return d.Clock()
	}
	return time.Now()
}

// recordLaunchFailure records the launch failure for commentID and
// returns the wrapped error so the caller can propagate it. The
// attempt counter from the foundation ticket (#2209) is read,
// incremented, and persisted alongside the per-trigger retry-budget
// stamp (issue #2211) via MarkSeenWithBudget so the gate in
// processPR can skip re-launches whose budget has not yet elapsed.
// The launch-failure backoff schedule (#2210) provides the
// duration; this ticket moves the gate from the launch goroutine
// to processPR so the per-PR slot is no longer held across the
// sleep. The seen-cache hook is NOT fired (failure is not in
// shouldSkipDedupStatus) so the S6 retryable contract is
// preserved — the next tick's processPR re-launches the trigger
// after the stamp elapses. Honours ctx cancellation observed
// before MarkSeen by leaving the status untouched (matching the
// "stays pending on cancellation" semantic pinned by issue
// #1846).
func (d *Daemon) recordLaunchFailure(ctx context.Context, commentID string, state *ReviewStateStore, cause error) error {
	if state == nil {
		return cause
	}
	if cerr := ctx.Err(); cerr != nil {
		d.logf("comment %s: ctx cancelled before launch failure recorded; leaving status untouched (issue #1846)", commentID)
		return cerr
	}
	attempts := ReadFailureAttempts(state, commentID) + 1
	backoff := d.effectiveLaunchBackoff(attempts)
	stamp := d.now().Add(backoff)
	if err := state.MarkSeenWithBudget(commentID, "failure", attempts, stamp); err != nil {
		d.logf("mark %s failure (attempts=%d, nextAttemptAt=%v): %v", commentID, attempts, stamp, err)
	}
	d.SetNextAttemptAt(state.PR(), commentID, stamp)
	return cause
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
