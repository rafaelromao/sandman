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

// verifyRetryMax is the maximum number of attempts for review comment
// verification, accounting for GitHub API eventual consistency.
const verifyRetryMax = 3

// verifyRetryBackoff is the delay between verification retry attempts.
const verifyRetryBackoff = 5 * time.Second

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
}

// New returns a Daemon configured with the project defaults for the
// polling interval and clock.
func New(baseDir string, gh GitHubClient, prompts Renderer, runner BatchRunner, cfg *config.Config, broadcaster io.Writer) *Daemon {
	return &Daemon{
		BaseDir:      baseDir,
		GitHub:       gh,
		Prompts:      prompts,
		Runner:       runner,
		Config:       cfg,
		Broadcaster:  broadcaster,
		Clock:        time.Now,
		Trigger:      nil,
		PollInterval: PollingInterval,
		busy:         make(chan struct{}, 1),
	}
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

	// Cross-run dedup: load any prior review-state.json files for this
	// PR from the batches index. A (pr, commentID) pair that is
	// terminal-seen in any prior run is skipped. ADR-0034 §3 accepts
	// the rename-loser trade-off; this scan only catches what is
	// already persisted on disk when the scan starts.
	globalSeen, err := d.loadGlobalSeenForPR(prNumber)
	if err != nil {
		d.logf("load global seen for PR %d: %v", prNumber, err)
	}

	var unprocessed []unseenTrigger
	for _, t := range triggers {
		if globalSeen[t.comment.ID] {
			d.logf("comment %s already terminal-seen, skipping", t.comment.ID)
			continue
		}
		unprocessed = append(unprocessed, t)
	}
	if len(unprocessed) == 0 {
		return nil
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
		if err := state.MarkSeen(comment.ID, "success"); err != nil {
			d.logf("mark comment %s seen: %v", comment.ID, err)
		}
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

// loadGlobalSeenForPR scans every review-state.json on disk and returns
// the set of (prNumber, commentID) pairs that are terminal-seen for the
// given PR. It is the slice-4 wiring: cross-run dedup via the batches
// index. On a missing index (ENOENT), batchindex.Load returns an empty
// index and dedup gracefully degrades to "nothing seen." On a corrupt or
// unreadable index, the error is logged and the caller continues with an
// empty set — this is acceptable per ADR-0034 §3 (rename-loser
// re-processes).
//
// Terminal-status deviation from PRD 1218: PRD 1218 lists only
// success/failure/aborted as terminal statuses. The global-dedup skip
// rule only treats "success" as terminal (failed triggers are retried on
// the next tick). "superseded" is also treated as terminal since an
// obsolete trigger should not be re-processed even though its run did not
// succeed. This deviation is intentional.
func (d *Daemon) loadGlobalSeenForPR(prNumber int) (map[string]bool, error) {
	seen := map[string]bool{}
	indexPath := daemon.BatchesIndexPath(d.BaseDir)
	idx, err := batchindex.Load(indexPath)
	if err != nil {
		return nil, fmt.Errorf("load batches index: %w", err)
	}

	for _, entry := range idx.Entries {
		if entry.Kind != batchindex.KindReview || entry.PR != prNumber {
			continue
		}
		// Look for a review-state.json at <entry.Path>/runs/review/review-state.json.
		statePath := filepath.Join(entry.Path, "runs", "review", "review-state.json")
		state, err := batchindex.ReadReviewState(filepath.Join(entry.Path, "runs", "review"))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			d.logf("read review state %s: %v", statePath, err)
			continue
		}
		for _, sc := range state.SeenComments {
			if shouldSkipDedupStatus(sc.Status) {
				seen[sc.CommentID] = true
			}
		}
	}
	return seen, nil
}

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
func shouldSkipDedupStatus(status string) bool {
	return status == "success" || status == "superseded"
}

// prepareReviewRun creates the run folder and state store for a new review
// run. It is called by processPR before TryClaim so that the claim can be
// persisted to the state file before launchReview is called. The returned
// *daemon.RunSession must be passed to launchReview; processPR does not
// close it.
func (d *Daemon) prepareReviewRun(prNumber int, commentID string) (string, string, *daemon.RunSession, *ReviewStateStore, error) {
	ts, shortid, err := runid.NewBatch()
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("generate batch ID: %w", err)
	}
	batchDirName := runid.NewBatchID(runid.KindReview, 1, fmt.Sprintf("%d", prNumber), ts, shortid)

	var reviewIssueNumber int
	if pr, err := d.GitHub.FetchPR(prNumber); err == nil {
		reviewIssueNumber = pr.LinkedIssueNumber()
	}

	var subject string
	if reviewIssueNumber > 0 {
		subject = fmt.Sprintf("%d-PR%d", reviewIssueNumber, prNumber)
	} else {
		subject = fmt.Sprintf("PR%d", prNumber)
	}
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

	state, err := NewReviewStateStore(d.ReviewStatePath(reviewRunFolder), prNumber)
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
	verifyStart := d.now()
	if _, err := d.Runner.RunBatch(ctx, req); err != nil {
		return fmt.Errorf("run batch: %w", err)
	}
	if err := d.verifyReviewPosted(ctx, prNumber, verifyStart, commentID); err != nil {
		return fmt.Errorf("review verification: %w", err)
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

// verifyReviewPosted checks whether a new PR comment was posted after the
// given timestamp, excluding the trigger comment. It retries up to 3
// times with 5-second backoff to handle GitHub API eventual consistency.
func (d *Daemon) verifyReviewPosted(ctx context.Context, prNumber int, since time.Time, excludeCommentID string) error {
	var lastErr error
	for attempt := 0; attempt < verifyRetryMax; attempt++ {
		if attempt > 0 {
			d.logf("PR #%d: review verification attempt %d/%d, retrying in %v", prNumber, attempt+1, verifyRetryMax, verifyRetryBackoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(verifyRetryBackoff):
			}
		}

		comments, err := d.GitHub.ListPRComments(prNumber)
		if err != nil {
			lastErr = fmt.Errorf("list PR comments: %w", err)
			d.logf("PR #%d: review verification: %v", prNumber, lastErr)
			continue
		}

		for _, c := range comments {
			if c.ID == excludeCommentID {
				continue
			}
			if c.CreatedAt.After(since) && c.Body != "" {
				d.logf("PR #%d: review comment verified (ID %s, posted at %v)", prNumber, c.ID, c.CreatedAt)
				return nil
			}
		}
		lastErr = fmt.Errorf("no review comment found on PR #%d after %v", prNumber, since)
	}

	d.logf("PR #%d: review verification failed after %d attempts: %v", prNumber, verifyRetryMax, lastErr)
	return lastErr
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
