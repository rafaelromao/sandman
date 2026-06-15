package sandbox

import (
	"os"
	"os/exec"
)

// processWrapper wraps the OS process spawned by an exec.Cmd so that
// superviseShutdown can wait for the actual exit via a channel instead of
// a wall-clock sleep. The wrapper owns the single goroutine that calls
// cmd.Wait — waitCmd and any other internal helper must use WaitDone,
// not call cmd.Wait themselves, to avoid the double-Wait contract that
// Go's exec package enforces. The wrapper is returned by
// WorktreeSandbox.Process and ContainerSandbox.Process and is safe to
// use concurrently from the supervisor goroutine and the agent
// goroutine.
type processWrapper struct {
	proc      *os.Process
	cmd       *exec.Cmd
	done      chan struct{}
	waitError error
}

// newProcessWrapper registers cmd as owned by the wrapper's Wait
// goroutine. Calling newProcessWrapper twice on the same cmd would
// leak a second Wait goroutine and trigger Go's "Wait was already
// called" error.
func newProcessWrapper(cmd *exec.Cmd) *processWrapper {
	w := &processWrapper{
		proc: cmd.Process,
		cmd:  cmd,
		done: make(chan struct{}),
	}
	go func() {
		w.waitError = cmd.Wait()
		close(w.done)
	}()
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

// exitErr returns the result of cmd.Wait once WaitDone has fired.
// Callers should only consult it after WaitDone has closed; before
// that, the field has its zero value.
func (w *processWrapper) exitErr() error {
	return w.waitError
}
