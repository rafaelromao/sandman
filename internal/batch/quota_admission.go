package batch

import (
	"context"
	"errors"
	"sync"
	"time"
)

var errQuotaUnavailable = errors.New("provider quota recovery exhausted")

// batchQuotaGate governs admission, not lifecycle. Deferred rows retain their
// logical owner, cancel registration and dependency channels while this gate
// is held. Recovery wakes them; expiry fails them without another agent launch.
type batchQuotaGate struct {
	mu      sync.Mutex
	limited map[int]bool
	failed  bool
	wake    chan struct{}
}

func newBatchQuotaGate() *batchQuotaGate {
	return &batchQuotaGate{limited: map[int]bool{}, wake: make(chan struct{})}
}

func (g *batchQuotaGate) paused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failed || len(g.limited) > 0
}

// A cancelled or otherwise terminal row no longer owns a recovery episode.
// Retirement wakes admission so another owned row can re-test availability.
func (g *batchQuotaGate) retire(issue int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.limited, issue)
	close(g.wake)
	g.wake = make(chan struct{})
}

func (g *batchQuotaGate) report(issue int, result AgentRunResult, wasProbe bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case result.Status == "aborted" && g.limited[issue]:
		delete(g.limited, issue)
	case result.Status == "success":
		delete(g.limited, issue)
	case result.UsageLimitReached && result.Status == "await":
		g.limited[issue] = true
	case result.UsageLimitReached && result.Status == "failure":
		g.failed = true
		delete(g.limited, issue)
	case wasProbe && !result.UsageLimitReached && result.Status == "await":
		delete(g.limited, issue)
	case wasProbe && result.Status == "failure":
		g.failed = true
		delete(g.limited, issue)
	}
	close(g.wake)
	g.wake = make(chan struct{})
}

// modelProgress clears only the recovery owner's pause. It is called from a
// selected agent strategy while that attempt is still running, so eligible
// siblings can acquire capacity without waiting for the owner to return.
func (g *batchQuotaGate) modelProgress(issue int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failed {
		return
	}
	if _, ok := g.limited[issue]; ok {
		delete(g.limited, issue)
		close(g.wake)
		g.wake = make(chan struct{})
	}
}

func (g *batchQuotaGate) limit(issue int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failed || g.limited[issue] {
		return
	}
	g.limited[issue] = true
	close(g.wake)
	g.wake = make(chan struct{})
}

func (g *batchQuotaGate) wait(ctx context.Context) error {
	return g.waitObserved(ctx, nil, 0)
}

func (g *batchQuotaGate) waitObserved(ctx context.Context, observe func() error, interval time.Duration) error {
	var tick <-chan time.Time
	if observe != nil {
		if interval <= 0 {
			interval = time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		if observe != nil {
			if err := observe(); err != nil {
				return err
			}
		}
		g.mu.Lock()
		failed, paused, wake := g.failed, len(g.limited) > 0, g.wake
		g.mu.Unlock()
		if failed {
			return errQuotaUnavailable
		}
		if !paused {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-tick:
		}
	}
}
