package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// reactionCall records a single reaction method invocation.
type reactionCall struct {
	kind        string // "add_comment", "add_issue", "remove_comment", "remove_issue"
	commentID   string
	issueNumber int
	content     string
	reactionID  string
}

// fakeGH is a test double for the GitHubClient surface area used by the
// review daemon.
type fakeGH struct {
	mu                    sync.Mutex
	prs                   []github.PR
	comments              map[int][]github.PRComment
	prFetch               map[int]*github.PR
	listErr               error
	commentErr            map[int]error
	fetchErr              map[int]error
	authenticatedLogin    string
	authenticatedLoginErr error
	listCalls             int
	commentCalls          map[int]int // tracks ListPRComments calls per PR number
	reactionCalls         []reactionCall
	addReactionID         int // auto-increment for fake reaction IDs
}

func (f *fakeGH) AuthenticatedLogin(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authenticatedLogin == "" && f.authenticatedLoginErr == nil {
		return "sandman", nil
	}
	return f.authenticatedLogin, f.authenticatedLoginErr
}

func (f *fakeGH) ListOpenPRs(ctx context.Context) ([]github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.prs, f.listErr
}

func (f *fakeGH) ListPRComments(ctx context.Context, number int) ([]github.PRComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentCalls == nil {
		f.commentCalls = make(map[int]int)
	}
	f.commentCalls[number]++
	if f.commentErr != nil {
		if err, ok := f.commentErr[number]; ok {
			return nil, err
		}
	}
	comments := f.comments[number]
	result := make([]github.PRComment, len(comments))
	copy(result, comments)
	return result, nil
}

func (f *fakeGH) FetchPR(ctx context.Context, number int) (*github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil {
		if err, ok := f.fetchErr[number]; ok {
			return nil, err
		}
	}
	body := "B"
	if pr, ok := f.prFetch[number]; ok {
		body = pr.Body
		return &github.PR{Number: pr.Number, Title: pr.Title, Body: body}, nil
	}
	return &github.PR{Number: number, Title: "T", Body: body}, nil
}

func (f *fakeGH) RepoName(ctx context.Context) (string, error) {
	return "owner/repo", nil
}

func (f *fakeGH) AddCommentReaction(ctx context.Context, commentID, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addReactionID++
	id := fmt.Sprintf("react-%d", f.addReactionID)
	f.reactionCalls = append(f.reactionCalls, reactionCall{kind: "add_comment", commentID: commentID, content: content, reactionID: id})
	return id, nil
}

func (f *fakeGH) AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addReactionID++
	id := fmt.Sprintf("react-%d", f.addReactionID)
	f.reactionCalls = append(f.reactionCalls, reactionCall{kind: "add_issue", issueNumber: issueNumber, content: content, reactionID: id})
	return id, nil
}

func (f *fakeGH) RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactionCalls = append(f.reactionCalls, reactionCall{kind: "remove_comment", commentID: commentID, reactionID: reactionID})
	return nil
}

func (f *fakeGH) RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactionCalls = append(f.reactionCalls, reactionCall{kind: "remove_issue", issueNumber: issueNumber, reactionID: reactionID})
	return nil
}

type capturedRequest struct {
	calls int
	mu    sync.Mutex
	last  batch.Request
}

func (c *capturedRequest) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.last = req
	return &batch.Result{}, nil
}

// Calls returns the captured call count under the lock so test goroutines
// observe the RunBatch writer's increment reliably on every platform
// (Linux's memory model tolerated an unlocked read; macOS exposes the
// race as a persistent zero — see TestDaemon_RestartRecoversPendingFromDisk).
func (c *capturedRequest) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// lockedBuffer is a goroutine-safe wrapper around bytes.Buffer used as the
// Daemon.Broadcaster fixture in tests. The daemon writes from a background
// goroutine while the test reads buf.String() to assert on log output;
// bytes.Buffer is not safe for concurrent use, and wiring a raw
// *bytes.Buffer here regresses issue #1034 under `go test -race`.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.buf.Bytes()...)
}

// decisionCapturingRunner wraps a capturedRequest so the fake agent
// writes a deterministic <worktree>/decision.md before RunBatch
// returns. Issue #1846 changed launchReview so a review run is not
// considered "complete" until decision.md is read and posted; this
// shim lets the pre-S3 test fixtures (slice C, slice D, daemon_test)
// continue to drive the launch path without each test having to
// fabricate the file inline.
//
// Issue #1953: decision.md lives in the per-row worktree, not the
// run folder. The worktree path is derived from the branch the
// prompt ran under (req.PromptConfig.Branch) and the worktree base
// directory the daemon computes (cfg.WorktreeDir, defaulting to
// ".sandman/worktrees" to match cmd/review.go's production wiring).
type decisionCapturingRunner struct {
	*capturedRequest
	body        string
	worktreeDir string
}

func (d *decisionCapturingRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	worktreeDir := req.WorktreeDir
	if worktreeDir == "" {
		if d.worktreeDir != "" {
			worktreeDir = d.worktreeDir
		} else {
			worktreeDir = filepath.Join(".sandman", "worktrees")
		}
	}
	worktreePath := filepath.Join(worktreeDir, req.PromptConfig.Branch)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		return nil, fmt.Errorf("mkdir worktree: %w", err)
	}
	path := filepath.Join(worktreePath, "decision.md")
	if err := os.WriteFile(path, []byte(d.body), 0644); err != nil {
		return nil, fmt.Errorf("write decision.md: %w", err)
	}
	return d.capturedRequest.RunBatch(ctx, req)
}

func newDecisionRunner() *decisionCapturingRunner {
	return &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: "ok", worktreeDir: ".sandman/worktrees"}
}

func newDaemonForTest(t *testing.T, gh GitHubClient, runner BatchRunner, cfg *config.Config) (*Daemon, *lockedBuffer, string) {
	t.Helper()
	dir := testenv.MkdirShort(t, "sm-review-")
	t.Chdir(dir)
	buf := &lockedBuffer{}
	d := New(dir, gh, &prompt.Engine{}, runner, cfg, buf, 0, false, nil)
	d.PollInterval = 0
	d.postBackoffs = []time.Duration{0, 0, 0, 0, 0}
	d.launchBackoff = func(int) time.Duration { return 0 } // issue #2210 zero-cost seam
	return d, buf, dir
}

// tickAndWait runs tick then waits for all background review goroutines to
// complete (slotHeldCount == 0). Use in tests that assert on runner state
// or filesystem artifacts after tick — tick now returns before RunBatch
// finishes because reviews launch asynchronously.
func tickAndWait(t *testing.T, d *Daemon, ctx context.Context) {
	t.Helper()
	if err := d.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
}

func newDaemonForTestWithParallel(t *testing.T, gh GitHubClient, runner BatchRunner, cfg *config.Config, parallel int, parallelSet bool) (*Daemon, *lockedBuffer, string) {
	t.Helper()
	dir := testenv.MkdirShort(t, "sm-review-")
	t.Chdir(dir)
	buf := &lockedBuffer{}
	d := New(dir, gh, &prompt.Engine{}, runner, cfg, buf, parallel, parallelSet, nil)
	d.PollInterval = 0
	d.postBackoffs = []time.Duration{0, 0, 0, 0, 0}
	d.launchBackoff = func(int) time.Duration { return 0 } // issue #2210 zero-cost seam
	return d, buf, dir
}

// TestDaemon_ProcessPRCommentsSortedByCreatedAt verifies that only the
// newest unseen trigger is processed when multiple trigger comments exist
// with different creation times.
func TestDaemon_ProcessPRCommentsSortedByCreatedAt(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "102", Body: "/sandman review", CreatedAt: now.Add(2 * time.Hour)},
				{ID: "101", Body: "/sandman review", CreatedAt: now.Add(1 * time.Hour)},
				{ID: "103", Body: "/sandman review", CreatedAt: now.Add(3 * time.Hour)},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run (only newest trigger), got %d", runner.calls)
	}

	// Reactions should be added only for the newest comment (103).
	gh.mu.Lock()
	defer gh.mu.Unlock()
	for _, c := range gh.reactionCalls {
		if c.kind == "add_comment" {
			if c.commentID != "103" {
				t.Errorf("expected AddCommentReaction only for newest comment 103, got comment %s", c.commentID)
			}
		}
	}
}

