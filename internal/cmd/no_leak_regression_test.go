// Package cmd provides the sandman CLI.
//
// This file is the permanent regression guard for the "test data leaks into the
// real .sandman/" defect. It is part of the same lock-in as issue #1633 and the
// conversion ticket (PR #1654 / #1681) that widens the fix to every leaky test
// in internal/cmd/.
//
// Contract: no test in internal/cmd may write into .sandman/batches/ relative
// to the test process's cwd. When `go test` runs a test in this package, it
// chdirs to the package directory (internal/cmd), so the test process's cwd
// IS internal/cmd. If a test writes to .sandman/batches/ from that cwd, the
// write lands in internal/cmd/.sandman/batches/ — a "real" .sandman/ that
// would persist on disk and pollute the repo.
//
// This guard catches the regression: it snapshots the cwd-relative
// .sandman/batches/ count, runs `go test -count=1 ./internal/cmd/...` as a
// subprocess, and fails with the leaked entry names printed to the log if the
// count grew.
//
// References:
//   - #1655: this guard
//   - #1633: parent isolation fix for TestRun_AutoFlag* tests in run_test.go
//   - PR #1653: converts all 16 TestRun_AutoFlag* tests in run_test.go to newRunDepsAuto
//   - PR #1681 (#1654): widens the fix to review_test.go, run_test.go, portal_test.go, continue_e2e_test.go
//   - #1662 (open): isolates TestRun_AutoFlag_* tests in auto_test.go
//   - #1666 (open): isolates raw Dependencies{} sites in clean_test.go
//   - PR #1651 (feature:clean --orphaned) provides the recovery path for any
//     leak that slips through before this guard runs in CI.
package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// batchesDirCount returns the number of entries under <base>/.sandman/batches/.
// Returns 0 if the directory does not exist.
func batchesDirCount(t *testing.T, base string) int {
	t.Helper()
	dir := filepath.Join(base, ".sandman", "batches")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read %s: %v", dir, err)
	}
	return len(entries)
}

// batchesDirNames returns the sorted list of entry names under
// <base>/.sandman/batches/. Returns nil if the directory does not exist.
func batchesDirNames(t *testing.T, base string) []string {
	t.Helper()
	dir := filepath.Join(base, ".sandman", "batches")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// realRepoRoot returns the absolute path of the git worktree top-level so the
// subprocess can resolve ./internal/cmd/... regardless of where the parent
// test was invoked.
func realRepoRoot(t *testing.T) string {
	t.Helper()
	git := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := git.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v: %s", err, string(out))
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		t.Fatalf("git rev-parse --show-toplevel returned empty")
	}
	return root
}

func TestNoLeakToRealBatchesDir(t *testing.T) {
	if os.Getenv("SANDMAN_NO_LEAK_GUARD_SKIP") != "" {
		t.Skip("SANDMAN_NO_LEAK_GUARD_SKIP is set; guard is running as a subprocess of itself")
	}

	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	before := batchesDirCount(t, cwd)
	beforeNames := batchesDirNames(t, cwd)
	beforeSet := make(map[string]struct{}, len(beforeNames))
	for _, n := range beforeNames {
		beforeSet[n] = struct{}{}
	}

	root := realRepoRoot(t)
	cmd := exec.Command("go", "test", "-count=1", "-p", "1", "-parallel", "1", "./internal/cmd/...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "SANDMAN_NO_LEAK_GUARD_SKIP=1")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Logf("subprocess stdout:\n%s", stdout.String())
		t.Logf("subprocess stderr:\n%s", stderr.String())
		t.Fatalf("subprocess `go test -count=1 ./internal/cmd/...` failed: %v", err)
	}

	after := batchesDirCount(t, cwd)
	if after == before {
		t.Logf("ok: %s/.sandman/batches/ stayed at %d entries before and after subprocess run", cwd, before)
		return
	}

	afterNames := batchesDirNames(t, cwd)
	var leaked []string
	for _, n := range afterNames {
		if _, ok := beforeSet[n]; !ok {
			leaked = append(leaked, n)
		}
	}
	sort.Strings(leaked)

	t.Logf("subprocess stdout:\n%s", stdout.String())
	t.Logf("subprocess stderr:\n%s", stderr.String())
	t.Fatalf("cmd-package test leaked %d new entr%s into %s/.sandman/batches/ (before=%d after=%d):\n  %s",
		len(leaked),
		plural(len(leaked)),
		cwd,
		before,
		after,
		strings.Join(leaked, "\n  "),
	)
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
