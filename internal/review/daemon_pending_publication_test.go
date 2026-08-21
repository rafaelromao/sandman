package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/github"
)

func TestDaemon_PostFailure_RehydratesDurableDecisionAfterCleanup(t *testing.T) {
	const (
		prNumber  = 2471
		commentID = "c-durable-post"
		body      = "## Decision\n**APPROVED**\nexact body\n"
	)

	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {{ID: commentID, Body: "/sandman review"}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "durable review", Body: "body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: body}
	sustained := errors.New("publication unavailable")
	poster := &fakeCommentPoster{errs: []error{sustained, sustained, sustained, sustained, sustained, nil}}
	d, dir, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.CommentPoster = poster
	branch := reviewBranchName(prNumber, commentID)
	stageReviewWorktree(t, worktreeDir, branch)

	tickAndWait(t, d, context.Background())

	if runner.Calls() != 1 {
		t.Fatalf("first tick RunBatch calls = %d, want 1", runner.Calls())
	}
	if poster.Calls() != PostStepMaxAttempts {
		t.Fatalf("first tick PostComment calls = %d, want %d", poster.Calls(), PostStepMaxAttempts)
	}
	if gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Fatalf("post failure with a durable decision should clean up worktree %s", branch)
	}
	if gitBranchExists(t, branch) {
		t.Fatalf("post failure with a durable decision should delete branch %s", branch)
	}

	batchID := findReviewBatchID(t, dir)
	runID := findReviewRunID(t, dir)
	runDecisionPath := filepath.Join(dir, "batches", batchID, "runs", runID, "decision.md")
	gotBody, err := os.ReadFile(runDecisionPath)
	if err != nil {
		t.Fatalf("read durable decision: %v", err)
	}
	if string(gotBody) != body {
		t.Fatalf("durable decision body = %q, want %q", string(gotBody), body)
	}
	entry, ok := d.peekPendingPost(prNumber, commentID)
	if !ok {
		t.Fatal("pending publication entry missing after retry exhaustion")
	}
	if entry.runDir != filepath.Dir(runDecisionPath) {
		t.Fatalf("pending decision source = %q, want durable run folder %q", entry.runDir, filepath.Dir(runDecisionPath))
	}

	eventsBefore, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read events before recovery: %v", err)
	}
	tickAndWait(t, d, context.Background())
	eventsAfter, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read events after recovery: %v", err)
	}

	if runner.Calls() != 1 {
		t.Fatalf("recovery tick RunBatch calls = %d, want 1", runner.Calls())
	}
	if poster.Calls() != PostStepMaxAttempts+1 {
		t.Fatalf("recovery tick PostComment calls = %d, want %d", poster.Calls(), PostStepMaxAttempts+1)
	}
	_, postedBody := poster.Captured()
	if postedBody != body {
		t.Fatalf("recovered body = %q, want %q", postedBody, body)
	}
	if !d.IsTerminalSeen(prNumber, commentID) {
		t.Fatal("recovered publication should mark the review trigger successful")
	}
	if _, ok := d.peekPendingPost(prNumber, commentID); ok {
		t.Fatal("pending publication entry should be removed after recovery")
	}
	if string(eventsAfter) != string(eventsBefore) {
		t.Fatalf("publication recovery changed events.jsonl: before=%q after=%q", string(eventsBefore), string(eventsAfter))
	}
}

