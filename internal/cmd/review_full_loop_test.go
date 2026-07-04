package cmd

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
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/review"
)

// ghResponseForFullLoop returns the canned gh CLI response for a given
// gh subcommand and arg list. The full-loop test never invokes real
// `gh` — the fake exec runner wires these responses through
// `exec.CommandContext("echo", ...)` so the daemon's CLI path stays
// hermetic and macOS CI under heavy parallel load does not flake on
// shell-fork cold-start latency that `gh` (a large Go binary) incurs
// when several `go test -race -v ./...` runs share the runner.
//
// Each invocation returns synthetic data shaped exactly like real gh
// JSON so the daemon's parsers (PRComment.payload, RepoName) see the
// same struct types they would in production.
func ghResponseForFullLoop(name string, args []string, fixture reviewLoopGHFixture) string {
	if name != "gh" {
		return ""
	}
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "repo":
		// gh repo view --json owner,name
		return `{"name":"sandbox","owner":{"login":"example"}}`
	case "pr":
		if len(args) >= 2 {
			switch args[1] {
			case "list":
				// gh pr list --head <branch> --state all ...
				// Return a single open PR for our fixture.
				return fmt.Sprintf(`[{"number":1,"state":"open","mergedAt":null,"headRefName":"%s","headRefOid":"0000000000000000000000000000000000000000"}]`, fixture.featureBranch)
			case "view":
				return fmt.Sprintf(`{"number":1,"title":"Test PR","body":"%s","state":"open","mergedAt":null,"headRefName":"%s","headRefOid":"0000000000000000000000000000000000000000","closingIssuesReferences":[]}`, fixture.prBody, fixture.featureBranch)
			}
		}
	case "api":
		// args[0] is the "api" gh subcommand; the path is args[1].
		path := ""
		for i := 1; i < len(args); i++ {
			a := args[i]
			if !strings.HasPrefix(a, "--") && !strings.HasPrefix(a, "-") {
				path = a
				break
			}
		}
		if strings.Contains(path, "issues/") && strings.Contains(path, "/comments") {
			// Return the trigger + bot bodies in chronological order.
			return fmt.Sprintf(`[{"id":%s,"body":"%s","created_at":"%s","user":{"login":"user1"}},{"id":%s,"body":"%s","created_at":"%s","user":{"login":"bot"}}]`,
				fixture.triggerID, fixture.triggerBody, fixture.triggerCreatedAt,
				fixture.botID, fixture.botBody, fixture.botCreatedAt)
		}
		if strings.Contains(path, "/reactions") {
			// Reaction POST returns the new reaction id; DELETE returns nothing.
			if len(args) >= 2 && args[1] == "DELETE" {
				return ""
			}
			// POST /reactions — return a numeric reaction id.
			return `1`
		}
		// Default: empty list.
		return `[]`
	}
	return ""
}

// reviewLoopGHFixture is the canned gh data for the full-loop test.
type reviewLoopGHFixture struct {
	prBody           string
	featureBranch    string
	triggerID        string
	triggerBody      string
	triggerCreatedAt string
	botID            string
	botBody          string
	botCreatedAt     string
}

// reviewLoopFakeRunner is the fake exec runner for the full-loop test.
// It dispatches on the gh subcommand and returns canned JSON that the
// daemon's *github.CLIClient parses as if it came from real gh.
type reviewLoopFakeRunner struct {
	mu      sync.Mutex
	calls   int
	fixture reviewLoopGHFixture
}

func (f *reviewLoopFakeRunner) Run(ctx context.Context, name string, arg ...string) *exec.Cmd {
	f.mu.Lock()
	f.calls++
	argsCopy := append([]string(nil), arg...)
	fixture := f.fixture
	f.mu.Unlock()

	out := ghResponseForFullLoop(name, argsCopy, fixture)
	// echo prints its arguments separated by spaces and terminated
	// with a newline. This is the exact binary shape the daemon's
	// runCmd + CombinedOutput expects.
	if out == "" {
		return exec.CommandContext(ctx, "true")
	}
	return exec.CommandContext(ctx, "echo", out)
}

func (f *reviewLoopFakeRunner) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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

	t.Setenv("GH_TOKEN", "fake")
	t.Setenv("GITHUB_TOKEN", "fake")
	t.Setenv("SANDMAN_TEST_MODEL_OPENCODE", "opencode-go/deepseek-v4-flash")

	ghFixture := reviewLoopGHFixture{
		prBody:           "A test pull request",
		featureBranch:    "feature-x",
		triggerID:        triggerCommentID,
		triggerBody:      triggerBody,
		triggerCreatedAt: triggerCreatedAt,
		botID:            botCommentID,
		botBody:          botBody,
		botCreatedAt:     botCreatedAt,
	}
	ghRunner := &reviewLoopFakeRunner{fixture: ghFixture}
	ghClient := github.NewCLIClient(github.WithRunner(ghRunner))
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
