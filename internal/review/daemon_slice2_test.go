package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
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
	want := []string{"review-prompt.md", "review.sock"}
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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
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

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	promptPath := filepath.Join(d.BaseDir, "reviews", "review-prompt.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read review-prompt.md: %v", err)
	}
	if len(data) == 0 {
		t.Error("review-prompt.md should be non-empty")
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

// ensure prompt.Engine is referenced (for build pruning) — used by other
// tests in the package via newDaemonForTest.
var _ = prompt.Engine{}
