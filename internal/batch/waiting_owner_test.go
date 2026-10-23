package batch

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/paths"
)

// renewalHarness drives one waitOwner with an injected clock and pulse and
// records how it reports renewal failures.
type renewalHarness struct {
	t       *testing.T
	layout  paths.Layout
	batch   paths.BatchLocation
	owner   *waitOwner
	pulse   chan time.Time
	retried chan error
	failed  atomic.Int32

	mu  sync.Mutex
	now time.Time
}

func newRenewalHarness(t *testing.T, record daemon.RunWait, start time.Time) *renewalHarness {
	t.Helper()
	h := &renewalHarness{t: t, layout: paths.NewLayout(nil, t.TempDir()), pulse: make(chan time.Time), retried: make(chan error, 16), now: start}
	h.batch = h.layout.LocateBatch("batch", "")
	owner, err := newWaitOwnerWithFailure(h.layout, h.batch, record, h.clock, nil, h.pulse,
		func(error) { h.failed.Add(1) },
		func(err error) { h.retried <- err })
	if err != nil {
		t.Fatal(err)
	}
	h.owner = owner
	t.Cleanup(owner.close)
	return h
}

func (h *renewalHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

// tick advances the clock and delivers one renewal pulse.
func (h *renewalHarness) tick(at time.Time) {
	h.t.Helper()
	h.mu.Lock()
	h.now = at
	h.mu.Unlock()
	select {
	case h.pulse <- at:
	case <-h.owner.stopped:
		h.t.Fatal("renewal stopped before the pulse")
	case <-time.After(2 * time.Second):
		h.t.Fatal("renewal did not accept the pulse")
	}
}

func (h *renewalHarness) runDir() string { return h.batch.Run("row").Dir }

// breakWrites replaces the Run folder with a regular file, so reads and writes
// fail with ENOTDIR even for root.
func (h *renewalHarness) breakWrites() {
	h.t.Helper()
	if err := os.RemoveAll(h.runDir()); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.runDir(), nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *renewalHarness) restoreWrites() {
	h.t.Helper()
	if err := os.Remove(h.runDir()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *renewalHarness) awaitRetry() {
	h.t.Helper()
	select {
	case <-h.retried:
	case <-time.After(2 * time.Second):
		h.t.Fatal("renewal failure was not retried")
	}
}

func (h *renewalHarness) awaitLease(want time.Time) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		record, err := daemon.ReadRunWaitAt(h.layout, h.batch, "row")
		if err == nil && record.LeaseExpiresAt.Equal(want) {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("lease = %+v err=%v, want expiry %s", record, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *renewalHarness) awaitStopped() {
	h.t.Helper()
	select {
	case <-h.owner.stopped:
	case <-time.After(2 * time.Second):
		h.t.Fatal("renewal did not stop")
	}
}

func startedWait() daemon.RunWait {
	return daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: "42-fix", BaseBranch: "main", OperationID: "capacity", Ready: true}
}

func TestWaitOwner_TransientRenewalFailureIsRetried(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	h := newRenewalHarness(t, startedWait(), start)
	h.breakWrites()
	h.tick(start.Add(leaseRenewInterval))
	h.awaitRetry()
	h.restoreWrites()
	renewed := start.Add(2 * leaseRenewInterval)
	h.tick(renewed)
	h.awaitLease(renewed.Add(daemon.RunRecoveryGrace))
	if n := h.failed.Load(); n != 0 {
		t.Fatalf("transient renewal failure aborted owned intent %d times", n)
	}
}

func TestWaitOwner_RenewalGivesUpBeforeLastLeaseExpires(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	h := newRenewalHarness(t, startedWait(), start)
	h.breakWrites()
	lastRetry := start.Add(daemon.RunRecoveryGrace - leaseRenewInterval - time.Second)
	h.tick(lastRetry)
	h.awaitRetry()
	if n := h.failed.Load(); n != 0 {
		t.Fatalf("renewal gave up %d times while the last lease outlived the next renewal", n)
	}
	h.tick(start.Add(daemon.RunRecoveryGrace - leaseRenewInterval))
	h.awaitStopped()
	if n := h.failed.Load(); n != 1 {
		t.Fatalf("failure reported %d times, want exactly once", n)
	}
}

func TestWaitOwner_DeadlineCappedLeaseStillRetries(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	record := startedWait()
	record.Ready = false
	record.OperationID = "ci:17:head"
	record.OperationDeadline = start.Add(10 * time.Second)
	h := newRenewalHarness(t, record, start)
	h.breakWrites()
	h.tick(start.Add(20 * time.Second))
	h.awaitRetry()
	h.restoreWrites()
	h.tick(start.Add(50 * time.Second))
	h.awaitLease(record.OperationDeadline)
	if n := h.failed.Load(); n != 0 {
		t.Fatalf("deadline-capped lease aborted on a transient failure %d times", n)
	}
}

func TestWaitOwner_InvalidOwnershipFailsImmediately(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	h := newRenewalHarness(t, startedWait(), start)
	if err := os.WriteFile(filepath.Join(h.runDir(), "wait.json"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.tick(start.Add(leaseRenewInterval))
	h.awaitStopped()
	if n := h.failed.Load(); n != 1 {
		t.Fatalf("invalid ownership reported %d times, want once", n)
	}
	select {
	case err := <-h.retried:
		t.Fatalf("invalid ownership was retried: %v", err)
	default:
	}
}
