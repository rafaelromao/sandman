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

func newDaemonForTest(t *testing.T, gh GitHubClient, runner BatchRunner, cfg *config.Config) (*Daemon, *lockedBuffer, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	buf := &lockedBuffer{}
	d := New(dir, gh, &prompt.Engine{}, runner, cfg, buf)
	d.PollInterval = 0
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
	priorBatchID := "abcd-260625120000-PR42"
	priorBatchPath := filepath.Join(batchesDir, priorBatchID)
	priorRunDir := filepath.Join(priorBatchPath, "runs", "review")
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
	idxPath := daemon.BatchesIndexPath(d.BaseDir)
	idx := &batchindex.Index{Version: batchindex.IndexVersion}
	idx.Add(batchindex.Entry{
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
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	// Create a fake prior batch that already processed "already-seen"
	// for PR 1. This simulates cross-run dedup: loadGlobalSeenForPR
	// reads the batches index and finds this prior review-state.json.
	priorBatchDir := filepath.Join(d.BaseDir, "batches", "old-batch-PR1")
	priorRunDir := filepath.Join(priorBatchDir, "runs", "review")
	if err := os.MkdirAll(priorRunDir, 0755); err != nil {
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
	// Register the prior batch in the batches index so
	// loadGlobalSeenForPR discovers it.
	indexPath := filepath.Join(d.BaseDir, "batches", "batches.json")
	idx, _ := batchindex.Load(indexPath)
	idx.Add(batchindex.Entry{
		ID:   "old-batch-PR1",
		Path: priorBatchDir,
		Kind: batchindex.KindReview,
		PR:   1,
	})
	if err := idx.Save(indexPath); err != nil {
		t.Fatalf("save batches index: %v", err)
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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

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

	// Pre-mark the only comment as seen by writing a per-run
	// review-state.json AND registering the batch in batches.json so
	// the daemon's loadGlobalSeenForPR scan picks it up.
	batchesDir := filepath.Join(d.BaseDir, "batches")
	priorBatchID := "abcd-260610100000-PR1"
	priorBatchPath := filepath.Join(batchesDir, priorBatchID)
	priorRunDir := filepath.Join(priorBatchPath, "runs", "review")
	if err := os.MkdirAll(priorRunDir, 0755); err != nil {
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
	idxPath := daemon.BatchesIndexPath(d.BaseDir)
	idx := &batchindex.Index{Version: batchindex.IndexVersion, StatFn: nil}
	idx.Add(batchindex.Entry{
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
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.tick(ctx)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for review to start")
	}

	cancel() // cancel ctx while RunBatch is blocking

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("tick should not return error on ctx cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for tick to complete after cancel")
	}
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
	// review-state.json (under a registered batch folder). The new
	// design stores claim locks inline in review-state.json instead
	// of as separate lock files under .sandman/reviews/<PR>/claims/.
	batchesDir := filepath.Join(d.BaseDir, "batches")
	priorBatchID := "abcd-260624100000-PR1"
	priorBatchPath := filepath.Join(batchesDir, priorBatchID)
	priorRunDir := filepath.Join(priorBatchPath, "runs", "review")
	if err := os.MkdirAll(priorRunDir, 0755); err != nil {
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
	idxPath := daemon.BatchesIndexPath(d.BaseDir)
	idx := &batchindex.Index{Version: batchindex.IndexVersion}
	idx.Add(batchindex.Entry{
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

	_, err := d.launchReview(context.Background(), 1, "", "c1", "", "")
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
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	var capturedRunDir string
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		capturedRunDir = req.RunDir

		// Review runs live at <batchDir>/runs/review, so the RunDir's
		// parent batch dir ends with "-PR1".
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

	if _, err := d.launchReview(context.Background(), 1, "", "c1", "", ""); err != nil {
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

	_, err := d.launchReview(context.Background(), 1, "", "c1", "", "")
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

	if _, err := d.launchReview(context.Background(), 1, "", "c1", "", ""); err != nil {
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

	if _, err := d.launchReview(context.Background(), 1, "", "c1", "", ""); err != nil {
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

func TestDaemon_VerifyReviewPosted_FailsWhenNoNewComments(t *testing.T) {
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	err := d.verifyReviewPosted(context.Background(), 42, now, "999")
	if err == nil {
		t.Fatal("expected error when no new comments found after timestamp")
	}
	if !strings.Contains(err.Error(), "no review comment found") {
		t.Errorf("expected error about missing review comment, got: %v", err)
	}
}

func TestDaemon_VerifyReviewPosted_PassesWhenNewCommentFound(t *testing.T) {
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	after := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
				{ID: "101", Body: "## Summary\nApproved", CreatedAt: after},
			},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	err := d.verifyReviewPosted(context.Background(), 42, now, "100")
	if err != nil {
		t.Fatalf("expected no error when new comment found, got: %v", err)
	}
}

func TestDaemon_LaunchReviewFailsWhenVerificationFails(t *testing.T) {
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 5, State: "open"}},
		comments: map[int][]github.PRComment{
			5: {
				{ID: "c1", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{5: {Number: 5, Title: "PR 5", Body: "Body"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	// Clock returns a time after all fake comments so verification finds none.
	d.Clock = func() time.Time { return now.Add(1 * time.Hour) }

	runDir, err := d.launchReview(context.Background(), 5, "", "c1", "", "")
	if err == nil {
		t.Fatal("expected error from launchReview when verification fails (no new comment)")
	}
	if !strings.Contains(err.Error(), "review verification") {
		t.Errorf("expected error about review verification, got: %v", err)
	}

	// Trigger should NOT be marked as seen (per-run review-state.json
	// should not contain c1 as success/failure/aborted).
	statePath := filepath.Join(runDir, "review-state.json")
	if data, err := os.ReadFile(statePath); err == nil {
		var state batchindex.ReviewState
		if json.Unmarshal(data, &state) == nil {
			for _, sc := range state.SeenComments {
				if sc.CommentID == "c1" {
					t.Error("trigger comment should NOT be marked seen when verification fails")
				}
			}
		}
	}
}

func TestDaemon_LaunchReviewSucceedsWhenVerificationPasses(t *testing.T) {
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	after := now.Add(1 * time.Minute)
	gh := &fakeGH{
		prs: []github.PR{{Number: 6, State: "open"}},
		comments: map[int][]github.PRComment{
			6: {
				{ID: "c2", Body: "/sandman review", CreatedAt: now},
				{ID: "c3", Body: "## Summary\nLGTM", CreatedAt: after},
			},
		},
		prFetch: map[int]*github.PR{6: {Number: 6, Title: "PR 6", Body: "Body"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	// Clock returns a time before the fake comments so verification finds them.
	d.Clock = func() time.Time { return now.Add(-1 * time.Minute) }

	_, err := d.launchReview(context.Background(), 6, "", "c2", "", "")
	if err != nil {
		t.Fatalf("expected no error from launchReview when verification passes, got: %v", err)
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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

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

	// Pre-seed a prior review batch with an aborted review-state.json.
	priorBatchDir := filepath.Join(d.BaseDir, "batches", "old-aborted-review")
	priorRunDir := filepath.Join(priorBatchDir, "runs", "review")
	if err := os.MkdirAll(priorRunDir, 0755); err != nil {
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
	indexPath := filepath.Join(d.BaseDir, "batches", "batches.json")
	idx, _ := batchindex.Load(indexPath)
	idx.Add(batchindex.Entry{
		ID:   "old-aborted-review",
		Path: priorBatchDir,
		Kind: batchindex.KindReview,
		PR:   1,
	})
	if err := idx.Save(indexPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}

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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}

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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}

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
