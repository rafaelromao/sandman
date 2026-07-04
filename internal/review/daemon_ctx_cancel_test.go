package review

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// slowFakeGH blocks ListOpenPRs and ListPRComments on the caller's
// context until it is cancelled. Used by the slice-3 regression test
// for issue #1780: a hung gh call must release the calling goroutine
// when the daemon's ctx is cancelled, not wedge it.
type slowFakeGH struct {
	*fakeGH
	blockListOpenPRs  chan struct{}
	blockListComments chan struct{}
	mu                sync.Mutex
	listCount         int
	listCommentCount  int
}

func newSlowFakeGH() *slowFakeGH {
	return &slowFakeGH{
		fakeGH:            &fakeGH{},
		blockListOpenPRs:  make(chan struct{}),
		blockListComments: make(chan struct{}),
	}
}

func (s *slowFakeGH) ListOpenPRs(ctx context.Context) ([]github.PR, error) {
	s.mu.Lock()
	s.listCount++
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.blockListOpenPRs:
		return s.fakeGH.ListOpenPRs(ctx)
	}
}

func (s *slowFakeGH) ListPRComments(ctx context.Context, number int) ([]github.PRComment, error) {
	s.mu.Lock()
	s.listCommentCount++
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.blockListComments:
		return s.fakeGH.ListPRComments(ctx, number)
	}
}

// TestDaemon_HungGhReleaseOnCancel is the slice-3 regression: when a
// hung gh call blocks the daemon's tick, cancelling the daemon ctx
// must release the goroutine within bounded time. Without the
// ctx-aware `gh` plumbing from issue #1780 the daemon would hang
// indefinitely waiting for the response.
func TestDaemon_HungGhReleaseOnCancel(t *testing.T) {
	gh := newSlowFakeGH()
	gh.prs = []github.PR{{Number: 7, State: "open"}}
	gh.comments = map[int][]github.PRComment{7: {{ID: "c1", Body: "/sandman review", CreatedAt: time.Now()}}}

	d, _, _ := newDaemonForTest(t, gh, &capturedRequest{}, &config.Config{})

	ctx, cancel := context.WithCancel(context.Background())
	tickDone := make(chan error, 1)
	go func() {
		tickDone <- d.tick(ctx)
	}()

	// Give tick a moment to enter the hung ListOpenPRs.
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case err := <-tickDone:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("tick returned unexpected error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon tick did not return within 2 s after ctx cancel; gh call wedged it")
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if gh.listCount == 0 {
		t.Fatal("expected at least one ListOpenPRs call before cancel")
	}
}