func TestDaemon_OnlyNewestUnseenTriggerProcessed(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "102", Body: "/sandman review", CreatedAt: now.Add(2 * time.Hour)},
				{ID: "101", Body: "/sandman review", CreatedAt: now.Add(1 * time.Hour)},
				{ID: "103", Body: "/sandman review focus on x", CreatedAt: now.Add(3 * time.Hour)},
				{ID: "review-reply", Body: "## Summary\nApproved", CreatedAt: now.Add(4 * time.Hour)},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run (only newest trigger), got %d", runner.calls)
	}

	// The launched review should have the newest comment's focus ("focus on x" for ID "103").
	if runner.last.ReviewFocus != "focus on x" {
		t.Errorf("expected review focus from newest trigger (103), got %q", runner.last.ReviewFocus)
	}

	// Only the newest trigger (103) should get reactions; stale triggers (101, 102) get none.
	gh.mu.Lock()
	for _, c := range gh.reactionCalls {
		if c.kind == "add_comment" || c.kind == "remove_comment" {
			if c.commentID != "103" {
				t.Errorf("expected reactions only for newest comment 103, got reaction on comment %s", c.commentID)
			}
		}
	}
	gh.mu.Unlock()

	// Per-run review state lives at <runDir>/review-state.json. After
	// the tick, the launched run folder is captured by the runner. The
	// superseded comments (101, 102) and the trigger (103) should all
	// be terminal-seen in that file.
	statePath := filepath.Join(runner.last.RunDir, "review-state.json")
	state, err := batchindex.ReadReviewState(runner.last.RunDir)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	if state.PR != 1 {
		t.Errorf("review-state PR = %d, want 1", state.PR)
	}
	seenSet := map[string]bool{}
	for _, sc := range state.SeenComments {
		seenSet[sc.CommentID] = true
	}
	for _, id := range []string{"101", "102", "103"} {
		if !seenSet[id] {
			t.Errorf("comment %s should be marked as seen, got %+v (file: %s)", id, state.SeenComments, statePath)
		}
	}
	// Stale ones recorded as superseded, not success.
	supersededCount := 0
	for _, sc := range state.SeenComments {
		if sc.CommentID == "101" || sc.CommentID == "102" {
			supersededCount++
		}
	}
	if supersededCount != 2 {
		t.Errorf("expected 2 superseded comments (101, 102), got %d", supersededCount)
	}
}

func TestDaemon_TickLaunchesReviewForTriggerComment(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review focus on tests", CreatedAt: now},
				{ID: "101", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "PR 42", Body: "Body of 42"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		Sandbox:            "podman",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	if runner.last.Agent != "opencode" {
		t.Errorf("expected review agent 'opencode', got %q", runner.last.Agent)
	}
	if runner.last.Sandbox != "podman" {
		t.Errorf("expected sandbox 'podman', got %q", runner.last.Sandbox)
	}
	if !strings.Contains(runner.last.PromptConfig.PromptFlag, "PR 42") {
		t.Errorf("rendered prompt should mention PR title, got: %q", runner.last.PromptConfig.PromptFlag)
	}
	if !strings.Contains(runner.last.PromptConfig.PromptFlag, "focus on tests") {
		t.Errorf("rendered prompt should contain focus, got: %q", runner.last.PromptConfig.PromptFlag)
	}
	if !runner.last.Review {
		t.Errorf("expected Review=true on daemon review batch request, got false")
	}
	if runner.last.PRNumber != 42 {
		t.Errorf("expected PRNumber=42 on daemon review batch request, got %d", runner.last.PRNumber)
	}
	if runner.last.ReviewFocus != "focus on tests" {
		t.Errorf("expected ReviewFocus='focus on tests', got %q", runner.last.ReviewFocus)
	}
	if !strings.HasSuffix(runner.last.RunID, "-PR42") {
		t.Errorf("expected RunID to end with '-PR42', got %q", runner.last.RunID)
	}
	wantRunDirPrefix := filepath.Join(d.BaseDir, "batches", "")
	if !strings.HasPrefix(runner.last.RunDir, wantRunDirPrefix) {
		t.Errorf("expected RunDir to start with %q, got %q", wantRunDirPrefix, runner.last.RunDir)
	}

	// Per-run review state lives at <runDir>/review-state.json and
	// records the (pr, commentID) pair as terminal-seen after a
	// successful launch.
	state, err := batchindex.ReadReviewState(runner.last.RunDir)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	foundSeen := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "100" {
			foundSeen = true
			break
		}
	}
	if !foundSeen {
		t.Errorf("review-state should record comment 100 as seen, got %+v", state.SeenComments)
	}
}

func TestDaemon_TickLaunchesReviewsInParallel(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	after := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}, {Number: 2, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
				{ID: "101", Body: "## Summary\nLGTM", CreatedAt: after},
			},
			2: {
				{ID: "200", Body: "/sandman review", CreatedAt: now},
				{ID: "201", Body: "## Summary\nLGTM", CreatedAt: after},
			},
		},
		prFetch: map[int]*github.PR{
			1: {Number: 1, Title: "PR 1", Body: "Body 1"},
			2: {Number: 2, Title: "PR 2", Body: "Body 2"},
		},
	}
	started := make(chan int, 2)
	release := make(chan struct{})
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		started <- req.PRNumber
		<-release
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 2,
	})
	d.Clock = func() time.Time { return now }

	done := make(chan error, 1)
	go func() {
		done <- d.tick(context.Background())
	}()

	seen := map[int]struct{}{}
	for len(seen) < 2 {
		select {
		case prNumber := <-started:
			seen[prNumber] = struct{}{}
		case <-time.After(5 * time.Second):
			t.Fatal("expected both PR reviews to start in parallel")
		}
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not finish after releasing parallel reviews")
	}
	idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
}

func TestDaemon_ParallelOverrideCapsSlotPool(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	after := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{
			{Number: 1, State: "open"},
			{Number: 2, State: "open"},
			{Number: 3, State: "open"},
		},
		comments: map[int][]github.PRComment{
			1: {{ID: "100", Body: "/sandman review", CreatedAt: now}, {ID: "101", Body: "## Summary\nLGTM", CreatedAt: after}},
			2: {{ID: "200", Body: "/sandman review", CreatedAt: now}, {ID: "201", Body: "## Summary\nLGTM", CreatedAt: after}},
			3: {{ID: "300", Body: "/sandman review", CreatedAt: now}, {ID: "301", Body: "## Summary\nLGTM", CreatedAt: after}},
		},
		prFetch: map[int]*github.PR{
			1: {Number: 1, Title: "PR 1", Body: "Body 1"},
			2: {Number: 2, Title: "PR 2", Body: "Body 2"},
			3: {Number: 3, Title: "PR 3", Body: "Body 3"},
		},
	}
	started := make(chan int, 3)
	release := make(chan struct{})
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		started <- req.PRNumber
		<-release
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTestWithParallel(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 1,
	}, 2, true)
	d.Clock = func() time.Time { return now }

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	seen := map[int]struct{}{}
	for len(seen) < 2 {
		select {
		case prNumber := <-started:
			seen[prNumber] = struct{}{}
		case <-time.After(5 * time.Second):
			t.Fatalf("expected exactly 2 reviews to start in parallel (parallel=2), got %d so far", len(seen))
		}
	}

	select {
	case prNumber := <-started:
		t.Fatalf("expected third PR to wait for slot (slot pool cap = 2), but PR %d started", prNumber)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	waitIdle(t, d)
}

func TestDaemon_EffectiveParallelPrefersOverride(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "100", Body: "/sandman review", CreatedAt: now}, {ID: "101", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "PR 1", Body: "Body 1"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 7,
	})
	d.Parallel = 4
	d.ParallelSet = true

	tickAndWait(t, d, context.Background())

	if runner.last.Parallel != 4 {
		t.Errorf("expected effectiveParallel() to prefer d.Parallel (4) over cfg (7), got %d", runner.last.Parallel)
	}
}

func TestDaemon_EffectiveParallelFallsBackToConfig(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "100", Body: "/sandman review", CreatedAt: now}, {ID: "101", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "PR 1", Body: "Body 1"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 7,
	})
	// d.Parallel and d.ParallelSet left at zero values; cfg fallback should win.

	tickAndWait(t, d, context.Background())

	if runner.last.Parallel != 7 {
		t.Errorf("expected effectiveParallel() to fall back to cfg.EffectiveReviewParallel() (7), got %d", runner.last.Parallel)
	}
}

