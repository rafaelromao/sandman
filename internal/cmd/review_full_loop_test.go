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

// writeReviewDaemonGHShimFullLoop writes a gh shim for the full-loop test.
// Like writeReviewDaemonGHShim, it responds to gh api / gh pr * but also
// emits a pre-baked "bot" comment alongside the trigger on the api
// repos/issues/comments endpoint. The bot comment's body and creation
// timestamp are parameters so the test can anchor CreatedAt >= since
// (the daemon's pending-entry `since` timestamp). The bot comment always
// appears after the trigger in the api response (the GH API uses
// sort=created&direction=asc, so the older trigger comes first).
//
// triggerCreatedAt and botCreatedAt must be formatted RFC3339 strings.
// botCreatedAt must be strictly later than triggerCreatedAt; the daemon
// requires post-since comments to satisfy the lazy-verify contract.
func writeReviewDaemonGHShimFullLoop(t *testing.T, dir, triggerCommentID, triggerBody, triggerCreatedAt, botID, botBody, botCreatedAt string) {
	t.Helper()

	script := `#!/bin/sh
set -eu
shim_dir="` + dir + `"
trigger_id=` + triggerCommentID + `
trigger_ts="` + triggerCreatedAt + `"
bot_id=` + botID + `
bot_ts="` + botCreatedAt + `"
case "${1:-}" in
  repo)
    echo '{"name":"sandbox","owner":{"login":"example"}}'
    exit 0 ;;
  pr)
    case "${2:-}" in
      list)
        echo '[{"number":1,"state":"open","mergedAt":null,"headRefName":"feature-x","headRefOid":"0000000000000000000000000000000000000000"}]' ;;
      view)
        echo '{"number":1,"title":"Test PR","body":"A test pull request","state":"open","mergedAt":null,"headRefName":"feature-x","headRefOid":"0000000000000000000000000000000000000000"}' ;;
      comment)
        c=$(cat "$shim_dir/gh-comment.count" 2>/dev/null || echo 0)
        echo $((c+1)) > "$shim_dir/gh-comment.count"
        while [ $# -gt 0 ]; do case "$1" in --body) shift; printf '%s\n' "${1:-}" > "$shim_dir/gh-comment.body";; esac; shift; done
        echo "commented" ;;
    esac
    exit 0 ;;
  api)
    shift
    path=""
    for a do [ -z "$path" ] && case "$a" in --*) ;; *) path="$a" ;; esac; done
    case "$path" in
      *repos*issues*comments*)
        cat <<JSON
[{"id":$trigger_id,"body":"` + triggerBody + `","created_at":"$trigger_ts","user":{"login":"user1"}},{"id":$bot_id,"body":"` + botBody + `","created_at":"$bot_ts","user":{"login":"bot"}}]
JSON
        ;;
      *) echo "[]" ;;
    esac
    exit 0 ;;
  auth) exit 0 ;;
esac
echo "unhandled gh: $*" >&2
exit 1
`

	binPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim (full loop): %v", err)
	}
}

// reviewE2EWriteRunLogRunner is the fake batch runner for the full-loop
// (slice-4) test. It does the minimum an agent invocation would do in
// production without actually executing a real agent:
//
//  1. Captures the (c.calls, last) tuple so the test can assert which
//     batch.Request drove each launch.
//  2. Writes a real `run.log` at req.RunDir so the daemon's lazy-verify
//     path can grep it. The log line is the canonical `gh pr comment
//     <PR> --body "<botBody>"` invocation that the daemon uses to
//     attribute bot-posted bodies (issue #1759 B12, slice 4).
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
//     body in the run log matches the post-since PR comment the GH shim
//     returns, the trigger is recorded with terminal status success, and
//     SelfPostStore receives the (prNumber, sha256(body)) entry.
//   - Tick 3: the trigger is terminal-seen; the bot's body is
//     self-posted. processPR drops both before launching a new batch
//     and the runner's call counter does not increment.
//
// This test runs under CI != "" — no t.Skip, no //go:build e2e constraint.
// The fake runner is fully hermetic: it touches no gh, no shell, no agent
// sandbox. The only filesystem and CLI surface it uses is the run folder
// the daemon handed it via req.RunDir and a shell-shim gh on PATH (which
// behaves identically under CI).
func TestReviewDaemonE2E_FullLoopPastLaunchReview(t *testing.T) {
	repoDir := t.TempDir()
	initRunIntegrationRepo(t, repoDir)

	// Anchor `since` to a static time so the test is deterministic and
	// post-since filtering does not depend on wall-clock timing.
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	botTime := now.Add(1 * time.Minute)
	triggerCreatedAt := now.Format(time.RFC3339)
	botCreatedAt := botTime.Format(time.RFC3339)

	const triggerCommentID = "100"
	const botCommentID = "2000"
	triggerBody := "/sandman review check tests"
	botBody := "## Summary LGTM no blockers — bot reviewer"

	shimDir := t.TempDir()
	writeReviewDaemonGHShimFullLoop(t, shimDir, triggerCommentID, triggerBody, triggerCreatedAt, botCommentID, botBody, botCreatedAt)

	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "fake")
	t.Setenv("GITHUB_TOKEN", "fake")
	t.Setenv("SANDMAN_TEST_MODEL_OPENCODE", "opencode-go/deepseek-v4-flash")

	ghClient := &github.CLIClient{}
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

	broadcaster := daemon.NewBroadcaster()
	d := review.New(repoDir, ghClient, &prompt.Engine{}, runner, cfg, broadcaster, 0, false)
	d.PollInterval = 0
	d.Clock = func() time.Time { return now }

	// d.Run will call StartSocket internally; calling it twice is safe
	// but adds a small race where the socket is briefly usable from the
	// test goroutine before the daemon goroutine has started. Skipping
	// the upfront call lets Run own the socket lifecycle entirely.
	trigger := make(chan struct{}, 4)
	d.Trigger = trigger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	// Give the daemon's Run goroutine a brief moment to enter its
	// for-select loop before we send the first trigger. Without this,
	// macOS CI under heavy parallel load (processPR + portal + this
	// test concurrently producing `gh` subprocesses) can occasionally
	// drop the buffered trigger if the goroutine scheduling slips past
	// our send. A 100ms sleep is cheap and the test already drives its
	// own ticks explicitly so the daemon is otherwise idle.
	time.Sleep(100 * time.Millisecond)
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not stop after cancel")
		}
	}()

	// Tick 1: launch the review.
	//
	// macOS CI under heavy parallel test load occasionally takes longer
	// than the existing launch-only sibling's 5s budget to reach
	// RunBatch (the daemon's launched goroutine does several `gh`
	// subshell invocations before reaching `RunBatch`). We use 60s,
	// still bounded; locally the test completes in <400ms.
	trigger <- struct{}{}
	if !waitForReviewLaunch(t, runner, 60*time.Second) {
		t.Fatal("expected at least 1 batch run after tick 1, got 0")
	}
	if got := runner.Calls(); got != 1 {
		t.Fatalf("expected 1 batch run after tick 1, got %d", got)
	}

	// Wait for the launched goroutine to durably persist the pending
	// entry to disk before tick 2's promote step runs. WaitForIdle gates
	// on slot release (which happens after MarkSeen persists the pending
	// status).
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

	// Tick 3: the trigger is terminal-seen and the bot body is self-posted.
	// processPR must drop both before launching, so runner.Calls stays at 1.
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