func TestDaemon_CancelAfterDurableDecision_RehydratesSameProcess(t *testing.T) {
	const (
		prNumber  = 2475
		commentID = "c-cancel-after-persist"
		body      = "## Decision\n**APPROVED**\ncancelled publication\n"
	)
	updatedAt := mustParseTime(t, "2026-07-06T13:00:05Z")
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: updatedAt, UpdatedAt: updatedAt}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "cancelled publication", Body: "body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: body}
	poster := &fakeCommentPoster{}
	d, dir, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.CommentPoster = poster
	triggerKey := reviewTriggerKey(gh.comments[prNumber][0])
	branch := reviewBranchName(prNumber, triggerKey)
	stageReviewWorktree(t, worktreeDir, branch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.persistDecision = func(path string, decision []byte) error {
		err := atomicfs.WriteAtomic(path, decision, 0644)
		cancel()
		return err
	}

	tickAndWait(t, d, ctx)

	statePath := locateReviewStatePath(t, dir)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read pending review state: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal pending review state: %v", err)
	}
	foundPending := false
	for _, seen := range state.SeenComments {
		if seen.CommentID == triggerKey && seen.Status == "pending" {
			foundPending = true
			break
		}
	}
	if !foundPending {
		t.Fatalf("review-state.json missing pending status for %s: %s", triggerKey, stateBytes)
	}

	batchID := findReviewBatchID(t, dir)
	runID := findReviewRunID(t, dir)
	runDecisionPath := filepath.Join(dir, "batches", batchID, "runs", runID, "decision.md")
	if got, err := os.ReadFile(runDecisionPath); err != nil || string(got) != body {
		t.Fatalf("durable decision = %q, err=%v; want %q", got, err, body)
	}
	entry, ok := d.peekPendingPost(prNumber, triggerKey)
	if !ok {
		t.Fatalf("pending publication entry missing for %s", triggerKey)
	}
	if entry.runDir != filepath.Dir(runDecisionPath) {
		t.Fatalf("pending source = %q, want %q", entry.runDir, filepath.Dir(runDecisionPath))
	}
	if poster.Calls() != 0 {
		t.Fatalf("cancelled publication should not post, got %d calls", poster.Calls())
	}

	tickAndWait(t, d, context.Background())
	if runner.Calls() != 1 {
		t.Fatalf("same-process recovery RunBatch calls = %d, want 1", runner.Calls())
	}
	if poster.Calls() != 1 {
		t.Fatalf("same-process recovery PostComment calls = %d, want 1", poster.Calls())
	}
	if !d.IsTerminalSeen(prNumber, triggerKey) {
		t.Fatal("same-process recovery should mark the trigger successful")
	}
}

func TestDaemon_CancelAfterDurableDecision_RehydratesAfterRestart(t *testing.T) {
	const (
		prNumber  = 2476
		commentID = "c-cancel-restart"
		body      = "## Decision\n**CHANGES_REQUESTED**\nrestart cancellation\n"
	)
	updatedAt := mustParseTime(t, "2026-07-06T13:00:06Z")
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: updatedAt, UpdatedAt: updatedAt}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "restart cancellation", Body: "body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: body}
	poster := &fakeCommentPoster{}
	d1, dir, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d1.CommentPoster = poster
	triggerKey := reviewTriggerKey(gh.comments[prNumber][0])
	stageReviewWorktree(t, worktreeDir, reviewBranchName(prNumber, triggerKey))

	ctx, cancel := context.WithCancel(context.Background())
	d1.persistDecision = func(path string, decision []byte) error {
		err := atomicfs.WriteAtomic(path, decision, 0644)
		cancel()
		return err
	}
	tickAndWait(t, d1, ctx)
	cancel()

	if runner.Calls() != 1 {
		t.Fatalf("initial RunBatch calls = %d, want 1", runner.Calls())
	}
	if poster.Calls() != 0 {
		t.Fatalf("cancelled publication should not post, got %d calls", poster.Calls())
	}

	d2 := New(dir, gh, d1.Prompts, runner, d1.Config, &lockedBuffer{}, 0, false, poster)
	d2.PollInterval = 0
	d2.postBackoffs = []time.Duration{0, 0, 0, 0, 0}
	d2.launchBackoff = func(int) time.Duration { return 0 }
	if entry, ok := d2.peekPendingPost(prNumber, triggerKey); !ok {
		t.Fatalf("restart should rehydrate trigger %s", triggerKey)
	} else if !strings.HasSuffix(entry.runDir, filepath.Join("runs", findReviewRunID(t, dir))) {
		t.Fatalf("restart recovery source = %q, want durable run folder", entry.runDir)
	}

	tickAndWait(t, d2, context.Background())
	if runner.Calls() != 1 {
		t.Fatalf("restart recovery RunBatch calls = %d, want 1", runner.Calls())
	}
	if poster.Calls() != 1 {
		t.Fatalf("restart recovery PostComment calls = %d, want 1", poster.Calls())
	}
	if !d2.IsTerminalSeen(prNumber, triggerKey) {
		t.Fatal("restart recovery should mark the trigger successful")
	}
}

