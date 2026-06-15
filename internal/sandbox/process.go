package sandbox

import (
	"os"
	"os/exec"
	"sync"
)

// processWrapper wraps the OS process spawned by an exec.Cmd so that
// superviseShutdown can wait for the actual exit via a channel instead of
// a wall-clock sleep. The wrapper is returned by WorktreeSandbox.Process
// and ContainerSandbox.Process; it satisfies the sandbox.Process
// interface and is safe to use concurrently from the supervisor goroutine
// and the agent goroutine.
type processWrapper struct {
	proc     *os.Process
	cmd      *exec.Cmd
	done     chan struct{}
	waitOnce sync.Once
}

func newProcessWrapper(cmd *exec.Cmd) *processWrapper {
	w := &processWrapper{
		proc: cmd.Process,
		cmd:  cmd,
		done: make(chan struct{}),
	}
	w.waitOnce.Do(func() {
		go func() {
			_ = cmd.Wait()
			close(w.done)
		}()
	})
	return w
}

func (w *processWrapper) Signal(sig os.Signal) error {
	if w.proc == nil {
		return os.ErrInvalid
	}
	return w.proc.Signal(sig)
}

func (w *processWrapper) Kill() error {
	if w.proc == nil {
		return os.ErrInvalid
	}
	return w.proc.Kill()
}

func (w *processWrapper) WaitDone() <-chan struct{} {
	return w.done
}
