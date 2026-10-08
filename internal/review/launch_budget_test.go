package review

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/reviewlaunch"
	"github.com/rafaelromao/sandman/internal/testenv"
)

type launchBudgetGitHub struct{ *fakeGH }

func (g launchBudgetGitHub) FetchPR(_ context.Context, number int) (*github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := *g.prFetch[number]
	return &pr, nil
}

func writeObsoleteReviewerBudget(t *testing.T, root, trigger, head string, corrupt bool) {
	t.Helper()
	if head == "" {
		head = "unknown-head"
	}
	key := sha256.Sum256([]byte(trigger + "\x00" + strings.ToLower(head)))
	path := filepath.Join(root, "state", fmt.Sprintf("17.review-launch-%x.json", key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"protocol":"review-launch/v1","pull_request":17,"trigger":%q,"head_sha":%q,"attempts":3}`, trigger, head)
	if corrupt {
		data = "obsolete corrupt launch budget"
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReviewerLaunchRecoveryAcrossDaemonRestart(t *testing.T) {
	for _, failureKind := range []string{"launch", "missing-decision"} {
		for _, corrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/corrupt=%v", failureKind, corrupt), func(t *testing.T) {
				dir := testenv.MkdirShort(t, "sm-review-retry-")
				t.Chdir(dir)
				writeObsoleteReviewerBudget(t, dir, "request", "", corrupt)
				writeObsoleteReviewerBudget(t, dir, "request", "current-head", corrupt)
				gh := &fakeGH{
					prs:      []github.PR{{Number: 17, State: "open", HeadRefOid: "current-head"}},
					comments: map[int][]github.PRComment{17: {{ID: "request", Body: "/sandman review", CreatedAt: time.Now().UTC()}}},
					prFetch:  map[int]*github.PR{17: {Number: 17, State: "open", HeadRefOid: "current-head"}},
				}
				var calls, posts atomic.Int32
				runner := batchFunc(func(_ context.Context, req batch.Request) (*batch.Result, error) {
					if calls.Add(1) <= 5 {
						if failureKind == "launch" {
							return nil, errors.New("temporary reviewer launch failure")
						}
						return &batch.Result{}, nil
					}
					return &batch.Result{}, writeDecisionToWorktree(req, "APPROVED")
				})
				cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "opencode/foo", WorktreeDir: filepath.Join(dir, "worktrees")}
				for i := 0; i < 7; i++ {
					d := New(dir, launchBudgetGitHub{gh}, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, nil)
					d.launchBackoff = func(int) time.Duration { return 0 }
					d.CommentPoster = ownershipCommentPoster(func(_ context.Context, pr int, body string) error {
						count := posts.Add(1)
						gh.mu.Lock()
						gh.comments[pr] = append(gh.comments[pr], github.PRComment{ID: fmt.Sprintf("publication-%d", count), Body: body, CreatedAt: time.Now().UTC()})
						gh.mu.Unlock()
						return nil
					})
					tickAndWait(t, d, context.Background())
					if i < 5 && d.IsTerminalSeen(17, "request") {
						t.Fatal("retryable launch failure marked request terminal")
					}
				}
				if calls.Load() != 6 || posts.Load() != 1 {
					t.Fatalf("review never recovered or published twice: launches=%d posts=%d", calls.Load(), posts.Load())
				}
			})
		}
	}
}

type launchBudgetRepoFailure struct{ launchBudgetGitHub }

func (launchBudgetRepoFailure) RepoName(context.Context) (string, error) {
	return "", errors.New("repository lookup unavailable")
}

func TestReviewerPreparationFailuresRemainRetryable(t *testing.T) {
	for _, failure := range []string{"agent", "model", "repository"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			gh := launchBudgetGitHub{&fakeGH{prFetch: map[int]*github.PR{17: {Number: 17, HeadRefOid: "head"}}}}
			cfg := &config.Config{}
			if failure != "agent" {
				cfg.DefaultReviewAgent = "custom"
			}
			if failure == "repository" {
				cfg.DefaultReviewModel = "model"
			}
			runner := &failureRunner{err: errors.New("agent must not launch")}
			for attempt := 1; attempt <= 4; attempt++ {
				d := New(root, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, nil)
				if failure == "repository" {
					d.GitHub = launchBudgetRepoFailure{gh}
				}
				state, err := NewReviewStateStore(filepath.Join(root, "review-state.json"), 17, nil)
				if err != nil {
					t.Fatal(err)
				}
				err = d.launchReview(context.Background(), 17, "", "request", "", "", "", "", nil, state, false)
				if err == nil || strings.Contains(err.Error(), "REVIEW_LAUNCH_EXHAUSTED") {
					t.Fatalf("attempt=%d error=%v", attempt, err)
				}
				if got := ReadFailureAttempts(state, "request"); got != attempt {
					t.Fatalf("failure %s attempt diagnostics=%d, want %d", failure, got, attempt)
				}
			}
			if runner.calls.Load() != 0 {
				t.Fatal("preparation failure launched agent")
			}
		})
	}
}

type launchBudgetLookupFailure struct {
	launchBudgetGitHub
	calls *atomic.Int32
	empty bool
}

func (g launchBudgetLookupFailure) FetchPR(context.Context, int) (*github.PR, error) {
	g.calls.Add(1)
	if g.empty {
		return nil, nil
	}
	return nil, errors.New("PR lookup unavailable")
}

func TestReviewerPRLookupFailuresRemainRetryableAcrossRestart(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%v", empty), func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			var calls atomic.Int32
			gh := launchBudgetLookupFailure{launchBudgetGitHub: launchBudgetGitHub{&fakeGH{}}, calls: &calls, empty: empty}
			runner := &failureRunner{err: errors.New("agent must not launch")}
			for attempt := 1; attempt <= 5; attempt++ {
				state, err := NewReviewStateStore(filepath.Join(root, fmt.Sprintf("review-state-%d.json", attempt)), 17, nil)
				if err != nil || !state.TryClaim("request") {
					t.Fatalf("re-entry could not claim trigger: %v", err)
				}
				d := New(root, gh, &prompt.Engine{}, runner, &config.Config{}, &lockedBuffer{}, 1, true, nil)
				err = d.launchReview(context.Background(), 17, "", "request", "", "", "", "", nil, state, false)
				if err == nil || strings.Contains(err.Error(), "REVIEW_LAUNCH_EXHAUSTED") {
					t.Fatalf("restart=%d error=%v", attempt, err)
				}
				if state.IsClaimed("request") {
					t.Fatal("lookup failure retained trigger claim")
				}
				if got := ReadFailureAttempts(state, "request"); got != 1 {
					t.Fatalf("unknown-head failure diagnostics=%d, want one per reconstructed run", got)
				}
			}
			if calls.Load() != 5 || runner.calls.Load() != 0 {
				t.Fatalf("lookup calls=%d agent launches=%d, want five retryable lookups and no launch", calls.Load(), runner.calls.Load())
			}
		})
	}
}

type ownershipCommentPoster func(context.Context, int, string) error

func (f ownershipCommentPoster) PostComment(ctx context.Context, pr int, body string) error {
	return f(ctx, pr, body)
}

func TestReviewerRequestArtifactsStayExclusiveAcrossHeads(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-review-head-")
	t.Chdir(root)
	initReviewTestGitRepo(t, root)
	base := filepath.Join(root, ".sandman")
	cfg := &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model", WorktreeDir: filepath.Join(base, "worktrees")}
	gh := &fakeGH{prFetch: map[int]*github.PR{17: {Number: 17, HeadRefOid: "H1"}}}
	started, release := make(chan struct{}), make(chan struct{})
	var calls, posts atomic.Int32
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		calls.Add(1)
		path := filepath.Join(cfg.WorktreeDir, req.PromptConfig.Branch, "decision.md")
		if err := os.WriteFile(path, []byte("H1 decision"), 0o600); err != nil {
			return nil, err
		}
		close(started)
		select {
		case <-release:
			return &batch.Result{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	d := New(base, launchBudgetGitHub{gh}, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, nil)
	d.CommentPoster = ownershipCommentPoster(func(context.Context, int, string) error {
		posts.Add(1)
		claim, err := reviewlaunch.ClaimLaunch(filepath.Join(base, "state"), 17, "request", "")
		if claim != nil {
			_ = claim.Close()
		}
		if !errors.Is(err, reviewlaunch.ErrLaunchOwned) {
			return fmt.Errorf("publication lost request ownership: %v", err)
		}
		return nil
	})
	branch := reviewBranchName(17, "request")
	if err := os.MkdirAll(cfg.WorktreeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stageReviewWorktree(t, cfg.WorktreeDir, branch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	folder, runID, session, state, err := d.prepareReviewRun(ctx, 17, "request")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.launchReview(ctx, 17, "", "request", "", "", folder, runID, session, state, false) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("H1 reviewer did not start")
	}
	gh.mu.Lock()
	gh.prFetch[17] = &github.PR{Number: 17, HeadRefOid: "H2"}
	gh.mu.Unlock()
	second := New(base, launchBudgetGitHub{gh}, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, nil)
	secondState, err := NewReviewStateStore(filepath.Join(base, "second-state.json"), 17, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = second.launchReview(ctx, 17, "", "request", "", "", "", "", nil, secondState, false)
	if !errors.Is(err, reviewlaunch.ErrLaunchOwned) || calls.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("H2 overlapped H1 artifacts: error=%v launches=%d posts=%d", err, calls.Load(), posts.Load())
	}
	data, err := os.ReadFile(filepath.Join(cfg.WorktreeDir, branch, "decision.md"))
	if err != nil || string(data) != "H1 decision" || !gitWorktreeHasBranch(t, cfg.WorktreeDir, branch) {
		t.Fatalf("losing owner revised/deleted H1 artifacts: decision=%q error=%v", data, err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("H1 publication/cleanup did not finish")
	}
	if posts.Load() != 1 || gitWorktreeHasBranch(t, cfg.WorktreeDir, branch) {
		t.Fatal("owner did not finish publication and cleanup")
	}
	claim, err := reviewlaunch.ClaimLaunch(filepath.Join(base, "state"), 17, "request", "")
	if err != nil {
		t.Fatalf("request artifacts not released after cleanup: %v", err)
	}
	_ = claim.Close()
}
