//go:build e2e

package batch

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// TestRunBatch_ClaudePresetWorktreeEndToEnd runs the built-in claude preset
// command through a real git worktree and a real shell against a fake
// `claude` binary on PATH. The first launch stops at a subscription usage
// limit; the run must await instead of retrying, and the re-entry must resume
// the same conversation through Claude Code's --continue.
func TestRunBatch_ClaudePresetWorktreeEndToEnd(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBatch) {
		t.Skip("set SANDMAN_E2E_GATES=batch (or all) to run the claude preset e2e")
	}

	dir := testenv.MkdirShort(t, "sm-claude-preset-")
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
	stateDir := filepath.Join(dir, "claude-state")
	writeFakeClaudeCLI(t, filepath.Join(fakeBin, "claude"), stateDir)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	const issueNumber = 42
	branch := BranchName(issueNumber, "Claude usage limit e2e", "main")
	client := &contextRolloverGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{issues: map[int]*github.Issue{issueNumber: {
			Number: issueNumber,
			Title:  "Claude usage limit e2e",
			Body:   "Resume the same conversation after the limit resets.",
			State:  "closed",
		}}},
		stateDir: stateDir,
		branch:   branch,
	}
	store := &fakeConfigStore{config: &config.Config{
		DefaultAgent:     "claude",
		Agent:            "claude",
		Sandbox:          "worktree",
		WorktreeDir:      ".sandman/worktrees",
		CleanupWorktrees: func() *bool { value := false; return &value }(),
		Git:              config.GitConfig{BaseBranch: "main"},
		AgentProviders:   map[string]config.Agent{"claude": config.BuiltInAgentPresets["claude"].Agent("claude")},
	}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(dir, ".sandman", "events.jsonl")}
	var waits []time.Duration
	var errorLog bytes.Buffer
	o := NewOrchestrator(client, &retryRenderer{result: "# Task\n\nReply with SMOKE_OK.\nKeep it short."}, store, eventLog,
		WithErrorLog(&errorLog),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			awaitWait: func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return nil
			},
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{issueNumber}, Retries: 1, Model: "sonnet"})
	if err != nil {
		t.Fatalf("RunBatch: %v\nerror log:\n%s", err, errorLog.String())
	}
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		logged, _ := eventLog.Read()
		t.Fatalf("result = %+v, want one successful run\nevents=%+v\nerror log:\n%s", result, logged, errorLog.String())
	}
	if len(waits) != 1 || waits[0] != usageLimitPollInterval {
		t.Fatalf("await waits = %v, want one usage-limit poll", waits)
	}

	logged, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	runID := ""
	awaits := 0
	for _, event := range logged {
		switch event.Type {
		case "run.started":
			if runID == "" {
				runID = event.RunID
			}
		case "run.await":
			awaits++
			if event.Payload["await_reason"] != "usage-limit" {
				t.Fatalf("run.await payload = %+v, want await_reason usage-limit", event.Payload)
			}
		case "run.retry":
			t.Fatalf("usage limit took the retry path: %+v", event)
		}
	}
	if runID == "" || awaits != 1 {
		t.Fatalf("run id %q, awaits %d; events=%+v", runID, awaits, logged)
	}

	worktree, err := filepath.EvalSymlinks(filepath.Join(dir, ".sandman", "worktrees", branch))
	if err != nil {
		t.Fatalf("resolve worktree: %v", err)
	}
	// "$(cat ...)" strips the Task file's trailing newlines, as any shell does.
	prompt := strings.TrimRight(readFakeClaudeFile(t, stateDir, "launch-1.task"), "\n")
	wantFirst := []string{"-p", "--output-format", "stream-json", "--verbose", "--name", "Sandman " + runID + ": ", "--model", "sonnet", prompt}
	if got := readFakeClaudeArgs(t, stateDir, 1); !reflect.DeepEqual(got, wantFirst) {
		t.Fatalf("first launch argv:\n got %q\nwant %q", got, wantFirst)
	}
	if !strings.Contains(prompt, "Reply with SMOKE_OK.\nKeep it short.") {
		t.Fatalf("prompt argument = %q, want the rendered Task", prompt)
	}
	secondPrompt := strings.TrimRight(readFakeClaudeFile(t, stateDir, "launch-2.task"), "\n")
	wantSecond := []string{"-p", "--output-format", "stream-json", "--verbose", "--continue", "--name", "Sandman " + runID + ": ", "--model", "sonnet", secondPrompt}
	if got := readFakeClaudeArgs(t, stateDir, 2); !reflect.DeepEqual(got, wantSecond) {
		t.Fatalf("re-entry argv:\n got %q\nwant %q", got, wantSecond)
	}
	for launch := 1; launch <= 2; launch++ {
		env := readFakeClaudeFile(t, stateDir, "launch-"+string(rune('0'+launch))+".env")
		for _, want := range []string{"IS_SANDBOX=1\n", "DISABLE_AUTOUPDATER=1\n", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\n", "PWD=" + worktree + "\n"} {
			if !strings.Contains(env, want) {
				t.Fatalf("launch %d environment = %q, want %q", launch, env, want)
			}
		}
	}

	runLogs, err := filepath.Glob(filepath.Join(dir, ".sandman", "batches", "*", "runs", runID, "run.log"))
	if err != nil || len(runLogs) != 1 {
		t.Fatalf("run.log glob = %v, %v; want one run log", runLogs, err)
	}
	runLog, err := os.ReadFile(runLogs[0])
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}
	for _, want := range []string{"Error: You've hit your session limit · resets 3pm", "Result: success"} {
		if !strings.Contains(string(runLog), want) {
			t.Fatalf("run.log = %q, want rendered %q", runLog, want)
		}
	}
	if strings.Contains(string(runLog), `"type":"result"`) {
		t.Fatalf("run.log = %q, want rendered records instead of raw stream-json", runLog)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(runLogs[0]), "session.json")); !os.IsNotExist(err) {
		t.Fatalf("session.json stat = %v; claude runs keep no OpenCode session identity", err)
	}
	if committed := runGit(t, worktree, "show", "HEAD:claude-resumed.txt"); strings.TrimSpace(committed) != "claude resumed" {
		t.Fatalf("re-entry work = %q, want the resumed commit", committed)
	}
}

