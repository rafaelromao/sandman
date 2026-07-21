package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestPostDecision_PersistsDecisionMDToRunFolder pins issue #2224:
// after postDecision successfully reads decision.md from the worktree and
// posts the redacted body, the decision body must be persisted to
// <reviewRunFolder>/decision.md so the portal can still find it after
// the worktree is cleaned up by ClearReviewArtifacts.
//
// Regression guard: before this fix, decision.md lived only in the
// worktree. The launchReview defer fires ClearReviewArtifacts, which
// runs `git worktree remove --force`, deleting the worktree directory
// and decision.md with it. The portal then tried to read
// <worktreePath>/decision.md (from the manifest's WorktreePath, stamped
// by #2220) and found nothing, rendering Unclear for every
// review row.
func TestPostDecision_PersistsDecisionMDToRunFolder(t *testing.T) {
	const (
		prNumber  = 2224
		commentID = "c-persist"
		body      = "## Decision\n**APPROVED**\n"
	)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		prFetch: map[int]*github.PR{
			prNumber: {Number: prNumber, Title: "PR 2224", Body: "Body"},
		},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review"}},
		},
	}
	runner := &seamRunner{
		capturedRequest: &capturedRequest{},
		worktreeDir:     ".sandman/worktrees",
		beforeReturn: func(req batch.Request) {
			worktreePath := filepath.Join(req.WorktreeDir, req.PromptConfig.Branch)
			if err := os.MkdirAll(worktreePath, 0755); err != nil {
				t.Fatalf("mkdir worktree: %v", err)
			}
			if err := os.WriteFile(filepath.Join(worktreePath, "decision.md"), []byte(body), 0644); err != nil {
				t.Fatalf("write decision.md: %v", err)
			}
		},
	}
	poster := &fakeCommentPoster{}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		WorktreeDir:        ".sandman/worktrees",
	}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)

	tickAndWait(t, d, context.Background())

	if poster.Calls() != 1 {
		t.Fatalf("expected 1 PostComment call, got %d", poster.Calls())
	}

	// Locate the review run folder — the issue #2224 contract writes
	// decision.md there so it survives ClearReviewArtifacts.
	batchID := findReviewBatchID(t, dir)
	runID := findReviewRunID(t, dir)
	runDecisionPath := filepath.Join(dir, "batches", batchID, "runs", runID, "decision.md")
	gotBody, err := os.ReadFile(runDecisionPath)
	if err != nil {
		t.Fatalf("postDecision must persist decision.md to <reviewRunFolder>/decision.md so it survives worktree cleanup, got read err: %v (worktree will be removed by defer ClearReviewArtifacts; portal verdict reader needs this copy)", err)
	}
	if string(gotBody) != body {
		t.Errorf("run-folder decision.md body = %q, want %q (postDecision must copy the exact body it posted, verbatim, so the portal verdict reader sees what the agent wrote)", string(gotBody), body)
	}
}