func TestDaemon_CancelAfterDecisionPersistenceFailure_PreservesWorktree(t *testing.T) {
	const (
		prNumber  = 2477
		commentID = "c-cancel-worktree-fallback"
		body      = "## Decision\n**APPROVED**\nworktree fallback\n"
	)
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {{ID: commentID, Body: "/sandman review"}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "worktree fallback", Body: "body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: body}
	poster := &fakeCommentPoster{}
	d1, dir, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d1.CommentPoster = poster
	triggerKey := reviewTriggerKey(gh.comments[prNumber][0])
	branch := reviewBranchName(prNumber, triggerKey)
	stageReviewWorktree(t, worktreeDir, branch)

	ctx, cancel := context.WithCancel(context.Background())
	d1.persistDecision = func(string, []byte) error {
		cancel()
		return errors.New("run folder unavailable")
	}
	tickAndWait(t, d1, ctx)

	if !gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Fatal("cancelled publication must preserve the worktree fallback")
	}
	if !gitBranchExists(t, branch) {
		t.Fatal("cancelled publication must preserve the review branch fallback")
	}
	if entry, ok := d1.peekPendingPost(prNumber, triggerKey); !ok {
		t.Fatalf("pending fallback entry missing for %s", triggerKey)
	} else if entry.runDir != filepath.Join(worktreeDir, branch) {
		t.Fatalf("fallback source = %q, want %q", entry.runDir, filepath.Join(worktreeDir, branch))
	}

	d2 := New(dir, gh, d1.Prompts, runner, d1.Config, &lockedBuffer{}, 0, false, poster)
	d2.PollInterval = 0
	d2.postBackoffs = []time.Duration{0, 0, 0, 0, 0}
	d2.launchBackoff = func(int) time.Duration { return 0 }
	tickAndWait(t, d2, context.Background())

	if runner.Calls() != 1 {
		t.Fatalf("fallback recovery RunBatch calls = %d, want 1", runner.Calls())
	}
	if poster.Calls() != 1 {
		t.Fatalf("fallback recovery PostComment calls = %d, want 1", poster.Calls())
	}
}

func TestDaemon_RestartRehydratesDurableDecisionAfterCleanup(t *testing.T) {
	const (
		prNumber  = 2472
		commentID = "c-durable-restart"
		body      = "## Decision\n**CHANGES_REQUESTED**\nrestart body\n"
	)

	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {{ID: commentID, Body: "/sandman review"}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "durable restart", Body: "body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: body}
	sustained := errors.New("publication unavailable")
	poster := &fakeCommentPoster{errs: []error{sustained, sustained, sustained, sustained, sustained, nil}}
	d1, dir, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d1.CommentPoster = poster
	branch := reviewBranchName(prNumber, commentID)
	stageReviewWorktree(t, worktreeDir, branch)

	tickAndWait(t, d1, context.Background())
	if runner.Calls() != 1 {
		t.Fatalf("first daemon RunBatch calls = %d, want 1", runner.Calls())
	}
	if gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Fatalf("first daemon should clean the review worktree")
	}

	d2 := New(dir, gh, d1.Prompts, runner, d1.Config, &lockedBuffer{}, 0, false, poster)
	d2.PollInterval = 0
	d2.postBackoffs = []time.Duration{0, 0, 0, 0, 0}
	d2.launchBackoff = func(int) time.Duration { return 0 }

	tickAndWait(t, d2, context.Background())

	if runner.Calls() != 1 {
		t.Fatalf("restart recovery RunBatch calls = %d, want 1", runner.Calls())
	}
	if poster.Calls() != PostStepMaxAttempts+1 {
		t.Fatalf("restart recovery PostComment calls = %d, want %d", poster.Calls(), PostStepMaxAttempts+1)
	}
	_, postedBody := poster.Captured()
	if postedBody != body {
		t.Fatalf("restart recovered body = %q, want %q", postedBody, body)
	}
	if !d2.IsTerminalSeen(prNumber, commentID) {
		t.Fatal("restart recovery should mark the review trigger successful")
	}
}

