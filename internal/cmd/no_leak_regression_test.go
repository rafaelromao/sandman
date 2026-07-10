// Package cmd provides the sandman CLI.
//
// This file is the permanent regression guard for the "test data leaks into the
// real .sandman/" defect. It is part of the same lock-in as issue #1633 and
// the conversion ticket (PR #1654 / #1681) that widens the fix to every leaky
// test in internal/cmd/. Issue #2114 optimized this guard: instead of running
// the full cmd test suite as a subprocess, it now runs only the subset of tests
// whose source files do NOT call t.TempDir() — those are the only tests that
// could potentially write to the real .sandman/ directory.
//
// Contract: no test in internal/cmd may write into .sandman/batches/ relative
// to the test process's cwd. When `go test` runs a test in this package, it
// chdirs to the package directory (internal/cmd), so the test process's cwd
// IS internal/cmd. If a test writes to .sandman/batches/ from that cwd, the
// write lands in internal/cmd/.sandman/batches/ — a "real" .sandman/ that
// would persist on disk and pollute the repo.
//
// This guard catches the regression: it snapshots the cwd-relative
// .sandman/batches/ count, runs only potentially-leaking tests as a
// subprocess, and fails with the leaked entry names printed to the log if the
// count grew.
//
// Performance note (#2114): tests using t.TempDir() are inherently isolated
// from the real .sandman/ — t.TempDir() creates a temp directory and the
// batch code uses TMPDIR, so .sandman/ writes go to the temp dir. By filtering
// to only tests in files that don't use t.TempDir(), we run a much smaller
// test subset while preserving the detection contract.
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
//   - #2114: optimized guard to run only non-t.TempDir tests as subprocess
package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// testsInFilesWithoutTempDir returns all test names from test files that do
// NOT call t.TempDir(). These are the tests that could potentially write to
// the real .sandman/ directory and are the only ones that need to run in
// the leak guard's subprocess.
//
// File-level granularity is used: if a test file contains 10 tests and only
// 1 of them doesn't use t.TempDir(), all 10 tests are included. This is a
// known imprecision — see the package doc — but avoids the complexity of
// per-test function-level source analysis.
//
// The approach:
//   - Enumerate all tests via "go test -list"
//   - Find test files that don't contain "t.TempDir()"
//   - Return all tests from those files
func testsInFilesWithoutTempDir(t *testing.T) []string {
	t.Helper()

	root := realRepoRoot(t)

	cmd := exec.Command("go", "test", "-list", ".*", "./internal/cmd/...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test -list: %v: %s", err, string(out))
	}

	var allTests []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Test") {
			allTests = append(allTests, line)
		}
	}

	if len(allTests) == 0 {
		return nil
	}

	files, err := filepath.Glob(filepath.Join(root, "internal/cmd", "*_test.go"))
	if err != nil {
		t.Fatalf("filepath.Glob: %v", err)
	}

	var tempDirFiles []string
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(content), "t.TempDir()") {
			rel, err := filepath.Rel(root, f)
			if err != nil {
				continue
			}
			tempDirFiles = append(tempDirFiles, rel)
		}
	}

	tempDirFileSet := make(map[string]bool, len(tempDirFiles))
	for _, f := range tempDirFiles {
		tempDirFileSet[f] = true
	}

	var filtered []string
	for _, test := range allTests {
		srcFile := sourceFileForTest(t, root, test)
		if srcFile == "" {
			continue
		}
		if !tempDirFileSet[srcFile] {
			filtered = append(filtered, test)
		}
	}

	return filtered
}

// sourceFileForTest returns the source file path for a given test name by
// searching for the test function definition in the cmd test files.
// Returns a path relative to root (e.g., "internal/cmd/model_test.go").
func sourceFileForTest(t *testing.T, root, testName string) string {
	t.Helper()
	pattern := "func " + regexp.QuoteMeta(testName) + `(t \*testing\.T)`
	cmd := exec.Command("grep", "-r", "-l", pattern, "internal/cmd")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(files) > 0 && files[0] != "" {
		return files[0]
	}
	return ""
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

	potentiallyLeaking := testsInFilesWithoutTempDir(t)
	t.Logf("running leak guard against %d tests (from files without t.TempDir) out of total enumerated", len(potentiallyLeaking))

	root := realRepoRoot(t)

	var cmd *exec.Cmd
	if len(potentiallyLeaking) == 0 {
		t.Log("no tests without t.TempDir found; skipping subprocess run")
	} else {
		pattern := "^(" + strings.Join(potentiallyLeaking, "|") + ")$"
		cmd = exec.Command("go", "test", "-count=1", "-run", pattern, "./internal/cmd/...")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "SANDMAN_NO_LEAK_GUARD_SKIP=1")
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Logf("subprocess stdout:\n%s", stdout.String())
			t.Logf("subprocess stderr:\n%s", stderr.String())
			t.Fatalf("subprocess `go test -run <pattern> ./internal/cmd/...` failed: %v", err)
		}
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
