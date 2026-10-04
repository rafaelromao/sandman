package review

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/reviewlaunch"
)

type launchBudgetGitHub struct{ *fakeGH }

func (g launchBudgetGitHub) FetchPR(_ context.Context, number int) (*github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := *g.prFetch[number]
	return &pr, nil
}

func TestReviewerLaunchBudgetSurvivesDaemonRestart(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gh := &fakeGH{
		prs:      []github.PR{{Number: 17, State: "open", HeadRefOid: "current-head"}},
		comments: map[int][]github.PRComment{17: {{ID: "request", Body: "/sandman review", CreatedAt: time.Now().UTC()}}},
		prFetch:  map[int]*github.PR{17: {Number: 17, State: "open", HeadRefOid: "current-head"}},
	}
	runner := &failureRunner{err: errors.New("reviewer cannot launch")}
	for i := 0; i < 5; i++ {
		d := New(dir, launchBudgetGitHub{gh}, &prompt.Engine{}, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "opencode/foo"}, &lockedBuffer{}, 1, true, nil)
		d.launchBackoff = func(int) time.Duration { return 0 }
		tickAndWait(t, d, context.Background())
	}
	if got := runner.calls.Load(); got != 3 {
		t.Fatalf("review launches across restart=%d, want three bounded failures", got)
	}
	budget, err := reviewlaunch.Read(filepath.Join(dir, "state"), 17, "request", "current-head")
	if err != nil || budget.Attempts != 3 {
		t.Fatalf("durable request budget=%+v error=%v", budget, err)
	}
	gh.comments[17] = append(gh.comments[17], github.PRComment{ID: "new-request", Body: "/sandman review", CreatedAt: time.Now().UTC().Add(time.Second)})
	d := New(dir, launchBudgetGitHub{gh}, &prompt.Engine{}, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "opencode/foo"}, &lockedBuffer{}, 1, true, nil)
	d.launchBackoff = func(int) time.Duration { return 0 }
	tickAndWait(t, d, context.Background())
	if got := runner.calls.Load(); got != 4 {
		t.Fatalf("fresh request failed to receive its own budget: launches=%d", got)
	}
}