func TestDaemon_DecisionPersistenceFailurePreservesWorktreeForRecovery(t *testing.T) {
	const (
		prNumber  = 2473
		commentID = "c-preserve-on-write-failure"
		body      = "## Decision\n**APPROVED**\npreserved body\n"
	)

	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {{ID: commentID, Body: "/sandman review"}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "preserve review", Body: "body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: body}
	sustained := errors.New("publication unavailable")
	poster := &fakeCommentPoster{errs: []error{sustained, sustained, sustained, sustained, sustained, nil}}
	d1, dir, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d1.CommentPoster = poster
	d1.persistDecision = func(string, []byte) error { return errors.New("run folder unavailable") }
	branch := reviewBranchName(prNumber, commentID)
	stageReviewWorktree(t, worktreeDir, branch)

	tickAndWait(t, d1, context.Background())

	if runner.Calls() != 1 {
		t.Fatalf("first daemon RunBatch calls = %d, want 1", runner.Calls())
	}
	if !gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Fatal("failed durable persistence must preserve the review worktree")
	}
	if !gitBranchExists(t, branch) {
		t.Fatal("failed durable persistence must preserve the review branch")
	}
	statePath := locateReviewStatePath(t, dir)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read pending review state: %v", err)
	}
	if !strings.Contains(string(stateBytes), `"status": "pending"`) {
		t.Fatalf("review state = %s, want pending publication", stateBytes)
	}

	d2 := New(dir, gh, d1.Prompts, runner, d1.Config, &lockedBuffer{}, 0, false, poster)
	d2.PollInterval = 0
	d2.postBackoffs = []time.Duration{0, 0, 0, 0, 0}
	d2.launchBackoff = func(int) time.Duration { return 0 }

	tickAndWait(t, d2, context.Background())

	if runner.Calls() != 1 {
		t.Fatalf("preserved-source recovery RunBatch calls = %d, want 1", runner.Calls())
	}
	if poster.Calls() != PostStepMaxAttempts+1 {
		t.Fatalf("preserved-source recovery PostComment calls = %d, want %d", poster.Calls(), PostStepMaxAttempts+1)
	}
	_, postedBody := poster.Captured()
	if postedBody != body {
		t.Fatalf("preserved-source recovery body = %q, want %q", postedBody, body)
	}
	if !d2.IsTerminalSeen(prNumber, commentID) {
		t.Fatal("preserved-source recovery should mark the review trigger successful")
	}
}

func TestDaemon_DecisionPersistenceFailurePreservesWorktreeAfterSuccessfulPost(t *testing.T) {
	const (
		prNumber  = 2474
		commentID = "c-preserve-after-success"
		body      = "## Decision\n**APPROVED**\nsuccess body\n"
	)

	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {{ID: commentID, Body: "/sandman review"}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "preserve success", Body: "body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: body}
	poster := &fakeCommentPoster{}
	d, dir, worktreeDir := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.CommentPoster = poster
	d.persistDecision = func(string, []byte) error { return errors.New("run folder unavailable") }
	branch := reviewBranchName(prNumber, commentID)
	stageReviewWorktree(t, worktreeDir, branch)

	tickAndWait(t, d, context.Background())

	if runner.Calls() != 1 {
		t.Fatalf("RunBatch calls = %d, want 1", runner.Calls())
	}
	if poster.Calls() != 1 {
		t.Fatalf("PostComment calls = %d, want 1", poster.Calls())
	}
	if gitWorktreeHasBranch(t, worktreeDir, branch) {
		t.Fatal("successful publication should clean up the review worktree")
	}
	if gitBranchExists(t, branch) {
		t.Fatal("successful publication should delete the review branch")
	}
	statePath := locateReviewStatePath(t, dir)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read successful review state: %v", err)
	}
	if !strings.Contains(string(stateBytes), `"status": "success"`) {
		t.Fatalf("review state = %s, want successful publication", stateBytes)
	}
}
