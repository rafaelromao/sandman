package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestVersionProbe_DefaultShellsOut(t *testing.T) {
	// Save and restore the production seam.
	prev := VersionProbe
	defer func() { VersionProbe = prev }()

	VersionProbe = func() (string, error) {
		return "1.17.19", nil
	}

	got, err := VersionProbe()
	if err != nil {
		t.Fatalf("unexpected error from stubbed probe: %v", err)
	}
	if got != "1.17.19" {
		t.Errorf("expected %q, got %q", "1.17.19", got)
	}
}

func TestVersionProbe_ReturnsEmptyOnError(t *testing.T) {
	prev := VersionProbe
	defer func() { VersionProbe = prev }()

	VersionProbe = func() (string, error) {
		return "", fmt.Errorf("exec: no such file")
	}

	got, err := VersionProbe()
	if err == nil {
		t.Errorf("expected error, got nil with version %q", got)
	}
	if got != "" {
		t.Errorf("expected empty version on error, got %q", got)
	}
}

func TestVersionProbe_StubReturnsEmpty(t *testing.T) {
	prev := VersionProbe
	defer func() { VersionProbe = prev }()

	VersionProbe = func() (string, error) {
		return "", nil
	}

	got, err := VersionProbe()
	if err != nil {
		t.Fatalf("unexpected error from stubbed probe: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for absent opencode, got %q", got)
	}
}

// installFakeOpencode puts an executable `opencode` shell script first on
// PATH for the duration of t, so the real probe execs it instead of any
// opencode installed on the host.
func installFakeOpencode(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// installFakeOpencodeWithHelper installs a fake opencode that first
// starts a helper inheriting its stdout and publishes the helper's PID,
// then runs rest. It returns the PID file path.
func installFakeOpencodeWithHelper(t *testing.T, rest string) string {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	installFakeOpencode(t, fmt.Sprintf("/bin/sleep 600 &\necho $! > '%s'\n%s", pidFile, rest))
	return pidFile
}

// publishedHelper waits for the PID a fake opencode published and kills
// that helper when t ends, so a failing test never leaks it.
func publishedHelper(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake opencode did not publish its helper PID")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requireProcessGone fails t unless pid exits within a short grace period.
func requireProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && processExists(pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("probe helper %d outlived the probe", pid)
	}
}

func processExists(pid int) bool {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) >= 3 && fields[2] == "Z" {
				return false
			}
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func TestVersionProbe_ParsesHostBinaryOutput(t *testing.T) {
	installFakeOpencode(t, "echo '1.18.30+abc123 (build)'\n")

	got, err := VersionProbe()
	if err != nil {
		t.Fatalf("unexpected error from host probe: %v", err)
	}
	if got != "1.18.30+abc123" {
		t.Errorf("expected %q, got %q", "1.18.30+abc123", got)
	}
}

// TestVersionProbe_KillsHangingHostBinaryAndItsHelpers pins the bound on
// a host opencode that never answers: the probe returns the timeout as an
// error, and its whole process group is killed, including a helper that
// inherited the output pipe.
func TestVersionProbe_KillsHangingHostBinaryAndItsHelpers(t *testing.T) {
	pidFile := installFakeOpencodeWithHelper(t, "wait\n")

	const timeout = time.Second
	start := time.Now()
	got, err := probeVersionWithin(timeout)
	elapsed := time.Since(start)
	helper := publishedHelper(t, pidFile)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error from a hanging probe, got version %q, err %v", got, err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("timeout error dropped the underlying exit error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty version on timeout, got %q", got)
	}
	if limit := timeout + versionProbeWaitDelay + 3*time.Second; elapsed > limit {
		t.Fatalf("hanging probe returned after %s, want under %s", elapsed, limit)
	}
	requireProcessGone(t, helper)
}

// TestVersionProbe_AcceptsVersionWhenHelperHoldsOutput pins WaitDelay:
// once opencode prints its version and exits cleanly, a helper still
// holding its stdout neither extends the wait nor discards the version,
// and the helper does not outlive the probe.
func TestVersionProbe_AcceptsVersionWhenHelperHoldsOutput(t *testing.T) {
	pidFile := installFakeOpencodeWithHelper(t, "echo 1.18.30\n")

	start := time.Now()
	got, err := probeVersionWithin(time.Minute)
	elapsed := time.Since(start)
	helper := publishedHelper(t, pidFile)

	if err != nil {
		t.Fatalf("unexpected error when only a helper held output: %v", err)
	}
	if got != "1.18.30" {
		t.Errorf("expected %q, got %q", "1.18.30", got)
	}
	if limit := versionProbeWaitDelay + 3*time.Second; elapsed > limit {
		t.Fatalf("probe waited %s for a helper holding its output, want under %s", elapsed, limit)
	}
	requireProcessGone(t, helper)
}

// probeChildEnv switches TestVersionProbe_InterruptedProbeLeavesNoHelper
// into its child role. The value says whether the child installs its own
// interrupt handler first.
const probeChildEnv = "SANDMAN_OPENCODE_PROBE_CHILD"

// TestVersionProbe_InterruptedProbeLeavesNoHelper interrupts a child
// process during a hanging probe, as an operator Ctrl-C would. Without a
// handler of its own (`sandman review`, `sandman init`) the child must
// still die by SIGINT; with one (`sandman run`) the handler must still
// receive it. Either way the probe's process group must be gone.
func TestVersionProbe_InterruptedProbeLeavesNoHelper(t *testing.T) {
	if mode := os.Getenv(probeChildEnv); mode != "" {
		var handler chan os.Signal
		if mode == "handled" {
			handler = make(chan os.Signal, 1)
			signal.Notify(handler, os.Interrupt)
		}
		_, err := probeVersionWithin(time.Minute)
		fmt.Printf("probe: %v\n", err)
		if handler != nil {
			select {
			case <-handler:
				fmt.Println("handler received interrupt")
			case <-time.After(5 * time.Second):
			}
		}
		return
	}

	for _, mode := range []string{"unhandled", "handled"} {
		t.Run(mode, func(t *testing.T) {
			pidFile := installFakeOpencodeWithHelper(t, "wait\n")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var out bytes.Buffer
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVersionProbe_InterruptedProbeLeavesNoHelper$")
			child.Env = append(os.Environ(), probeChildEnv+"="+mode)
			child.Stdout, child.Stderr = &out, &out
			if err := child.Start(); err != nil {
				t.Fatalf("start child: %v", err)
			}
			helper := publishedHelper(t, pidFile)

			interrupted := time.Now()
			if err := child.Process.Signal(os.Interrupt); err != nil {
				t.Fatalf("interrupt child: %v", err)
			}
			waitErr := child.Wait()
			if elapsed := time.Since(interrupted); elapsed > 10*time.Second {
				t.Fatalf("child took %s to finish after the interrupt", elapsed)
			}

			status, _ := child.ProcessState.Sys().(syscall.WaitStatus)
			switch mode {
			case "unhandled":
				if !status.Signaled() || status.Signal() != syscall.SIGINT {
					t.Fatalf("child without a handler must die by SIGINT, got %v; output:\n%s", waitErr, out.String())
				}
			case "handled":
				if waitErr != nil {
					t.Fatalf("child with a handler must exit normally, got %v; output:\n%s", waitErr, out.String())
				}
				for _, want := range []string{"interrupted by interrupt", "handler received interrupt"} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("child output is missing %q:\n%s", want, out.String())
					}
				}
			}
			requireProcessGone(t, helper)
		})
	}
}
