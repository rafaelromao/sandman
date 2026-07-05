package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_BoundedRetry_GrepsLogBeforeFailing pins B13: when the
// cycle counter reaches pendingMaxCycles but the run log contains
// bodies the bot posted, the entry settles as success (not failure).
// The bodies are recorded into SelfPostStore. This closes the
// "bot was slower than 90s and the daemon would have wrongly
// labelled success as failure" race.
//
// The pre-#1759 behavior marked the entry as failure after
// pendingMaxCycles with no second-chance check. The new behavior
// greps the run log first; only an empty log triggers the failure
// escape.
func TestDaemon_BoundedRetry_GrepsLogBeforeFailing(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	botBody := "## Summary\nLGTM, no blockers."

	dir := t.TempDir()
	runLogPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 7 --body \"" + botBody + "\"\n"
	if err := os.WriteFile(runLogPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	// No post-`since` PR comments — the bot's body is in the log
	// but GitHub's eventual consistency hasn't surfaced it yet.
	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{7: {Number: 7, Title: "PR 7", Body: "B"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }
	d.selfPosts = mustSelfPostStore(t, d.BaseDir)

	// First tick: launch + register pending.
	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}

	// Drive enough ticks to reach the bounded-retry boundary.
	// After the last tick the entry must settle as success (not
	// failure) because the run log has the bot's body.
	statePath := runner.last.RunDir
	for i := 0; i < pendingMaxCycles; i++ {
		// Overwrite the entry's runLogPath each cycle so the
		// grace path can read the test's run.log fixture (the
		// daemon defaults to <runDir>/run.log, which is empty
		// in this test because no agent actually wrote it).
		d.pendingMu.Lock()
		for i, entries := range d.pendingReviews {
			for j := range entries {
				d.pendingReviews[i][j].runLogPath = runLogPath
			}
		}
		d.pendingMu.Unlock()
		if err := d.tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i+2, err)
		}
	}

	state, err := batchindex.ReadReviewState(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	foundSuccess := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "trigger" && sc.Status == "success" {
			foundSuccess = true
		}
		if sc.CommentID == "trigger" && sc.Status == "failure" {
			t.Errorf("expected success (grace path), got failure (issue #1759 B13)")
		}
	}
	if !foundSuccess {
		t.Errorf("expected trigger recorded as success via bounded-retry grace, got %+v", state.SeenComments)
	}
	if !d.selfPosts.IsSelfPosted(7, botBody) {
		t.Errorf("expected bot body to be recorded via grace path")
	}
}

// TestDaemon_BoundedRetry_FailsWhenLogIsEmpty pins the negative half
// of B13: when the cycle counter reaches pendingMaxCycles AND the
// run log is empty (the bot truly failed), the entry settles as
// failure. This is the regression of the old behavior for the
// truly-failed case.
func TestDaemon_BoundedRetry_FailsWhenLogIsEmpty(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	runLogPath := filepath.Join(dir, "run.log")
	// Empty log — the bot never posted anything.
	if err := os.WriteFile(runLogPath, []byte(""), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {
				{ID: "trigger", Body: "/sandman review", CreatedAt: now.Add(-1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{7: {Number: 7, Title: "PR 7", Body: "B"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }
	d.selfPosts = mustSelfPostStore(t, d.BaseDir)

	// First tick: launch + register pending.
	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}

	// Drive enough ticks to exhaust the bounded retry budget.
	statePath := runner.last.RunDir
	for i := 0; i < pendingMaxCycles; i++ {
		// Overwrite the entry's runLogPath each cycle so the
		// grace path can read the test's run.log fixture (the
		// daemon defaults to <runDir>/run.log, which is empty
		// in this test because no agent actually wrote it).
		d.pendingMu.Lock()
		for i, entries := range d.pendingReviews {
			for j := range entries {
				d.pendingReviews[i][j].runLogPath = runLogPath
			}
		}
		d.pendingMu.Unlock()
		if err := d.tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i+2, err)
		}
	}

	state, err := batchindex.ReadReviewState(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	foundFailure := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "trigger" && sc.Status == "failure" {
			foundFailure = true
		}
		if sc.CommentID == "trigger" && sc.Status == "success" {
			t.Errorf("expected failure (empty log), got success")
		}
	}
	if !foundFailure {
		t.Errorf("expected trigger recorded as failure after bounded cycles, got %+v", state.SeenComments)
	}

	// Verify the trigger's review-state.json is well-formed JSON.
	data, err := os.ReadFile(filepath.Join(statePath, "review-state.json"))
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var s batchindex.ReviewState
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("decode review-state.json: %v", err)
	}
}
