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

// TestDaemon_PR1942_DecisionInWorktreeFallback is the canonical
// regression test for the PR #1942 failure shape: the reviewer bot
// wrote decision.md to its worktree CWD instead of the run folder
// the daemon used to read from. Issue #1953 makes the worktree the
// canonical artifact path; this test pins the new contract end-to-
// end.
//
// The test reproduces the exact on-disk shape seen in PR #1942:
//
//   - <runFolder>/decision.md is absent.
//   - <worktree>/decision.md exists with the redacted body.
//   - run.json carries the WorktreePath that links the two.
//
// Asserts the daemon posts the redacted body and records
// MarkSeen("success").
func TestDaemon_PR1942_DecisionInWorktreeFallback(t *testing.T) {
	const (
		prNumber  = 1942
		commentID = "4907298566"
		workBody  = "## Summary\nPR 1942 review (canonical worktree). /sandman ok.\n"
	)

	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{
			prNumber: {{ID: commentID, Body: "/sandman review", CreatedAt: time.Now()}},
		},
		prFetch: map[int]*github.PR{prNumber: {Number: prNumber, Title: "PR 1942", Body: "Body"}},
	}

	// Captured by the seamRunner so the test can assert that the
	// canonical-worktree path was used as the prompt's {{RUN_DIR}}.
	var capturedRunDir string
	runner := &seamRunner{
		capturedRequest: &capturedRequest{},
		worktreeDir:     ".sandman/worktrees",
		beforeReturn: func(req batch.Request) {
			capturedRunDir = req.RunDir
		},
	}

	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
		WorktreeDir:        ".sandman/worktrees",
	}

	poster := &fakeCommentPoster{}
	d, _, dir := newDaemonForTestS3(t, gh, runner, cfg, poster)

	// Pre-create the worktree dir + decision.md BEFORE the first
	// tick fires. The daemon's launchReview computes the worktree
	// path deterministically from (prNumber, commentID,
	// cfg.WorktreeDir) and reads decision.md from there on the
	// first tick.
	worktreePath := d.reviewWorktreePath(prNumber, commentID)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "decision.md"), []byte(workBody), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	tickAndWait(t, d, context.Background())

	if capturedRunDir == "" {
		t.Fatal("seamRunner did not capture req.RunDir; aborting")
	}
	if got := poster.Calls(); got != 1 {
		t.Fatalf("expected exactly 1 PostComment call from the canonical-worktree path, got %d", got)
	}
	gotPR, gotBody := poster.Captured()
	if gotPR != prNumber {
		t.Errorf("PostComment pr=%d, want %d", gotPR, prNumber)
	}
	wantBody := RedactBody(workBody)
	if gotBody != wantBody {
		t.Errorf("posted body mismatch:\n want=%q\n got =%q", wantBody, gotBody)
	}

	// MarkSeen("success") must be persisted.
	statePath := locateReviewStatePath(t, dir)
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
}
