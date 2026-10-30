package batch

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

// leaseRenewInterval is how often a live owner renews its schedule lease.
const leaseRenewInterval = 30 * time.Second

// waitOwner keeps schedule evidence fresh while the logical row is suspended.
// The enclosing row holds its exclusive RunID claim until terminal persistence.
type waitOwner struct {
	mu     sync.Mutex
	layout paths.Layout
	batch  paths.BatchLocation
	record daemon.RunWait
	now    func() time.Time
	log    events.EventLog
	// renewedAt is when the lease was last written successfully. Retries
	// are measured from it against the uncapped recovery grace: a lease
	// capped by its operation deadline cannot be extended by renewal, and
	// the held RunID claim, not the lease, protects the live row.
	renewedAt time.Time
	done      chan struct{}
	stopped   chan struct{}
	stopOnce  sync.Once
}

func newWaitOwner(layout paths.Layout, batch paths.BatchLocation, record daemon.RunWait, now func() time.Time, log events.EventLog, pulse ...<-chan time.Time) (*waitOwner, error) {
	var ticks <-chan time.Time
	if len(pulse) > 0 {
		ticks = pulse[0]
	}
	return newWaitOwnerWithFailure(layout, batch, record, now, log, ticks, nil, nil)
}

// The owner renews record at batch's frozen location; record must carry the
// Batch's public identity, never its physical directory name. A renewal that
// fails with ErrRunWaitInvalid calls onFailure at once. Any other failure is
// reported to onRetry and retried on later ticks; onFailure is called only
// once the last successful renewal's recovery grace would lapse before the
// next renewal.
func newWaitOwnerWithFailure(layout paths.Layout, batch paths.BatchLocation, record daemon.RunWait, now func() time.Time, log events.EventLog, pulse <-chan time.Time, onFailure, onRetry func(error)) (*waitOwner, error) {
	renewedAt := now()
	if err := daemon.RenewRunWait(layout, batch, record, renewedAt); err != nil {
		return nil, err
	}
	owner := &waitOwner{layout: layout, batch: batch, record: record, now: now, log: log, renewedAt: renewedAt, done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(owner.stopped)
		var ticks <-chan time.Time
		if pulse != nil {
			ticks = pulse
		} else {
			ticker := time.NewTicker(leaseRenewInterval)
			defer ticker.Stop()
			ticks = ticker.C
		}
		for {
			select {
			case <-owner.done:
				return
			case <-ticks:
				owner.mu.Lock()
				at := owner.now()
				err := daemon.RenewRunWait(owner.layout, owner.batch, owner.record, at)
				if err == nil {
					owner.renewedAt = at
				}
				renewedAt := owner.renewedAt
				owner.mu.Unlock()
				if err == nil {
					continue
				}
				if renewalRetryable(err, at, renewedAt) {
					if onRetry != nil {
						onRetry(err)
					}
					continue
				}
				if onFailure != nil {
					onFailure(err)
				}
				return
			}
		}
	}()
	return owner, nil
}

// renewalRetryable reports whether a failed renewal at at may be retried: the
// failure is not invalid ownership, and the next renewal still lands inside the
// uncapped recovery grace of the last successful renewal.
func renewalRetryable(err error, at, renewedAt time.Time) bool {
	return !errors.Is(err, daemon.ErrRunWaitInvalid) && at.Add(leaseRenewInterval).Before(renewedAt.Add(daemon.RunRecoveryGrace))
}

func (o *waitOwner) checkpoint(row RowSpec, ready bool, nextPoll time.Duration) error {
	var state events.RunState
	if o.log != nil {
		states, err := events.ReadRunStates(o.log)
		if err != nil {
			return err
		}
		state = states[row.RunID]
	}
	if state.IsTerminal() {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record := o.record
	if o.log != nil {
		record.InitialAdmission = !state.HasStarted()
	} else if !ready || row.quotaAllowance.probing() || row.Mode == ModeContinue {
		record.InitialAdmission = false
	}
	if !record.InitialAdmission {
		record.Dependencies = nil
	}
	record.Branch = state.Branch()
	if record.Branch == "" {
		record.Branch = row.Branches[row.IssueNumber]
	}
	record.Ready, record.UsageLimitProbe = ready, row.quotaAllowance.probing()
	record.UsageLimitAccountingUnavailable = row.quotaAllowance.accountingUnavailableForCheckpoint()
	record.ReuseSession = row.quotaAllowance.allowSessionReuse(row.ReuseSession)
	record.PreviousRunID = row.PreviousRunIDs[row.IssueNumber]
	record.PreviousBatchID = row.PreviousRunBatchIDs[row.IssueNumber]
	record.NextPollAt = o.now().Add(nextPoll)
	record.OperationID, record.OperationDeadline = "admission", time.Time{}
	if state.IsAwaiting() && !state.IsCapacityQueued() && state.AwaitEvent != nil {
		if deadline, gate, ok := lifecycleDeadline(state.AwaitEvent.Payload); ok {
			record.OperationDeadline = deadline
			record.OperationID = fmt.Sprintf("%s:%d", gate, deadline.Unix())
		}
		if id, deadline, ok := githubRecoveryOperation(state.AwaitEvent.Payload); ok {
			// A GitHub recovery wait is bounded by its 24-hour window; retained
			// review or CI deadlines are not enforced while GitHub is down.
			record.OperationID, record.OperationDeadline = id, deadline
		}
		if continuityHoldPayload(state.AwaitEvent.Payload) {
			// A continuity hold waits for external recovery without an
			// operation deadline; it keeps the ordinary ownerless grace.
			record.OperationID, record.OperationDeadline = continuityHoldOperation(row.RunID), time.Time{}
		}
		if state.AwaitReason() == "usage-limit" {
			record.UsageLimitProbe = true
			record.OperationID, record.OperationDeadline = "quota:"+row.RunID, time.Time{}
		}
	}
	if ready && !record.UsageLimitProbe {
		record.OperationID, record.OperationDeadline = "capacity", time.Time{}
	}
	if row.quotaAllowance.probing() {
		record.OperationID, record.OperationDeadline = "quota:"+row.RunID, time.Time{}
	}
	at := o.now()
	if err := daemon.RenewRunWait(o.layout, o.batch, record, at); err != nil {
		return err
	}
	o.record = record
	o.renewedAt = at
	return nil
}

func (o *waitOwner) close() {
	if o == nil {
		return
	}
	o.stopOnce.Do(func() { close(o.done) })
	<-o.stopped
}