func writeFakeClaudeCLI(t *testing.T, path, stateDir string) {
	t.Helper()
	script := strings.ReplaceAll(`#!/bin/sh
set -eu
state_dir=__STATE_DIR__
mkdir -p "$state_dir"
n=$(( $(cat "$state_dir/count" 2>/dev/null || echo 0) + 1 ))
printf '%s\n' "$n" > "$state_dir/count"
for arg in "$@"; do printf '%s\0' "$arg"; done > "$state_dir/launch-$n.args"
printf 'IS_SANDBOX=%s\nDISABLE_AUTOUPDATER=%s\nCLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=%s\nPWD=%s\n' \
  "${IS_SANDBOX:-}" "${DISABLE_AUTOUPDATER:-}" "${CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC:-}" "$(pwd -P)" > "$state_dir/launch-$n.env"
cp .sandman/task.md "$state_dir/launch-$n.task"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"conv-1"}'
if [ "$n" = 1 ]; then
  printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"result":"You'"'"'ve hit your session limit · resets 3pm"}'
  exit 1
fi
printf 'claude resumed\n' > claude-resumed.txt
git add claude-resumed.txt
git commit -m 'claude checkpoint' >/dev/null
git rev-parse HEAD > "$state_dir/head"
touch "$state_dir/succeeded"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`, "__STATE_DIR__", shellQuoteForTest(stateDir))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
}

func readFakeClaudeArgs(t *testing.T, stateDir string, launch int) []string {
	t.Helper()
	data := readFakeClaudeFile(t, stateDir, "launch-"+string(rune('0'+launch))+".args")
	return strings.Split(strings.TrimSuffix(data, "\x00"), "\x00")
}

func readFakeClaudeFile(t *testing.T, stateDir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, name))
	if err != nil {
		t.Fatalf("read fake claude %s: %v", name, err)
	}
	return string(data)
}
