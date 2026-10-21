package review

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func runSnapshotGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestReviewSnapshotReachesRunnerSeparatelyFromComparisonBase(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-snap-")
	t.Chdir(root)
	initReviewTestGitRepo(t, root)
	base := filepath.Join(root, ".sandman")
	cfg := &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model", WorktreeDir: filepath.Join(base, "worktrees")}
	const head = "6ac5aac4f6be27726dcd2adb60e78e86767a14bc"
	gh := launchBudgetGitHub{&fakeGH{prFetch: map[int]*github.PR{36: {Number: 36, HeadRefOid: head, HeadRefName: "fork-feature", BaseRefName: "release"}}}}
	called := false
	runner := batchFunc(func(_ context.Context, req batch.Request) (*batch.Result, error) {
		called = true
		if req.ReviewHeadSHA != head || req.BaseBranch != "release" {
			t.Errorf("review checkout = %q, comparison base = %q", req.ReviewHeadSHA, req.BaseBranch)
		}
		return nil, errors.New("stop before launch")
	})
	d := New(base, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, nil)
	folder, id, session, state, err := d.prepareReviewRun(context.Background(), 36, "request")
	if err != nil {
		t.Fatal(err)
	}
	_ = d.launchReview(context.Background(), 36, "", "request", "", "", folder, id, session, state, false)
	if !called {
		t.Fatal("runner did not receive review")
	}
}

func TestReviewSnapshotHeadChangeDoesNotPublishOrConsumeRequest(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-pub-")
	t.Chdir(root)
	initReviewTestGitRepo(t, root)
	gh := &fakeGH{prs: []github.PR{{Number: 36, State: "open"}}, prFetch: map[int]*github.PR{36: {Number: 36, HeadRefOid: "old-head"}}, comments: map[int][]github.PRComment{36: {{ID: "request", Body: "/sandman review", CreatedAt: time.Now()}}}}
	runner := batchFunc(func(_ context.Context, req batch.Request) (*batch.Result, error) {
		gh.mu.Lock()
		gh.prFetch[36].HeadRefOid = "new-head"
		gh.mu.Unlock()
		return &batch.Result{}, writeDecisionToWorktree(req, "APPROVED")
	})
	d := New(filepath.Join(root, ".sandman"), launchBudgetGitHub{gh}, &prompt.Engine{}, runner, &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model", WorktreeDir: filepath.Join(root, ".sandman", "worktrees")}, &lockedBuffer{}, 1, true, nil)
	posts := 0
	d.CommentPoster = ownershipCommentPoster(func(context.Context, int, string) error { posts++; return nil })
	tickAndWait(t, d, context.Background())
	if posts != 0 || d.IsTerminalSeen(36, "request") {
		t.Fatalf("stale review published/consumed: posts=%d terminal=%v", posts, d.IsTerminalSeen(36, "request"))
	}
}
