package review

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const backoffPR = 3043

// backoffHarness serves one review request to a daemon whose launches always
// fail, using the production backoff schedule and a controlled clock.
type backoffHarness struct {
	t      *testing.T
	dir    string
	gh     *fakeGH
	runner *failureRunner
	now    time.Time
	d      *Daemon
}

func newBackoffHarness(t *testing.T) *backoffHarness {
	t.Helper()
	dir := testenv.MkdirShort(t, "sm-backoff-")
	t.Chdir(dir)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	h := &backoffHarness{t: t, dir: dir, now: now, runner: &failureRunner{err: errors.New("opencode: Argument list too long")}}
	h.gh = &fakeGH{
		prs:      []github.PR{{Number: backoffPR, State: "open"}},
		comments: map[int][]github.PRComment{backoffPR: {{ID: "c1", Body: "/sandman review", CreatedAt: now, UpdatedAt: now}}},
		prFetch:  map[int]*github.PR{backoffPR: {Number: backoffPR, Title: "T", Body: "B"}},
	}
	h.restart()
	return h
}

func (h *backoffHarness) restart() {
	d := New(h.dir, h.gh, &prompt.Engine{}, h.runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "m"}, &lockedBuffer{}, 1, true, nil)
	d.PollInterval = 0
	d.Clock = func() time.Time { return h.now }
	h.d = d
}

func (h *backoffHarness) key() string { return reviewTriggerKey(h.gh.comments[backoffPR][0]) }

func (h *backoffHarness) tick() int {
	h.t.Helper()
	tickAndWait(h.t, h.d, context.Background())
	return int(h.runner.calls.Load())
}

