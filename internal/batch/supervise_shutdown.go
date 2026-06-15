package batch

import (
	"context"
	"syscall"
	"time"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

// superviseShutdown signals SIGTERM to proc, then waits for whichever
// happens first: proc.WaitDone (the real exit), the killTimeout
// deadline, or ctx cancellation. On the killTimeout path it escalates
// to proc.Kill. The returned channel closes once proc.WaitDone fires,
// so the caller can fan in N supervisors and proceed when all
// processes have actually exited (no time.Sleep).
//
// superviseShutdown does not own the lifecycle of proc: it does not
// call Wait on the underlying OS process. The sandbox.Process wrapper
// drives WaitDone via a goroutine in the sandbox package.
func superviseShutdown(ctx context.Context, proc sandbox.Process, killTimeout time.Duration) <-chan struct{} {
	if proc == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	done := make(chan struct{})
	if killTimeout <= 0 {
		killTimeout = 10 * time.Second
	}

	go func() {
		defer close(done)
		_ = proc.Signal(syscall.SIGTERM)

		waitDone := proc.WaitDone()
		timer := time.NewTimer(killTimeout)
		defer timer.Stop()

		select {
		case <-waitDone:
			return
		case <-timer.C:
			_ = proc.Kill()
		case <-ctx.Done():
			_ = proc.Kill()
		}

		<-waitDone
	}()

	return done
}
