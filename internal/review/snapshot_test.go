package review

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

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
