//go:build e2e

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

// e2eAgentScript returns an agent command body that records the container
// hostname and workdir. The test reads the log file's mtime to reason about
// start ordering (the log file is created when the agent first writes, and
// the mtime has nanosecond resolution on most filesystems). The sleep
// duration is long enough to cover goroutine scheduling overhead so all
// concurrent starts complete their acquire before the first one finishes.
func e2eAgentScript() string {
	return `
set -eu
printf 'container-identity=%s\n' "$(hostname)"
printf 'container-workdir=%s\n' "$PWD"
sleep 2
`
}

func newE2EQueueingHome(t *testing.T) string {
	t.Helper()
	homeDir, err := os.MkdirTemp("", "sandman-effective-parallel-home-")
	if err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0755); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	return homeDir
}

func e2eQueueingGitConfig(homeDir, remoteDir string) string {
	return fmt.Sprintf("[user]\n\tname = Test\n[url %q]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n", "file://"+remoteDir)
}

// runEffectiveParallelE2E runs an effective-parallel e2e test scenario. It
// primes podman, writes the sandman Dockerfile, and dispatches the run with
// the given parallel / capacity / max settings.
func runEffectiveParallelE2E(t *testing.T, issues []int, parallel, capacity, maxContainers int, prs map[string]*github.PR) (out, dir string) {
	t.Helper()
	if !podmanAvailable(t) {
		t.Skip("podman not available")
	}

	dir = t.TempDir()
	t.Chdir(dir)
	remoteDir := initRunIntegrationRepoWithRemote(t, dir)
	runGit(t, dir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	writeSandmanDockerfile(t, dir)

	homeDir := newE2EQueueingHome(t)
	gitConfig := e2eQueueingGitConfig(homeDir, remoteDir)
	if err := os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitConfig), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	t.Setenv("HOME", homeDir)
	if podmanOut, err := exec.Command("podman", "run", "--rm", "alpine", "echo", "ok").CombinedOutput(); err != nil {
		t.Fatalf("warm podman: %v: %s", err, podmanOut)
	}

	issueMap := map[int]*github.Issue{}
	issueArgs := []string{}
	for _, n := range issues {
		issueMap[n] = &github.Issue{Number: n, Title: titleForIssue(n), Body: fmt.Sprintf("Body for %d.", n)}
		issueArgs = append(issueArgs, fmt.Sprintf("%d", n))
	}
	gh := &fakeGitHubClient{
		issues: issueMap,
		prs:    prs,
	}

	script := issueAwareAgentCommand(e2eAgentScript())
	deps := newRunIntegrationDepsWithSandbox(config.Agent{Name: "test-agent", Command: script}, "podman", gh)

	args := []string{
		"--sandbox", "podman",
		"--parallel", fmt.Sprintf("%d", parallel),
		"--container-capacity", fmt.Sprintf("%d", capacity),
		"--max-containers", fmt.Sprintf("%d", maxContainers),
	}
	args = append(args, issueArgs...)

	runOut, err := executeRunCommand(t, deps, args...)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, runOut)
	}
	return runOut, dir
}

func titleForIssue(n int) string {
	titles := map[int]string{
		21: "Alpha Task", 22: "Beta Task", 23: "Gamma Task", 24: "Delta Task",
		31: "Alpha Task", 32: "Beta Task", 33: "Gamma Task", 34: "Delta Task",
		41: "Alpha Task", 42: "Beta Task", 43: "Gamma Task", 44: "Delta Task",
		51: "Alpha Task", 52: "Beta Task", 53: "Gamma Task", 54: "Delta Task",
		61: "Alpha Task", 62: "Beta Task", 63: "Gamma Task", 64: "Delta Task",
	}
	return titles[n]
}

func prsForIssues(issues []int) map[string]*github.PR {
	prs := map[string]*github.PR{}
	for _, n := range issues {
		branch := fmt.Sprintf("%d-%s", n, slugForIssue(n))
		prs[branch] = &github.PR{Number: n, State: "closed", Merged: true, HeadRefName: branch, HeadRefOid: ""}
	}
	return prs
}

