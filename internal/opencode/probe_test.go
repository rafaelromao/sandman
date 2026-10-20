package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// inherited the output pipe and would otherwise leave a marker behind.
func TestVersionProbe_KillsHangingHostBinaryAndItsHelpers(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "helper-survived")
	const helperDelay = 3 * time.Second
	installFakeOpencode(t, fmt.Sprintf("(/bin/sleep %d; echo survived > '%s') &\nexec /bin/sleep 600\n", int(helperDelay/time.Second), marker))

	const timeout = time.Second
	start := time.Now()
	got, err := probeVersion(timeout)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error from a hanging probe, got version %q, err %v", got, err)
	}
	if got != "" {
		t.Errorf("expected empty version on timeout, got %q", got)
	}
	if limit := timeout + versionProbeWaitDelay + 3*time.Second; elapsed > limit {
		t.Fatalf("hanging probe returned after %s, want under %s", elapsed, limit)
	}
	time.Sleep(time.Until(start.Add(helperDelay + time.Second)))
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("helper outlived the timed-out probe")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat helper marker: %v", err)
	}
}

// TestVersionProbe_DoesNotWaitForHelperHoldingOutput pins WaitDelay: once
// the probe itself exits, a helper still holding its stdout cannot extend
// the wait, and the probe reports the overrun as a failed probe.
func TestVersionProbe_DoesNotWaitForHelperHoldingOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	installFakeOpencode(t, fmt.Sprintf("echo 1.18.30\n/bin/sleep 30 &\necho $! > '%s'\n", pidFile))

	start := time.Now()
	got, err := probeVersion(time.Minute)
	elapsed := time.Since(start)
	if data, readErr := os.ReadFile(pidFile); readErr == nil {
		if helper, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil {
			t.Cleanup(func() { _ = syscall.Kill(helper, syscall.SIGKILL) })
		}
	}

	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("expected a WaitDelay error, got version %q, err %v", got, err)
	}
	if got != "" {
		t.Errorf("expected empty version when output stays open, got %q", got)
	}
	if limit := versionProbeWaitDelay + 3*time.Second; elapsed > limit {
		t.Fatalf("probe waited %s for a helper holding its output, want under %s", elapsed, limit)
	}
}