// TestPostDecision_PersistsBeforeCleanup pins the ordering invariant for
// issue #2224: the run-folder copy must exist BEFORE ClearReviewArtifacts
// removes the worktree. The launchReview defer fires after postDecision
// returns, so postDecision must complete the copy before it returns.
// This test asserts the run-folder copy exists immediately after the
// tick completes (which is after postDecision returns and the defer has
// fired — meaning the copy happened before the defer fired).
func TestPostDecision_PersistsBeforeCleanup(t *testing.T) {
	const (
		prNumber  = 2224
		commentID = "c-ordering"
		body      = "## Decision\n**CHANGES_REQUESTED**\n"
	)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		prFetch: map[int]*github.PR{
			prNumber: {Number: prNumber, Title: "PR 2224 ordering", Body: "Body"},
		},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review"}},
		},
	}
	runner := &seamRunner{
		capturedRequest: &capturedRequest{},
		worktreeDir:     ".sandman/worktrees",
		beforeReturn: func(req batch.Request) {
			worktreePath := filepath.Join(req.WorktreeDir, req.PromptConfig.Branch)
			if err := os.MkdirAll(worktreePath, 0755); err != nil {
				t.Fatalf("mkdir worktree: %v", err)
			}
			if err := os.WriteFile(filepath.Join(worktreePath, "decision.md"), []byte(body), 0644); err != nil {
				t.Fatalf("write decision.md: %v", err)
			}
		},
	}
	poster := &fakeCommentPoster{}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		WorktreeDir:        ".sandman/worktrees",
	}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)

	tickAndWait(t, d, context.Background())

	// After the tick: postDecision returned successfully, the defer
	// fired, ClearReviewArtifacts removed the worktree. The run-folder
	// copy must exist (postDecision wrote it before returning).
	batchID := findReviewBatchID(t, dir)
	runID := findReviewRunID(t, dir)
	runDecisionPath := filepath.Join(dir, "batches", batchID, "runs", runID, "decision.md")
	gotBody, err := os.ReadFile(runDecisionPath)
	if err != nil {
		t.Fatalf("run-folder decision.md must exist after postDecision returns and the worktree is cleaned up; got read err: %v", err)
	}
	if string(gotBody) != body {
		t.Errorf("run-folder decision.md body = %q, want %q", string(gotBody), body)
	}
}

// TestPostDecision_NoCopyOnMissingDecisionMD pins the negative case for
// issue #2224: when decision.md is missing from the worktree (the
// retryable-missing branch, issue #1949), postDecision must NOT write a
// run-folder copy. The copy is a success-path optimization; copying an
// absent file would create a phantom decision.md that the portal would
// render as a verdict, masking the real failure.
func TestPostDecision_NoCopyOnMissingDecisionMD(t *testing.T) {
	const (
		prNumber  = 2224
		commentID = "c-missing"
	)
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		prFetch: map[int]*github.PR{
			prNumber: {Number: prNumber, Title: "PR 2224 missing", Body: "Body"},
		},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review"}},
		},
	}
	runner := &seamRunner{
		capturedRequest: &capturedRequest{},
		worktreeDir:     ".sandman/worktrees",
		// No beforeReturn: decision.md is never written.
	}
	poster := &fakeCommentPoster{}
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		WorktreeDir:        ".sandman/worktrees",
	}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)

	tickAndWait(t, d, context.Background())

	if poster.Calls() != 0 {
		t.Fatalf("postDecision must NOT call PostComment when decision.md is missing, got %d calls", poster.Calls())
	}

	batchID := findReviewBatchID(t, dir)
	runID := findReviewRunID(t, dir)
	runDecisionPath := filepath.Join(dir, "batches", batchID, "runs", runID, "decision.md")
	if _, err := os.Stat(runDecisionPath); err == nil {
		t.Errorf("run-folder decision.md must NOT exist when the worktree's decision.md was missing (the copy is a success-path optimization; copying an absent file would create a phantom verdict that masks the real failure)")
	}
}

// findReviewBatchID locates the per-review batch directory under the
// test daemon's base dir. The review daemon creates one batch dir per
// (pr, comment) pair (see runid.NewBatchID), so there is exactly one
// entry under <dir>/batches/ after a single-tick test.
func findReviewBatchID(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "batches"))
	if err != nil {
		t.Fatalf("read batches dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 review batch under %s, got %d", filepath.Join(dir, "batches"), len(entries))
	}
	return entries[0].Name()
}

// findReviewRunID locates the per-row run directory under the review
// batch. The review daemon writes one runs/<runID> per launch.
func findReviewRunID(t *testing.T, dir string) string {
	t.Helper()
	batchID := findReviewBatchID(t, dir)
	runsDir := filepath.Join(dir, "batches", batchID, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 run under %s, got %d", runsDir, len(entries))
	}
	return entries[0].Name()
}