func TestDaemon_LaunchReviewPropagatesAgentModelParallelOverrides(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 30, State: "open"}},
		comments: map[int][]github.PRComment{
			30: {{ID: "c30", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{30: {Number: 30, Title: "PR 30", Body: "Body"}},
	}
	runner := &capturedRequest{}
	cfg := &config.Config{
		DefaultReviewAgent:    "config-agent",
		DefaultReviewModel:    "config/model",
		DefaultReviewParallel: 9,
		AgentProviders: map[string]config.Agent{
			"claude": {Preset: "claude", Command: "claude"},
		},
	}
	d, _, _ := newDaemonForTestWithParallel(t, gh, runner, cfg, 5, true)
	d.Agent = "claude"
	d.Model = "anthropic/claude-sonnet-4"

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	if runner.last.Agent != "claude" {
		t.Errorf("expected agent %q from override, got %q", "claude", runner.last.Agent)
	}
	if runner.last.Model != "anthropic/claude-sonnet-4" {
		t.Errorf("expected model %q from override, got %q", "anthropic/claude-sonnet-4", runner.last.Model)
	}
	if runner.last.Parallel != 5 {
		t.Errorf("expected parallel %d from override, got %d", 5, runner.last.Parallel)
	}
}

func TestDaemon_TickAddsReactionAndRemovesAfterReview(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review focus on tests", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "PR 42", Body: "Body of 42"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	tickAndWait(t, d, context.Background())

	// Verify reactions were added and then removed.
	foundAddComment := false
	foundAddIssue := false
	foundRemoveComment := false
	foundRemoveIssue := false
	var commentReactionID, issueReactionID string
	for _, c := range gh.reactionCalls {
		switch c.kind {
		case "add_comment":
			if c.commentID == "100" && c.content == "eyes" {
				foundAddComment = true
				commentReactionID = c.reactionID
			}
		case "add_issue":
			if c.issueNumber == 42 && c.content == "eyes" {
				foundAddIssue = true
				issueReactionID = c.reactionID
			}
		case "remove_comment":
			if c.commentID == "100" && c.reactionID == commentReactionID {
				foundRemoveComment = true
			}
		case "remove_issue":
			if c.issueNumber == 42 && c.reactionID == issueReactionID {
				foundRemoveIssue = true
			}
		}
	}
	if !foundAddComment {
		t.Error("expected AddCommentReaction for comment 100 with eyes, not found. Calls:", gh.reactionCalls)
	}
	if !foundAddIssue {
		t.Error("expected AddIssueReaction for PR 42 with eyes, not found. Calls:", gh.reactionCalls)
	}
	if !foundRemoveComment {
		t.Error("expected RemoveCommentReaction for comment 100, not found. Calls:", gh.reactionCalls)
	}
	if !foundRemoveIssue {
		t.Error("expected RemoveIssueReaction for PR 42, not found. Calls:", gh.reactionCalls)
	}

	// Verify the review was launched.
	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
}

func TestDaemon_ReactionRemovedOnRunBatchError(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "x", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "T", Body: "Body"}},
	}

	errRunner := func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		return nil, errors.New("batch exploded")
	}

	d, _, _ := newDaemonForTest(t, gh, batchFunc(errRunner), &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	tickAndWait(t, d, context.Background())

	// Reactions should still be removed even though RunBatch failed.
	foundRemoveComment := false
	foundRemoveIssue := false
	for _, c := range gh.reactionCalls {
		if c.kind == "remove_comment" && c.commentID == "x" {
			foundRemoveComment = true
		}
		if c.kind == "remove_issue" && c.issueNumber == 1 {
			foundRemoveIssue = true
		}
	}
	if !foundRemoveComment {
		t.Error("expected comment reaction cleanup after RunBatch error, got calls:", gh.reactionCalls)
	}
	if !foundRemoveIssue {
		t.Error("expected issue reaction cleanup after RunBatch error, got calls:", gh.reactionCalls)
	}
}

func TestDaemon_TickSkipsSeenComment(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "100", Body: "/sandman review again", CreatedAt: now}},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	batchesDir := filepath.Join(d.BaseDir, "batches")
	priorBatchID := "260625120000-abcd-PR42"
	priorBatchPath := filepath.Join(batchesDir, priorBatchID)
	priorRowID := deriveReviewRowID(priorBatchID, 42)
	priorRunDir := filepath.Join(priorBatchPath, "runs", priorRowID)
	if err := os.MkdirAll(priorRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := batchindex.WriteReviewState(priorRunDir, batchindex.ReviewState{
		PR: 42,
		SeenComments: []batchindex.SeenComment{
			{CommentID: "100", Status: "success", Timestamp: now},
		},
		Claims: map[string]batchindex.Claim{},
	}); err != nil {
		t.Fatal(err)
	}
	priorManifest := batchindex.RunManifest{
		RunID:     priorRowID,
		BatchID:   priorBatchID,
		PR:        42,
		Kind:      batchindex.KindReview,
		CreatedAt: now,
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(priorRunDir, priorManifest); err != nil {
		t.Fatal(err)
	}
	idxPath := daemon.BatchesIndexPath(d.BaseDir)
	idx := &batchindex.Index{Version: batchindex.IndexVersion}
	idx.AddBatch(batchindex.Batch{
		ID:        priorBatchID,
		Path:      priorBatchPath,
		Kind:      batchindex.KindReview,
		Status:    batchindex.StatusActive,
		CreatedAt: now,
		PR:        42,
	})
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	if err := d.InvalidateSeenCache(); err != nil {
		t.Fatalf("InvalidateSeenCache: %v", err)
	}

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runner.calls != 0 {
		t.Errorf("expected no batch runs, got %d", runner.calls)
	}
}

func TestDaemon_StaleTriggersLogged(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "old1", Body: "/sandman review", CreatedAt: now},
				{ID: "old2", Body: "/sandman review", CreatedAt: now.Add(1 * time.Minute)},
				{ID: "latest", Body: "/sandman review", CreatedAt: now.Add(10 * time.Minute)},
				{ID: "review-reply", Body: "## Summary\nLGTM", CreatedAt: now.Add(11 * time.Minute)},
			},
		},
	}
	runner := &capturedRequest{}
	d, buf, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}

	output := buf.String()
	if !strings.Contains(output, "skipping stale trigger comment old1") {
		t.Errorf("expected log to mention stale trigger old1, got %q", output)
	}
	if !strings.Contains(output, "skipping stale trigger comment old2") {
		t.Errorf("expected log to mention stale trigger old2, got %q", output)
	}
	if !strings.Contains(output, "newer latest exists") {
		t.Errorf("expected log to mention newest comment 'latest', got %q", output)
	}
}

func TestDaemon_MixedSeenAndUnseenTriggers(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "already-seen", Body: "/sandman review", CreatedAt: now},
				{ID: "unseen-new", Body: "/sandman review focus: newer", CreatedAt: now.Add(5 * time.Minute)},
				{ID: "unseen-old", Body: "/sandman review focus: older", CreatedAt: now.Add(2 * time.Minute)},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// Create a fake prior batch that already processed "already-seen"
	// for PR 1. This simulates cross-run dedup: the seen-cache
	// hydration reads the canonical run folder from the batches
	// index and finds this prior review-state.json.
	const priorBatchID = "old1-260610000000-PR1"
	priorBatchDir := filepath.Join(d.BaseDir, "batches", priorBatchID)
	priorRowID := deriveReviewRowID(priorBatchID, 1)
	priorRunDir := filepath.Join(priorBatchDir, "runs", priorRowID)
	if err := os.MkdirAll(priorRunDir, 0o755); err != nil {
		t.Fatalf("create prior run dir: %v", err)
	}
	priorState := batchindex.ReviewState{
		PR: 1,
		SeenComments: []batchindex.SeenComment{
			{CommentID: "already-seen", Status: "success", Timestamp: now},
		},
	}
	if err := batchindex.WriteReviewState(priorRunDir, priorState); err != nil {
		t.Fatalf("write prior review state: %v", err)
	}
	priorManifest := batchindex.RunManifest{
		RunID:     priorRowID,
		BatchID:   priorBatchID,
		PR:        1,
		Kind:      batchindex.KindReview,
		CreatedAt: now,
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(priorRunDir, priorManifest); err != nil {
		t.Fatalf("write prior run manifest: %v", err)
	}
	// Register the prior batch in the batches index so
	// loadSeenCache discovers it.
	indexPath := filepath.Join(d.BaseDir, "batches", "batches.json")
	idx, _ := batchindex.Load(indexPath)
	idx.AddBatch(batchindex.Batch{
		ID:   priorBatchID,
		Path: priorBatchDir,
		Kind: batchindex.KindReview,
		PR:   1,
	})
	if err := idx.Save(indexPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run (only newest unseen), got %d", runner.calls)
	}

	// The focus should come from the newest unseen trigger: "unseen-new".
	if runner.last.ReviewFocus != "focus: newer" {
		t.Errorf("expected review focus 'focus: newer' from newest unseen, got %q", runner.last.ReviewFocus)
	}

	// Verify "already-seen" was NOT double-processed (no reaction for it).
	gh.mu.Lock()
	for _, c := range gh.reactionCalls {
		if c.commentID == "already-seen" {
			t.Errorf("seen comment 'already-seen' should not get any reactions")
		}
	}
	gh.mu.Unlock()
}