// attempts returns the attempt count of every Run that recorded key.
func (h *backoffHarness) attempts(key string) map[int]int {
	h.t.Helper()
	paths, err := filepath.Glob(filepath.Join(h.dir, "batches", "*", "runs", "*", "review-state.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	counts := map[int]int{}
	for _, path := range paths {
		store, err := NewReviewStateStore(path, backoffPR, nil)
		if err != nil {
			h.t.Fatal(err)
		}
		if n := ReadFailureAttempts(store, key); n > 0 {
			counts[n]++
		}
	}
	return counts
}

// failOnce waits out the recorded delay, expects exactly one more launch and
// returns the delay it recorded.
func (h *backoffHarness) failOnce() time.Duration {
	h.t.Helper()
	before := int(h.runner.calls.Load())
	if next := h.d.NextAttemptAt(backoffPR, h.key()); !next.IsZero() {
		h.now = next.Add(-time.Second)
		if got := h.tick(); got != before {
			h.t.Fatalf("relaunched before the next attempt was due: %d launches", got)
		}
		h.now = next
	}
	if got := h.tick(); got != before+1 {
		h.t.Fatalf("launches = %d, want %d", got, before+1)
	}
	return h.d.NextAttemptAt(backoffPR, h.key()).Sub(h.now)
}

func TestDaemon_LaunchBackoffGrowsAcrossRunFoldersAndRestarts(t *testing.T) {
	h := newBackoffHarness(t)
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 320 * time.Second, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}

	for i, delay := range want {
		if i == 3 || i == 7 {
			h.restart()
		}
		// failOnce proves no early relaunch only while a next-attempt time is
		// known, so a restart or refresh must not lose it.
		if i > 0 && h.d.NextAttemptAt(backoffPR, h.key()).IsZero() {
			t.Fatalf("attempt %d: the next-attempt time was lost", i+1)
		}
		if got := h.failOnce(); got != delay {
			t.Fatalf("attempt %d: delay = %v, want %v", i+1, got, delay)
		}
	}

	counts := h.attempts(h.key())
	for attempt := 1; attempt <= len(want); attempt++ {
		if counts[attempt] != 1 {
			t.Fatalf("recorded attempts = %v, want one Run for each of 1..%d", counts, len(want))
		}
	}
}

func TestDaemon_LaunchBackoffStartsOverForAnEditedRequest(t *testing.T) {
	h := newBackoffHarness(t)
	h.failOnce()
	h.failOnce()
	h.failOnce()

	h.gh.comments[backoffPR][0].UpdatedAt = h.now.Add(time.Minute)

	if got := h.failOnce(); got != 10*time.Second {
		t.Fatalf("edited request delay = %v, want the first step", got)
	}
	if counts := h.attempts(h.key()); counts[1] != 1 || len(counts) != 1 {
		t.Fatalf("edited request attempts = %v, want a fresh count", counts)
	}
}

// A record written before request revisions existed is keyed by the comment ID.
func TestDaemon_LaunchBackoffKeepsTheCountOfALegacyRecord(t *testing.T) {
	h := newBackoffHarness(t)
	h.gh.comments[backoffPR][0].UpdatedAt = time.Time{}
	h.failOnce()
	h.failOnce()
	h.failOnce()
	h.now = h.d.NextAttemptAt(backoffPR, h.key())

	h.gh.comments[backoffPR][0].UpdatedAt = h.now.Add(-time.Hour)
	h.restart()

	if got := h.tick(); got != 4 {
		t.Fatalf("launches = %d, want 4", got)
	}
	if got := h.d.NextAttemptAt(backoffPR, h.key()).Sub(h.now); got != 80*time.Second {
		t.Fatalf("delay after migrating the legacy record = %v, want 80s", got)
	}

	// The migrated count stays with that revision: a later edit starts over.
	h.gh.comments[backoffPR][0].UpdatedAt = h.now.Add(time.Minute)
	h.restart()
	if got := h.failOnce(); got != 10*time.Second {
		t.Fatalf("delay of the next edit = %v, want the first step", got)
	}
}

// The records of one request revision are read from many Runs in no fixed
// order, so the merged count and next-attempt time must not depend on it.
func TestDaemon_RetryStateDoesNotDependOnRecordOrder(t *testing.T) {
	at := func(minute int) *time.Time {
		stamp := time.Date(2026, 11, 26, 12, minute, 0, 0, time.UTC)
		return &stamp
	}
	for _, tc := range []struct {
		name     string
		records  [2]batchindex.SeenComment
		attempts int
		next     *time.Time
	}{
		{"highest count supplies the time", [2]batchindex.SeenComment{{Attempts: 2, NextAttemptAt: at(30)}, {Attempts: 3, NextAttemptAt: at(10)}}, 3, at(10)},
		{"a tie keeps the later time", [2]batchindex.SeenComment{{Attempts: 2, NextAttemptAt: at(1)}, {Attempts: 2, NextAttemptAt: at(10)}}, 2, at(10)},
		{"a pending record resets nothing", [2]batchindex.SeenComment{{Attempts: 2, NextAttemptAt: at(1)}, {Status: "pending"}}, 2, at(1)},
		{"an uncounted quota pause keeps its time", [2]batchindex.SeenComment{{Status: "pending"}, {NextAttemptAt: at(10)}}, 0, at(10)},
	} {
		for _, reversed := range []bool{false, true} {
			records := tc.records
			if reversed {
				records[0], records[1] = records[1], records[0]
			}
			d := &Daemon{nextAttempt: map[int]map[string]time.Time{}, launchAttempts: map[int]map[string]int{}}
			for _, record := range records {
				record.CommentID = "c1"
				d.hydrateRetryLocked(backoffPR, record)
			}
			if got := d.launchAttempts[backoffPR]["c1"]; got != tc.attempts {
				t.Errorf("%s (reversed=%t): attempts = %d, want %d", tc.name, reversed, got, tc.attempts)
			}
			if got := d.nextAttempt[backoffPR]["c1"]; !got.Equal(*tc.next) {
				t.Errorf("%s (reversed=%t): next attempt = %v, want %v", tc.name, reversed, got, *tc.next)
			}
		}
	}
}

// limitedOnceRunner fails every review launch, except the one that reaches the
// usage limit, and lets the quota probe succeed.
type limitedOnceRunner struct {
	*failureRunner
	limitedLaunch int32
}

func (r limitedOnceRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	if req.PromptConfig.PromptFlag == "quota-probe" {
		return &batch.Result{Runs: []batch.AgentRunResult{{Status: "success"}}}, nil
	}
	if r.calls.Add(1) == r.limitedLaunch {
		return &batch.Result{Runs: []batch.AgentRunResult{{Status: "failure", UsageLimitReached: true}}}, errors.New("prompt-only run failed")
	}
	return nil, r.err
}

func TestDaemon_QuotaPauseDoesNotAdvanceTheLaunchBackoff(t *testing.T) {
	h := newBackoffHarness(t)
	h.d.Runner = limitedOnceRunner{failureRunner: h.runner, limitedLaunch: 3}
	h.failOnce()
	h.failOnce()
	h.failOnce() // the usage limit: the daemon pauses
	if !h.d.IsQuotaPaused() {
		t.Fatal("daemon did not pause")
	}
	if counts := h.attempts(h.key()); counts[2] != 2 || counts[3] != 0 {
		t.Fatalf("attempts at the pause = %v, want the carried count 2 unchanged", counts)
	}

	h.now = h.d.QuotaPausedUntil()
	h.tick() // the probe recovers
	if h.d.IsQuotaPaused() {
		t.Fatal("daemon stayed paused")
	}

	if got := h.failOnce(); got != 40*time.Second {
		t.Fatalf("delay after recovery = %v, want the third step", got)
	}
}
