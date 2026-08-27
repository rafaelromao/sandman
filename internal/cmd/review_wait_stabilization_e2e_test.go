//go:build e2e

package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestReviewWaitStabilization_PendingLifecycleStaysForeground(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioReviewWait) {
		t.Skip("set SANDMAN_E2E_GATES=review_wait to run the review-wait stabilization scenario")
	}

	binPath := buildSandmanBinary(t)
	runReviewWaitScenario(t, binPath, false)
}

func TestReviewWaitStabilization_CancellationAbortsForegroundWait(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioReviewWait) {
		t.Skip("set SANDMAN_E2E_GATES=review_wait to run the review-wait stabilization scenario")
	}

	binPath := buildSandmanBinary(t)
	runReviewWaitScenario(t, binPath, true)
}

func TestReviewWaitStabilization_CIFailureFixReviewMerge(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioReviewWait) {
		t.Skip("set SANDMAN_E2E_GATES=review_wait to run the review-wait stabilization scenario")
	}

	binPath := buildSandmanBinary(t)
	repoDir := testenv.MkdirShort(t, "sm-review-remediate-")
	initRunIntegrationRepoWithRemote(t, repoDir)

	sandmanDir := filepath.Join(repoDir, ".sandman")
	shimDir := filepath.Join(sandmanDir, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("create shim directory: %v", err)
	}
	statePath := filepath.Join(sandmanDir, "review-remediate.state")
	callLogPath := filepath.Join(sandmanDir, "gh.calls")
	agentCountPath := filepath.Join(sandmanDir, "agent.count")
	writeReviewRemediationGHShim(t, shimDir, statePath, callLogPath)
	writeReviewRemediationAgent(t, filepath.Join(shimDir, "fake-agent"), statePath, agentCountPath)
	writeReviewWaitConfig(t, sandmanDir, filepath.Join(shimDir, "fake-agent"))

	portalURL := startPortalBinary(t, binPath, repoDir, shimDir)
	cmd := exec.Command(binPath, "run", "--agent", "fake", "--sandbox", "worktree", "--parallel", "1", "--retries", "0", "--run-idle-timeout", "1", "42")
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	t.Cleanup(func() {
		t.Logf("sandman stdout:\n%s", stdout.String())
		t.Logf("sandman stderr:\n%s", stderr.String())
	})
	cmd.Env = append(os.Environ(),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_TOKEN=fake",
		"GITHUB_TOKEN=fake",
		"HOME="+filepath.Join(repoDir, ".sandman-test-home"),
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sandman run: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	eventsPath := filepath.Join(sandmanDir, "events.jsonl")
	waitForReviewWaitEvent(t, eventsPath, 42, "run.resumed")
	waitForReviewWaitEvent(t, eventsPath, 42, "run.await")
	if got := readTrimmedFile(t, agentCountPath); got != "2" {
		t.Fatalf("agent launches = %s, want 2 after CI remediation", got)
	}
	logs := readReviewWaitEvents(t, eventsPath)
	if got := countReviewWaitEvents(logs, 42, "run.finished"); got != 0 {
		t.Fatalf("run finished before delegated review resolved: %d", got)
	}
	waitForPortalRun(t, portalURL, 42, func(run portalRun) bool {
		return run.Status == "waiting" && run.SocketPath != "" && run.FinishedAt == nil
	})

	if err := os.WriteFile(statePath, []byte("merged\n"), 0o644); err != nil {
		t.Fatalf("resolve pull request fixture: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sandman run after review merge: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
	case <-time.After(150 * time.Second):
		t.Fatal("sandman run did not finish after CI remediation and pull-request merge")
	}
	logs = readReviewWaitEvents(t, eventsPath)
	if countReviewWaitEvents(logs, 42, "run.resumed") != 1 {
		t.Fatalf("run.resumed events = %d, want 1 CI remediation", countReviewWaitEvents(logs, 42, "run.resumed"))
	}
	if countReviewWaitEvents(logs, 42, "run.await") == 0 {
		t.Fatal("expected review await after CI remediation")
	}
	if countReviewWaitEvents(logs, 42, "run.finished") != 1 {
		t.Fatalf("run.finished events = %d, want 1", countReviewWaitEvents(logs, 42, "run.finished"))
	}
	terminalRun := waitForPortalRun(t, portalURL, 42, func(run portalRun) bool {
		return run.Status == "success" && run.FinishedAt != nil
	})
	if terminalRun.FinishedAt == nil {
		t.Fatal("merged portal row has no terminal timestamp")
	}
}

func runReviewWaitScenario(t *testing.T, binPath string, cancelPending bool) {
	t.Helper()
	repoDir := testenv.MkdirShort(t, "sm-review-wait-")
	initRunIntegrationRepoWithRemote(t, repoDir)

	sandmanDir := filepath.Join(repoDir, ".sandman")
	shimDir := filepath.Join(sandmanDir, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("create shim directory: %v", err)
	}
	statePath := filepath.Join(sandmanDir, "review-wait.state")
	callLogPath := filepath.Join(sandmanDir, "gh.calls")
	agentStartedPath := filepath.Join(sandmanDir, "agent.started")
	writeReviewWaitGHShim(t, shimDir, statePath, callLogPath)
	writeReviewWaitAgent(t, filepath.Join(shimDir, "fake-agent"), agentStartedPath)
	writeReviewWaitConfig(t, sandmanDir, filepath.Join(shimDir, "fake-agent"))

	portalURL := startPortalBinary(t, binPath, repoDir, shimDir)

	cmd := exec.Command(binPath, "run", "--agent", "fake", "--sandbox", "worktree", "--parallel", "1", "--retries", "0", "--run-idle-timeout", "1", "42", "43")
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	t.Cleanup(func() {
		t.Logf("sandman stdout:\n%s", stdout.String())
		t.Logf("sandman stderr:\n%s", stderr.String())
	})
	cmd.Env = append(os.Environ(),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_TOKEN=fake",
		"GITHUB_TOKEN=fake",
		"HOME="+filepath.Join(repoDir, ".sandman-test-home"),
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sandman run: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	eventsPath := filepath.Join(sandmanDir, "events.jsonl")
	waitForReviewWaitEvent(t, eventsPath, 42, "run.started")
	waitForReviewWaitEvent(t, eventsPath, 42, "run.await")
	waitForFile(t, agentStartedPath)
	waitForReviewWaitSocket(t, repoDir)
	pendingRun := waitForPortalRun(t, portalURL, 42, func(run portalRun) bool {
		return run.Status == "waiting" && run.SocketPath != "" && run.FinishedAt == nil
	})
	if pendingRun.Status != "waiting" {
		t.Fatalf("pending portal status = %q, want waiting", pendingRun.Status)
	}

	logs := readReviewWaitEvents(t, eventsPath)
	for _, eventType := range []string{"run.idle_timeout", "run.retry", "run.finished", "run.aborted"} {
		if got := countReviewWaitEvents(logs, 42, eventType); got != 0 {
			t.Fatalf("pending parent %s events = %d, want 0", eventType, got)
		}
	}
	if countReviewWaitEvents(logs, 43, "run.started") != 0 {
		t.Fatal("dependent started while parent lifecycle was pending")
	}
	select {
	case err := <-done:
		t.Fatalf("sandman run exited during foreground wait: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	default:
	}
	assertReviewWaitRemainsPending(t, eventsPath, repoDir, portalURL, done)

	if cancelPending {
		status, body := writeAbortRequest(t, portalURL, pendingRun.Key, 42)
		if status != 200 {
			t.Fatalf("abort status = %d, body = %s", status, body)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("canceled sandman run exited successfully")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("canceled sandman run did not exit")
		}
		terminalRun := waitForPortalRun(t, portalURL, 42, func(run portalRun) bool {
			return run.Status == "aborted" && run.FinishedAt != nil
		})
		if terminalRun.FinishedAt == nil {
			t.Fatal("canceled portal row has no terminal timestamp")
		}
		logs = readReviewWaitEvents(t, eventsPath)
		if countReviewWaitEvents(logs, 42, "run.aborted") != 1 {
			t.Fatalf("aborted parent events = %d, want 1", countReviewWaitEvents(logs, 42, "run.aborted"))
		}
		for _, eventType := range []string{"run.retry", "run.finished"} {
			if got := countReviewWaitEvents(logs, 42, eventType); got != 0 {
				t.Fatalf("canceled parent %s events = %d, want 0", eventType, got)
			}
		}
		if countReviewWaitEvents(logs, 43, "run.started") != 0 {
			t.Fatal("dependent started after parent cancellation")
		}
		return
	}

	if err := os.WriteFile(statePath, []byte("merged\n"), 0o644); err != nil {
		t.Fatalf("resolve pull request fixture: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sandman run after merge: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
	case <-time.After(150 * time.Second):
		t.Fatal("sandman run did not finish after pull-request resolution")
	}
	terminalRun := waitForPortalRun(t, portalURL, 42, func(run portalRun) bool {
		return run.Status == "success" && run.FinishedAt != nil
	})
	if terminalRun.FinishedAt == nil {
		t.Fatal("merged portal row has no terminal timestamp")
	}
	logs = readReviewWaitEvents(t, eventsPath)
	if countReviewWaitEvents(logs, 42, "run.finished") != 1 {
		t.Fatalf("merged parent finished events = %d, want 1", countReviewWaitEvents(logs, 42, "run.finished"))
	}
	if countReviewWaitEvents(logs, 42, "run.aborted") != 0 {
		t.Fatal("merged parent emitted run.aborted")
	}
	if countReviewWaitEvents(logs, 43, "run.started") != 1 {
		t.Fatalf("dependent started events = %d, want 1 after parent completion", countReviewWaitEvents(logs, 43, "run.started"))
	}
}

func assertReviewWaitRemainsPending(t *testing.T, eventsPath, repoDir, portalURL string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("sandman run exited before idle-timeout observation completed: %v", err)
		default:
		}
		logs := readReviewWaitEvents(t, eventsPath)
		for _, eventType := range []string{"run.idle_timeout", "run.retry", "run.finished", "run.aborted"} {
			if got := countReviewWaitEvents(logs, 42, eventType); got != 0 {
				t.Fatalf("pending parent %s events = %d during idle-timeout observation, want 0", eventType, got)
			}
		}
		sockets, _ := filepath.Glob(filepath.Join(repoDir, ".sandman", "batches", "*", "batch.sock"))
		if len(sockets) == 0 {
			t.Fatal("batch socket disappeared during foreground observation")
		}
		run := waitForPortalRun(t, portalURL, 42, func(run portalRun) bool {
			return run.Status == "waiting" && run.SocketPath != "" && run.FinishedAt == nil
		})
		if run.LastOutputAt == nil || time.Since(*run.LastOutputAt) > 5*time.Second {
			t.Fatalf("portal marked foreground wait stale: lastOutputAt=%v", run.LastOutputAt)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func writeReviewWaitConfig(t *testing.T, sandmanDir, agentPath string) {
	t.Helper()
	config := fmt.Sprintf(`agent: fake
review_command: /oc review
parallel: 1
retries: 0
run_idle_timeout: 1
worktree_dir: .sandman/worktrees
sandbox: worktree
git:
  base_branch: main
agents:
  fake:
    command: %s
`, agentPath)
	if err := os.WriteFile(filepath.Join(sandmanDir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatalf("write review-wait config: %v", err)
	}
}

func writeReviewWaitAgent(t *testing.T, path, startedPath string) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nprintf started > %q\nexit 0\n", startedPath)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake review-wait agent: %v", err)
	}
}

func writeReviewWaitGHShim(t *testing.T, dir, statePath, callLogPath string) {
	t.Helper()
	script := strings.ReplaceAll(strings.ReplaceAll(`#!/bin/sh
set -eu
state_file="__STATE__"
call_log="__CALLS__"
printf '%s\n' "$*" >> "$call_log"

if [ "${1:-}" = "auth" ] && [ "${2:-}" = "token" ]; then
  printf 'fake-token\n'
  exit 0
fi
if [ "${1:-}" = "repo" ] && [ "${2:-}" = "view" ]; then
  printf '{"name":"sandbox","owner":{"login":"example"}}\n'
  exit 0
fi
if [ "${1:-}" = "api" ]; then
  path=""
  for arg in "$@"; do
    case "$arg" in
      repos/*) path="$arg" ;;
    esac
  done
  case "$path" in
    repos/example/sandbox/issues/42)
      issue_state=OPEN
      if [ -f "$state_file" ] && [ "$(tr -d '\\n' < "$state_file")" = "merged" ]; then issue_state=CLOSED; fi
      printf '{"number":42,"title":"parent","body":"Parent implementation","state":"%s","labels":[]}\n' "$issue_state" ; exit 0 ;;
    repos/example/sandbox/issues/43)
      printf '{"number":43,"title":"dependent","body":"## Blocked by\\n- #42","state":"OPEN","labels":[]}\n' ; exit 0 ;;
    */dependencies/blocked_by|*/events|*/sub_issues*)
      printf '[]\n' ; exit 0 ;;
    */comments*)
      printf '[]\n' ; exit 0 ;;
  esac
fi

if [ "${1:-}" = "pr" ] && [ "${2:-}" = "list" ]; then
  head=""
  previous=""
  for arg in "$@"; do
    if [ "$previous" = "--head" ]; then head="$arg"; fi
    previous="$arg"
  done
  if case "$head" in 43-*) true ;; *) false ;; esac; then
    printf '[{"number":43,"title":"dependent","body":"Closes #43","state":"MERGED","mergedAt":"2026-08-21T00:00:00Z","headRefName":"%s","headRefOid":"dependent-sha","reviewDecision":"APPROVED","mergeStateStatus":"CLEAN","statusCheckRollup":"success"}]\n' "$head"
  elif [ -f "$state_file" ] && [ "$(tr -d '\n' < "$state_file")" = "merged" ]; then
    printf '[{"number":42,"title":"parent","body":"Closes #42","state":"MERGED","mergedAt":"2026-08-21T00:00:00Z","headRefName":"%s","headRefOid":"current-sha","reviewDecision":"APPROVED","mergeStateStatus":"CLEAN","statusCheckRollup":"success"}]\n' "$head"
  else
    printf '[{"number":42,"title":"parent","body":"Closes #42","state":"OPEN","mergedAt":null,"headRefName":"%s","headRefOid":"current-sha","reviewDecision":"","mergeStateStatus":"BLOCKED","statusCheckRollup":"pending"}]\n' "$head"
  fi
  exit 0
fi
if [ "${1:-}" = "pr" ] && [ "${2:-}" = "view" ]; then
  printf '{"number":42,"title":"parent","body":"Closes #42","state":"OPEN","mergedAt":null,"headRefName":"42-parent","headRefOid":"current-sha","closingIssuesReferences":[{"number":42}]}\n'
  exit 0
fi
printf 'unexpected gh command: %s\n' "$*" >&2
exit 1
`, "__STATE__", statePath), "__CALLS__", callLogPath)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write review-wait gh shim: %v", err)
	}
}

func writeReviewRemediationAgent(t *testing.T, path, statePath, countPath string) {
	t.Helper()
	script := strings.ReplaceAll(strings.ReplaceAll(`#!/bin/sh
set -eu
count_file="__COUNT__"
state_file="__STATE__"
count=0
if [ -f "$count_file" ]; then count=$(tr -d '\n' < "$count_file"); fi
count=$((count + 1))
printf '%s' "$count" > "$count_file"
if [ "$count" -ge 2 ]; then printf 'review-pending\n' > "$state_file"; fi
exit 0
`, "__COUNT__", countPath), "__STATE__", statePath)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake review-remediation agent: %v", err)
	}
}

func writeReviewRemediationGHShim(t *testing.T, dir, statePath, callLogPath string) {
	t.Helper()
	script := strings.ReplaceAll(strings.ReplaceAll(`#!/bin/sh
set -eu
state_file="__STATE__"
call_log="__CALLS__"
printf '%s\n' "$*" >> "$call_log"
state=ci-failed
if [ -f "$state_file" ]; then state=$(tr -d '\n' < "$state_file"); fi

if [ "${1:-}" = "auth" ] && [ "${2:-}" = "token" ]; then
  printf 'fake-token\n'
  exit 0
fi
if [ "${1:-}" = "repo" ] && [ "${2:-}" = "view" ]; then
  printf '{"name":"sandbox","owner":{"login":"example"}}\n'
  exit 0
fi
if [ "${1:-}" = "api" ]; then
  path=""
  for arg in "$@"; do
    case "$arg" in
      repos/*) path="$arg" ;;
    esac
  done
  case "$path" in
    repos/example/sandbox/issues/42)
      issue_state=OPEN
      if [ "$state" = "merged" ]; then issue_state=CLOSED; fi
      printf '{"number":42,"title":"parent","body":"Parent implementation","state":"%s","labels":[]}\n' "$issue_state" ; exit 0 ;;
    */dependencies/blocked_by|*/events|*/sub_issues*)
      printf '[]\n' ; exit 0 ;;
    */comments*)
      printf '[]\n' ; exit 0 ;;
  esac
fi

if [ "${1:-}" = "pr" ] && [ "${2:-}" = "list" ]; then
  head=""
  previous=""
  for arg in "$@"; do
    if [ "$previous" = "--head" ]; then head="$arg"; fi
    previous="$arg"
  done
  case "$state" in
    merged)
      printf '[{"number":42,"title":"parent","body":"Closes #42","state":"MERGED","mergedAt":"2026-08-21T00:00:00Z","headRefName":"%s","headRefOid":"current-sha","reviewDecision":"APPROVED","mergeStateStatus":"CLEAN","statusCheckRollup":"success"}]\n' "$head" ;;
    review-pending)
      printf '[{"number":42,"title":"parent","body":"Closes #42","state":"OPEN","mergedAt":null,"headRefName":"%s","headRefOid":"current-sha","reviewDecision":"REVIEW_REQUIRED","mergeStateStatus":"BLOCKED","statusCheckRollup":"success"}]\n' "$head" ;;
    *)
      printf '[{"number":42,"title":"parent","body":"Closes #42","state":"OPEN","mergedAt":null,"headRefName":"%s","headRefOid":"current-sha","reviewDecision":"APPROVED","mergeStateStatus":"CLEAN","statusCheckRollup":"failure"}]\n' "$head" ;;
  esac
  exit 0
fi
if [ "${1:-}" = "pr" ] && [ "${2:-}" = "view" ]; then
  if [ "$state" = "merged" ]; then
    printf '{"number":42,"title":"parent","body":"Closes #42","state":"MERGED","mergedAt":"2026-08-21T00:00:00Z","headRefName":"42-parent","headRefOid":"current-sha","closingIssuesReferences":[{"number":42}]}\n'
  else
    printf '{"number":42,"title":"parent","body":"Closes #42","state":"OPEN","mergedAt":null,"headRefName":"42-parent","headRefOid":"current-sha","closingIssuesReferences":[{"number":42}]}\n'
  fi
  exit 0
fi
if [ "${1:-}" = "pr" ] && [ "${2:-}" = "merge" ]; then
  printf 'merged\n' > "$state_file"
  exit 0
fi
printf 'unexpected gh command: %s\n' "$*" >&2
exit 1
`, "__STATE__", statePath), "__CALLS__", callLogPath)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write review-remediation gh shim: %v", err)
	}
}

func readTrimmedFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}

func waitForReviewWaitEvent(t *testing.T, path string, issue int, eventType string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range readReviewWaitEvents(t, path) {
			if event.Issue == issue && event.Type == eventType {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	callLog, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "gh.calls"))
	t.Fatalf("timed out waiting for issue %d %s; events=%v; gh calls=%s", issue, eventType, readReviewWaitEvents(t, path), callLog)
}

func waitForReviewWaitSocket(t *testing.T, repoDir string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		batchSockets, _ := filepath.Glob(filepath.Join(repoDir, ".sandman", "batches", "*", "batch.sock"))
		if len(batchSockets) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for live batch socket")
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func readReviewWaitEvents(t *testing.T, path string) []events.Event {
	t.Helper()
	log := &events.JSONLLogger{Path: path}
	result, err := log.Read()
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read review-wait events: %v", err)
	}
	return result
}

func countReviewWaitEvents(events []events.Event, issue int, eventType string) int {
	count := 0
	for _, event := range events {
		if event.Issue == issue && event.Type == eventType {
			count++
		}
	}
	return count
}
