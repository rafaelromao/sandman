//go:build e2e

package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// TestRunBatch_ContextRolloverWorktreeEndToEnd exercises the complete
// production retry path with a real git worktree and WorktreeSandbox. The
// existing unit tests cover each seam independently; this test protects the
// failure that motivated the feature: a stuck OpenCode process must not keep
// writing after the clean retry starts, and the retry must inherit all durable
// work from the exhausted attempt.
func TestRunBatch_ContextRolloverWorktreeEndToEnd(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBatch) {
		t.Skip("set SANDMAN_E2E_GATES=batch (or all) to run context rollover e2e")
	}

	t.Run("clean retry preserves worktree state", func(t *testing.T) {
		runContextRolloverWorktreeE2E(t, false)
	})
	t.Run("final exhaustion remains failure", func(t *testing.T) {
		runContextRolloverWorktreeE2E(t, true)
	})
}

func runContextRolloverWorktreeE2E(t *testing.T, finalExhaustion bool) {
	t.Helper()

	dir := testenv.MkdirShort(t, "sm-context-rollover-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0o755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	fakeBin := filepath.Join(dir, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "gh"), []byte("#!/bin/sh\nprintf '%s\\n' '[]'\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateDir := filepath.Join(dir, "context-rollover-state")
	scriptPath := filepath.Join(dir, "fake-opencode.sh")
	writeContextRolloverAgent(t, scriptPath, stateDir, finalExhaustion)

	const issueNumber = 42
	branch := BranchName(issueNumber, "Context rollover e2e", "main")
	issue := &github.Issue{
		Number: issueNumber,
		Title:  "Context rollover e2e",
		Body:   "Preserve durable progress across a context rollover.",
		State:  "closed",
	}
	client := &contextRolloverGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{issues: map[int]*github.Issue{issueNumber: issue}},
		stateDir:         stateDir,
		branch:           branch,
	}
	store := &fakeConfigStore{config: &config.Config{
		DefaultAgent: "opencode",
		Agent:        "opencode",
		Sandbox:      "worktree",
		WorktreeDir:  ".sandman/worktrees",
		Git:          config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"opencode": {
				Preset:  "opencode",
				Command: scriptPath,
			},
		},
	}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(dir, ".sandman", "events.jsonl")}
	o := NewOrchestrator(client, &retryRenderer{result: "# Task\n\nInitial durable work."}, store, eventLog)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{issueNumber}, Retries: 1})
	if finalExhaustion {
		if err == nil {
			t.Fatal("expected final context exhaustion to return an error")
		}
	} else if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if result == nil || len(result.Runs) != 1 {
		t.Fatalf("result = %+v, want one run", result)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", branch)
	run := result.Runs[0]
	if run.Branch != branch {
		t.Fatalf("result branch = %q, want %q", run.Branch, branch)
	}
	if run.WorktreePath != worktreePath {
		t.Fatalf("result worktree = %q, want %q", run.WorktreePath, worktreePath)
	}
	if run.RetriesTotal != 2 {
		t.Fatalf("RetriesTotal = %d, want two attempts", run.RetriesTotal)
	}
	wantStatus := "success"
	if finalExhaustion {
		wantStatus = "failure"
	}
	if run.Status != wantStatus {
		state, _ := os.ReadFile(filepath.Join(stateDir, "launches"))
		task, _ := os.ReadFile(filepath.Join(worktreePath, ".sandman", "task.md"))
		logged, _ := eventLog.Read()
		t.Fatalf("status = %q, want %q; result=%+v launches=%q task=%q events=%+v", run.Status, wantStatus, run, state, task, logged)
	}
	if finalExhaustion != run.ContextExhausted {
		t.Fatalf("ContextExhausted = %v, want %v", run.ContextExhausted, finalExhaustion)
	}

	committed := runGit(t, worktreePath, "show", "HEAD:context-committed.txt")
	if strings.TrimSpace(committed) != "committed checkpoint" {
		t.Fatalf("committed checkpoint = %q", committed)
	}
	status := runGit(t, worktreePath, "status", "--short")
	if !strings.Contains(status, "context-uncommitted.txt") {
		t.Fatalf("uncommitted progress was not preserved; git status = %q", status)
	}
	if finalExhaustion {
		if _, err := os.Stat(filepath.Join(worktreePath, "context-recovered.txt")); !os.IsNotExist(err) {
			t.Fatalf("final exhausted attempt unexpectedly completed recovery: %v", err)
		}
	} else if _, err := os.Stat(filepath.Join(worktreePath, "context-recovered.txt")); err != nil {
		t.Fatalf("clean retry did not run to completion: %v", err)
	}

	task, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "task.md"))
	if err != nil {
		t.Fatalf("read recovery Task: %v", err)
	}
	if !strings.Contains(string(task), "Context Recovery Guard") || !strings.Contains(string(task), "Initial durable work.") {
		t.Fatalf("recovery Task lost the checkpoint-first handoff or original task:\n%s", task)
	}

	launches, err := os.ReadFile(filepath.Join(stateDir, "launches"))
	if err != nil {
		t.Fatalf("read launch count: %v", err)
	}
	if got := strings.TrimSpace(string(launches)); got != "1\n2" {
		t.Fatalf("agent launches = %q, want exactly two sequential attempts", got)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(worktreePath, "context-concurrent-write.txt")); !os.IsNotExist(err) {
		t.Fatalf("stopped attempt kept writing after retry boundary: %v", err)
	}

	logged, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	assertContextRolloverEvents(t, logged, branch, finalExhaustion)
}

