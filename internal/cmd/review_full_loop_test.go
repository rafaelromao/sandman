package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"github.com/rafaelromao/sandman/internal/review"
)

// fullLoopGH is a hermetic in-memory GitHubClient for the full-loop
// test. It returns canned PR data without spawning any subprocess,
// which is what the existing internal/review/daemon_test.go fakeGH
// pattern does. The full-loop test mirrors that approach so the
// daemon's prList → prView → api pipeline is exercised in-process
// instead of through the gh shell.
//
// Concurrent calls are safe: the embedded mutex guards every read.
type fullLoopGH struct {
	mu       sync.Mutex
	prs      []github.PR
	comments map[int][]github.PRComment
	prFetch  map[int]*github.PR
	repoName string
	reactID  int
}

func (f *fullLoopGH) ListOpenPRs(ctx context.Context) ([]github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]github.PR(nil), f.prs...), nil
}

func (f *fullLoopGH) ListPRComments(ctx context.Context, number int) ([]github.PRComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.comments[number]
	out := make([]github.PRComment, len(src))
	copy(out, src)
	return out, nil
}

func (f *fullLoopGH) FetchPR(ctx context.Context, number int) (*github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pr, ok := f.prFetch[number]; ok {
		cp := *pr
		return &cp, nil
	}
	return &github.PR{Number: number, Title: "Test PR", Body: "A test pull request", State: "open"}, nil
}

func (f *fullLoopGH) RepoName(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repoName != "" {
		return f.repoName, nil
	}
	return "example/sandbox", nil
}

func (f *fullLoopGH) AddCommentReaction(ctx context.Context, commentID, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactID++
	return fmt.Sprintf("react-%d", f.reactID), nil
}

func (f *fullLoopGH) AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactID++
	return fmt.Sprintf("react-%d", f.reactID), nil
}

func (f *fullLoopGH) RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error {
	return nil
}

func (f *fullLoopGH) RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error {
	return nil
}

// reviewE2EWriteRunLogRunner is the fake batch runner for the full-loop
// test. It does the minimum an agent invocation would do in production
// without executing a real agent:
//
//  1. Captures the (c.calls, last) tuple so the test can assert which
//     batch.Request drove each launch.
//  2. Writes a real `run.log` at req.RunDir so the daemon's lazy-verify
//     path can grep it. The log line is the canonical `gh pr comment
//     <PR> --body "<botBody>"` invocation the daemon uses to attribute
//     bot-posted bodies (issue #1759 B12, slice 4).
//
// `run.json` (the per-row manifest) is NOT written here; the daemon's
// `prepareReviewRun` already materialised it before `RunBatch` was invoked.
//
// The runner returns synchronously so the launched goroutine completes
// (and durably persists the `pending` entry to disk) before the test's
// next tick fires `promotePendingReviews`.
type reviewE2EWriteRunLogRunner struct {
	mu      sync.Mutex
	calls   int
	last    batch.Request
	botBody string
}

func (r *reviewE2EWriteRunLogRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.last = req

	logPath := filepath.Join(req.RunDir, "run.log")
	if err := os.MkdirAll(req.RunDir, 0755); err != nil {
		return nil, err
	}
	logLine := fmt.Sprintf("[%s] %s $ gh pr comment %d --body %q\n",
		req.RunID, time.Now().UTC().Format("15:04:05"), req.PRNumber, r.botBody)
	if err := os.WriteFile(logPath, []byte(logLine), 0644); err != nil {
		return nil, err
	}
	return &batch.Result{}, nil
}

