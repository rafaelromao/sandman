package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestDaemon_PR1942_DecisionInWorktreeFallback is the regression test
// for the PR #1942 failure shape: the reviewer bot wrote decision.md
// to its worktree CWD instead of the run folder the daemon reads
// from, and the daemon previously marked the request as terminal
// failure because the run-folder stat returned ENOENT. After the fix,
// postDecision consults run.json's WorktreePath, reads the artifact
// from the worktree, posts it, and records MarkSeen("success").
//
// The test reproduces the exact on-disk shape seen in PR #1942:
//
//   - <runFolder>/decision.md is absent.
//   - <worktreePath>/decision.md exists with the redacted body.
//   - run.json carries the worktreePath that links the two.
//
// It is a behavioural seam test for the root-cause fix: the
// worktree-fallback rescue happens inside postDecision, after
// RunBatch has returned and before the daemon decides between
// failure and success.
func TestDaemon_PR1942_DecisionInWorktreeFallback(t *testing.T) {
	const (
		prNumber  = 1942
		commentID = "4907298566"
		workBody  = "## Summary\nPR 1942 review (worktree fallback). /sandman ok.\n"
	)

	// Worktree path is repo-relative per batchindex.RunManifest.
	// The daemon's baseDir is the test's tmp dir; the worktree
	// resolves under <baseDir>/<worktreeRel>.
	worktreeRel := filepath.Join(".sandman", "worktrees", "sandman", "review-1942-4907298566")

	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: time.Now()}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 1942", Body: "Body"}},
	}

	// Captured by the daemon construction so the beforeReturn hook
	// can resolve the repo-relative worktree path.
	var baseDir string
	var capturedRunDir string

	runner := &seamRunner{
		capturedRequest: &capturedRequest{},
		beforeReturn: func(req batch.Request) {
			capturedRunDir = req.RunDir
			// Patch run.json's WorktreePath with the deterministic
			// relative path. The real flow has the orchestrator
			// overwrite this field after RunBatch returns; the
			// test captures that effect inline so the daemon-only
			// seam (no orchestrator) still exercises the fallback.
			manifestPath := filepath.Join(req.RunDir, "run.json")
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				return
			}
			var manifest batchindex.RunManifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				return
			}
			manifest.WorktreePath = worktreeRel
			patched, err := json.Marshal(manifest)
			if err != nil {
				return
			}
			if err := os.WriteFile(manifestPath, patched, 0644); err != nil {
				return
			}
		},
	}

	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}

	poster := &fakeCommentPoster{}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)
	baseDir = dir

	// Pre-create the worktree directory + decision.md BEFORE the
	// first tick fires. The daemon's prepareReviewRun does NOT
	// create the worktree (the real container sandbox does); the
	// fallback resolver reads the worktree path recorded in
	// run.json, so writing the artifact here mirrors the real
	// container-sandbox shape where the agent has already written
	// decision.md before the daemon's post step runs.
	wtDir := filepath.Join(baseDir, worktreeRel)
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "decision.md"), []byte(workBody), 0644); err != nil {
		t.Fatalf("write decision.md in worktree: %v", err)
	}

	tickAndWait(t, d, context.Background())

	if capturedRunDir == "" {
		t.Fatal("seamRunner did not capture req.RunDir; aborting")
	}
	if got := poster.Calls(); got != 1 {
		t.Fatalf("expected exactly 1 PostComment call from the worktree fallback, got %d", got)
	}
	gotPR, gotBody := poster.Captured()
	if gotPR != prNumber {
		t.Errorf("PostComment pr=%d, want %d", gotPR, prNumber)
	}
	wantBody := RedactBody(workBody)
	if gotBody != wantBody {
		t.Errorf("posted body mismatch:\n want=%q\n got =%q", wantBody, gotBody)
	}

	// MarkSeen("success") must be persisted on the first tick.
	statePath := locateReviewStatePath(t, baseDir)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review-state.json: %v", err)
	}
	var state batchindex.ReviewState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("unmarshal review-state.json: %v", err)
	}
	gotSuccess := false
	for _, sc := range state.SeenComments {
		if sc.CommentID == commentID && sc.Status == "success" {
			gotSuccess = true
			break
		}
	}
	if !gotSuccess {
		t.Errorf("review-state.json missing MarkSeen(success) for %s: %s", commentID, string(stateBytes))
	}
	// Sanity: the run folder really was missing decision.md (the
	// fallback had to do the work), so a regression that silently
	// reads from the run folder would be caught here.
	if _, err := os.Stat(filepath.Join(capturedRunDir, "decision.md")); err == nil {
		t.Errorf("run folder unexpectedly has decision.md; the fallback did not need to fire")
	}
}
