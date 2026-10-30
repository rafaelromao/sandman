package batch

import (
	"context"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

type rowDriverExecutor interface {
	ExecuteRun(context.Context, RunSpec, ContinuationCursor) (AgentRunResult, bool)
	ObserveAwaitingRun(context.Context, RunSpec, ContinuationCursor) lifecycleVerdict
	PersistAwait(context.Context, RunSpec, ContinuationCursor, map[string]any) error
	FinishObserved(context.Context, RunSpec, ContinuationCursor, lifecycleVerdict) AgentRunResult
}

type rowDriverActions interface {
	Abort(context.Context, RunSpec, ContinuationCursor, AgentRunResult) AgentRunResult
}

// rowDriverGates adapts batch-wide admission and per-run ownership to the
// row-scoped lifecycle. The driver chooses when to wait, acquire, checkpoint,
// and release; RunBatch continues to own the concrete schedulers.
type rowDriverGates interface {
	Acquire(context.Context, bool, awaitOpportunity, func() (rowObservationOutcome, error), time.Duration) (awaitOpportunity, rowObservationOutcome, error)
	QuotaPaused() bool
	WaitQuota(context.Context, func() (rowObservationOutcome, error), time.Duration) (rowObservationOutcome, error)
	Wait(context.Context, time.Duration) error
	Release(bool)
	RecordQuotaPaused(RunSpec, ContinuationCursor, bool, bool, time.Time) error
	RecordReady(RunSpec, ContinuationCursor, map[string]any, time.Time) error
	Checkpoint(RunSpec, ContinuationCursor, bool, time.Duration) error
	Report(AgentRunResult, bool)
}

type rowDriverClock interface {
	Now() time.Time
}

type systemRowDriverClock struct{}

func (systemRowDriverClock) Now() time.Time { return time.Now() }

type rowDriverClockFunc func() time.Time

func (f rowDriverClockFunc) Now() time.Time { return f() }

type rowObservationKind uint8

const (
	rowObservationNone rowObservationKind = iota
	rowObservationTerminal
	rowObservationAcceptanceFailed
	rowObservationAdmissionChanged
)

// rowObservationOutcome carries lifecycle decisions independently from gate
// operation errors. Terminal observation, failed acceptance, and changed
// admission are data rather than sentinel errors.
type rowObservationOutcome struct {
	Kind    rowObservationKind
	Verdict lifecycleVerdict
	Err     error
}

type rowDriverOutcome struct {
	Result     AgentRunResult
	Started    bool
	Cursor     ContinuationCursor
	ObservedAt time.Time
	Err        error
}

type rowDriverOptions struct {
	Awaiting             bool
	Ready                bool
	ReleaseAwaitCapacity bool
	Recovering           bool
	NextPollAt           time.Time
	Opportunity          awaitOpportunity
	AdvanceTurn          func()
	OnYield              func()
	PollInterval         func(int) time.Duration
}

// rowDriver owns all mutable lifecycle state for one admitted Run. Only its
// continuation cursor changes during re-entry; RunSpec remains stable.
type rowDriver struct {
	executor rowDriverExecutor
	actions  rowDriverActions
	gates    rowDriverGates
	clock    rowDriverClock
}

func newRowDriver(executor rowDriverExecutor, gates rowDriverGates, clock rowDriverClock, actions ...rowDriverActions) *rowDriver {
	if clock == nil {
		clock = systemRowDriverClock{}
	}
	driver := &rowDriver{executor: executor, gates: gates, clock: clock}
	if len(actions) > 0 {
		driver.actions = actions[0]
	}
	return driver
}

func (d *rowDriver) Drive(ctx context.Context, run RunSpec, cursor ContinuationCursor, options ...rowDriverOptions) rowDriverOutcome {
	var opts rowDriverOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.PollInterval == nil {
		opts.PollInterval = func(int) time.Duration { return time.Minute }
	}
	state := rowDriverState{
		cursor: cursor, awaiting: opts.Awaiting, ready: opts.Ready,
		releaseAwaitCapacity: opts.ReleaseAwaitCapacity,
		recovering:           opts.Recovering, nextPollAt: opts.NextPollAt,
		opportunity: opts.Opportunity,
	}
	result, started, err := d.drive(ctx, run, &state, opts)
	return rowDriverOutcome{Result: result, Started: started, Cursor: state.cursor, ObservedAt: d.clock.Now(), Err: err}
}

type rowDriverState struct {
	cursor               ContinuationCursor
	awaitPoll            int
	awaiting             bool
	ready                bool
	releaseAwaitCapacity bool
	recovering           bool
	nextPollAt           time.Time
	opportunity          awaitOpportunity
	started              bool
	yielded              bool
}

func (d *rowDriver) drive(ctx context.Context, run RunSpec, state *rowDriverState, opts rowDriverOptions) (AgentRunResult, bool, error) {
	var result AgentRunResult
	abortRequested := false
	wait := func(interval time.Duration) error {
		return d.gates.Wait(ctx, interval)
	}
	nextPoll := func() time.Duration {
		if state.cursor.quotaAllowance.probing() {
			return usageLimitPollInterval
		}
		if state.recovering && state.nextPollAt.After(d.clock.Now()) {
			return state.nextPollAt.Sub(d.clock.Now())
		}
		return opts.PollInterval(state.awaitPoll)
	}
	accept := func(verdict lifecycleVerdict) error {
		switch verdict.Action {
		case lifecycleAwait:
			if verdict.Evidence != nil {
				if err := d.executor.PersistAwait(ctx, run, state.cursor, verdict.Evidence); err != nil {
					return err
				}
			}
			state.awaiting, state.ready = true, false
			if next, ok := githubRecoveryNextAttempt(verdict.Evidence); ok && next.After(d.clock.Now()) {
				// A GitHub recovery wait sleeps until its next hourly attempt
				// instead of re-observing every poll interval.
				state.recovering, state.nextPollAt = true, next
			}
		case lifecycleResume:
			if err := d.gates.RecordReady(run, state.cursor, verdict.Evidence, d.clock.Now()); err != nil {
				state.awaiting, state.ready = true, false
				return d.gates.Checkpoint(run, state.cursor, false, nextPoll())
			}
			state.awaiting, state.ready = true, true
		}
		if state.ready {
			return d.gates.Checkpoint(run, state.cursor, true, 0)
		}
		return d.gates.Checkpoint(run, state.cursor, false, nextPoll())
	}
	waitNext := func() error {
		interval := nextPoll()
		if state.recovering && state.nextPollAt.After(d.clock.Now()) {
			state.recovering = false
		}
		state.awaitPoll++
		return wait(interval)
	}
	acceptQuotaReady := func() error {
		state.cursor.quotaAllowance = state.cursor.quotaAllowance.startProbe(d.clock.Now())
		extras := map[string]any{"gate": "usage-limit", "reason": "quota-probe-ready", "next_action": "recheck provider availability and continue through configured ordinary retries when polling is consumed"}
		for key, value := range state.cursor.quotaAllowance.quotaReadyEvidence() {
			extras[key] = value
		}
		if err := d.gates.RecordReady(run, state.cursor, extras, d.clock.Now()); err != nil {
			state.cursor.quotaAllowance = state.cursor.quotaAllowance.withoutAccounting()
			state.cursor.ReuseSession = state.cursor.quotaAllowance.allowSessionReuse(state.cursor.ReuseSession)
		}
		state.awaiting, state.ready = true, true
		return d.gates.Checkpoint(run, state.cursor, true, 0)
	}
	selected := func(verdict lifecycleVerdict) rowObservationOutcome {
		return rowObservationOutcome{Kind: rowObservationTerminal, Verdict: verdict}
	}
	for {
		if ctx.Err() != nil {
			abortRequested = true
			break
		}
		if state.cursor.quotaAllowance.probing() {
			verdict := d.executor.ObserveAwaitingRun(ctx, run, state.cursor)
			if ctx.Err() != nil {
				abortRequested = true
				break
			}
			if verdict.supersedesQuotaProbe() {
				result = d.executor.FinishObserved(ctx, run, state.cursor, verdict)
				d.gates.Report(result, true)
				break
			}
			if state.recovering && state.nextPollAt.After(d.clock.Now()) {
				interval := state.nextPollAt.Sub(d.clock.Now())
				if err := wait(interval); err != nil {
					abortRequested = true
					break
				}
				state.cursor.quotaAllowance = state.cursor.quotaAllowance.completedPoll(interval)
				state.recovering = false
				if err := acceptQuotaReady(); err != nil {
					abortRequested = true
					break
				}
			}
		}
		if state.awaiting && !state.ready && !state.cursor.quotaAllowance.probing() {
			verdict := d.executor.ObserveAwaitingRun(ctx, run, state.cursor)
			if ctx.Err() != nil {
				abortRequested = true
				break
			}
			if !verdict.handled() {
				verdict = failureForUnhandledObservation(verdict)
			}
			if verdict.Action != lifecycleAwait && verdict.Action != lifecycleResume {
				result = d.executor.FinishObserved(ctx, run, state.cursor, verdict)
				break
			}
			if err := accept(verdict); err != nil {
				abortRequested = true
				break
			}
			if !state.ready {
				if err := waitNext(); err != nil {
					abortRequested = true
					break
				}
				continue
			}
		}
		if !state.cursor.quotaAllowance.probing() && d.gates.QuotaPaused() {
			if err := d.gates.RecordQuotaPaused(run, state.cursor, state.awaiting, state.ready, d.clock.Now()); err != nil {
				// A pause projection is diagnostic; ownership and admission remain
				// authoritative even when this compatibility event cannot be written.
			}
			if err := d.gates.Checkpoint(run, state.cursor, !state.awaiting || state.ready, 0); err != nil {
				abortRequested = true
				break
			}
			if !state.releaseAwaitCapacity {
				status := "queued"
				if state.awaiting {
					status = "await"
				}
				result = AgentRunResult{IssueNumber: run.IssueNumber, Issue: issueRef(run.IssueNumber), RunID: run.RunID, Status: status, Branch: run.Branch}
				break
			}
			var observe func() (rowObservationOutcome, error)
			if state.awaiting {
				observe = func() (rowObservationOutcome, error) {
					verdict := d.executor.ObserveAwaitingRun(ctx, run, state.cursor)
					if ctx.Err() != nil {
						return rowObservationOutcome{}, ctx.Err()
					}
					if verdict.Action == lifecycleAwait || verdict.Action == lifecycleResume && !state.ready {
						if err := accept(verdict); err != nil {
							return rowObservationOutcome{Kind: rowObservationAcceptanceFailed, Err: err}, nil
						}
						return rowObservationOutcome{}, nil
					}
					if verdict.handled() && verdict.Action != lifecycleResume && verdict.Action != lifecycleAwait {
						return selected(verdict), nil
					}
					return rowObservationOutcome{}, nil
				}
			}
			if opts.AdvanceTurn != nil {
				opts.AdvanceTurn()
			}
			outcome, err := d.gates.WaitQuota(ctx, observe, opts.PollInterval(state.awaitPoll))
			if ctx.Err() != nil {
				abortRequested = true
				break
			}
			if outcome.Kind == rowObservationTerminal {
				result = d.executor.FinishObserved(ctx, run, state.cursor, outcome.Verdict)
				break
			}
			if outcome.Kind == rowObservationAcceptanceFailed || err != nil {
				abortRequested = true
				break
			}
			continue
		}
		var observe func() (rowObservationOutcome, error)
		if state.awaiting && state.ready && !state.cursor.quotaAllowance.probing() {
			observe = func() (rowObservationOutcome, error) {
				verdict := d.executor.ObserveAwaitingRun(ctx, run, state.cursor)
				if ctx.Err() != nil {
					return rowObservationOutcome{}, ctx.Err()
				}
				if !verdict.handled() {
					verdict = failureForUnhandledObservation(verdict)
				}
				if verdict.Action != lifecycleResume {
					return rowObservationOutcome{Kind: rowObservationAdmissionChanged, Verdict: verdict}, nil
				}
				return rowObservationOutcome{}, nil
			}
		}
		var err error
		var outcome rowObservationOutcome
		state.opportunity, outcome, err = d.gates.Acquire(ctx, state.awaiting, state.opportunity, observe, opts.PollInterval(state.awaitPoll))
		if ctx.Err() != nil {
			if err == nil && outcome.Kind == rowObservationNone {
				d.gates.Release(false)
			}
			abortRequested = true
			break
		}
		if outcome.Kind == rowObservationAdmissionChanged {
			if outcome.Verdict.Action == lifecycleAwait {
				if err := accept(outcome.Verdict); err != nil {
					abortRequested = true
					break
				}
				if err := waitNext(); err != nil {
					abortRequested = true
					break
				}
				continue
			}
			result = d.executor.FinishObserved(ctx, run, state.cursor, outcome.Verdict)
			break
		}
		if outcome.Kind == rowObservationTerminal {
			result = d.executor.FinishObserved(ctx, run, state.cursor, outcome.Verdict)
			break
		}
		if outcome.Kind == rowObservationAcceptanceFailed {
			abortRequested = true
			break
		}
		if err != nil {
			abortRequested = true
			break
		}
		if !state.cursor.quotaAllowance.probing() && d.gates.QuotaPaused() {
			d.gates.Release(false)
			continue
		}
		if ctx.Err() != nil {
			d.gates.Release(false)
			abortRequested = true
			break
		}
		var started bool
		result, started = d.executor.ExecuteRun(ctx, run, state.cursor)
		state.started = state.started || started
		d.gates.Report(result, state.cursor.quotaAllowance.probing())
		if !result.UsageLimitReached {
			state.cursor.quotaAllowance = state.cursor.quotaAllowance.clear()
		}
		d.gates.Release(started)
		if result.Status == "await" && !state.releaseAwaitCapacity {
			interval := opts.PollInterval(state.awaitPoll)
			if result.UsageLimitReached {
				state.cursor.quotaAllowance = state.cursor.quotaAllowance.continueProbe(result.UsageLimitDeadline)
				interval = usageLimitPollInterval
			}
			if err := d.gates.Checkpoint(run, state.cursor, false, interval); err != nil {
				abortRequested = true
			}
		}
		if result.Status != "await" || !state.releaseAwaitCapacity {
			break
		}
		if !state.yielded && opts.OnYield != nil {
			opts.OnYield()
			state.yielded = true
		}
		if opts.AdvanceTurn != nil {
			opts.AdvanceTurn()
		}
		state.cursor.Mode = ModeContinue
		state.cursor.PreviousRunID = run.RunID
		state.cursor.PreviousRunBatchID = run.BatchID
		state.cursor.ReuseSession = true
		state.awaiting, state.ready = true, false
		if result.UsageLimitReached {
			state.cursor.quotaAllowance = state.cursor.quotaAllowance.continueProbe(result.UsageLimitDeadline)
			if err := accept(lifecycleVerdict{Action: lifecycleAwait}); err != nil {
				abortRequested = true
				break
			}
			if err := wait(usageLimitPollInterval); err != nil {
				abortRequested = true
				break
			}
			state.cursor.quotaAllowance = state.cursor.quotaAllowance.completedPoll(usageLimitPollInterval)
			if err := acceptQuotaReady(); err != nil {
				abortRequested = true
				break
			}
			continue
		}
		if err := accept(lifecycleVerdict{Action: lifecycleAwait}); err != nil {
			abortRequested = true
			break
		}
		if err := waitNext(); err != nil {
			abortRequested = true
			break
		}
	}
	if ctx.Err() != nil && !events.RunStatusFromPayload(result.Status).IsTerminal() {
		abortRequested = true
	}
	if abortRequested && d.actions != nil {
		result = d.actions.Abort(ctx, run, state.cursor, result)
	}
	return result, state.started, nil
}
