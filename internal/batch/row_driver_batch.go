package batch

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

type batchRowDriverGates struct {
	startGate *batchStartGate
	quotaGate *batchQuotaGate
	owner     *waitOwner
	log       events.EventLog
	title     string
	issue     int
	batchID   string
	options   runSessionOptions
	errorLog  io.Writer
}

func (g *batchRowDriverGates) Acquire(ctx context.Context, awaiting bool, opportunity awaitOpportunity, observe func() (rowObservationOutcome, error), interval time.Duration) (awaitOpportunity, rowObservationOutcome, error) {
	if observe != nil {
		return g.startGate.acquireObserved(ctx, &batchStartWaiter{
			awaiting: true, lastChance: opportunity.lastChance,
			ordinaryStartsAtChance: opportunity.ordinaryStarts,
		}, observe, interval)
	}
	if awaiting {
		opportunity, err := g.startGate.AcquireAwaiting(ctx, opportunity)
		return opportunity, rowObservationOutcome{}, err
	}
	opportunity, err := g.startGate.AcquireWithOpportunity(ctx, false)
	return opportunity, rowObservationOutcome{}, err
}

func (g *batchRowDriverGates) QuotaPaused() bool { return g.quotaGate.paused() }

func (g *batchRowDriverGates) WaitQuota(ctx context.Context, observe func() (rowObservationOutcome, error), interval time.Duration) (rowObservationOutcome, error) {
	return g.quotaGate.waitObserved(ctx, observe, interval)
}

func (g *batchRowDriverGates) Wait(ctx context.Context, interval time.Duration) error {
	wait := g.options.awaitWait
	if wait == nil {
		wait = waitForAwaitPoll
	}
	return wait(ctx, interval)
}

func (g *batchRowDriverGates) Release(started bool) {
	if started {
		g.startGate.Release()
	} else {
		g.startGate.ReleaseWithoutDelay()
	}
}

func (g *batchRowDriverGates) RecordQuotaPaused(run RunSpec, cursor ContinuationCursor, awaiting, ready bool, at time.Time) error {
	if g.log == nil || awaiting && !ready {
		return nil
	}
	extras := map[string]any{
		"gate": "usage-limit", "reason": "usage-limit-paused",
		"next_action": "resume after provider usage limit resets; Sandman did not start another run while suspended",
	}
	row := run.row(cursor)
	err := logCapacityQueuedContinuationAt(g.log, at, run.RunID, run.IssueNumber, g.batchID, row, extras, g.title)
	if err != nil && g.errorLog != nil {
		fmt.Fprintf(g.errorLog, "warning: persist usage-limit pause for issue %d: %v\n", g.issue, err)
	}
	return err
}

func (g *batchRowDriverGates) RecordReady(run RunSpec, cursor ContinuationCursor, extras map[string]any, at time.Time) error {
	err := logCapacityQueuedContinuationAt(g.log, at, run.RunID, run.IssueNumber, g.batchID, run.row(cursor), extras, g.title)
	if err != nil && g.errorLog != nil {
		fmt.Fprintf(g.errorLog, "warning: persist ready continuation for issue %d: %v; keeping the run in its external wait\n", g.issue, err)
	}
	return err
}

func (g *batchRowDriverGates) Checkpoint(run RunSpec, cursor ContinuationCursor, ready bool, interval time.Duration) error {
	return g.owner.checkpoint(run.row(cursor), ready, interval)
}

func (g *batchRowDriverGates) Report(result AgentRunResult, wasProbe bool) {
	g.quotaGate.report(g.issue, result, wasProbe)
}

type batchRowDriverActions struct {
	orchestrator *Orchestrator
	executor     *runExecutor
}

func (a batchRowDriverActions) Abort(ctx context.Context, run RunSpec, cursor ContinuationCursor, result AgentRunResult) AgentRunResult {
	return a.orchestrator.applyAbort(ctx, a.executor, run.row(cursor), result, nil)
}

// rowTurnController owns its idempotence state rather than exposing a mutable
// flag through a closure captured by RunBatch.
type rowTurnController struct {
	enabled        bool
	turn           int
	mu             *sync.Mutex
	cond           *sync.Cond
	servingTurn    *int
	completedTurns map[int]struct{}
	once           sync.Once
	stateMu        sync.Mutex
	advanced       bool
}

func (t *rowTurnController) Advance() {
	if t == nil || !t.enabled {
		return
	}
	t.once.Do(func() {
		t.mu.Lock()
		t.completedTurns[t.turn] = struct{}{}
		for {
			if _, ok := t.completedTurns[*t.servingTurn]; !ok {
				break
			}
			delete(t.completedTurns, *t.servingTurn)
			*t.servingTurn++
		}
		t.cond.Broadcast()
		t.mu.Unlock()
		t.stateMu.Lock()
		t.advanced = true
		t.stateMu.Unlock()
	})
}

func (t *rowTurnController) Advanced() bool {
	if t == nil || !t.enabled {
		return false
	}
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.advanced
}

func (t *rowTurnController) Wait(ctx context.Context, abort func()) bool {
	if t == nil || !t.enabled || t.Advanced() {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for *t.servingTurn != t.turn {
		if ctx.Err() != nil {
			abort()
			return false
		}
		t.cond.Wait()
	}
	return true
}
