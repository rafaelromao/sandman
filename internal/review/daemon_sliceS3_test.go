package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// fakeCommentPoster is the CommentPoster fake used by the seam-1
// integration tests (issue #1846). The seam-1 contract reads:
//
//   - PostComment is called exactly once per daemon tick that
//     launches a review run for a PR with a valid /sandman review
//     trigger.
//   - The body passed to PostComment is RedactBody(<decision.md>)
//     byte-for-byte.
//   - The "err" field, when non-nil, is returned from PostComment
//     so the test can simulate a failed post.
//   - "release" is a channel the test can close to make
//     PostComment block (used by the ctx-cancel-during-post test).
type fakeCommentPoster struct {
	mu         sync.Mutex
	prNumber   int
	body       string
	calls      int
	err        error
	release    chan struct{}
	gotRelease bool
}

func (f *fakeCommentPoster) PostComment(ctx context.Context, prNumber int, body string) error {
	f.mu.Lock()
	f.calls++
	f.prNumber = prNumber
	f.body = body
	release := f.release
	f.mu.Unlock()
	if release != nil {
		select {
		case <-release:
			f.mu.Lock()
			f.gotRelease = true
			f.mu.Unlock()
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func (f *fakeCommentPoster) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCommentPoster) Captured() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prNumber, f.body
}

func (f *fakeCommentPoster) Released() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotRelease
}

// seamRunner is a capturedRequest decorator that lets each test inject
// pre-run side effects (e.g. write a decision.md file) before the
// RunBatch call returns. The seam-1 happy-path test uses this to
// pre-populate decision.md AFTER prepareReviewRun creates the run
// folder, so the call ordering is deterministic.
type seamRunner struct {
	*capturedRequest
	beforeReturn func(req batch.Request)
}

func (s *seamRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	if s.beforeReturn != nil {
		s.beforeReturn(req)
	}
	return s.capturedRequest.RunBatch(ctx, req)
}

// newDaemonForTestS3 is the seam-1 / S3-aware helper used by the
// issue #1846 tests. It accepts a CommentPoster fake so the helpers
// do not have to be threaded through newDaemonForTest (which has
// dozens of existing callers). The fakeGH/Runner/cfg mirrors
// newDaemonForTest's signature so the seam-1 test reads as the
// typical daemon test shape.
func newDaemonForTestS3(t *testing.T, gh GitHubClient, runner BatchRunner, cfg *config.Config, poster CommentPoster) (*Daemon, *lockedBuffer, string) {
	t.Helper()
	dir := testenv.MkdirShort(t, "sm-review-s3-")
	t.Chdir(dir)
	buf := &lockedBuffer{}
	d := New(dir, gh, &prompt.Engine{}, runner, cfg, buf, 0, false, poster)
	d.PollInterval = 0
	return d, buf, dir
}

// TestDaemon_S3_HappyPath_PostsRedactedDecision is the seam-1
// integration test pinned by issue #1846:
//
//   - Pre-populate <runDir>/decision.md with a body that contains
//     /sandman review, /Sandman, and /SANDMAN substrings.
//   - Run a single tick of the daemon with the existing GitHubClient
//     fake and a new CommentPoster fake.
//   - Assert the captured posted body is the redacted version of
//     decision.md (lowercase sandman, no leading slash).
//   - Assert the captured posted body contains no "/sandman"
//     substring under case-insensitive search.
//   - Assert MarkSeen("success") was persisted to review-state.json.
func TestDaemon_S3_HappyPath_PostsRedactedDecision(t *testing.T) {
	const prNumber = 4242

	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: "c-s3-1", Body: "/sandman review", CreatedAt: mustParseTime(t, "2026-07-04T12:00:00Z")}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR S3", Body: "Body"}},
	}

	// Pre-populate decision.md AFTER prepareReviewRun creates
	// the run folder, so the daemon reads it during the post step.
	runner := &seamRunner{
		capturedRequest: &capturedRequest{},
		beforeReturn: func(req batch.Request) {
			body := "/sandman review please.\n/Sandman reply.\n/SANDMAN echo.\nplain sandman stays.\n"
			path := filepath.Join(req.RunDir, "decision.md")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatalf("mkdir run dir: %v", err)
			}
			if err := os.WriteFile(path, []byte(body), 0644); err != nil {
				t.Fatalf("write decision.md: %v", err)
			}
		},
	}

	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}

	poster := &fakeCommentPoster{}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)
	tickAndWait(t, d, context.Background())

	if poster.calls != 1 {
		t.Fatalf("expected 1 PostComment call, got %d", poster.calls)
	}
	gotPR, gotBody := poster.Captured()
	if gotPR != prNumber {
		t.Errorf("expected PostComment called with pr=%d, got %d", prNumber, gotPR)
	}
	wantBody := RedactBody("/sandman review please.\n/Sandman reply.\n/SANDMAN echo.\nplain sandman stays.\n")
	if gotBody != wantBody {
		t.Errorf("posted body mismatch:\n want=%q\n got =%q", wantBody, gotBody)
	}
	if containsSlashSandman(gotBody) {
		t.Errorf("posted body must not contain /sandman substring (case-insensitive); got %q", gotBody)
	}

	statePath := locateReviewStatePath(t, dir)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	found := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "c-s3-1" && sc.Status == "success" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("review-state.json missing MarkSeen(success) for c-s3-1: %s", string(stateBytes))
	}
}

