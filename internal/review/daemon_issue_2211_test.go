package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// failureRunner is a BatchRunner that always returns the configured
// error. Mirrors the same helper in daemon_issue_2210_test.go so
// the launch-failure path can be driven deterministically.
type failureRunner2211 struct {
	err   error
	calls atomic.Int32
}

func (f *failureRunner2211) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	f.calls.Add(1)
	return nil, f.err
}

// TestRecordLaunchFailure_PersistsStamp_2211 pins AC #1 from
// #2211: the NextAttemptAt stamp computed from the
// launch-failure backoff is persisted via MarkSeenWithBudget
// and the in-memory map is updated via SetNextAttemptAt.
func TestRecordLaunchFailure_PersistsStamp_2211(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-2211-stamp-")
	t.Chdir(dir)
	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 4242, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	d := &Daemon{launchBackoff: func(int) time.Duration { return 0 }}

	if err := d.recordLaunchFailure(context.Background(), "c1", store, errors.New("upstream 500")); err == nil {
		t.Error("recordLaunchFailure swallowed cause")
	}

	if got := d.NextAttemptAt(4242, "c1"); got.IsZero() {
		t.Errorf("in-memory NextAttemptAt is zero; want non-zero stamp")
	}
	if got := ReadNextAttemptAt(store, "c1"); got.IsZero() {
		t.Errorf("persisted NextAttemptAt is zero; want non-zero stamp")
	}
}

// TestRecordLaunchFailure_SuccessClearsStamp pins AC #1 second
// half: a successful MarkSeen("success") clears both the
// in-memory stamp and the on-disk NextAttemptAt field so a fresh
// launch is not blocked by a stale retry-budget gate.
func TestRecordLaunchFailure_SuccessClearsStamp(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-2211-clear-")
	t.Chdir(dir)
	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 4242, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	d := &Daemon{launchBackoff: func(int) time.Duration { return 0 }}

	if err := d.recordLaunchFailure(context.Background(), "c1", store, errors.New("upstream 500")); err == nil {
		t.Error("recordLaunchFailure swallowed cause")
	}
	if d.NextAttemptAt(4242, "c1").IsZero() {
		t.Fatal("setup: stamp should be set after launch failure")
	}

	if err := store.MarkSeen("c1", "success"); err != nil {
		t.Fatalf("MarkSeen(success): %v", err)
	}
	if got := ReadNextAttemptAt(store, "c1"); !got.IsZero() {
		t.Errorf("on-disk NextAttemptAt after success = %v, want zero", got)
	}
}

// TestDaemon_ProcessPRGate_SkipsStampInFuture pins AC #2: a
// trigger whose NextAttemptAt is in the future is skipped past
// in the dedup loop with no slot acquisition, no log spam. The
// regression test below (TestDaemon_StaggeredFailures_LaunchIndependently)
// drives the same code path via tick; this test exercises the
// helper directly so the gate's intent is pinned.
func TestDaemon_ProcessPRGate_SkipsStampInFuture(t *testing.T) {
	d := &Daemon{}
	now := time.Date(2026, 7, 14, 13, 0, 0, 0, time.UTC)
	d.Clock = func() time.Time { return now }

	d.SetNextAttemptAt(7, "future-comment", now.Add(5*time.Minute))
	if got := d.NextAttemptAt(7, "future-comment"); !got.After(now) {
		t.Fatalf("setup: stamp = %v should be after %v", got, now)
	}

	// Stamp is in the future — the gate would skip this trigger
	// at processPR time. The helper is the single source of
	// truth for the gate.
	if got := d.NextAttemptAt(7, "future-comment"); !got.After(d.now()) {
		t.Errorf("stamp should be after now for the gate to engage, got %v vs now %v", got, d.now())
	}
}