func slugForIssue(n int) string {
	slugs := map[int]string{
		21: "alpha-task", 22: "beta-task", 23: "gamma-task", 24: "delta-task",
		31: "alpha-task", 32: "beta-task", 33: "gamma-task", 34: "delta-task",
		41: "alpha-task", 42: "beta-task", 43: "gamma-task", 44: "delta-task",
		51: "alpha-task", 52: "beta-task", 53: "gamma-task", 54: "delta-task",
		61: "alpha-task", 62: "beta-task", 63: "gamma-task", 64: "delta-task",
	}
	return slugs[n]
}

func readQueueingLogField(t *testing.T, dir string, issue int, prefix string) string {
	t.Helper()
	logPath := findRunLogForIssue(t, dir, issue)
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log for issue %d: %v", issue, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(logData)), "\n") {
		if idx := strings.Index(line, prefix); idx >= 0 {
			return strings.TrimSpace(line[idx+len(prefix):])
		}
	}
	return ""
}

func readStartTimestamp(t *testing.T, dir string, issue int) int64 {
	t.Helper()
	logPath := findRunLogForIssue(t, dir, issue)
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log for issue %d: %v", issue, err)
	}
	return info.ModTime().UnixNano()
}

// TestRun_BatchEffectiveParallel_AutoMode verifies the auto mode
// (max_containers=0) case where container_capacity equals the requested
// parallel: all 4 issues share one container and run in parallel.
func TestRun_BatchEffectiveParallel_AutoMode(t *testing.T) {
	issues := []int{21, 22, 23, 24}
	out, dir := runEffectiveParallelE2E(t, issues, 4, 4, 0, prsForIssues(issues))
	if !strings.Contains(out, "Summary: 4 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	hostnames := map[string]struct{}{}
	for _, issue := range issues {
		hostname := readQueueingLogField(t, dir, issue, "container-identity=")
		if hostname == "" {
			t.Fatalf("missing container identity for issue %d", issue)
		}
		hostnames[hostname] = struct{}{}
	}
	if got := len(hostnames); got != 1 {
		t.Fatalf("expected all 4 issues on 1 container (capacity=4), got %d distinct hostnames: %v", got, hostnames)
	}

	// With capacity=4, all 4 issues run concurrently. Their start timestamps
	// should be within a small window of each other.
	assertConcurrentStarts(t, dir, issues, 2*time.Second)
}

// TestRun_BatchEffectiveParallel_AutoModeSpawnsPerCapacity verifies that
// in auto mode (max_containers=0) with parallel=4, capacity=2, the pool
// spawns 2 concurrent containers (4/2 = 2) and all 4 issues run
// concurrently.
func TestRun_BatchEffectiveParallel_AutoModeSpawnsPerCapacity(t *testing.T) {
	issues := []int{31, 32, 33, 34}
	out, dir := runEffectiveParallelE2E(t, issues, 4, 2, 0, prsForIssues(issues))
	if !strings.Contains(out, "Summary: 4 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	hostnames := map[string]struct{}{}
	for _, issue := range issues {
		hostname := readQueueingLogField(t, dir, issue, "container-identity=")
		if hostname == "" {
			t.Fatalf("missing container identity for issue %d", issue)
		}
		hostnames[hostname] = struct{}{}
	}
	// In auto mode with effectiveParallel=4 and capacity=2, the pool
	// spawns 2 concurrent containers, each holding 2 runs.
	if got := len(hostnames); got != 2 {
		t.Fatalf("expected 2 distinct containers (auto mode, parallel=4 capacity=2), got %d: %v", got, hostnames)
	}

	// All 4 issues should start concurrently.
	assertConcurrentStarts(t, dir, issues, 2*time.Second)
}

// TestRun_BatchEffectiveParallel_ExplicitMax verifies the explicit
// max_containers path: 4 slots across 2 containers means 4 concurrent starts.
func TestRun_BatchEffectiveParallel_ExplicitMax(t *testing.T) {
	issues := []int{41, 42, 43, 44}
	out, dir := runEffectiveParallelE2E(t, issues, 4, 2, 2, prsForIssues(issues))
	if !strings.Contains(out, "Summary: 4 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	hostnames := map[string]struct{}{}
	for _, issue := range issues {
		hostname := readQueueingLogField(t, dir, issue, "container-identity=")
		if hostname == "" {
			t.Fatalf("missing container identity for issue %d", issue)
		}
		hostnames[hostname] = struct{}{}
	}
	// 4 slots across 2 containers -> 2 distinct hostnames.
	if got := len(hostnames); got != 2 {
		t.Fatalf("expected 2 distinct containers (capacity=2 * max=2 = 4 slots), got %d: %v", got, hostnames)
	}

	// With effectiveParallel=4, all 4 issues run concurrently.
	assertConcurrentStarts(t, dir, issues, 2*time.Second)
}

// TestRun_BatchEffectiveParallel_SerialByTurn verifies the effectiveParallel=1
// path: only 1 runs at a time and FIFO order is preserved. This uses
// explicit max_containers=1 so the cap (capacity*max=1) forces serial
// execution; auto mode (max=0) would now permit full parallelism.
func TestRun_BatchEffectiveParallel_SerialByTurn(t *testing.T) {
	issues := []int{51, 52, 53, 54}
	out, dir := runEffectiveParallelE2E(t, issues, 4, 1, 1, prsForIssues(issues))
	if !strings.Contains(out, "Summary: 4 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	// With effectiveParallel=1, each issue starts only after the previous one
	// finishes. The 4 start timestamps should be in increasing order, each
	// separated by at least the sleep duration (2s minus scheduling slack).
	starts := make(map[int]int64, len(issues))
	for _, issue := range issues {
		starts[issue] = readStartTimestamp(t, dir, issue)
	}
	for i := 1; i < len(issues); i++ {
		gap := starts[issues[i]] - starts[issues[i-1]]
		if gap < int64(1*time.Second) {
			t.Fatalf("expected at least 1s between consecutive starts (effectiveParallel=1), got %v between %d and %d\nstarts: %v",
				time.Duration(gap), issues[i-1], issues[i], starts)
		}
	}
}

// TestRun_BatchEffectiveParallel_UnlimitedParallel verifies the parallel=0
// unlimited path: all 4 run in parallel, one per container.
func TestRun_BatchEffectiveParallel_UnlimitedParallel(t *testing.T) {
	issues := []int{61, 62, 63, 64}
	out, dir := runEffectiveParallelE2E(t, issues, 0, 1, 0, prsForIssues(issues))
	if !strings.Contains(out, "Summary: 4 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	hostnames := map[string]struct{}{}
	for _, issue := range issues {
		hostname := readQueueingLogField(t, dir, issue, "container-identity=")
		if hostname == "" {
			t.Fatalf("missing container identity for issue %d", issue)
		}
		hostnames[hostname] = struct{}{}
	}
	// parallel=0 is unlimited, so the pool auto-scales: 1 issue per container.
	if got := len(hostnames); got != 4 {
		t.Fatalf("expected 4 distinct containers (parallel=0 unlimited, capacity=1 per container), got %d: %v", got, hostnames)
	}

	// All 4 should start concurrently.
	assertConcurrentStarts(t, dir, issues, 2*time.Second)
}

// assertConcurrentStarts verifies that all issues started within the given
// window of the first start. This is a proxy for "ran concurrently": if the
// orchestrator serialised them, later starts would be much later.
func assertConcurrentStarts(t *testing.T, dir string, issues []int, window time.Duration) {
	t.Helper()
	if len(issues) == 0 {
		return
	}
	starts := make([]int64, len(issues))
	for i, issue := range issues {
		starts[i] = readStartTimestamp(t, dir, issue)
	}
	// Find the earliest start.
	var earliest int64 = starts[0]
	for _, s := range starts[1:] {
		if s < earliest {
			earliest = s
		}
	}
	for i, s := range starts {
		gap := s - earliest
		if gap > window.Nanoseconds() {
			t.Fatalf("issue %d started %v after the earliest start (window %v) - not concurrent\nstarts: %v",
				issues[i], time.Duration(gap), window, starts)
		}
	}
}