func writeContextRolloverAgent(t *testing.T, path, stateDir string, finalExhaustion bool) {
	t.Helper()
	mode := "success"
	if finalExhaustion {
		mode = "failure"
	}
	script := fmt.Sprintf(`#!/bin/sh
set -eu

state_dir=%s
mode=%s
mkdir -p "$state_dir"
attempt_file="$state_dir/attempt"
launches_file="$state_dir/launches"

if [ ! -f "$attempt_file" ]; then
  printf '1\n' > "$attempt_file"
  printf 'committed checkpoint\n' > context-committed.txt
  git add context-committed.txt
  git commit -m 'agent checkpoint' >/dev/null
  printf 'uncommitted progress\n' > context-uncommitted.txt
  printf '1\n' >> "$launches_file"
  (sleep 1; printf 'late write\n' > context-concurrent-write.txt) &
  printf 'Error: prompt is too long\nError: prompt is too long\n' >&2
  wait
  exit 1
fi

printf '2\n' > "$attempt_file"
printf '2\n' >> "$launches_file"
git rev-parse HEAD > "$state_dir/head"
if [ ! -f context-committed.txt ] || [ ! -f context-uncommitted.txt ]; then
  exit 20
fi
if ! grep -q 'Context Recovery Guard' .sandman/task.md; then
  exit 21
fi
if [ "$mode" = "failure" ]; then
  (sleep 1; printf 'late write\n' > context-concurrent-write.txt) &
  printf 'Error: prompt is too long\nError: prompt is too long\n' >&2
  wait
  exit 1
fi

printf 'recovered\n' > context-recovered.txt
touch "$state_dir/succeeded"
exit 0
`, shellQuoteForTest(stateDir), shellQuoteForTest(mode))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake OpenCode: %v", err)
	}
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func assertContextRolloverEvents(t *testing.T, logged []events.Event, branch string, finalExhaustion bool) {
	t.Helper()
	var started, retry, finished *events.Event
	startedAt, retryAt, finishedAt := -1, -1, -1
	for i := range logged {
		event := &logged[i]
		if event.Type == "run.started" && started == nil {
			started, startedAt = event, i
		}
		if event.Type == "run.retry" {
			retry, retryAt = event, i
		}
		if event.Type == "run.finished" {
			finished, finishedAt = event, i
		}
	}
	if started == nil || retry == nil || finished == nil {
		t.Fatalf("events = %+v, want started, retry, and finished lifecycle", logged)
	}
	if !(startedAt < retryAt && retryAt < finishedAt) {
		t.Fatalf("event order = started:%d retry:%d finished:%d", startedAt, retryAt, finishedAt)
	}
	if retry.Payload["reason"] != "context-exhausted" {
		t.Fatalf("retry reason = %v, want context-exhausted", retry.Payload["reason"])
	}
	if retry.Payload["attempt"] != float64(2) && retry.Payload["attempt"] != 2 {
		t.Fatalf("retry attempt = %v, want 2", retry.Payload["attempt"])
	}
	if retry.Payload["branch"] != branch {
		t.Fatalf("retry branch = %v, want %q", retry.Payload["branch"], branch)
	}
	if finished.Payload["branch"] != branch {
		t.Fatalf("finished branch = %v, want %q", finished.Payload["branch"], branch)
	}
	if finalExhaustion {
		if finished.Payload["context_exhausted"] != true {
			t.Fatalf("finished payload = %+v, want context_exhausted=true", finished.Payload)
		}
		for _, event := range logged {
			if event.Type == "run.aborted" {
				t.Fatalf("final exhaustion emitted abort event: %+v", logged)
			}
		}
	} else if finished.Payload["context_exhausted"] == true {
		t.Fatalf("successful retry retained terminal context marker: %+v", finished.Payload)
	}
	for _, event := range logged {
		if event.RunID != started.RunID {
			t.Fatalf("event RunID = %q, want stable %q", event.RunID, started.RunID)
		}
	}
}

var _ prompt.IssueRenderer = (*retryRenderer)(nil)

type contextRolloverGitHubClient struct {
	*fakeGitHubClient
	stateDir string
	branch   string
	mu       sync.Mutex
}

func (c *contextRolloverGitHubClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if branch != c.branch {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(c.stateDir, "succeeded")); err == nil {
		head, _ := os.ReadFile(filepath.Join(c.stateDir, "head"))
		return &github.PR{
			Number:      42,
			State:       "closed",
			Merged:      true,
			HeadRefName: branch,
			HeadRefOid:  strings.TrimSpace(string(head)),
			Body:        "Closes #42",
		}, nil
	}
	return &github.PR{Number: 42, State: "closed", HeadRefName: branch}, nil
}