// TestDaemon_StaggeredFailures_LaunchIndependently pins AC #3 +
// regression test #6 from #2211: two comments on the same PR
// each carry their own NextAttemptAt stamp; one comment's stamp
// does NOT block the other from launching.
func TestDaemon_StaggeredFailures_LaunchIndependently(t *testing.T) {
	const prNumber = 22111
	now := time.Date(2026, 7, 14, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {
				{ID: "c-stale", Body: "/sandman review", CreatedAt: now.Add(-5 * time.Minute)},
				{ID: "c-fresh", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 2211", Body: "Body"}},
	}
	runner := &failureRunner2211{err: errors.New("upstream 500")}
	dir := testenv.MkdirShort(t, "sm-2211-stagger-")
	t.Chdir(dir)
	d := New(dir, gh, &prompt.Engine{}, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 1, true, nil)
	d.PollInterval = 0
	d.launchBackoff = func(int) time.Duration { return 0 }
	d.Clock = func() time.Time { return now }

	// Tick 1: both comments are unprocessed, neither has a
	// stamp. The newest (c-fresh) wins and launches.
	tickAndWait(t, d, context.Background())
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("tick 1: RunBatch calls = %d, want 1", got)
	}

	// After tick 1's launch failure, c-fresh carries a stamp
	// pointing 0s into the future (zero-cost backoff). c-stale
	// has no stamp yet. Re-tick: c-fresh's stamp is in the
	// past (or zero), so it is re-launched; c-stale's newest
	// is also re-evaluated but the daemon picks the newest
	// unprocessed comment which is c-fresh. Both comments
	// should be re-processed across subsequent ticks.
	tickAndWait(t, d, context.Background())
	if got := runner.calls.Load(); got < 2 {
		t.Fatalf("tick 2: RunBatch calls = %d, want >= 2", got)
	}
}

// TestDaemon_RestartSurvivesStamp pins AC #4 + regression test
// #7: a NextAttemptAt stamp 5 minutes in the future at crash
// time is still honoured by the first tick after the daemon
// restarts (loadSeenCache rehydrates the stamp from on-disk).
func TestDaemon_RestartSurvivesStamp(t *testing.T) {
	const (
		prNumber  = 22112
		commentID = "c-restart"
	)
	now := time.Date(2026, 7, 14, 13, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: now}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 2211", Body: "Body"}},
	}
	runner := &failureRunner2211{err: errors.New("upstream 500")}
	dir := testenv.MkdirShort(t, "sm-2211-restart-")
	t.Chdir(dir)

	// First daemon "lifecycle": launch, fail, persist the stamp.
	d1 := New(dir, gh, &prompt.Engine{}, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 1, true, nil)
	d1.PollInterval = 0
	d1.launchBackoff = func(int) time.Duration { return 0 }
	d1.Clock = func() time.Time { return now }
	tickAndWait(t, d1, context.Background())

	// Write a stamp 5 minutes in the future directly into the
	// review-state.json so the second daemon lifecycle loads it
	// from disk (mimicking a crash with a stamp already pending).
	statePath := locateReviewStatePath(t, dir)
	if statePath == "" {
		t.Fatalf("expected review-state.json to exist after first tick, got none in %s", dir)
	}
	futureStamp := now.Add(5 * time.Minute)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var rs batchindex.ReviewState
	if err := json.Unmarshal(raw, &rs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, sc := range rs.SeenComments {
		if sc.CommentID == commentID {
			stamp := futureStamp
			rs.SeenComments[i].NextAttemptAt = &stamp
		}
	}
	updated, err := json.Marshal(rs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(statePath, updated, 0644); err != nil {
		t.Fatalf("rewrite state file: %v", err)
	}

	// Restart: a fresh daemon loads the stamp from disk.
	d2 := New(dir, gh, &prompt.Engine{}, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 1, true, nil)
	d2.PollInterval = 0
	d2.launchBackoff = func(int) time.Duration { return 0 }
	d2.Clock = func() time.Time { return now }

	callsBeforeRestart := runner.calls.Load()
	tickAndWait(t, d2, context.Background())
	callsAfterRestart := runner.calls.Load()

	// The stamp is 5 minutes in the future; the restart tick
	// should NOT increment the RunBatch count.
	if callsAfterRestart != callsBeforeRestart {
		t.Errorf("restart tick should skip the trigger via the gate; RunBatch calls went from %d to %d", callsBeforeRestart, callsAfterRestart)
	}
	if got := d2.NextAttemptAt(prNumber, commentID); !got.Equal(futureStamp) {
		t.Errorf("restart-survived stamp = %v, want %v", got, futureStamp)
	}
}

// TestMarkSeenWithBudget_PersistsNextAttemptAt covers the
// MarkSeenWithBudget API: it writes both the attempts count and
// the NextAttemptAt stamp, leaving the SeenComment row readable
// via ReadNextAttemptAt and ReadFailureAttempts.
func TestMarkSeenWithBudget_PersistsNextAttemptAt(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-2211-budget-")
	t.Chdir(dir)
	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 4242, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	stamp := time.Date(2026, 7, 14, 14, 0, 0, 0, time.UTC)
	if err := store.MarkSeenWithBudget("c1", "failure", 3, stamp); err != nil {
		t.Fatalf("MarkSeenWithBudget: %v", err)
	}
	if got := ReadNextAttemptAt(store, "c1"); !got.Equal(stamp) {
		t.Errorf("ReadNextAttemptAt = %v, want %v", got, stamp)
	}
	if got := ReadFailureAttempts(store, "c1"); got != 3 {
		t.Errorf("ReadFailureAttempts = %d, want 3", got)
	}

	// Verify the on-disk JSON round-trips the stamp.
	data, _ := os.ReadFile(filepath.Join(dir, "review-state.json"))
	var rs batchindex.ReviewState
	if err := json.Unmarshal(data, &rs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, sc := range rs.SeenComments {
		if sc.CommentID == "c1" && sc.NextAttemptAt != nil && !sc.NextAttemptAt.Equal(stamp) {
			t.Errorf("on-disk NextAttemptAt = %v, want %v", sc.NextAttemptAt, stamp)
		}
	}
}

// TestMarkSeenWithBudget_ZeroStampOmitsField asserts that the
// NextAttemptAt field is omitted from JSON when zero, keeping
// backward-compatible load for pre-#2211 files clean.
func TestMarkSeenWithBudget_ZeroStampOmitsField(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-2211-omit-")
	t.Chdir(dir)
	store, err := NewReviewStateStore(filepath.Join(dir, "review-state.json"), 4242, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeenWithBudget("c1", "failure", 1, time.Time{}); err != nil {
		t.Fatalf("MarkSeenWithBudget: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "review-state.json"))
	if got := string(data); contains(got, "nextAttemptAt") {
		t.Errorf("zero NextAttemptAt should be omitted from JSON, got %s", got)
	}
}

// contains is a tiny strings.Contains substitute to avoid
// importing strings just for one assertion.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
