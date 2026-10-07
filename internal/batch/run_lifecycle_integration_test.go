package batch

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/shellenv"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// Hermetic vertical slice: real worktrees, prompt engine, AgentRun, process
// cancellation, append-only logs, sockets and atomic manifests behind Execute.
// No provider binary, credentials or container runtime is needed.
func TestRunExecutorLifecycleWorktree(t *testing.T) {
	for _, kind := range []string{"issue", "prompt", "review"} {
		for _, scenario := range []string{"success", "retry", "abort"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				dir := testenv.MkdirShort(t, "lifecycle-wt-")
				t.Chdir(dir)
				initGitRepo(t, dir)
				// Match the scaffold's runtime-state exclusion so prompt retries
				// exercise real branch reset without deleting the preserved Task.
				if err := os.WriteFile(".gitignore", []byte(".sandman/\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, dir, "add", ".gitignore")
				runGit(t, dir, "commit", "-m", "exclude runtime state")
				state := filepath.Join(dir, "launches")
				script := filepath.Join(dir, "agent.sh")
				body := `#!/bin/sh
set -eu
state=$1
scenario=$2
kind=$3
printf 'launch\n' >> "$state"
printf 'agent-ready\n'
if [ "$scenario" = abort ]; then
  sleep 30
  printf 'unexpected completion\n'
fi
if [ "$scenario" = retry ] && [ "$(wc -l < "$state")" -eq 1 ]; then
  printf 'first-attempt-failed\n'
  exit 1
fi
if [ "$kind" = issue ]; then
  printf '\n## Status: already resolved\n' >> .sandman/task.md
fi
printf 'agent-complete\n'
`
				if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
				cleanup := false
				cfg := &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees"), CleanupWorktrees: &cleanup}
				client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Lifecycle", Body: "Issue-specific task"}}, prs: map[string]*github.PR{}}
				el := &events.JSONLLogger{Path: filepath.Join(dir, "events.jsonl")}
				verified := 0
				o := NewOrchestrator(client, &prompt.Engine{}, nil, el, WithErrorLog(io.Discard), WithVerifyPath(func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
					verified++
					if in.Issue == nil || in.Issue.Number != 42 || in.WorkDir == "" {
						t.Fatalf("issue verification input: %+v", in)
					}
					return VerifyVerified, []OracleCheck{{Name: "lifecycle-demo", Details: map[string]any{"verified": true}}}
				}))
				row := RowSpec{BatchID: orchTestRunTS + "-" + orchTestRunShortID, RunID: orchTestRunTS + "-" + orchTestRunShortID + "-prompt", BaseBranch: "main", Branches: map[int]string{0: "lifecycle-prompt"},
					RenderCfg: prompt.RenderConfig{PromptFlag: "# Task\n\nLifecycle {{ISSUE_NUMBER}} {{ISSUE_BODY}} on {{BASE_BRANCH}}"}}
				branch, runKind := "lifecycle-prompt", batchindex.KindPromptOnly
				if kind == "issue" {
					row.IssueNumber, row.RunTS, row.RunShortID, row.RunID = 42, orchTestRunTS, orchTestRunShortID, ""
					branch, runKind = "42-lifecycle", batchindex.KindIssue
				} else if kind == "review" {
					row.IssueNumber = 42 // linked review metadata must not select issue policy
					row.Review, row.PRNumber, row.ReviewFocus = true, 73, "correctness"
					row.RunID = orchTestRunTS + "-" + orchTestRunShortID + "-PR73"
					row.RenderCfg.TaskPrompt = "# Task\n\nReview PR73 for correctness"
					runKind = batchindex.KindReview
				}
				runID := row.RunID
				if kind == "issue" {
					runID = buildRunID(42, row.RunTS, row.RunShortID)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				output := &lifecycleDemoOutput{socketPath: daemon.RunSocketPath(o.layout.BatchDir(row.BatchID), runID)}
				if scenario == "abort" {
					output.cancel = cancel
				}
				row.OutputWriter = output
				bc := BatchConfig{Cfg: cfg, IdentityResolver: newPromptOnlyIdentityResolver(dir), AgentName: "demo", SandboxMode: "worktree",
					AgentCfg: config.Agent{Command: fmt.Sprintf("%s %s %s %s", shellenv.Quote(script), shellenv.Quote(state), shellenv.Quote(scenario), shellenv.Quote(kind))}}
				if scenario == "retry" {
					bc.Retries = 1
				}
				result, started := o.newRunExecutor(ctx, bc, defaultSandboxFactory{}, nil).Execute(ctx, row)
				want := "success"
				if scenario == "abort" {
					want = "aborted"
				}
				if !started || result.Status != want || result.RetriesTotal != bc.Retries+1 {
					t.Fatalf("result=%+v started=%v want=%s", result, started, want)
				}
				if output.err != nil {
					t.Fatalf("agent's live endpoint: %v", output.err)
				}
				if (verified > 0) != (kind == "issue" && scenario != "abort") {
					t.Fatalf("mode-specific verification: kind=%s scenario=%s calls=%d", kind, scenario, verified)
				}
				manifest, err := daemon.ReadRunManifest(o.layout.BatchDir(row.BatchID), runID)
				if err != nil || manifest.Kind != runKind || string(manifest.Status) != want || manifest.Branch != branch {
					t.Fatalf("manifest=%+v error=%v", manifest, err)
				}
				if row.Review && manifest.PR != row.PRNumber {
					t.Fatalf("lost Review metadata: %+v", manifest)
				}
				evs, err := el.Read()
				if err != nil {
					t.Fatal(err)
				}
				states := events.ProjectRunStates(evs)
				if len(states) != 1 || states[0].Status() != want || len(states[0].Retries) != bc.Retries {
					t.Fatalf("lifecycle projection: %+v", states)
				}
				log, err := os.ReadFile(o.layout.RunLogPath(row.BatchID, runID))
				if err != nil || !strings.Contains(string(log), "["+runID+"]") || !strings.Contains(string(log), "agent-ready") || strings.Contains(string(log), "unexpected completion") {
					t.Fatalf("saved log=%q error=%v", log, err)
				}
				if scenario == "retry" && (!strings.Contains(string(log), "first-attempt-failed") || !strings.Contains(string(log), "agent-complete")) {
					t.Fatalf("retry truncated append-only log: %s", log)
				}
				task, err := os.ReadFile(filepath.Join(cfg.WorktreeDir, branch, ".sandman", "task.md"))
				if err != nil || !strings.Contains(string(task), "Lifecycle") && !strings.Contains(string(task), "Review PR73") {
					t.Fatalf("mode prompt=%q error=%v", task, err)
				}
				if scenario == "success" && (strings.Contains(string(task), "Continuation Freshness Guard") || row.Review && strings.Contains(string(task), "Runtime Context")) {
					t.Fatalf("fresh mode prompt acquired continuation/implementation context: %s", task)
				}
				if conn, err := net.Dial("unix", output.socketPath); err == nil {
					_ = conn.Close()
					t.Fatal("terminal endpoint is still live")
				}
			})
		}
	}
}

type lifecycleDemoOutput struct {
	once       sync.Once
	socketPath string
	cancel     context.CancelFunc
	err        error
}

func (w *lifecycleDemoOutput) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "agent-ready") {
		w.once.Do(func() {
			conn, err := net.Dial("unix", w.socketPath)
			w.err = err
			if err == nil {
				_ = conn.Close()
			}
			if w.cancel != nil {
				w.cancel()
			}
		})
	}
	return len(data), nil
}
