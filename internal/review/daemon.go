package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// PollingInterval is the default interval at which the daemon scans open PRs
// for /sandman review comments. It is exported so tests and the CLI can
// reference the same constant.
const PollingInterval = 30 * time.Second

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
		busy:         make(chan struct{}, cfg.EffectiveReviewParallel()),
	}
}

// PRDir returns the per-PR state directory path.
func (d *Daemon) PRDir(prNumber int) string {
	return filepath.Join(d.BaseDir, "reviews", strconv.Itoa(prNumber))
}

// SocketPath returns the absolute path of the daemon's control socket.
func (d *Daemon) SocketPath() string {
	return filepath.Join(d.BaseDir, "review.sock")
}

// SetSocket stores a pre-built ControlSocket on the daemon. The cmd layer
// uses this to share a Broadcaster-driven socket with attach; tests can
// also inject a custom socket.
func (d *Daemon) SetSocket(s *daemon.ControlSocket) {
	d.controlSocket = s
}

// StartSocket ensures the .sandman dir exists and starts the control
// socket. Safe to call multiple times.
func (d *Daemon) StartSocket() error {
	if err := os.MkdirAll(d.BaseDir, 0755); err != nil {
		return fmt.Errorf("create sandman dir: %w", err)
	}
	if d.controlSocket == nil {
		d.controlSocket = daemon.NewControlSocketWithName(d.BaseDir, "review.sock", daemon.NewBroadcaster())
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
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := d.processPR(ctx, pr.Number); err != nil {
				d.logf("process PR #%d: %v", pr.Number, err)
			}
		}()
	}
	wg.Wait()
	return nil
}

// processPR scans one PR's comments and launches a review agent for the
// newest unseen /sandman review trigger. A per-PR ClaimStore provides
// cross-process safety; SeenCommentsStore.TryClaim prevents intra-process
// races. Stale unseen triggers are marked as seen and skipped. Errors are
// logged but not returned.
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

	prDir := d.PRDir(prNumber)
	if err := os.MkdirAll(prDir, 0755); err != nil {
		return fmt.Errorf("create PR dir: %w", err)
	}
	store, err := NewSeenCommentsStore(prDir)
	if err != nil {
		return fmt.Errorf("open seen store: %w", err)
	}

	cs, err := NewClaimStore(prDir, time.Hour)
	if err != nil {
		return fmt.Errorf("create claim store: %w", err)
	}
	defer cs.Close()

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
		if !store.TryClaim(comment.ID) {
			continue
		}
		claimed, err := cs.TryClaim(comment.ID)
		if err != nil {
			d.logf("claim error for comment %s: %v", comment.ID, err)
			if err := store.Mark(comment.ID); err != nil {
				d.logf("mark claim-failed comment %s seen: %v", comment.ID, err)
			}
			continue
		}
		if !claimed {
			d.logf("comment %s already claimed, skipping", comment.ID)
			if err := store.Mark(comment.ID); err != nil {
				d.logf("mark already-claimed comment %s seen: %v", comment.ID, err)
			}
			continue
		}
		triggers = append(triggers, unseenTrigger{comment: comment, focus: focus})
	}
	if len(triggers) == 0 {
		return nil
	}

	newest := triggers[0]
	for i := 1; i < len(triggers); i++ {
		if triggers[i].comment.CreatedAt.After(newest.comment.CreatedAt) {
			newest = triggers[i]
		}
	}

	for _, t := range triggers {
		if t.comment.ID != newest.comment.ID {
			if err := store.Mark(t.comment.ID); err != nil {
				d.logf("mark stale comment %s seen: %v", t.comment.ID, err)
			}
			cs.Release(t.comment.ID)
			d.logf("skipping stale trigger comment %s (newer %s exists)", t.comment.ID, newest.comment.ID)
		}
	}

	comment := newest.comment
	focus := newest.focus
	commentReactionID, commentErr := d.GitHub.AddCommentReaction(comment.ID, "eyes")
	if commentErr != nil {
		d.logf("add reaction to comment %s: %v", comment.ID, commentErr)
	}
	prReactionID, prErr := d.GitHub.AddIssueReaction(prNumber, "eyes")
	if prErr != nil {
		d.logf("add reaction to PR #%d: %v", prNumber, prErr)
	}

	if err := d.launchReview(ctx, prNumber, prDir, focus, comment.ID, commentReactionID, prReactionID); err != nil {
		d.logf("launch review for PR #%d comment %s: %v", prNumber, comment.ID, err)
		return nil
	}
	if err := store.Mark(comment.ID); err != nil {
		d.logf("mark comment %s seen: %v", comment.ID, err)
	}
	cs.Release(comment.ID)
	return nil
}

// launchReview renders the review prompt and runs the batch. The PR
// metadata is re-fetched via the GitHub client so the prompt reflects
// the current title and body.
func (d *Daemon) launchReview(ctx context.Context, prNumber int, prDir, focus, commentID, commentReactionID, prReactionID string) error {
	defer func() {
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

	promptPath := filepath.Join(prDir, "pr-review-prompt.md")
	if err := os.WriteFile(promptPath, []byte(rendered), 0644); err != nil {
		return fmt.Errorf("write prompt file: %w", err)
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

	var reviewIssueNumber int
	if pr, err := d.GitHub.FetchPR(prNumber); err == nil {
		reviewIssueNumber = pr.LinkedIssueNumber()
	}

	runID := fmt.Sprintf("PR%d", prNumber)
	runDir := daemon.RunDir(d.BaseDir, nil, runID)

	broadcaster := daemon.NewBroadcaster()
	ctlSocket := daemon.NewControlSocket(runDir, broadcaster)
	if err := ctlSocket.Start(); err != nil {
		return fmt.Errorf("start control socket: %w", err)
	}
	defer os.RemoveAll(runDir)
	defer ctlSocket.Stop()

	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{}, CreatedAt: time.Now()}); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

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
		OutputWriter: d.Broadcaster,
		Review:       true,
		PRNumber:     prNumber,
		IssueNumber:  reviewIssueNumber,
		ReviewFocus:  focus,
		RunID:        runID,
		RunDir:       runDir,
	}
	if _, err := d.Runner.RunBatch(ctx, req); err != nil {
		return fmt.Errorf("run batch: %w", err)
	}
	return nil
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
