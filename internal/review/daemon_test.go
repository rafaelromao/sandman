package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
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
	mu            sync.Mutex
	prs           []github.PR
	comments      map[int][]github.PRComment
	prFetch       map[int]*github.PR
	listErr       error
	commentErr    map[int]error
	fetchErr      map[int]error
	listCalls     int
	reactionCalls []reactionCall
	addReactionID int // auto-increment for fake reaction IDs
}

func (f *fakeGH) ListOpenPRs() ([]github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.prs, f.listErr
}

func (f *fakeGH) ListPRComments(number int) ([]github.PRComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeGH) FetchPR(number int) (*github.PR, error) {
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

func (f *fakeGH) RepoName() (string, error) {
	return "owner/repo", nil
}

func (f *fakeGH) AddCommentReaction(commentID, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addReactionID++
	id := fmt.Sprintf("react-%d", f.addReactionID)
	f.reactionCalls = append(f.reactionCalls, reactionCall{kind: "add_comment", commentID: commentID, content: content, reactionID: id})
	return id, nil
}

func (f *fakeGH) AddIssueReaction(issueNumber int, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addReactionID++
	id := fmt.Sprintf("react-%d", f.addReactionID)
	f.reactionCalls = append(f.reactionCalls, reactionCall{kind: "add_issue", issueNumber: issueNumber, content: content, reactionID: id})
	return id, nil
}

func (f *fakeGH) RemoveCommentReaction(commentID, reactionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactionCalls = append(f.reactionCalls, reactionCall{kind: "remove_comment", commentID: commentID, reactionID: reactionID})
	return nil
}

func (f *fakeGH) RemoveIssueReaction(issueNumber int, reactionID string) error {
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

func newDaemonForTest(t *testing.T, gh GitHubClient, runner BatchRunner, cfg *config.Config) (*Daemon, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	var buf bytes.Buffer
	d := New(dir, gh, &prompt.Engine{}, runner, cfg, &buf)
	d.PollInterval = 0
	return d, &buf, dir
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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

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
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run (only newest trigger), got %d", runner.calls)
	}

	// The launched review should have the newest comment's focus ("focus on x" for ID "103").
	if runner.last.ReviewFocus != "focus on x" {
		t.Errorf("expected review focus from newest trigger (103), got %q", runner.last.ReviewFocus)
	}

	// Only the newest trigger (103) should get reactions; stale triggers (101, 102) get none.
	gh.mu.Lock()
	defer gh.mu.Unlock()
	for _, c := range gh.reactionCalls {
		if c.kind == "add_comment" || c.kind == "remove_comment" {
			if c.commentID != "103" {
				t.Errorf("expected reactions only for newest comment 103, got reaction on comment %s", c.commentID)
			}
		}
	}

	// All three triggers should be marked as seen in the store.
	store, err := NewSeenCommentsStore(d.PRDir(1))
	if err != nil {
		t.Fatalf("open seen store: %v", err)
	}
	if !store.Has("101") {
		t.Error("stale trigger 101 should be marked as seen")
	}
	if !store.Has("102") {
		t.Error("stale trigger 102 should be marked as seen")
	}
	if !store.Has("103") {
		t.Error("newest trigger 103 should be marked as seen after successful review")
	}
}

func TestDaemon_TickLaunchesReviewForTriggerComment(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review focus on tests"},
				{ID: "101", Body: "unrelated comment"},
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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

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
	if runner.last.RunID != "PR42" {
		t.Errorf("expected RunID='PR42' on daemon review batch request, got %q", runner.last.RunID)
	}
	wantRunDir := filepath.Join(d.BaseDir, "runs", "PR42")
	if runner.last.RunDir != wantRunDir {
		t.Errorf("expected RunDir=%q, got %q", wantRunDir, runner.last.RunDir)
	}

	dir := d.PRDir(42)
	seenPath := filepath.Join(dir, "seen-comments.jsonl")
	data, err := os.ReadFile(seenPath)
	if err != nil {
		t.Fatalf("read seen file: %v", err)
	}
	if !strings.Contains(string(data), "100") {
		t.Errorf("seen file should contain 100, got %q", string(data))
	}
}

func TestDaemon_TickLaunchesReviewsInParallel(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}, {Number: 2, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "100", Body: "/sandman review"}},
			2: {{ID: "200", Body: "/sandman review"}},
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

	done := make(chan error, 1)
	go func() {
		done <- d.tick(context.Background())
	}()

	seen := map[int]struct{}{}
	for len(seen) < 2 {
		select {
		case prNumber := <-started:
			seen[prNumber] = struct{}{}
		case <-time.After(2 * time.Second):
			t.Fatal("expected both PR reviews to start in parallel")
		}
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not finish after releasing parallel reviews")
	}
}

func TestDaemon_TickAddsReactionAndRemovesAfterReview(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review focus on tests"},
			},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "PR 42", Body: "Body of 42"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

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
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "100", Body: "/sandman review again"}},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{})

	dir := d.PRDir(42)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seen-comments.jsonl"), []byte("100\n"), 0644); err != nil {
		t.Fatal(err)
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
			},
		},
	}
	runner := &capturedRequest{}
	d, buf, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

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

	// Pre-mark "already-seen" as seen.
	prDir := d.PRDir(1)
	if err := os.MkdirAll(prDir, 0755); err != nil {
		t.Fatalf("create PR dir: %v", err)
	}
	store, err := NewSeenCommentsStore(prDir)
	if err != nil {
		t.Fatalf("open seen store: %v", err)
	}
	if err := store.Mark("already-seen"); err != nil {
		t.Fatalf("mark already-seen: %v", err)
	}

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
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
	}
	runner := &capturedRequest{}
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

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	if runner.last.Sandbox != "podman" {
		t.Errorf("expected sandbox 'podman' from cfg.Sandbox fallback, got %q", runner.last.Sandbox)
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

	prDir := d.PRDir(1)
	if err := os.MkdirAll(prDir, 0755); err != nil {
		t.Fatal(err)
	}
	err := d.launchReview(context.Background(), 1, prDir, "", "c1", "", "")
	if err == nil {
		t.Fatal("expected error from launchReview when model is empty")
	}
	if !strings.Contains(err.Error(), "review model is not set") {
		t.Errorf("expected error about missing review model, got: %v", err)
	}
}
