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
// starts a helper inheriting its stdout and atomically publishes the
// helper's PID, then runs rest. It returns the PID file path.
func installFakeOpencodeWithHelper(t *testing.T, rest string) string {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	installFakeOpencode(t, fmt.Sprintf("/bin/sleep 600 &\necho $! > '%[1]s.tmp' && mv '%[1]s.tmp' '%[1]s'\n%[2]s", pidFile, rest))
	return pidFile
}

// awaitHelperPID waits for the PID a fake opencode published and kills
// that helper when t ends if it is still running, so a failing test
// never leaks it.
func awaitHelperPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("parse helper PID %q: %v", data, err)
			}
			t.Cleanup(func() {
				if processExists(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			return pid
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

// TestVersionProbe_KillsHangingHostBinaryWithinItsBound pins the
// production bound on a host opencode that never answers: VersionProbe
// waits the full five seconds, returns the timeout as an error, and
// kills its whole process group, including a helper that inherited the
// output pipe.
func TestVersionProbe_KillsHangingHostBinaryWithinItsBound(t *testing.T) {
	pidFile := installFakeOpencodeWithHelper(t, "wait\n")

	start := time.Now()
	got, err := VersionProbe()
	elapsed := time.Since(start)
	helper := awaitHelperPID(t, pidFile)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error from a hanging probe, got version %q, err %v", got, err)
	}
	if !strings.Contains(err.Error(), "within 5s") {
		t.Errorf("timeout error does not name the five-second bound: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("timeout error dropped the underlying exit error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty version on timeout, got %q", got)
	}
	if elapsed < versionProbeTimeout {
		t.Errorf("hanging probe gave up after %s, before its %s bound", elapsed, versionProbeTimeout)
	}
	if limit := versionProbeTimeout + versionProbeWaitDelay + 3*time.Second; elapsed > limit {
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
	helper := awaitHelperPID(t, pidFile)

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

// probeChildEnv switches TestVersionProbe_SignalledProbeLeavesNoHelper
// into its child role. Its value says whether the child installs its own
// interrupt handler before probing.
const probeChildEnv = "SANDMAN_OPENCODE_PROBE_CHILD"

// TestVersionProbe_SignalledProbeLeavesNoHelper signals a child process
// during a hanging probe, as an operator would. Without a handler of its
// own (`sandman review`, `sandman init`) the child must still die by the
// signal; with one (`sandman run`) the handler must receive the original
// signal and its re-delivery. Either way the probe's process group must
// be gone.
func TestVersionProbe_SignalledProbeLeavesNoHelper(t *testing.T) {
	if value := os.Getenv(probeChildEnv); value != "" {
		runSignalledProbeChild(value)
		return
	}

	diesBy := func(sig syscall.Signal) func(*testing.T, *os.ProcessState, string) {
		return func(t *testing.T, state *os.ProcessState, out string) {
			status, _ := state.Sys().(syscall.WaitStatus)
			if !status.Signaled() || status.Signal() != sig {
				t.Fatalf("child without a handler must die by %v, got %v; output:\n%s", sig, state, out)
			}
		}
	}
	cases := []struct {
		name    string
		sig     syscall.Signal
		handler bool
		verify  func(*testing.T, *os.ProcessState, string)
	}{
		{name: "interrupt without handler", sig: syscall.SIGINT, verify: diesBy(syscall.SIGINT)},
		{name: "terminate without handler", sig: syscall.SIGTERM, verify: diesBy(syscall.SIGTERM)},
		{name: "hangup without handler", sig: syscall.SIGHUP, verify: diesBy(syscall.SIGHUP)},
		{name: "interrupt with handler", sig: syscall.SIGINT, handler: true, verify: func(t *testing.T, state *os.ProcessState, out string) {
			if !state.Success() {
				t.Fatalf("child with a handler must exit normally, got %v; output:\n%s", state, out)
			}
			for _, want := range []string{"interrupted by interrupt", "handler received 2 interrupts"} {
				if !strings.Contains(out, want) {
					t.Errorf("child output is missing %q:\n%s", want, out)
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if signal.Ignored(tc.sig) {
				t.Skipf("%v is ignored here, and the child would inherit that", tc.sig)
			}
			pidFile := installFakeOpencodeWithHelper(t, "wait\n")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var out bytes.Buffer
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVersionProbe_SignalledProbeLeavesNoHelper$")
			child.Env = append(os.Environ(), probeChildEnv+"="+strconv.FormatBool(tc.handler))
			child.Stdout, child.Stderr = &out, &out
			if err := child.Start(); err != nil {
				t.Fatalf("start child: %v", err)
			}
			helper := awaitHelperPID(t, pidFile)

			signalled := time.Now()
			if err := child.Process.Signal(tc.sig); err != nil {
				t.Fatalf("signal child: %v", err)
			}
			_ = child.Wait()
			if elapsed := time.Since(signalled); elapsed > 10*time.Second {
				t.Fatalf("child took %s to finish after the signal", elapsed)
			}
			tc.verify(t, child.ProcessState, out.String())
			requireProcessGone(t, helper)
		})
	}
}

// runSignalledProbeChild is the child role: it probes against the hanging
// fake on PATH and, when asked to install a handler, reports how many
// interrupts that handler received.
func runSignalledProbeChild(value string) {
	withHandler, _ := strconv.ParseBool(value)
	var handler chan os.Signal
	if withHandler {
		handler = make(chan os.Signal, 4)
		signal.Notify(handler, os.Interrupt)
	}
	_, err := probeVersionWithin(time.Minute)
	fmt.Printf("probe: %v\n", err)
	if handler == nil {
		return
	}
	received := 0
	timeout := time.After(5 * time.Second)
	for received < 2 {
		select {
		case <-handler:
			received++
		case <-timeout:
			fmt.Printf("handler received %d interrupts\n", received)
			return
		}
	}
	select {
	case <-handler:
		received++
	case <-time.After(200 * time.Millisecond):
	}
	fmt.Printf("handler received %d interrupts\n", received)
}
