package batch

import (
	"fmt"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

// waitOwner keeps schedule evidence fresh while the logical row is suspended.
// The enclosing row holds its exclusive RunID claim until terminal persistence.
type waitOwner struct {
	mu       sync.Mutex
	batchDir string
	record   daemon.RunWait
	now      func() time.Time
	log      events.EventLog
	done     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

func newWaitOwner(batchDir string, record daemon.RunWait, now func() time.Time, log events.EventLog) (*waitOwner, error) {
	if err := daemon.RenewRunWait(batchDir, record, now()); err != nil {
		return nil, err
	}
	owner := &waitOwner{batchDir: batchDir, record: record, now: now, log: log, done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(owner.stopped)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-owner.done:
				return
			case <-ticker.C:
				owner.mu.Lock()
				_ = daemon.RenewRunWait(owner.batchDir, owner.record, owner.now())
				owner.mu.Unlock()
			}
		}
	}()
	return owner, nil
}

func (o *waitOwner) checkpoint(row RowSpec, ready bool, nextPoll time.Duration) error {
	states, err := events.ReadRunStates(o.log)
	if err != nil {
		return err
	}
	state := states[row.RunID]
	if state.IsTerminal() {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record := o.record
	record.InitialAdmission = !state.HasStarted()
	if !record.InitialAdmission {
		record.Dependencies = nil
	}
	record.Branch = state.Branch()
	if record.Branch == "" {
		record.Branch = row.Branches[row.IssueNumber]
	}
	record.Ready, record.UsageLimitProbe = ready, row.UsageLimitProbe
	record.PreviousRunID = row.PreviousRunIDs[row.IssueNumber]
	record.PreviousBatchID = row.PreviousRunBatchIDs[row.IssueNumber]
	record.NextPollAt = o.now().Add(nextPoll)
	record.OperationID, record.OperationDeadline = "admission", time.Time{}
	if state.IsAwaiting() && !state.IsCapacityQueued() && state.AwaitEvent != nil {
		if deadline, gate, ok := lifecycleDeadline(state.AwaitEvent.Payload); ok {
			record.OperationDeadline = deadline
			record.OperationID = fmt.Sprintf("%s:%d", gate, deadline.Unix())
		}
		if state.AwaitReason() == "usage-limit" {
			record.UsageLimitProbe = true
			record.OperationDeadline = row.UsageLimitDeadline
			if record.OperationDeadline.IsZero() {
				if seconds, ok := lifecycleDeadlineSeconds(state.AwaitEvent.Payload["usage_limit_deadline_unix_seconds"]); ok {
					record.OperationDeadline = time.Unix(seconds, 0)
				}
			}
			record.OperationID = fmt.Sprintf("quota:%d", record.OperationDeadline.Unix())
		}
	}
	if ready {
		record.OperationID, record.OperationDeadline = "capacity", time.Time{}
	}
	if err := daemon.RenewRunWait(o.batchDir, record, o.now()); err != nil {
		return err
	}
	o.record = record
	return nil
}

func (o *waitOwner) close() {
	if o == nil {
		return
	}
	o.stopOnce.Do(func() { close(o.done) })
	<-o.stopped
}