func TestDaemon_TickCaseInsensitive(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {{ID: "1", Body: "/SANDMAN Review please"}},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Errorf("expected 1 batch run, got %d", runner.calls)
	}
}

func TestDaemon_StartSocketCreatesReviewSock(t *testing.T) {
	gh := &fakeGH{}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{})

	if err := d.StartSocket(); err != nil {
		t.Fatalf("StartSocket: %v", err)
	}
	defer d.Stop()

	conn, err := net.Dial("unix", d.SocketPath())
	if err != nil {
		t.Fatalf("connect to review.sock: %v", err)
	}
	conn.Close()
}

func TestDaemon_RunRespondsToTrigger(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "request", AuthorLogin: "SandMan", Body: "/sandman review"}},
		},
	}
	runner := newDecisionRunner()
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	})

	trigger := make(chan struct{}, 4)
	d.Trigger = trigger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()

	trigger <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gh.mu.Lock()
		calls := gh.listCalls
		gh.mu.Unlock()
		if calls >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	gh.mu.Lock()
	calls := gh.listCalls
	gh.mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected exactly 1 list call, got %d", calls)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runner.Calls() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := runner.Calls(); got != 1 {
		t.Fatalf("review launches = %d, want 1 for authenticated request", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestDaemon_RunFailsClosedWhenAuthenticatedLoginLookupFails(t *testing.T) {
	gh := &fakeGH{authenticatedLoginErr: errors.New("not authenticated")}
	d, _, _ := newDaemonForTest(t, gh, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	})
	d.Trigger = make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := d.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "authenticated GitHub login") {
		t.Fatalf("Run error = %v, want authenticated-login lookup failure", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if gh.listCalls != 0 {
		t.Fatalf("ListOpenPRs calls = %d, want 0 after authentication failure", gh.listCalls)
	}
}

func TestDaemon_RunFailsClosedWhenAuthenticatedLoginIsEmpty(t *testing.T) {
	gh := &fakeGH{authenticatedLogin: " "}
	d, _, _ := newDaemonForTest(t, gh, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	})
	d.Trigger = make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := d.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "authenticated GitHub login is empty") {
		t.Fatalf("Run error = %v, want empty authenticated-login failure", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if gh.listCalls != 0 {
		t.Fatalf("ListOpenPRs calls = %d, want 0 after empty authentication login", gh.listCalls)
	}
}

func TestDaemon_IgnoresReviewRequestsFromOtherUsers(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "foreign", AuthorLogin: "other-user", Body: "/sandman review"}},
		},
	}
	runner := newDecisionRunner()
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	})
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	if got := runner.Calls(); got != 0 {
		t.Fatalf("review launches = %d, want 0", got)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.reactionCalls) != 0 {
		t.Fatalf("reactions = %#v, want none", gh.reactionCalls)
	}
}

func TestDaemon_AuthorizedRequestIsNotSupersededByNewerForeignRequest(t *testing.T) {
	now := time.Now()
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "authorized", AuthorLogin: "SandMan", Body: "/sandman review focus on tests", CreatedAt: now},
				{ID: "foreign", AuthorLogin: "other-user", Body: "/sandman review", CreatedAt: now.Add(time.Minute)},
			},
		},
	}
	runner := newDecisionRunner()
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	})
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	if got := runner.Calls(); got != 1 {
		t.Fatalf("review launches = %d, want 1", got)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if got := runner.last.ReviewFocus; got != "focus on tests" {
		t.Fatalf("review focus = %q, want authorized request focus", got)
	}
}

func TestDaemon_StopCancelsInflightBatch(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "x", Body: "/sandman review"}},
		},
	}

	released := make(chan struct{})
	started := make(chan struct{})
	blockingRunner := func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		close(started)
		<-ctx.Done()
		<-released
		return nil, ctx.Err()
	}

	gh.prFetch = map[int]*github.PR{1: {Number: 1, Title: "T", Body: "B"}}
	d, _, _ := newDaemonForTest(t, gh, batchFunc(blockingRunner), &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	<-started
	close(released)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

type batchFunc func(ctx context.Context, req batch.Request) (*batch.Result, error)

func (f batchFunc) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	return f(ctx, req)
}

func TestDaemon_ListOpenPRsErrorIsLogged(t *testing.T) {
	gh := &fakeGH{listErr: errList}
	runner := &capturedRequest{}
	d, buf, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	})

	trigger := make(chan struct{}, 1)
	d.Trigger = trigger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()

	trigger <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "list open PRs") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(buf.String(), "list open PRs") {
		t.Errorf("expected log to mention list open PRs, got %q", buf.String())
	}
}

// TestDaemon_BroadcasterFixtureIsSafeUnderRace locks in the contract that
// the Broadcaster slot wired by newDaemonForTest is safe for concurrent
// use by the daemon goroutine and the test goroutine under `go test -race`.
// The test runs the daemon's Run loop, drives it via Trigger to log a line,
// and reads buf.String() from the test goroutine while writes are in flight.
// If the fixture regresses to a raw *bytes.Buffer (issue #1034), the race
// detector will fail this test.
func TestDaemon_BroadcasterFixtureIsSafeUnderRace(t *testing.T) {
	// Each iteration re-creates the fixture via newDaemonForTest so a
	// stale writer from a previous iteration cannot poison the next
	// iteration's buf assertion.
	for i := 0; i < 5; i++ {
		gh := &fakeGH{listErr: errList}
		runner := &capturedRequest{}
		d, buf, _ := newDaemonForTest(t, gh, runner, &config.Config{
			DefaultReviewAgent: "opencode",
			DefaultReviewModel: "m",
		})

		trigger := make(chan struct{}, 1)
		d.Trigger = trigger
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_ = d.Run(ctx)
			close(done)
		}()

		trigger <- struct{}{}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(buf.String(), "list open PRs") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done

		if !strings.Contains(buf.String(), "list open PRs") {
			t.Fatalf("iteration %d: expected log to mention list open PRs, got %q", i, buf.String())
		}
	}
}

// TestLockedBuffer_ConcurrentWriteAndRead pins the contract of lockedBuffer:
// concurrent writers do not lose bytes, and Bytes() returns an independent
// copy of the underlying buffer (so callers can mutate the result without
// aliasing the internal bytes.Buffer's view).
func TestLockedBuffer_ConcurrentWriteAndRead(t *testing.T) {
	const (
		writers       = 8
		bytesPerWrite = 32
	)
	buf := &lockedBuffer{}
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		w := w
		go func() {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte('a' + w)}, bytesPerWrite)
			for i := 0; i < 50; i++ {
				if _, err := buf.Write(payload); err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got := buf.String()
	if len(got) != writers*50*bytesPerWrite {
		t.Fatalf("string length = %d, want %d (writes were lost or duplicated)", len(got), writers*50*bytesPerWrite)
	}
	counts := make(map[byte]int, writers)
	for i := 0; i < len(got); i++ {
		counts[got[i]]++
	}
	for w := 0; w < writers; w++ {
		key := byte('a' + w)
		if counts[key] != 50*bytesPerWrite {
			t.Errorf("byte %q count = %d, want %d", key, counts[key], 50*bytesPerWrite)
		}
	}

	b := buf.Bytes()
	if len(b) != writers*50*bytesPerWrite {
		t.Fatalf("Bytes() length = %d, want %d", len(b), writers*50*bytesPerWrite)
	}
	b[0] = 'X'
	if buf.Bytes()[0] == 'X' {
		t.Fatal("Bytes() must return a copy; mutating the result aliased the internal buffer")
	}
}

var errList = jsonError("list failed")

type jsonError string

func (e jsonError) Error() string { return string(e) }

func TestDaemon_RunFailsFastOnInvalidReviewAgent(t *testing.T) {
	gh := &fakeGH{}
	runner := &capturedRequest{}
	d, buf, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "nonexistent-agent",
	})

	trigger := make(chan struct{}, 1)
	d.Trigger = trigger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Run to return an error for invalid review agent")
		}
		if !strings.Contains(err.Error(), "nonexistent-agent") {
			t.Errorf("expected error to mention agent name, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Run did not return for invalid review agent")
	}

	cancel()
	if !strings.Contains(buf.String(), "review agent validation failed") {
		t.Errorf("expected validation log, got %q", buf.String())
	}
}

func TestDaemon_RunFailsFastOnMissingReviewModel(t *testing.T) {
	gh := &fakeGH{}
	runner := &capturedRequest{}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "",
		DefaultModel:       "",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)

	trigger := make(chan struct{}, 1)
	d.Trigger = trigger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Run to return an error for missing review model")
		}
		if !strings.Contains(err.Error(), "review model is not set") {
			t.Errorf("expected error about missing review model, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Run did not return for missing review model")
	}

	cancel()
}

