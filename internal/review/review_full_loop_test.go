package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
)

// fullLoopInitGitRepo initialises a fresh git repo at dir with a single
// empty commit on main. The daemon's ClearReviewArtifacts runs
// `git worktree remove` etc. on this repo, so a real git working tree
// is required. Mirrors internal/cmd/run_integration_test.go's
// initRunIntegrationRepo helper but lives in-package to avoid a
// cross-package test helper dependency.
func fullLoopInitGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "checkout", "-b", "main"},
		{"git", "commit", "--allow-empty", "-m", "init"},
	}
	for _, cmd := range cmds {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
}

// fullLoopWriteRunLogRunner is the fake batch runner for the slice-4
// full-loop test. It does the minimum an agent invocation would do in
// production without executing a real agent:
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
type fullLoopWriteRunLogRunner struct {
	mu      sync.Mutex
	calls   int
	last    batch.Request
	botBody string
}

func (r *fullLoopWriteRunLogRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
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
func (r *fullLoopWriteRunLogRunner) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestReviewDaemon_FullLoopPastLaunchReview exercises the post-launch
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
// This test is in `package review` (not `package cmd`) so it can call
// the daemon's `tick` method directly. The slice-3 subagent's review
// recommended this location when the equivalent test pattern via
// `d.Run` + trigger channel proved flaky on macOS GitHub CI under
// heavy `go test -race -v ./...` parallel load: the daemon's Run
// goroutine never gets scheduled. The in-package `tick` direct call
// is the macOS-CI-blessed pattern used by every other daemon test.
//
// The test is fully hermetic: it injects a fake GitHubClient at the
// daemon's `GitHubClient` interface seam and a fake batch runner at
// `BatchRunner`. No shell, no real agent sandbox, no external
// service. The only filesystem writes are the run.log the runner
// produces and the per-run review-state.json the daemon materialises
// via its existing seams.
func TestReviewDaemon_FullLoopPastLaunchReview(t *testing.T) {
	repoDir := t.TempDir()
	fullLoopInitGitRepo(t, repoDir)

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

	gh := &fakeGH{
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

	runner := &fullLoopWriteRunLogRunner{botBody: botBody}
	var logBuf lockedBuffer
	cfg := &config.Config{
		DefaultModel:       "opencode-go/deepseek-v4-flash",
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode-go/deepseek-v4-flash",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	d := New(repoDir, gh, &prompt.Engine{}, runner, cfg, &logBuf, 0, false)
	d.PollInterval = 0
	d.Clock = func() time.Time { return now }

	// Drive the daemon via the same Trigger channel pattern as
	// TestDaemon_RunRespondsToTrigger (daemon_test.go). macOS CI under
	// heavy `go test -race -v ./...` parallel load starves ad-hoc
	// goroutines; the daemon's Run goroutine is the macOS-CI-blessed
	// scheduling domain. The test still owns the only tick
	// invocations (the trigger channel is the gate) so behaviour is
	// fully deterministic — PollInterval=0 means the ticker is
	// suppressed while d.Trigger is non-nil.
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

	// Tick 1: processPR observes the trigger; the launched goroutine
	// registers the trigger as pending.
	trigger <- struct{}{}
	// Wait for the launched goroutine to durably persist the pending
	// entry to disk before tick 2's promote step runs. The goroutine
	// races with tick's wg.Wait(); it may have set runner.last.RunDir
	// already, or it may not have started RunBatch yet. Wait for the
	// runner to observe at least one call so runner.last.RunDir is
	// populated, then poll the state file at that path.
	runDir := waitForRunDirAndPending(t, runner, triggerCommentID, 10*time.Second)

	// Tick 2: promotePendingReviews → promotePendingComment → MarkSeen("success").
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	waitForReviewStateStatus(t, runDir, triggerCommentID, "success", 5*time.Second)

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
	expectedKey := fmt.Sprintf("pr-1-%s", fullLoopSha256Hex(strings.TrimRight(strings.ToLower(botBody), " \t\n")))
	rawEntry, ok := sp[expectedKey]
	if !ok {
		t.Errorf("expected self-posted.json key %q, got keys: %v", expectedKey, fullLoopMapKeys(sp))
	} else {
		var entry struct {
			PRNumber int    `json:"pr_number"`
			SHA256   string `json:"sha256"`
		}
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			t.Errorf("decode self-posted.json entry %q: %v", expectedKey, err)
		} else if entry.PRNumber != 1 {
			t.Errorf("self-posted.json entry pr_number=%d, want 1", entry.PRNumber)
		} else if entry.SHA256 == "" || entry.SHA256 != fullLoopSha256Hex(strings.TrimRight(strings.ToLower(botBody), " \t\n")) {
			t.Errorf("self-posted.json entry sha256=%q, want %q", entry.SHA256, fullLoopSha256Hex(strings.TrimRight(strings.ToLower(botBody), " \t\n")))
		}
	}

	// Tick 3: the trigger is terminal-seen and the bot body is
	// self-posted. processPR must drop both before launching, so
	// runner.Calls stays at 1.
	callsBefore := runner.Calls()
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	// Give the daemon a moment to process the tick (synchronous, no
	// goroutine involved at this stage — processPR's tick is purely
	// synchronous once the launched goroutine from tick 1 has settled).
	if got := runner.Calls(); got != callsBefore {
		t.Errorf("expected runner.calls to stay at %d after tick 3, got %d", callsBefore, got)
	}
	// The trigger's terminal status is durable on disk and remains "success".
	waitForReviewStateStatus(t, runDir, triggerCommentID, "success", 5*time.Second)
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
	last, _ := batchindex.ReadReviewState(runDir)
	t.Fatalf("timed out waiting for pending %s in %s; last seen state: %+v",
		commentID, filepath.Join(runDir, "review-state.json"), last)
}

// waitForRunDirAndPending polls the runner until it has observed at
// least one RunBatch call (so runner.last.RunDir is populated), then
// polls the per-run review-state.json for the trigger's pending
// status. The two-stage wait is required because the launched
// goroutine races with tick's wg.Wait(): it may not have started
// RunBatch by the time tick returns, so reading runner.last.RunDir
// at that point yields "" and the state-file poll looks at the wrong
// directory.
func waitForRunDirAndPending(t *testing.T, r *fullLoopWriteRunLogRunner, commentID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var runDir string
	for time.Now().Before(deadline) {
		if r.Calls() > 0 {
			r.mu.Lock()
			runDir = r.last.RunDir
			r.mu.Unlock()
			if runDir != "" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runDir == "" {
		t.Fatalf("timed out waiting for runner to record RunDir; calls=%d", r.Calls())
	}
	waitForPendingInReviewState(t, runDir, commentID, timeout)
	return runDir
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
	last, _ := batchindex.ReadReviewState(runDir)
	t.Fatalf("timed out waiting for status %q on %s in %s; last seen state: %+v",
		status, commentID, filepath.Join(runDir, "review-state.json"), last)
}

// fullLoopSha256Hex returns the lowercase hex sha256 of body with the
// same normalization SelfPostStore.hashBody uses (trim trailing
// whitespace, lowercase). Mirrors review.hashBody without importing
// the unexported symbol directly.
func fullLoopSha256Hex(body string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimRight(body, " \t\n"))))
	return hex.EncodeToString(sum[:])
}

// fullLoopMapKeys returns the keys of m for diagnostic output.
func fullLoopMapKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