// TestDaemon_S3_MissingDecision_FailsClosed asserts the missing-file
// branch: when decision.md is absent after RunBatch returns, the
// daemon records MarkSeen("failure") and returns an error.
func TestDaemon_S3_MissingDecision_FailsClosed(t *testing.T) {
	const prNumber = 4243

	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: "c-s3-2", Body: "/sandman review", CreatedAt: mustParseTime(t, "2026-07-04T12:00:01Z")}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR S3 missing", Body: "Body"}},
	}
	runner := &capturedRequest{} // no decision.md written
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	poster := &fakeCommentPoster{}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)
	tickAndWait(t, d, context.Background())

	if poster.calls != 0 {
		t.Errorf("expected 0 PostComment calls when decision.md is missing, got %d", poster.calls)
	}
	statePath := locateReviewStatePath(t, dir)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	found := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "c-s3-2" && sc.Status == "failure" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("review-state.json missing MarkSeen(failure) for c-s3-2: %s", string(stateBytes))
	}
}

// TestDaemon_S3_FailedPost_FailsClosed asserts the post-error branch:
// when PostComment returns an error, the daemon records
// MarkSeen("failure") and returns the error.
func TestDaemon_S3_FailedPost_FailsClosed(t *testing.T) {
	const prNumber = 4244

	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: "c-s3-3", Body: "/sandman review", CreatedAt: mustParseTime(t, "2026-07-04T12:00:02Z")}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR S3 postfail", Body: "Body"}},
	}
	runner := &seamRunner{
		capturedRequest: &capturedRequest{},
		beforeReturn: func(req batch.Request) {
			path := filepath.Join(req.RunDir, "decision.md")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatalf("mkdir run dir: %v", err)
			}
			if err := os.WriteFile(path, []byte("payload"), 0644); err != nil {
				t.Fatalf("write decision.md: %v", err)
			}
		},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	poster := &fakeCommentPoster{err: errors.New("gh pr comment: simulated failure")}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)
	tickAndWait(t, d, context.Background())

	if poster.calls != 1 {
		t.Fatalf("expected 1 PostComment call, got %d", poster.calls)
	}
	statePath := locateReviewStatePath(t, dir)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	found := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "c-s3-3" && sc.Status == "failure" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("review-state.json missing MarkSeen(failure) for c-s3-3: %s", string(stateBytes))
	}
}

// TestDaemon_S3_CtxCancelDuringPost_StaysPending asserts the
// ctx-cancel branch: when ctx is cancelled while PostComment is
// in flight, the daemon does NOT call MarkSeen for the trigger.
func TestDaemon_S3_CtxCancelDuringPost_StaysPending(t *testing.T) {
	const prNumber = 4245

	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: "c-s3-4", Body: "/sandman review", CreatedAt: mustParseTime(t, "2026-07-04T12:00:03Z")}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR S3 cancel", Body: "Body"}},
	}
	release := make(chan struct{})
	// Use a runner that BLOCKS until release so the post step
	// is held at PostComment until the test cancels ctx; this
	// pins "ctx cancellation DURING post" rather than "ctx
	// cancellation BEFORE post".
	runner := &seamRunner{
		capturedRequest: &capturedRequest{},
		beforeReturn: func(req batch.Request) {
			path := filepath.Join(req.RunDir, "decision.md")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatalf("mkdir run dir: %v", err)
			}
			if err := os.WriteFile(path, []byte("payload"), 0644); err != nil {
				t.Fatalf("write decision.md: %v", err)
			}
		},
	}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}
	poster := &fakeCommentPoster{release: release}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)

	tickCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := d.tick(tickCtx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Wait for the launch goroutine to enter the post step
	// (PostComment is blocked on the release channel).
	deadline := time.Now().Add(2 * time.Second)
	for poster.calls == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if poster.calls != 1 {
		t.Fatalf("expected PostComment to be entered before ctx cancel, got %d calls", poster.calls)
	}

	cancel()
	close(release)
	if err := d.WaitForIdle(tickCtx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForIdle: %v", err)
	}

	statePath := locateReviewStatePath(t, dir)
	if statePath == "" {
		// no MarkSeen was called; no review-state.json is acceptable.
		return
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	for _, sc := range state.SeenComments {
		if sc.CommentID == "c-s3-4" {
			t.Errorf("review-state.json must NOT record MarkSeen on ctx-cancel; got %s for c-s3-4", sc.Status)
		}
	}
}

// containsSlashSandman reports whether body contains a "/sandman"
// substring under case-insensitive search. Used by the seam-1
// integration test to pin the redactor contract.
func containsSlashSandman(body string) bool {
	return strings.Contains(strings.ToLower(body), "/sandman")
}

// locateReviewStatePath walks <baseDir> for the first
// review-state.json file under the per-row RunID layout
// (`<baseDir>/batches/<batchName>/runs/<rowID>/review-state.json`).
// The seam-1 tests do not assume the exact batches index layout
// (the daemon writes it in prepareReviewRun); the helper returns
// the first match.
func locateReviewStatePath(t *testing.T, batchesRoot string) string {
	t.Helper()
	var found string
	err := filepath.Walk(batchesRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info != nil && !info.IsDir() && filepath.Base(path) == "review-state.json" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", batchesRoot, err)
	}
	return found
}

// mustParseTime parses a layout-known timestamp or fails the test.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return v
}