func TestDaemon_RunHonorsAgentModelOverridesAtStartup(t *testing.T) {
	gh := &fakeGH{}
	runner := &capturedRequest{}
	// Deliberately leave config review_agent/review_model empty.
	cfg := &config.Config{
		DefaultReviewAgent: "",
		DefaultReviewModel: "",
		AgentProviders: map[string]config.Agent{
			"claude": {Preset: "claude", Command: "claude"},
		},
	}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Agent = "claude"
	d.Model = "anthropic/claude-sonnet-4"

	trigger := make(chan struct{}, 1)
	d.Trigger = trigger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx)
	}()

	select {
	case err := <-done:
		t.Fatalf("Run returned unexpectedly with overrides set: %v", err)
	case <-time.After(150 * time.Millisecond):
		// Run did not fail-fast on empty config because overrides were honored.
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestDaemon_LaunchReviewPropagatesSandboxParams(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 10, State: "open"}},
		comments: map[int][]github.PRComment{
			10: {{ID: "c1", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{10: {Number: 10, Title: "PR 10", Body: "Body"}},
	}
	runner := &capturedRequest{}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Sandbox = "podman"
	d.ContainerCapacity = 3
	d.ContainerCapacitySet = true
	d.MaxContainers = 2
	d.MaxContainersSet = true

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	if runner.last.Sandbox != "podman" {
		t.Errorf("expected sandbox 'podman', got %q", runner.last.Sandbox)
	}
	if runner.last.ContainerCapacity != 3 {
		t.Errorf("expected ContainerCapacity 3, got %d", runner.last.ContainerCapacity)
	}
	if !runner.last.ContainerCapacitySet {
		t.Errorf("expected ContainerCapacitySet=true")
	}
	if runner.last.MaxContainers != 2 {
		t.Errorf("expected MaxContainers 2, got %d", runner.last.MaxContainers)
	}
	if !runner.last.MaxContainersSet {
		t.Errorf("expected MaxContainersSet=true")
	}
}

func TestDaemon_LaunchReviewPropagatesAgentModelOverrides(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 20, State: "open"}},
		comments: map[int][]github.PRComment{
			20: {{ID: "c-agent", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{20: {Number: 20, Title: "PR 20", Body: "Body"}},
	}
	runner := &capturedRequest{}
	cfg := &config.Config{
		DefaultReviewAgent: "config-agent",
		DefaultReviewModel: "config/model",
		AgentProviders: map[string]config.Agent{
			"claude": {Preset: "claude", Command: "claude"},
		},
	}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Agent = "claude"
	d.Model = "anthropic/claude-sonnet-4"

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	if runner.last.Agent != "claude" {
		t.Errorf("expected agent 'claude' from override, got %q", runner.last.Agent)
	}
	if runner.last.Model != "anthropic/claude-sonnet-4" {
		t.Errorf("expected model 'anthropic/claude-sonnet-4' from override, got %q", runner.last.Model)
	}
}

func TestDaemon_LaunchReviewPropagatesRunDirToRenderedPrompt(t *testing.T) {
	// Issue #1953: launchReview must pass the per-row worktree path on
	// the PRData that RenderReview substitutes into {{RUN_DIR}}. The
	// worktree IS the canonical review artifact location; the agent's
	// CWD is the worktree, so the prompt's <RUN_DIR> resolves to the
	// path the daemon reads back from.
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "c-rundir", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "PR 42", Body: "Body"}},
	}
	runner := &capturedRequest{}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	wantWorktreeDir := d.reviewWorktreePath(42, "c-rundir")
	if !strings.Contains(runner.last.PromptConfig.PromptFlag, "RunDir: "+wantWorktreeDir) {
		t.Errorf("rendered prompt must contain `RunDir: %s` (per-row worktree, issue #1953), got prompt:\n%s", wantWorktreeDir, runner.last.PromptConfig.PromptFlag)
	}
	if strings.Contains(runner.last.PromptConfig.PromptFlag, "{{RUN_DIR}}") {
		t.Errorf("rendered prompt must not retain the unfilled {{RUN_DIR}} placeholder, got prompt:\n%s", runner.last.PromptConfig.PromptFlag)
	}
}

// TestDaemon_LaunchReviewRebasesRunDirForContainerSandbox pins the
// issue #1902 + issue #1953 contract: when the daemon is wired in
// production shape (BaseDir = <repoRoot>/.sandman, matching
// cmd/review.go) and the sandbox mode is container-style
// (podman/docker), the prompt's {{RUN_DIR}} substitution MUST be
// the container-visible form (/workspace/<rel>) of the per-row
// worktree path so the agent writes decision.md to a path that
// lands on the bind-mounted host filesystem the daemon reads back.
//
// Issue #1953 changed the canonical artifact path from the run
// folder to the worktree; the prompt's <RUN_DIR> follows the
// worktree. req.RunDir stays on the run folder (used by the
// orchestrator to place run.log and run.json).
//
// This test uses production wiring with `cfg.WorktreeDir =
// ".sandman/worktrees"` so it exercises the layout-vs-cfg path
// the reviewer's regression test called for. The daemon's
// reviewWorktreeBase must agree with the orchestrator's
// NewSandbox worktree location (which also uses the resolved
// WorktreeDir), so the agent's decision.md lands where the
// daemon reads it back.
func TestDaemon_LaunchReviewRebasesRunDirForContainerSandbox(t *testing.T) {
	for _, mode := range []string{"podman", "docker"} {
		t.Run(mode, func(t *testing.T) {
			gh := &fakeGH{
				prs: []github.PR{{Number: 42, State: "open"}},
				comments: map[int][]github.PRComment{
					42: {{ID: "c-cpath", Body: "/sandman review"}},
				},
				prFetch: map[int]*github.PR{42: {Number: 42, Title: "PR 42", Body: "Body"}},
			}
			runner := &capturedRequest{}
			cfg := &config.Config{
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/foo",
				WorktreeDir:        ".sandman/worktrees",
			}
			// Production wiring: BaseDir is <repoRoot>/.sandman (see
			// cmd/review.go). The baseDir-ending-in-".sandman" guard
			// is what enables the path translation.
			repoRoot := testenv.MkdirShort(t, "sm-review-cpath-")
			sandmanDir := filepath.Join(repoRoot, ".sandman")
			if err := os.MkdirAll(sandmanDir, 0755); err != nil {
				t.Fatalf("mkdir sandmanDir: %v", err)
			}
			t.Chdir(repoRoot)
			buf := &lockedBuffer{}
			d := New(sandmanDir, gh, &prompt.Engine{}, runner, cfg, buf, 0, false, nil)
			d.PollInterval = 0
			d.Sandbox = mode

			tickAndWait(t, d, context.Background())

			if runner.calls != 1 {
				t.Fatalf("expected 1 batch run, got %d", runner.calls)
			}
			hostRunDir := runner.last.RunDir
			if hostRunDir == "" {
				t.Fatalf("expected non-empty RunDir in batch request, got empty")
			}
			// req.RunDir stays on the run folder — orchestrator
			// uses it to place run.log and run.json.
			if !strings.HasPrefix(hostRunDir, repoRoot) {
				t.Errorf("req.RunDir must stay host-absolute (%s...), got %q", repoRoot, hostRunDir)
			}
			prompt := runner.last.PromptConfig.PromptFlag
			// The prompt MUST contain the container-visible form of
			// the worktree path (issue #1953). The worktree lives
			// under <repoRoot>/.sandman/worktrees/ on the host, so
			// the container-visible form starts with
			// /workspace/.sandman/worktrees/.
			wantWorktree := d.reviewWorktreePath(42, "c-cpath")
			containerForm := strings.Replace(wantWorktree, repoRoot, sandbox.ContainerWorkspaceMount, 1)
			if !strings.Contains(prompt, "RunDir: "+containerForm) {
				t.Errorf("rendered prompt must contain `RunDir: %s` (container-visible worktree form, issue #1953), got prompt:\n%s", containerForm, prompt)
			}
			if strings.Contains(prompt, "{{RUN_DIR}}") {
				t.Errorf("rendered prompt must not retain the unfilled {{RUN_DIR}} placeholder, got prompt:\n%s", prompt)
			}
		})
	}
}

