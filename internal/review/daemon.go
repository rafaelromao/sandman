package review

import (
	"context"
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
	focus       string
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
	controlSocket        *daemon.ControlSocket
	busy                 chan struct{}
	promptOnce           sync.Once
	seenCache            map[int]map[string]bool
	seenCacheMu          sync.RWMutex
	pendingMu            sync.Mutex
	pendingReviews       map[int][]pendingReviewEntry
}

// New returns a Daemon configured with the project defaults for the
// polling interval and clock. The seen cache is hydrated eagerly from
// the on-disk batches index (issue #1480 slice A). A missing or
// unreadable index yields an empty cache; the rename-loser trade-off
// from ADR-0034 §3 means a stale skip is acceptable.
func New(baseDir string, gh GitHubClient, prompts Renderer, runner BatchRunner, cfg *config.Config, broadcaster io.Writer) *Daemon {
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
		busy:           make(chan struct{}, 1),
		seenCache:      map[int]map[string]bool{},
		pendingReviews: map[int][]pendingReviewEntry{},
	}
	if err := d.loadSeenCache(); err != nil {
		d.logf("load seen cache: %v", err)
	}
	return d
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
// on-disk batches index and every review-state.json. Existing entries
// are replaced.
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
		runDir := filepath.Join(entry.Path, "runs", "review")
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

// InvalidateSeenCache forces a rebuild of the seen cache by re-running
// the on-disk scan. Callers use this when an out-of-band change to
// .sandman/batches.json or a review-state.json is observed (e.g. via
// fsnotify) or as a slow-tick recovery path.
func (d *Daemon) InvalidateSeenCache() error {
	return d.loadSeenCache()
}

// SocketPath returns the absolute path of the daemon's control socket.
// The socket lives under .sandman/reviews/ alongside the shared prompt
// template, so the daemon's on-disk footprint is just two files plus
// run folders under .sandman/batches/.
func (d *Daemon) SocketPath() string {
	return filepath.Join(d.BaseDir, "reviews", "review.sock")
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
// Acceptance criterion #3 from issue #1224: "review-state.json lives
// at <batch>/runs/<run>/review-state.json". For review runs the
// batch dir contains a single run folder <batch>/runs/<run>/, so
// callers pass that path; this helper joins the state filename.
func (d *Daemon) ReviewStatePath(runDir string) string {
	return filepath.Join(runDir, "review-state.json")
}

// reviewRunDir returns the per-run folder under a review batch
// (<batchDir>/runs/<runID>). The review daemon writes
// review-state.json here; the existing per-batch batch.sock stays at
// the batch root for the orchestrator's attach/streaming surface.
func reviewRunDir(batchDir string) string {
	return filepath.Join(batchDir, "runs", "review")
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
		if _, err := d.Config.ResolveAgentProvider(d.Config.EffectiveReviewAgent()); err != nil {
			d.logf("review agent validation failed: %v", err)
			return err
		}
		if strings.TrimSpace(d.Config.EffectiveReviewModel()) == "" {
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
	limit := 1
	if d.Config != nil {
		limit = d.Config.EffectiveReviewParallel()
	}
	if limit < 1 {
		limit = 1
	}

	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, pr := range prs {
		pr := pr
		wg.Add(1)
		go func() {
			defer wg.Done()
			canLaunch := true
			select {
			case sem <- struct{}{}:
			default:
				d.logf("PR #%d: parallel limit reached, will read comments without launching", pr.Number)
				canLaunch = false
			}
			defer func() {
				if canLaunch {
					<-sem
				}
			}()
			if err := d.processPR(ctx, pr.Number, canLaunch); err != nil {
				d.logf("process PR #%d: %v", pr.Number, err)
			}
		}()
	}
	wg.Wait()
	return nil
}

// processPR scans one PR's comments and launches a review agent for the
// newest unseen /sandman review trigger. When canLaunch is false, the
// function reads comments and identifies triggers but does not launch a
// review, add reactions, or mark the trigger as seen. This ensures that
// triggers are not dropped when the parallel limit is saturated.
//
// The dedup state lives in the run folder's `review-state.json`, managed
// by ReviewStateStore. No per-PR directory is created under `.sandman/reviews/`.
//
// Acceptance criteria #1 and #3 from issue #1224:
//   - No code path creates `.sandman/reviews/<PR>/`
//   - `review-state.json` lives at `<batch>/runs/<run>/review-state.json`
func (d *Daemon) processPR(ctx context.Context, prNumber int, canLaunch bool) error {
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

	if !canLaunch {
		d.logf("PR #%d: cannot launch (semaphore saturated), will retry on next tick", prNumber)
		return nil
	}

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
		d.registerPendingReview(prNumber, comment.ID, focus, d.now(), statePath, state)
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
func (d *Daemon) prepareReviewRun(prNumber int, commentID string) (string, string, *daemon.RunSession, *ReviewStateStore, error) {
	ts, shortid, err := runid.NewBatch()
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("generate batch ID: %w", err)
	}
	batchDirName := runid.NewBatchID(runid.KindReview, 1, fmt.Sprintf("%d", prNumber), ts, shortid)

	subject := fmt.Sprintf("PR%d", prNumber)
	perRowRunID := runid.NewRunID(runid.KindReview, subject, ts, shortid)

	rs := daemon.NewRunSession(d.BaseDir, batchDirName)
	manifest := daemon.BatchManifest{BatchId: batchDirName, CreatedAt: time.Now(), RunKind: "review", Issues: []int{}, PR: &prNumber}
	if err := rs.Prepare(manifest); err != nil {
		_ = rs.Close()
		return "", "", nil, nil, fmt.Errorf("prepare review run session: %w", err)
	}

	runDir := rs.RunDir()
	reviewRunFolder := reviewRunDir(runDir)
	if err := os.MkdirAll(reviewRunFolder, 0755); err != nil {
		_ = rs.Close()
		return "", "", nil, nil, fmt.Errorf("create review run folder: %w", err)
	}

	runManifest := batchindex.RunManifest{
		RunID:     "review",
		BatchID:   batchDirName,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := daemon.WriteRunManifest(runDir, "review", runManifest); err != nil {
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
		agentName = d.Config.EffectiveReviewAgent()
		modelName = d.Config.EffectiveReviewModel()
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
		ContainerCapacity:    d.ContainerCapacity,
		ContainerCapacitySet: d.ContainerCapacitySet,
		MaxContainers:        d.MaxContainers,
		MaxContainersSet:     d.MaxContainersSet,
		PromptConfig: prompt.RenderConfig{
			PromptFlag: rendered,
			Branch:     fmt.Sprintf("sandman/review-%d-%s", prNumber, commentID),
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
func (d *Daemon) registerPendingReview(prNumber int, commentID, focus string, since time.Time, statePath string, store *ReviewStateStore) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	d.pendingReviews[prNumber] = append(d.pendingReviews[prNumber], pendingReviewEntry{
		commentID:   commentID,
		focus:       focus,
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
