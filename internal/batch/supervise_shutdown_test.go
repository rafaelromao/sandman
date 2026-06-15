package batch

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

// fakeSuperviseProcess is a sandbox.Process that records Signal and Kill
// calls and lets the test trigger WaitDone on demand. It is intentionally
// independent from fakeProcess in agent_run_test.go so that supervise-shutdown
// tests can drive the exit semantics without colliding with the heartbeat
// and process tests.
type fakeSuperviseProcess struct {
	mu            sync.Mutex
	sigTermCalled atomic.Bool
	killCalled    atomic.Bool
	exited        chan struct{}
}

func newFakeSuperviseProcess() *fakeSuperviseProcess {
	return &fakeSuperviseProcess{exited: make(chan struct{})}
}

func (p *fakeSuperviseProcess) Signal(sig os.Signal) error {
	if sig == syscall.SIGTERM {
		p.sigTermCalled.Store(true)
	}
	return nil
}

func (p *fakeSuperviseProcess) Kill() error {
	p.killCalled.Store(true)
	p.SignalExited()
	return nil
}

func (p *fakeSuperviseProcess) WaitDone() <-chan struct{} {
	return p.exited
}

// SignalExited closes the WaitDone channel, mimicking the OS process
// exiting. Safe to call once.
func (p *fakeSuperviseProcess) SignalExited() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.exited:
	default:
		close(p.exited)
	}
}

func (p *fakeSuperviseProcess) snapshot() (sigTerm, kill bool) {
	return p.sigTermCalled.Load(), p.killCalled.Load()
}

var _ sandbox.Process = (*fakeSuperviseProcess)(nil)

// waitForSIGTERM polls the proc's recorded state until either the SIGTERM
// is observed or the deadline elapses. Returns the final (sigTerm, kill)
// snapshot.
func waitForSIGTERM(t *testing.T, proc *fakeSuperviseProcess, deadline time.Duration) (sigTerm, kill bool) {
	t.Helper()
	timeout := time.NewTimer(deadline)
	defer timeout.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		sigTerm, kill = proc.snapshot()
		if sigTerm {
			return sigTerm, kill
		}
		select {
		case <-timeout.C:
			return sigTerm, kill
		case <-tick.C:
		}
	}
}

func TestSuperviseShutdown(t *testing.T) {
	t.Run("exits_on_SIGTERM", func(t *testing.T) {
		proc := newFakeSuperviseProcess()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := superviseShutdown(ctx, proc, 5*time.Second)

		// Wait for SIGTERM to land before signalling exit, so we can
		// prove the shutdown path actually sent the signal — not just
		// that the returned channel happens to close for some other reason.
		if sigTerm, _ := waitForSIGTERM(t, proc, 2*time.Second); !sigTerm {
			t.Fatal("expected Signal(SIGTERM) to be called on entry")
		}

		proc.SignalExited()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("superviseShutdown did not close its returned channel after WaitDone fired")
		}

		if _, kill := proc.snapshot(); kill {
			t.Fatal("expected Kill to NOT be called when process exits on SIGTERM")
		}
	})

	t.Run("ignores_SIGTERM_is_killed_after_timeout", func(t *testing.T) {
		proc := newFakeSuperviseProcess()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		const killTimeout = 80 * time.Millisecond
		done := superviseShutdown(ctx, proc, killTimeout)

		// Confirm SIGTERM is sent first; we then wait for the kill to
		// fire and the channel to close, both inside the timeout window.
		if sigTerm, _ := waitForSIGTERM(t, proc, 2*time.Second); !sigTerm {
			t.Fatal("expected Signal(SIGTERM) to be called on entry")
		}

		// The process is still alive (no SignalExited yet), so the only
		// way the returned channel can close is via the killTimeout
		// path: SIGKILL is sent, then WaitDone is observed.
		select {
		case <-done:
		case <-time.After(killTimeout + 2*time.Second):
			t.Fatal("superviseShutdown did not close its returned channel after killTimeout elapsed")
		}

		_, kill := proc.snapshot()
		if !kill {
			t.Fatal("expected Kill to be called after killTimeout elapsed")
		}
	})

	t.Run("ctx_cancelled_process_is_killed", func(t *testing.T) {
		proc := newFakeSuperviseProcess()

		ctx, cancel := context.WithCancel(context.Background())

		// Use a large killTimeout so the only way the goroutine
		// reaches the kill path is via ctx cancellation.
		done := superviseShutdown(ctx, proc, 30*time.Second)

		if sigTerm, _ := waitForSIGTERM(t, proc, 2*time.Second); !sigTerm {
			t.Fatal("expected Signal(SIGTERM) to be called on entry")
		}

		// Cancel ctx; the supervisor must escalate to Kill and close
		// the returned channel once WaitDone is observed.
		cancel()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("superviseShutdown did not close its returned channel after ctx cancellation")
		}

		_, kill := proc.snapshot()
		if !kill {
			t.Fatal("expected Kill to be called after ctx cancellation")
		}
	})
}
