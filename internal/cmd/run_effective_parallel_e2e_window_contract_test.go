package cmd

import (
	"os"
	"strings"
	"testing"
)

// TestEffectiveParallelE2EConcurrencyWindowsLockStability pins the
// concurrent-start windows used by the e2e effective-parallel tests in
// internal/cmd/run_effective_parallel_e2e_test.go. These windows were
// widened after PR #2449's Full Regression - Linux run flaked on
// TestRun_BatchEffectiveParallel_UnlimitedParallel: that workload
// spawns 4 separate containers, so per-issue podman-start latency is
// not amortised across runs the way it is in the shared-container
// tests. The podman engine also serialises some container-start work
// internally under load, so on a saturated CI runner one of the four
// starts can land 2-7 s after the earliest.
//
// This contract locks the windows so a future tightening does not
// silently reintroduce the same flake. It is a pure unit test that
// reads the source file at test time and asserts the literal duration
// strings still match. It runs as part of `make check` on every
// pull request.
func TestEffectiveParallelE2EConcurrencyWindowsLockStability(t *testing.T) {
	const path = "internal/cmd/run_effective_parallel_e2e_test.go"
	// go test runs tests with cwd at the package directory
	// (internal/cmd), so the path is relative to that cwd.
	source := readRepositoryFile(t, "run_effective_parallel_e2e_test.go")

	// AutoMode + AutoModeSpawnsPerCapacity + ExplicitMax: each shares one
	// or two containers across the 4 runs, so per-issue podman-start
	// latency is bounded by goroutine scheduling, not podman. 2 s is
	// plenty. The three call sites must each keep the 2s literal so a
	// future reviewer cannot silently shrink one of them.
	const sharedWindow = "assertConcurrentStarts(t, dir, issues, 2*time.Second)"
	sharedCount := strings.Count(source, sharedWindow)
	if sharedCount < 3 {
		t.Errorf("expected at least 3 shared-container effective-parallel tests (AutoMode, AutoModeSpawnsPerCapacity, ExplicitMax) to keep the 2s concurrent-start window; source at %s has %d matches for %q", path, sharedCount, sharedWindow)
	}

	// UnlimitedParallel (parallel=0, capacity=1): 4 separate container
	// starts. Podman can serialise container-start work internally, so
	// 8 s absorbs CI scheduling slack while still flagging a real
	// regression where the orchestrator falls back to serial execution.
	const unlimitedWindow = "assertConcurrentStarts(t, dir, issues, 8*time.Second)"
	if !strings.Contains(source, unlimitedWindow) {
		t.Errorf("UnlimitedParallel workload must keep the 8s concurrent-start window (see %s); tightening it reintroduces the PR #2449 flake", path)
	}

	// The widening must be justified in a comment so the next maintainer
	// does not re-tighten it by accident.
	if !strings.Contains(source, "PR #2449") {
		t.Errorf("the widened window must be justified with a comment referencing PR #2449 (see %s)", path)
	}
}

// readRepositoryFile is a tiny test helper that resolves a path
// relative to the cmd package directory and reads it. Mirrors the
// shape used by scripts/release_workflow_test.go.
func readRepositoryFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