// Calls returns the captured call count under the lock.
func (r *reviewE2EWriteRunLogRunner) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestReviewDaemonE2E_FullLoopPastLaunchReview exercises the post-launch
// half of the review daemon's lazy-verify loop:
//
//   - Tick 1: processPR observes the trigger and the fake runner is
//     invoked. The runner writes a real run.log with a scripted
//     `gh pr comment 1 --body "<botBody>"` line. The launched goroutine
//     registers the trigger as pending.
//   - Tick 2: promotePendingReviews → promotePendingComment. The bot's
//     body in the run log matches the post-since PR comment the GH fake
//     returns, the trigger is recorded with terminal status success, and
//     SelfPostStore receives the (prNumber, sha256(body)) entry.
//   - Tick 3: the trigger is terminal-seen; the bot's body is
//     self-posted. processPR drops both before launching a new batch
//     and the runner's call counter does not increment.
//
// This test runs under CI != "" — no t.Skip, no //go:build e2e constraint.
// The test is fully hermetic: it injects a fake GitHubClient at the
// daemon's `GitHubClient` interface seam (slice-1 contract) and a fake
// batch runner at `BatchRunner`. No shell, no real agent sandbox, no
// external service. The only filesystem writes are the run.log the
// runner produces and the per-run review-state.json the daemon
// materialises via its existing seams.
func TestReviewDaemonE2E_FullLoopPastLaunchReview(t *testing.T) {
	repoDir := t.TempDir()
	initRunIntegrationRepo(t, repoDir)

	// Anchor `since` to a static time so the test is deterministic
	// and post-since filtering does not depend on wall-clock timing.
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	botTime := now.Add(1 * time.Minute)

	const (
		triggerCommentID = "100"
		botCommentID     = "2000"
	)
	triggerBody := "/sandman review check tests"
	botBody := "## Summary LGTM no blockers — bot reviewer"

	ghClient := &fullLoopGH{
		repoName: "example/sandbox",
		prs: []github.PR{
			{Number: 1, State: "open", HeadRefName: "feature-x", HeadRefOid: "0000000000000000000000000000000000000000"},
		},
		comments: map[int][]github.PRComment{
			1: {
				{ID: triggerCommentID, Body: triggerBody, CreatedAt: now},
				{ID: botCommentID, Body: botBody, CreatedAt: botTime},
			},
		},
		prFetch: map[int]*github.PR{
			1: {Number: 1, Title: "Test PR", Body: "A test pull request", State: "open", HeadRefName: "feature-x", HeadRefOid: "0000000000000000000000000000000000000000"},
		},
	}

	runner := &reviewE2EWriteRunLogRunner{botBody: botBody}
	cfg := &config.Config{
		DefaultModel:       "opencode-go/deepseek-v4-flash",
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode-go/deepseek-v4-flash",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	d := review.New(repoDir, ghClient, &prompt.Engine{}, runner, cfg, daemon.NewBroadcaster(), 0, false)
	d.PollInterval = 0
	d.Clock = func() time.Time { return now }

	if err := d.StartSocket(); err != nil {
		t.Fatalf("StartSocket: %v", err)
	}

	trigger := make(chan struct{}, 4)
	d.Trigger = trigger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not stop after cancel")
		}
	}()

	// Tick 1: launch the review.
	trigger <- struct{}{}
	if !waitForReviewLaunch(t, runner, 5*time.Second) {
		t.Fatal("expected at least 1 batch run after tick 1, got 0")
	}
	if got := runner.Calls(); got != 1 {
		t.Fatalf("expected 1 batch run after tick 1, got %d", got)
	}

	// Wait for the launched goroutine to durably persist the pending
	// entry to disk before tick 2's promote step runs. WaitForIdle
	// gates on slot release (which happens after MarkSeen persists
	// the pending status).
	runDir := runner.last.RunDir
	idleCtx, cancelIdle := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelIdle()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("daemon did not reach idle after tick 1: %v", err)
	}
	waitForPendingInReviewState(t, runDir, "100", 5*time.Second)

	// Tick 2: promotePendingReviews → promotePendingComment → MarkSeen("success").
	trigger <- struct{}{}
	waitForReviewStateStatus(t, runDir, "100", "success", 5*time.Second)

	// Assert self-posted.json is written with the composite (prNumber, sha256) key.
	spPath := filepath.Join(repoDir, "reviews", "self-posted.json")
	data, err := os.ReadFile(spPath)
	if err != nil {
		t.Fatalf("read self-posted.json: %v", err)
	}
	var sp map[string]json.RawMessage
	if err := json.Unmarshal(data, &sp); err != nil {
		t.Fatalf("decode self-posted.json: %v", err)
	}
	expectedKey := fmt.Sprintf("pr-1-%s", sha256Hex(strings.TrimRight(strings.ToLower(botBody), " \t\n")))
	rawEntry, ok := sp[expectedKey]
	if !ok {
		t.Errorf("expected self-posted.json key %q, got keys: %v", expectedKey, mapKeys(sp))
	} else {
		var entry struct {
			PRNumber int    `json:"pr_number"`
			SHA256   string `json:"sha256"`
		}
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			t.Errorf("decode self-posted.json entry %q: %v", expectedKey, err)
		} else if entry.PRNumber != 1 {
			t.Errorf("self-posted.json entry pr_number=%d, want 1", entry.PRNumber)
		} else if entry.SHA256 == "" || entry.SHA256 != sha256Hex(strings.TrimRight(strings.ToLower(botBody), " \t\n")) {
			t.Errorf("self-posted.json entry sha256=%q, want %q", entry.SHA256, sha256Hex(strings.TrimRight(strings.ToLower(botBody), " \t\n")))
		}
	}

	// Tick 3: the trigger is terminal-seen and the bot body is
	// self-posted. processPR must drop both before launching, so
	// runner.Calls stays at 1.
	callsBefore := runner.Calls()
	trigger <- struct{}{}
	// Give tick a small window to run to completion.
	time.Sleep(200 * time.Millisecond)
	if got := runner.Calls(); got != callsBefore {
		t.Errorf("expected runner.calls to stay at %d after tick 3, got %d", callsBefore, got)
	}
	// The trigger's terminal status is durable on disk and remains "success".
	waitForReviewStateStatus(t, runDir, "100", "success", 5*time.Second)
}