// TestDaemon_LaunchReviewNoOpRunDirWhenBaseDirNotSandman pins the
// guard: when BaseDir does NOT end in ".sandman" (the test-fixture
// shape used by newDaemonForTest, where a tmp dir is passed directly),
// the translation is a no-op even under a container sandbox mode.
// This preserves the existing contract pinned by
// TestDaemon_LaunchReviewPropagatesRunDirToRenderedPrompt: the prompt
// contains the worktree path verbatim. The guard exists so a
// misconfigured daemon (no .sandman layout to compute the repo root
// from) degrades to the legacy host-path behaviour rather than guessing.
func TestDaemon_LaunchReviewNoOpRunDirWhenBaseDirNotSandman(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "c-noop", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "PR 42", Body: "Body"}},
	}
	runner := &capturedRequest{}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Sandbox = "podman"

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	// When BaseDir does not end in ".sandman", the translation must
	// no-op: the prompt contains the host-absolute worktree path
	// verbatim. This is the guard's safety property — a test fixture
	// without the .sandman layout degrades to legacy behaviour rather
	// than silently producing /workspace paths against an unknown root.
	wantWorktree := d.reviewWorktreePath(42, "c-noop")
	if !strings.Contains(runner.last.PromptConfig.PromptFlag, "RunDir: "+wantWorktree) {
		t.Errorf("rendered prompt must contain host-absolute `RunDir: %s` (worktree, issue #1953) when BaseDir guard does not match, got prompt:\n%s", wantWorktree, runner.last.PromptConfig.PromptFlag)
	}
	if strings.Contains(runner.last.PromptConfig.PromptFlag, sandbox.ContainerWorkspaceMount) {
		t.Errorf("rendered prompt must NOT contain %q when BaseDir guard does not match, got prompt:\n%s", sandbox.ContainerWorkspaceMount, runner.last.PromptConfig.PromptFlag)
	}
}

func TestDaemon_LaunchReviewFallsBackToConfigForAgentModel(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 21, State: "open"}},
		comments: map[int][]github.PRComment{
			21: {{ID: "c-fallback", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{21: {Number: 21, Title: "PR 21", Body: "Body"}},
	}
	runner := &capturedRequest{}
	cfg := &config.Config{
		DefaultReviewAgent: "config-agent",
		DefaultReviewModel: "config/model",
		AgentProviders: map[string]config.Agent{
			"config-agent": {Preset: "opencode", Command: "opencode"},
		},
	}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	// Leave d.Agent and d.Model empty to exercise config fallback.

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	if runner.last.Agent != "config-agent" {
		t.Errorf("expected agent 'config-agent' from cfg fallback, got %q", runner.last.Agent)
	}
	if runner.last.Model != "config/model" {
		t.Errorf("expected model 'config/model' from cfg fallback, got %q", runner.last.Model)
	}
}

func TestDaemon_LaunchReviewFallsBackToConfigSandbox(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 11, State: "open"}},
		comments: map[int][]github.PRComment{
			11: {{ID: "c2", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{11: {Number: 11, Title: "PR 11", Body: "Body"}},
	}
	runner := &capturedRequest{}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		Sandbox:            "podman",
	}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	// Deliberately leave d.Sandbox empty to exercise the cfg.Sandbox fallback.

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	if runner.last.Sandbox != "podman" {
		t.Errorf("expected sandbox 'podman' from cfg.Sandbox fallback, got %q", runner.last.Sandbox)
	}
}

func TestDaemon_ProcessPRLaunchesNewestTriggerOnly(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "1", Body: "/sandman review", CreatedAt: time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)},
				{ID: "2", Body: "/sandman review", CreatedAt: time.Date(2026, 6, 10, 11, 0, 0, 0, time.UTC)},
				{ID: "3", Body: "/sandman review", CreatedAt: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)},
			},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "PR 1", Body: "Body"}},
	}
	runner := &capturedRequest{}

	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	d, buf, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Clock = func() time.Time { return time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC) }

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected exactly 1 batch run for the newest trigger, got %d", runner.calls)
	}
	if !strings.Contains(runner.last.PromptConfig.Branch, "3") {
		t.Errorf("expected launch for newest comment (ID 3), got branch %q", runner.last.PromptConfig.Branch)
	}
	if !strings.Contains(buf.String(), "stale trigger comment") {
		t.Errorf("expected stale comment log, got %q", buf.String())
	}
}

func TestDaemon_OnlyNewestTriggerIgnoredWhenAllStale(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "1", Body: "/sandman review", CreatedAt: time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)},
				{ID: "review-reply", Body: "## Summary\nLGTM", CreatedAt: time.Date(2026, 6, 10, 11, 0, 0, 0, time.UTC)},
			},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "PR 1", Body: "Body"}},
	}
	runner := &capturedRequest{}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)

	// Pre-mark the only comment as seen by writing the canonical
	// per-row review-state.json AND registering the batch in
	// batches.json so the daemon's loadSeenCache picks it up.
	batchesDir := filepath.Join(d.BaseDir, "batches")
	priorBatchID := "260610100000-abcd-PR1"
	priorBatchPath := filepath.Join(batchesDir, priorBatchID)
	priorRowID := deriveReviewRowID(priorBatchID, 1)
	priorRunDir := filepath.Join(priorBatchPath, "runs", priorRowID)
	if err := os.MkdirAll(priorRunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := batchindex.WriteReviewState(priorRunDir, batchindex.ReviewState{
		PR: 1,
		SeenComments: []batchindex.SeenComment{
			{CommentID: "1", Status: "success", Timestamp: time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)},
		},
		Claims: map[string]batchindex.Claim{},
	}); err != nil {
		t.Fatal(err)
	}
	priorManifest := batchindex.RunManifest{
		RunID:     priorRowID,
		BatchID:   priorBatchID,
		PR:        1,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(priorRunDir, priorManifest); err != nil {
		t.Fatal(err)
	}
	idxPath := daemon.BatchesIndexPath(d.BaseDir)
	idx := &batchindex.Index{Version: batchindex.IndexVersion, StatFn: nil}
	idx.AddBatch(batchindex.Batch{
		ID:        priorBatchID,
		Path:      priorBatchPath,
		Kind:      batchindex.KindReview,
		Status:    batchindex.StatusActive,
		CreatedAt: time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC),
		PR:        1,
	})
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	if err := d.InvalidateSeenCache(); err != nil {
		t.Fatalf("InvalidateSeenCache: %v", err)
	}

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runner.calls != 0 {
		t.Errorf("expected no batch runs when only trigger is already seen, got %d", runner.calls)
	}
}

func TestDaemon_ContextCancellationPropagates(t *testing.T) {
	started := make(chan struct{}, 1)

	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "1", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "PR 1", Body: "Body"}},
	}

	blockingRunner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})

	cfg := &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 1,
	}
	d, _, _ := newDaemonForTest(t, gh, blockingRunner, cfg)

	ctx, cancel := context.WithCancel(context.Background())

	// Tick returns immediately (async launch). The goroutine blocks on ctx.Done().
	if err := d.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for review to start")
	}

	cancel() // cancel ctx while RunBatch is blocking

	waitIdle(t, d) // wait for goroutine to finish and release the slot
}

func TestDaemon_ClaimFailureSkipsComment(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "100", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "PR 1", Body: "Body"}},
	}
	runner := &capturedRequest{}
	d, buf, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 2,
	})

	// Pre-mark the comment as already claimed by writing a per-run
	// review-state.json (under a registered batch folder, in the
	// canonical runs/<rowID>/ layout). The new design stores claim
	// locks inline in review-state.json instead of as separate lock
	// files under .sandman/reviews/<PR>/claims/.
	batchesDir := filepath.Join(d.BaseDir, "batches")
	priorBatchID := "260624100000-abcd-PR1"
	priorBatchPath := filepath.Join(batchesDir, priorBatchID)
	priorRowID := deriveReviewRowID(priorBatchID, 1)
	priorRunDir := filepath.Join(priorBatchPath, "runs", priorRowID)
	if err := os.MkdirAll(priorRunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := batchindex.WriteReviewState(priorRunDir, batchindex.ReviewState{
		PR: 1,
		SeenComments: []batchindex.SeenComment{
			{CommentID: "100", Status: "success", Timestamp: time.Now()},
		},
		Claims: map[string]batchindex.Claim{},
	}); err != nil {
		t.Fatal(err)
	}
	priorManifest := batchindex.RunManifest{
		RunID:     priorRowID,
		BatchID:   priorBatchID,
		PR:        1,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(priorRunDir, priorManifest); err != nil {
		t.Fatal(err)
	}
	idxPath := daemon.BatchesIndexPath(d.BaseDir)
	idx := &batchindex.Index{Version: batchindex.IndexVersion}
	idx.AddBatch(batchindex.Batch{
		ID:        priorBatchID,
		Path:      priorBatchPath,
		Kind:      batchindex.KindReview,
		Status:    batchindex.StatusActive,
		CreatedAt: time.Now(),
		PR:        1,
	})
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	if err := d.InvalidateSeenCache(); err != nil {
		t.Fatalf("InvalidateSeenCache: %v", err)
	}

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if runner.calls != 0 {
		t.Errorf("expected no batch runs when comment is already claimed, got %d", runner.calls)
	}
	if !strings.Contains(buf.String(), "already claimed") && !strings.Contains(buf.String(), "already terminal-seen") {
		t.Errorf("expected 'already claimed' or 'already terminal-seen' log, got %q", buf.String())
	}
}

func TestDaemon_LaunchReviewErrorsOnMissingModel(t *testing.T) {

	gh := &fakeGH{
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "T", Body: "B"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "",
		DefaultModel:       "",
	})

	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "",
		DefaultModel:       "",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}
	d.Config = cfg

	err := d.launchReview(context.Background(), 1, "", "c1", "", "", "", "", nil, nil, false)
	if err == nil {
		t.Fatal("expected error from launchReview when model is empty")
	}
	if !strings.Contains(err.Error(), "review model is not set") {
		t.Errorf("expected error about missing review model, got: %v", err)
	}
}

