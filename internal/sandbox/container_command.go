package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const containerCleanupTimeout = 10 * time.Second

// runContainerCommand owns both cancellation and Wait. Killing the process
// group also stops helpers holding output pipes; WaitDelay bounds pipe draining
// if a helper has detached. No command or cancellation goroutine is abandoned.
func runContainerCommand(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done, supervised := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(supervised)
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	<-supervised
	return out.Bytes(), errors.Join(ctx.Err(), err)
}

func boundedContainerCommand(execFn func(string, ...string) *exec.Cmd, binary string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), containerCleanupTimeout)
	defer cancel()
	return runContainerCommand(ctx, execFn(binary, args...))
}