// waitForReviewLaunch polls the fake runner until it observes at least
// one RunBatch call or the deadline expires. On timeout it returns false.
func waitForReviewLaunch(t *testing.T, r *reviewE2EWriteRunLogRunner, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.Calls() > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// waitForPendingInReviewState polls the per-run review-state.json
// until the trigger appears with terminal status "pending" or the
// deadline expires.
func waitForPendingInReviewState(t *testing.T, runDir, commentID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := batchindex.ReadReviewState(runDir)
		if err == nil {
			for _, sc := range state.SeenComments {
				if sc.CommentID == commentID && sc.Status == "pending" {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pending %s in %s", commentID, filepath.Join(runDir, "review-state.json"))
}

// waitForReviewStateStatus polls the per-run review-state.json until
// the trigger carries the given terminal status or the deadline expires.
func waitForReviewStateStatus(t *testing.T, runDir, commentID, status string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := batchindex.ReadReviewState(runDir)
		if err == nil {
			for _, sc := range state.SeenComments {
				if sc.CommentID == commentID && sc.Status == status {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	last := readLastSeenState(t, runDir)
	t.Fatalf("timed out waiting for status %q on %s in %s; last seen state: %+v",
		status, commentID, filepath.Join(runDir, "review-state.json"), last)
}

// readLastSeenState reads the latest review-state.json for diagnostic
// output on timeout (no fallback to nil-state required).
func readLastSeenState(t *testing.T, statePath string) *batchindex.ReviewState {
	t.Helper()
	state, err := batchindex.ReadReviewState(statePath)
	if err != nil {
		return nil
	}
	return &state
}

// sha256Hex returns the lowercase hex sha256 of body with the same
// normalization SelfPostStore.hashBody uses (trim trailing whitespace,
// lowercase). Mirrors review.hashBody without importing the unexported
// symbol directly.
func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimRight(body, " \t\n"))))
	return hex.EncodeToString(sum[:])
}

// mapKeys returns the keys of m for diagnostic output.
func mapKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
