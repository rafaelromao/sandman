package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
const PollingInterval = 60 * time.Second

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
	EditComment(commentID, body string) error
	EditPRBody(prNumber int, body string) error
}

// BatchRunner is the subset of batch.Runner used by the review daemon.
type BatchRunner interface {
	RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error)
}

// PromptRenderer renders review prompts.
type PromptRenderer interface {
	RenderReview(cfg prompt.RenderConfig, data prompt.PRData) (string, error)
}

// Daemon polls the repo for /sandman review comments and launches review
// agents serially.
type Daemon struct {
	BaseDir       string
	GitHub        GitHubClient
	Prompts       PromptRenderer
	Runner        BatchRunner
	Config        *config.Config
	Broadcaster   io.Writer
	Clock         Clock
	Trigger       Trigger
	PollInterval  time.Duration
	controlSocket *daemon.ControlSocket
	busy          chan struct{}
}

// New returns a Daemon configured with the project defaults for the
// polling interval and clock.
func New(baseDir string, gh GitHubClient, prompts PromptRenderer, runner BatchRunner, cfg *config.Config, broadcaster io.Writer) *Daemon {
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
	for _, pr := range prs {
		if err := d.processPR(ctx, pr.Number); err != nil {
			d.logf("process PR #%d: %v", pr.Number, err)
			continue
		}
	}
	return nil
}

// processPR scans one PR's comments and launches a review agent for each
// unseen /sandman review trigger. Errors fetching comments or running
// the batch are logged but not returned; the loop continues.
func (d *Daemon) processPR(ctx context.Context, prNumber int) error {
	comments, err := d.GitHub.ListPRComments(prNumber)
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	if len(comments) == 0 {
		return nil
	}

	prDir := d.PRDir(prNumber)
	if err := os.MkdirAll(prDir, 0755); err != nil {
		return fmt.Errorf("create PR dir: %w", err)
	}
	store, err := NewSeenCommentsStore(prDir)
	if err != nil {
		return fmt.Errorf("open seen store: %w", err)
	}

	for _, comment := range comments {
		focus, ok := ParseTrigger(comment.Body)
		if !ok {
			continue
		}
		if store.Has(comment.ID) {
			continue
		}

		// Signal that this trigger comment is being processed.
		if err := d.GitHub.EditComment(comment.ID, eyePrefix+comment.Body); err != nil {
			d.logf("add emoji to comment %s: %v", comment.ID, err)
		}
		pr, fetchErr := d.GitHub.FetchPR(prNumber)
		if fetchErr == nil {
			if err := d.GitHub.EditPRBody(prNumber, eyePrefix+pr.Body); err != nil {
				d.logf("add emoji to PR #%d body: %v", prNumber, err)
			}
		} else {
			d.logf("fetch PR #%d for emoji prepend: %v", prNumber, fetchErr)
		}

		if err := d.launchReview(ctx, prNumber, prDir, focus, comment.ID); err != nil {
			d.logf("launch review for PR #%d comment %s: %v", prNumber, comment.ID, err)
			continue
		}
		if err := store.Mark(comment.ID); err != nil {
			d.logf("mark comment %s seen: %v", comment.ID, err)
		}
	}
	return nil
}

// eyePrefix is the emoji prefix added to trigger comments and PR bodies
// to signal that a review is in progress.
const eyePrefix = "👁️ "

// stripEye removes the eyePrefix prefix from s, returning the original
// string if the prefix is not present.
func stripEye(s string) string {
	cleaned, _ := strings.CutPrefix(s, eyePrefix)
	return cleaned
}

// launchReview renders the review prompt and runs the batch. The PR
// metadata is re-fetched via the GitHub client so the prompt reflects
// the current title and body.
func (d *Daemon) launchReview(ctx context.Context, prNumber int, prDir, focus, commentID string) error {
	defer func() {
		// Strip the eye signal from the PR body after the review
		// finishes, unconditionally.
		if pr, err := d.GitHub.FetchPR(prNumber); err == nil {
			if cleaned := stripEye(pr.Body); cleaned != pr.Body {
				if err := d.GitHub.EditPRBody(prNumber, cleaned); err != nil {
					d.logf("remove emoji from PR #%d body: %v", prNumber, err)
				}
			}
		} else {
			d.logf("fetch PR #%d for emoji cleanup: %v", prNumber, err)
		}
		// Strip the eye signal from the trigger comment body.
		if comments, err := d.GitHub.ListPRComments(prNumber); err == nil {
			for _, c := range comments {
				if c.ID == commentID {
					if cleaned := stripEye(c.Body); cleaned != c.Body {
						if err := d.GitHub.EditComment(commentID, cleaned); err != nil {
							d.logf("remove emoji from comment %s: %v", commentID, err)
						}
					}
					break
				}
			}
		} else {
			d.logf("list comments for emoji cleanup: %v", err)
		}
	}()

	pr, err := d.GitHub.FetchPR(prNumber)
	if err != nil {
		return fmt.Errorf("fetch PR: %w", err)
	}

	rendered, err := d.Prompts.RenderReview(prompt.RenderConfig{}, prompt.PRData{
		Number:      pr.Number,
		Title:       pr.Title,
		Body:        stripEye(pr.Body),
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
	sandboxMode := "worktree"
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
		Agent:   agentName,
		Model:   modelName,
		Sandbox: sandboxMode,
		PromptConfig: prompt.RenderConfig{
			PromptFlag: rendered,
			Branch:     fmt.Sprintf("sandman/review-%d-%s", prNumber, commentID),
		},
		OutputWriter: d.Broadcaster,
		Review:       true,
		PRNumber:     prNumber,
		ReviewFocus:  focus,
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