func TestDaemon_LaunchReviewCreatesControlSocketAndManifest(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "vc1", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "T", Body: "B"}},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
		WorktreeDir:        ".sandman/worktrees",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	var capturedRunDir string
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		capturedRunDir = req.RunDir

		// Issue #1846 (S3) + issue #1953: write decision.md to the
		// per-row worktree (the agent's CWD) so the post step
		// succeeds; this test asserts on socket/manifest creation,
		// not on the post step itself.
		worktreePath := filepath.Join(".sandman", "worktrees", req.PromptConfig.Branch)
		if err := os.MkdirAll(worktreePath, 0755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(worktreePath, "decision.md"), []byte("ok"), 0644); err != nil {
			return nil, err
		}

		// Per ADR-0030 the run folder is <batchDir>/runs/<runID>/, so
		// the RunDir's parent batch dir ends with "-PR1".
		batchDir := filepath.Dir(filepath.Dir(req.RunDir))
		if !strings.HasSuffix(batchDir, "-PR1") {
			t.Errorf("expected RunDir's batch dir to end with '-PR1', got %q (RunDir=%q)", batchDir, req.RunDir)
		}

		// The batch manifest lives at the batch dir level (not inside
		// the per-run folder).
		manifestPath := filepath.Join(batchDir, "batch.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Errorf("batch.json should exist at %s: %v", manifestPath, err)
		} else {
			var manifest daemon.BatchManifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Errorf("invalid batch.json: %v", err)
			}
			if len(manifest.Issues) != 0 {
				t.Errorf("expected empty Issues, got %v", manifest.Issues)
			}
			if manifest.CreatedAt.IsZero() {
				t.Errorf("expected non-zero CreatedAt")
			}
		}

		return &batch.Result{}, nil
	})

	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Config = cfg
	d.Clock = func() time.Time { return now }

	reviewRunFolder, perRowRunID, rs, state, prepErr := d.prepareReviewRun(context.Background(), 1, "c1")
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}
	if err := d.launchReview(context.Background(), 1, "", "c1", "", "", reviewRunFolder, perRowRunID, rs, state, false); err != nil {
		t.Fatalf("launchReview: %v", err)
	}

	if _, err := os.Stat(capturedRunDir); err != nil {
		t.Errorf("batch directory should still exist after launchReview, but stat returned: %v", err)
	}
}

func TestDaemon_LaunchReviewCleansUpRunDirOnError(t *testing.T) {
	gh := &fakeGH{
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "T", Body: "B"}},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	var capturedRunDir string
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		capturedRunDir = req.RunDir
		return nil, errors.New("batch exploded")
	})

	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Config = cfg

	reviewRunFolder, perRowRunID, rs, _, prepErr := d.prepareReviewRun(context.Background(), 1, "c1")
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}
	err := d.launchReview(context.Background(), 1, "", "c1", "", "", reviewRunFolder, perRowRunID, rs, nil, false)
	if err == nil {
		t.Fatal("expected error from launchReview")
	}

	if _, err := os.Stat(capturedRunDir); err != nil {
		t.Errorf("batch directory should still exist after launchReview error, but stat returned: %v", err)
	}
}

func TestDaemon_LaunchReviewReplacesStaleSocket(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "vc2", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "T", Body: "B"}},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	runDirChecked := make(chan struct{})
	var capturedRunDir string
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		capturedRunDir = req.RunDir

		batchDir := filepath.Dir(filepath.Dir(req.RunDir))
		if !strings.HasSuffix(batchDir, "-PR1") {
			t.Errorf("expected RunDir's batch dir to end with '-PR1', got %q (RunDir=%q)", batchDir, req.RunDir)
		}

		manifestPath := filepath.Join(batchDir, "batch.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Errorf("batch.json should exist: %v", err)
		} else {
			var manifest daemon.BatchManifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Errorf("invalid batch.json: %v", err)
			}
			if len(manifest.Issues) != 0 {
				t.Errorf("expected empty Issues, got %v", manifest.Issues)
			}
		}

		close(runDirChecked)
		return &batch.Result{}, nil
	})

	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Config = cfg
	d.Clock = func() time.Time { return now }

	reviewRunFolder, perRowRunID, rs, state, prepErr := d.prepareReviewRun(context.Background(), 1, "c1")
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}
	// Issue #1953: decision.md lives in the per-row worktree.
	worktreePath := d.reviewWorktreePath(1, "c1")
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "decision.md"), []byte("ok"), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}
	if err := d.launchReview(context.Background(), 1, "", "c1", "", "", reviewRunFolder, perRowRunID, rs, state, false); err != nil {
		t.Fatalf("launchReview: %v", err)
	}

	select {
	case <-runDirChecked:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for RunBatch to be called")
	}

	if _, err := os.Stat(capturedRunDir); err != nil {
		t.Errorf("batch directory should still exist after launchReview, but stat returned: %v", err)
	}
}

func TestDaemon_LaunchReviewRoutesOutputToPerPRSock(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "vc3", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "T", Body: "B"}},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	var capturedWriter io.Writer
	var capturedRunDir string

	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		capturedWriter = req.OutputWriter
		capturedRunDir = req.RunDir

		batchDir := filepath.Dir(filepath.Dir(req.RunDir))
		if !strings.HasSuffix(batchDir, "-PR1") {
			t.Errorf("expected RunDir's batch dir to end with '-PR1', got %q (RunDir=%q)", batchDir, req.RunDir)
		}

		return &batch.Result{}, nil
	})

	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Config = cfg
	d.Clock = func() time.Time { return now }

	reviewRunFolder, perRowRunID, rs, state, prepErr := d.prepareReviewRun(context.Background(), 1, "c1")
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}
	// Issue #1953: decision.md lives in the per-row worktree.
	worktreePath := d.reviewWorktreePath(1, "c1")
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "decision.md"), []byte("ok"), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}
	if err := d.launchReview(context.Background(), 1, "", "c1", "", "", reviewRunFolder, perRowRunID, rs, state, false); err != nil {
		t.Fatalf("launchReview: %v", err)
	}

	if capturedWriter == nil {
		t.Fatal("OutputWriter was nil")
	}

	if capturedWriter == d.Broadcaster {
		t.Errorf("OutputWriter should be rs.Broadcaster (per-PR), not d.Broadcaster (daemon-level)")
	}

	if capturedRunDir == "" {
		t.Error("RunDir should not be empty")
	}
}

// TestDaemon_TickFailingReviewRecordsFailure verifies that when a review
// launch fails (RunBatch returns an error), the trigger comment is
// recorded as "failure" in review-state.json, not "success". This is the
// missing seam that let the regression ship: the launch error was shadowed
// by a later NewReviewStateStore assignment, so MarkSeen always passed.
func TestDaemon_TickFailingReviewRecordsFailure(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "x", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "T", Body: "Body"}},
	}

	runner := &capturedRequest{}
	errRunner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		runner.mu.Lock()
		runner.calls++
		runner.last = req
		runner.mu.Unlock()
		return nil, errors.New("batch exploded")
	})

	d, _, _ := newDaemonForTest(t, gh, errRunner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}

	statePath := filepath.Join(runner.last.RunDir, "review-state.json")
	state, err := batchindex.ReadReviewState(runner.last.RunDir)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}

	foundFailure := false
	foundSuccess := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "x" {
			if sc.Status == "failure" {
				foundFailure = true
			}
			if sc.Status == "success" {
				foundSuccess = true
			}
		}
	}
	if !foundFailure {
		t.Errorf("comment x should be marked as failure in review-state.json, got: %+v (path: %s)", state.SeenComments, statePath)
	}
	if foundSuccess {
		t.Errorf("comment x should NOT be marked as success, got: %+v (path: %s)", state.SeenComments, statePath)
	}
}

