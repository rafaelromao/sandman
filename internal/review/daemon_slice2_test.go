package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

// TestDaemon_ReviewsDirContainsOnlySocketAndPrompt asserts the daemon
// leaves .sandman/reviews/ flat: after a successful tick with a trigger
// comment, the only entries under <BaseDir>/reviews/ are review.sock and
// review-prompt.md. No per-PR subdirectory is created. This locks in
// acceptance criteria #1 ("No code path creates .sandman/reviews/<PR>/")
// and #2 (".sandman/reviews/ contains only review.sock and review-prompt.md").
func TestDaemon_ReviewsDirContainsOnlySocketAndPrompt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("review daemon uses Unix socket path conventions; tracked by #1720")
	}
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
				{ID: "review-reply", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{7: {Number: 7, Title: "PR 7", Body: "B"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	if err := d.StartSocket(); err != nil {
		t.Fatalf("StartSocket: %v", err)
	}
	defer d.Stop()
	d.Clock = func() time.Time { return now }

	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}

	reviewsDir := filepath.Join(d.BaseDir, "reviews")
	entries, err := os.ReadDir(reviewsDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", reviewsDir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{"quality-rules.md", "review-prompt.md", "review.sock"}
	sort.Strings(want)

	if len(names) != len(want) {
		t.Fatalf("reviews dir contents: got %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("reviews entry %d: got %q, want %q (full list: %v)", i, n, want[i], names)
		}
	}
}

// TestDaemon_ProcessPRWritesReviewStateToRunFolder asserts that after
// a successful tick, the launched run folder (captured from the batch
// request) contains review-state.json with the (pr, commentID) pair
// recorded with a terminal status. This is acceptance criterion #3
// ("review-state.json lives at <batch>/runs/<run>/review-state.json").
func TestDaemon_ProcessPRWritesReviewStateToRunFolder(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 9, State: "open"}},
		comments: map[int][]github.PRComment{
			9: {
				{ID: "c1", Body: "/sandman review", CreatedAt: now},
				{ID: "review-comment", Body: "## Summary\nApproved", CreatedAt: now.Add(1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{9: {Number: 9, Title: "PR 9", Body: "B"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	tickAndWait(t, d, context.Background())
	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}

	runDir := runner.last.RunDir
	statePath := filepath.Join(runDir, "review-state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json at %s: %v", statePath, err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode review-state.json: %v", err)
	}
	if state.PR != 9 {
		t.Errorf("review-state PR = %d, want 9", state.PR)
	}
	found := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == "c1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("review-state should record comment c1 as seen, got %+v", state.SeenComments)
	}

	// No per-PR directory under <BaseDir>/reviews/.
	if _, err := os.Stat(filepath.Join(d.BaseDir, "reviews", "9")); !os.IsNotExist(err) {
		t.Errorf("expected no .sandman/reviews/9/ directory, got stat err: %v", err)
	}
}

// TestDaemon_ProcessPRDoesNotCreatePerPRDirectory asserts that even
// before a review launches (no comments match), no per-PR subdirectory
// is created under .sandman/reviews/.
func TestDaemon_ProcessPRDoesNotCreatePerPRDirectory(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 12, State: "open"}},
		comments: map[int][]github.PRComment{
			12: {{ID: "no-trigger", Body: "unrelated comment"}},
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
	if runner.calls != 0 {
		t.Fatalf("expected 0 batch runs, got %d", runner.calls)
	}

	// The reviews dir may exist (for review.sock) but no per-PR subdir.
	reviewsDir := filepath.Join(d.BaseDir, "reviews")
	if _, err := os.Stat(reviewsDir); err == nil {
		entries, _ := os.ReadDir(reviewsDir)
		for _, e := range entries {
			if e.IsDir() {
				t.Errorf("reviews dir should have no subdirs, found %s", e.Name())
			}
		}
	}
}

// TestDaemon_SharedReviewPromptFileExists pins the location of the
// shared review prompt template. After a tick with a trigger, the file
// .sandman/reviews/review-prompt.md exists. This is acceptance criterion
// #2 (".sandman/reviews/ contains only review.sock and review-prompt.md").
func TestDaemon_SharedReviewPromptFileExists(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 5, State: "open"}},
		comments: map[int][]github.PRComment{
			5: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
				{ID: "review-reply", Body: "## Summary\nLGTM", CreatedAt: now.Add(1 * time.Minute)},
			},
		},
		prFetch: map[int]*github.PR{5: {Number: 5, Title: "PR 5", Body: "B"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	tickAndWait(t, d, context.Background())

	promptPath := filepath.Join(d.BaseDir, "reviews", "review-prompt.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read review-prompt.md: %v", err)
	}
	if len(data) == 0 {
		t.Error("review-prompt.md should be non-empty")
	}
	want := prompt.DefaultPRReviewPrompt()
	if string(data) != want {
		t.Errorf("review-prompt.md should be the static PR-agnostic template\ngot (%d bytes):\n%s\nwant (%d bytes):\n%s", len(data), data, len(want), want)
	}
	if strings.Contains(string(data), "PR 5") {
		t.Errorf("review-prompt.md should not contain PR-specific title, got: %q", string(data))
	}

	// No per-PR prompt file under .sandman/reviews/<PR>/.
	for _, pr := range []int{5} {
		_, err := os.Stat(filepath.Join(d.BaseDir, "reviews", filepath.Base(promptPath)))
		_ = err
		if _, err := os.Stat(filepath.Join(d.BaseDir, "reviews", "5", "pr-review-prompt.md")); !os.IsNotExist(err) {
			t.Errorf("per-PR pr-review-prompt.md should not exist for PR %d, got stat err: %v", pr, err)
		}
	}
}

// TestDaemon_RespectsPreMaterialisedReviewFiles asserts that when the
// review prompt and quality rules are already on disk (e.g. because
// `sandman init` materialised them), the daemon's lazy-init path leaves
// them untouched. This lets users edit the files between init and the
// first review run.
func TestDaemon_RespectsPreMaterialisedReviewFiles(t *testing.T) {
	reviewsDir := filepath.Join(t.TempDir(), ".sandman", "reviews")
	if err := os.MkdirAll(reviewsDir, 0755); err != nil {
		t.Fatalf("mkdir reviews: %v", err)
	}
	editedPrompt := []byte("# user-edited review prompt\n")
	editedQuality := []byte("# user-edited quality rules\n")
	if err := os.WriteFile(filepath.Join(reviewsDir, "review-prompt.md"), editedPrompt, 0644); err != nil {
		t.Fatalf("write review-prompt.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reviewsDir, "quality-rules.md"), editedQuality, 0644); err != nil {
		t.Fatalf("write quality-rules.md: %v", err)
	}

	d := &Daemon{BaseDir: filepath.Dir(reviewsDir)}
	if err := d.initPromptTemplate(); err != nil {
		t.Fatalf("initPromptTemplate: %v", err)
	}

	promptData, err := os.ReadFile(filepath.Join(reviewsDir, "review-prompt.md"))
	if err != nil {
		t.Fatalf("read review-prompt.md: %v", err)
	}
	if string(promptData) != string(editedPrompt) {
		t.Errorf("daemon clobbered pre-existing review-prompt.md\ngot: %q\nwant: %q", promptData, editedPrompt)
	}
	qualityData, err := os.ReadFile(filepath.Join(reviewsDir, "quality-rules.md"))
	if err != nil {
		t.Fatalf("read quality-rules.md: %v", err)
	}
	if string(qualityData) != string(editedQuality) {
		t.Errorf("daemon clobbered pre-existing quality-rules.md\ngot: %q\nwant: %q", qualityData, editedQuality)
	}
}

// TestDaemon_QualityRulesPathMatchesReviewsDir asserts the runtime path
// exposed to the reviewer matches the file `sandman init` materialises.
func TestDaemon_QualityRulesPathMatchesReviewsDir(t *testing.T) {
	d := &Daemon{BaseDir: "/tmp/example/.sandman"}
	want := "/tmp/example/.sandman/reviews/quality-rules.md"
	if got := d.QualityRulesPath(); got != want {
		t.Errorf("QualityRulesPath: got %q, want %q", got, want)
	}
}

// ensure prompt.Engine is referenced (for build pruning) — used by other
// tests in the package via newDaemonForTest.
var _ = prompt.Engine{}

// TestDaemon_ConcurrentTickOnlyLaunchesOnce verifies that when two goroutines
// call tick concurrently on the same open PR with one /sandman review trigger,
// exactly one RunBatch is invoked. This is the core regression test for the
// busy=1 serialization change.
//
// The key insight: tick() uses a non-blocking select to acquire busy.
// If busy is already held, tick returns immediately ("previous tick still
// running, skipping") without calling processPR. So concurrent ticks
// result in only one RunBatch call.
func TestDaemon_ConcurrentTickOnlyLaunchesOnce(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "trigger1", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "PR 1", Body: "Body"}},
	}

	var runMu sync.Mutex
	runCalls := 0
	runnerFinished := make(chan struct{})

	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		runMu.Lock()
		runCalls++
		runMu.Unlock()
		<-runnerFinished
		return &batch.Result{}, nil
	})

	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	var tick2Wg sync.WaitGroup
	tick2Wg.Add(1)
	go func() {
		defer tick2Wg.Done()
		_ = d.tick(context.Background())
	}()

	<-time.After(50 * time.Millisecond)

	err := d.tick(context.Background())
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}

	close(runnerFinished)
	tick2Wg.Wait()

	idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}

	runMu.Lock()
	calls := runCalls
	runMu.Unlock()

	if calls != 1 {
		t.Errorf("expected exactly 1 RunBatch call with busy=1, got %d", calls)
	}
}

// TestDaemon_FailedLaunchRetriesNextTick verifies that when launchReview
// fails before review-state.json is written, the claim is released and the
// next tick retries the same comment.
func TestDaemon_FailedLaunchRetriesNextTick(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {
				{ID: "trigger1", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{1: {Number: 1, Title: "PR 1", Body: "Body"}},
	}

	var runMu sync.Mutex
	runCalls := 0
	runner := batchFunc(func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		runMu.Lock()
		runCalls++
		runMu.Unlock()
		return nil, errors.New("batch exploded")
	})

	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	tickAndWait(t, d, context.Background())

	runMu.Lock()
	firstCalls := runCalls
	runMu.Unlock()

	if firstCalls != 1 {
		t.Errorf("expected 1 RunBatch call on first tick, got %d", firstCalls)
	}

	runMu.Lock()
	runCalls = 0
	runMu.Unlock()

	tickAndWait(t, d, context.Background())

	runMu.Lock()
	secondCalls := runCalls
	runMu.Unlock()

	if secondCalls != 1 {
		t.Errorf("expected 1 RunBatch call on second tick (retry), got %d", secondCalls)
	}
}
