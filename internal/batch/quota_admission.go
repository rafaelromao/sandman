package batch

import (
	"context"
	"errors"
	"sync"
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

func (g *batchQuotaGate) report(issue int, result AgentRunResult, wasProbe bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case result.Status == "success":
		delete(g.limited, issue)
	case result.UsageLimitReached && result.Status == "await":
		g.limited[issue] = true
	case result.UsageLimitReached && result.Status == "failure":
		g.failed = true
		delete(g.limited, issue)
	case wasProbe && !result.UsageLimitReached:
		delete(g.limited, issue)
	}
	close(g.wake)
	g.wake = make(chan struct{})
}

func (g *batchQuotaGate) wait(ctx context.Context) error {
	for {
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
		}
	}
}