// TestDaemon_AbortedReviewRetriesOnNextTick verifies that a review
// trigger recorded as "aborted" in a prior run's review-state.json is
// not skipped during global dedup; it is re-processed on the next tick.
// This confirms the global-dedup skip rule intentionally deviates from
// PRD #1218's terminal run-status set for "aborted".
func TestDaemon_AbortedReviewRetriesOnNextTick(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "x", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "T", Body: "Body"}},
	}

	runner := &capturedRequest{}
	errRunner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		runner.mu.Lock()
		runner.calls++
		runner.last = req
		runner.mu.Unlock()
		return nil, errors.New("batch exploded")
	})

	d, _, _ := newDaemonForTest(t, gh, errRunner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	// Pre-seed a prior review batch with an aborted review-state.json
	// under the canonical per-row folder shape.
	const priorBatchID = "old1-260610000000-PR1"
	priorBatchDir := filepath.Join(d.BaseDir, "batches", priorBatchID)
	priorRowID := deriveReviewRowID(priorBatchID, 1)
	priorRunDir := filepath.Join(priorBatchDir, "runs", priorRowID)
	if err := os.MkdirAll(priorRunDir, 0o755); err != nil {
		t.Fatalf("create prior run dir: %v", err)
	}
	priorState := batchindex.ReviewState{
		PR: 1,
		SeenComments: []batchindex.SeenComment{
			{CommentID: "x", Status: "aborted", Timestamp: time.Now()},
		},
	}
	if err := batchindex.WriteReviewState(priorRunDir, priorState); err != nil {
		t.Fatalf("write prior review state: %v", err)
	}
	priorManifest := batchindex.RunManifest{
		RunID:     priorRowID,
		BatchID:   priorBatchID,
		PR:        1,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(priorRunDir, priorManifest); err != nil {
		t.Fatalf("write prior run manifest: %v", err)
	}
	indexPath := filepath.Join(d.BaseDir, "batches", "batches.json")
	idx, _ := batchindex.Load(indexPath)
	idx.AddBatch(batchindex.Batch{
		ID:   priorBatchID,
		Path: priorBatchDir,
		Kind: batchindex.KindReview,
		PR:   1,
	})
	if err := idx.Save(indexPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run on first tick (aborted not skipped), got %d", runner.calls)
	}
}

// TestDaemon_TickFailingReviewRetriesOnNextTick verifies that a failed
// review trigger (recorded as "failure" in review-state.json) is
// re-processed on the next daemon tick. This confirms the global dedup
// skip rule only treats "success" as terminal, not "failure", so failed
// triggers can be retried.
func TestDaemon_TickFailingReviewRetriesOnNextTick(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "x", Body: "/sandman review"}},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "T", Body: "Body"}},
	}

	runner := &capturedRequest{}
	errRunner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		runner.mu.Lock()
		runner.calls++
		runner.last = req
		runner.mu.Unlock()
		return nil, errors.New("batch exploded")
	})

	d, _, _ := newDaemonForTest(t, gh, errRunner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	tickAndWait(t, d, context.Background())

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run on first tick, got %d", runner.calls)
	}

	state, err := batchindex.ReadReviewState(runner.last.RunDir)
	if err != nil {
		t.Fatalf("read review state after first tick: %v", err)
	}

	foundFailure := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "x" && sc.Status == "failure" {
			foundFailure = true
			break
		}
	}
	if !foundFailure {
		t.Fatalf("first tick should have recorded failure, got: %+v", state.SeenComments)
	}

	tickAndWait(t, d, context.Background())

	if runner.calls != 2 {
		t.Errorf("expected 2 batch runs (retry on second tick), got %d", runner.calls)
	}
}

// TestDaemon_ConcurrentPRsDoNotClobberSharedPrompt verifies that when
// the daemon processes multiple PRs in parallel, the shared
// review-prompt.md file remains the static PR-agnostic template. PR
// title, body, and review focus must reach the agent only through the
// per-run batch request.
func TestDaemon_ConcurrentPRsDoNotClobberSharedPrompt(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}, {Number: 2, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "c1", Body: "/sandman review focus alpha", CreatedAt: now},
				{ID: "reply1", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)},
			},
			2: {
				{ID: "c2", Body: "/sandman review focus beta", CreatedAt: now},
				{ID: "reply2", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{
			1: {Number: 1, Title: "PR 1 Alpha", Body: "Body 1 Unique"},
			2: {Number: 2, Title: "PR 2 Beta", Body: "Body 2 Unique"},
		},
	}

	started := make(chan int, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	seenReqs := make(map[int]batch.Request)

	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		mu.Lock()
		seenReqs[req.PRNumber] = req
		mu.Unlock()
		started <- req.PRNumber
		<-release
		return &batch.Result{}, nil
	})

	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 2,
	})
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	done := make(chan error, 1)
	go func() {
		done <- d.tick(context.Background())
	}()

	seen := map[int]struct{}{}
	for len(seen) < 2 {
		select {
		case prNumber := <-started:
			seen[prNumber] = struct{}{}
		case <-time.After(5 * time.Second):
			t.Fatal("expected both PR reviews to start in parallel")
		}
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not finish after releasing parallel reviews")
	}
	idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}

	mu.Lock()
	r1, ok1 := seenReqs[1]
	r2, ok2 := seenReqs[2]
	mu.Unlock()
	if !ok1 || !ok2 {
		t.Fatalf("expected batch requests for both PRs, got PR1=%v PR2=%v", ok1, ok2)
	}
	if !strings.Contains(r1.PromptConfig.PromptFlag, "PR 1 Alpha") || !strings.Contains(r1.PromptConfig.PromptFlag, "focus alpha") {
		t.Errorf("PR1 request should contain PR-specific prompt, got: %q", r1.PromptConfig.PromptFlag)
	}
	if !strings.Contains(r2.PromptConfig.PromptFlag, "PR 2 Beta") || !strings.Contains(r2.PromptConfig.PromptFlag, "focus beta") {
		t.Errorf("PR2 request should contain PR-specific prompt, got: %q", r2.PromptConfig.PromptFlag)
	}

	promptPath := filepath.Join(d.BaseDir, "reviews", "review-prompt.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read review-prompt.md: %v", err)
	}
	want := prompt.DefaultPRReviewPrompt()
	if string(data) != want {
		t.Errorf("shared review-prompt.md should be the static template\ngot:\n%s\nwant:\n%s", data, want)
	}
	for _, prSpecific := range []string{"PR 1 Alpha", "Body 1 Unique", "focus alpha", "PR 2 Beta", "Body 2 Unique", "focus beta"} {
		if strings.Contains(string(data), prSpecific) {
			t.Errorf("shared review-prompt.md should not contain PR-specific string %q", prSpecific)
		}
	}
}

func TestDaemon_TickSaturationDoesNotDropTriggers(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	afterReview := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}, {Number: 2, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
			},
			2: {
				{ID: "200", Body: "/sandman review", CreatedAt: now},
				{ID: "201", Body: "## Review\nLooks good!", CreatedAt: afterReview},
			},
		},
		prFetch: map[int]*github.PR{
			1: {Number: 1, Title: "PR 1", Body: "Body 1"},
			2: {Number: 2, Title: "PR 2", Body: "Body 2"},
		},
	}
	started := make(chan int, 1)
	release := make(chan struct{})
	var runMu sync.Mutex
	runCalls := 0
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		runMu.Lock()
		runCalls++
		runMu.Unlock()
		select {
		case started <- req.PRNumber:
		default:
		}
		<-release
		return &batch.Result{}, nil
	})
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/foo",
		DefaultReviewParallel: 1,
	})
	d.Clock = func() time.Time { return now }

	// Tick returns immediately (async launch). PR 1 acquires the slot
	// and launches a background goroutine; PR 2 cannot get a slot and
	// returns, but its comments are still read (ListPRComments called).
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("PR 1 review did not start")
	}

	gh.mu.Lock()
	commentCalls1 := gh.commentCalls[1]
	commentCalls2 := gh.commentCalls[2]
	gh.mu.Unlock()

	if commentCalls1 == 0 {
		t.Errorf("ListPRComments should have been called for PR #1; got %d calls", commentCalls1)
	}
	if commentCalls2 == 0 {
		t.Errorf("ListPRComments should have been called for PR #2 (skipped but still read); got %d calls", commentCalls2)
	}

	close(release)
	waitIdle(t, d)

	runMu.Lock()
	calls := runCalls
	runMu.Unlock()
	if calls != 1 {
		t.Errorf("expected 1 batch run, got %d", calls)
	}
}
