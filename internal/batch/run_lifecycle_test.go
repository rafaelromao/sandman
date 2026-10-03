package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// These baselines are captured through the per-row seam before consolidation.
// The RunBatch characterization goldens remain a separate, unchanged contract.
func TestRunExecutorLifecycleContract(t *testing.T) {
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"issue", "prompt", "review"} {
		for _, scenario := range []struct {
			name     string
			outcomes []string
			retries  int
			want     string
		}{
			{"success", []string{"success"}, 0, "success"},
			{"retry", []string{"failure", "success"}, 1, "success"},
			{"failure", []string{"failure", "failure"}, 1, "failure"},
			{"abort", []string{"aborted"}, 0, "aborted"},
			{"cancel", []string{"cancel"}, 0, "aborted"},
		} {
			t.Run(kind+"/"+scenario.name, func(t *testing.T) {
				dir := testenv.MkdirShort(t, "lifecycle-")
				t.Chdir(dir)
				initGitRepo(t, dir)
				workDir := filepath.Join(dir, "wt")
				if err := os.MkdirAll(workDir, 0o755); err != nil {
					t.Fatal(err)
				}
				el := &events.JSONLLogger{Path: filepath.Join(dir, "events.jsonl")}
				client := &lifecycleGitHubClient{fakeGitHubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", Body: "Fix it."}}, prs: map[string]*github.PR{}}}
				row := RowSpec{BaseBranch: "main", Branches: map[int]string{0: "test-prompt-only"}, BatchID: orchTestRunTS + "-" + orchTestRunShortID,
					RunID: orchTestRunTS + "-" + orchTestRunShortID + "-prompt", RenderCfg: prompt.RenderConfig{TaskPrompt: "perform task", PromptFlag: "inline", PromptArgs: map[string]string{"EXTRA": "value"}}}
				branch := "test-prompt-only"
				if kind == "issue" {
					row.IssueNumber, row.RunTS, row.RunShortID = 42, orchTestRunTS, orchTestRunShortID
					row.RunID = ""
					branch = "42-fix-bug"
				} else if kind == "review" {
					row.Review, row.PRNumber, row.ReviewFocus = true, 73, "correctness"
					row.RunID = orchTestRunTS + "-" + orchTestRunShortID + "-PR73"
					row.PortalHidden = true
				}
				runID := row.RunID
				if kind == "issue" {
					runID = orchTestRunTS + "-" + orchTestRunShortID + "-42"
				}
				cfg := &config.Config{WorktreeDir: ".sandman/worktrees"}
				bc := BatchConfig{Cfg: cfg, IdentityResolver: noopIdentityResolver(), AgentName: "test-agent", AgentCfg: config.Agent{Command: "true", Model: "test-model"}, Variant: "test-variant", Parallel: 1, SandboxMode: "worktree", Retries: scenario.retries}
				sb := &fakeSandbox{workDir: workDir}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				factory := &lifecycleRunnableFactory{t: t, client: client, kind: kind, branch: branch, outcomes: scenario.outcomes, cancel: cancel}
				resets := 0
				o := NewOrchestrator(client, &noopRenderer{}, nil, el, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithRunSessionOpts(runSessionOptions{retryReset: func(context.Context, sandbox.Sandbox, string, string) error { resets++; return nil }}))
				factory.socketPath = daemon.RunSocketPath(o.layout.BatchDir(row.BatchID), runID)
				executor := o.newRunExecutor(ctx, bc, &fakeSandboxFactory{sandbox: sb}, nil)
				// The batch normally owns final endpoint cleanup; always clean the
				// fixture too, including before consolidation and on assertion failure.
				t.Cleanup(func() { _ = executor.coord.stopCommandServer(row.IssueNumber) })
				result, started := executor.Execute(ctx, row)
				if !started || result.Status != scenario.want {
					t.Fatalf("result=%+v started=%v", result, started)
				}
				evs, err := el.Read()
				if err != nil {
					t.Fatal(err)
				}
				states := events.ProjectRunStates(evs)
				if len(states) != 1 || states[0].Status() != result.Status {
					t.Fatalf("projection disagrees with result: %+v", states)
				}
				manifest, err := daemon.ReadRunManifest(o.layout.BatchDir(row.BatchID), runID)
				if err != nil {
					t.Fatal(err)
				}
				manifest.CreatedAt = time.Time{}
				metadata, err := json.MarshalIndent(manifest, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				configs, err := json.MarshalIndent(factory.configs, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				captured := charNetCapture(t, o, el, &Result{Runs: []AgentRunResult{result}}, nil, dir)
				captured += charNetNormalize(fmt.Sprintf("\n=== manifest ===\n%s\n=== prompts ===\n%s\n", metadata, configs), dir)
				captured += fmt.Sprintf("\n=== branch resets ===\n%d\n", resets)
				charNetGolden(t, pkgDir, "executor_"+kind+"_"+scenario.name+".golden", captured)
				if !sb.restoreHostPathsCalled || sb.stopCalled != (scenario.want == "success" && !row.Review) {
					t.Fatalf("cleanup restore=%v stop=%v review=%v", sb.restoreHostPathsCalled, sb.stopCalled, row.Review)
				}
				if conn, err := net.Dial("unix", factory.socketPath); err == nil {
					_ = conn.Close()
					t.Fatal("terminal run retained a live command endpoint")
				}
			})
		}
	}
}

type lifecycleRunnableFactory struct {
	t          *testing.T
	client     *lifecycleGitHubClient
	kind       string
	branch     string
	socketPath string
	outcomes   []string
	configs    []prompt.RenderConfig
	cancel     context.CancelFunc
}

func TestRunExecutorLifecycleAwaitOwnsOnlyItsEndpoint(t *testing.T) {
	dir := testenv.MkdirShort(t, "lifecycle-await-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	client := &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "First"}, 43: {Number: 43, Title: "Second"},
	}, prs: map[string]*github.PR{
		"42-first":  {Number: 72, State: "open", HeadRefName: "42-first", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
		"43-second": {Number: 73, State: "open", HeadRefName: "43-second", HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
	}}
	el := &events.JSONLLogger{Path: filepath.Join(dir, "events.jsonl")}
	o := NewOrchestrator(client, &noopRenderer{}, nil, el, WithErrorLog(io.Discard),
		WithRunnableFactory(&fakeRunnableFactory{results: []AgentRunResult{
			{Status: "success", Branch: "42-first"}, {Status: "success", Branch: "43-second"},
		}}), WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "current-sha", nil }}))
	bc := BatchConfig{Cfg: &config.Config{WorktreeDir: ".sandman/worktrees"}, AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver()}
	executor := o.newRunExecutor(t.Context(), bc, &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(dir, "wt")}}, nil)
	row := RowSpec{IssueNumber: 42, Branches: map[int]string{42: "42-first", 43: "43-second"}, BaseBranch: "main", BatchID: orchTestRunTS + "-" + orchTestRunShortID, RunTS: orchTestRunTS, RunShortID: orchTestRunShortID}
	for _, issue := range []int{42, 43} {
		row.IssueNumber = issue
		result, started := executor.Execute(t.Context(), row)
		if !started || result.Status != "await" {
			t.Fatalf("issue %d result=%+v started=%v", issue, result, started)
		}
		t.Cleanup(func() { _ = executor.coord.stopCommandServer(issue) })
		path := daemon.RunSocketPath(o.layout.BatchDir(row.BatchID), buildRunID(issue, row.RunTS, row.RunShortID))
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatalf("await lost endpoint for %d: %v", issue, err)
		}
		_ = conn.Close()
	}
	before, err := el.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range events.ProjectRunStates(before) {
		if !state.IsAwaiting() || !state.IsActive() {
			t.Fatalf("await projection: %+v", state)
		}
	}
	client.prs["42-first"] = &github.PR{Number: 72, State: "closed", Merged: true, HeadRefName: "42-first", Body: "Closes #42"}
	row.IssueNumber, row.Mode = 42, ModeContinue
	result, started := executor.Execute(t.Context(), row)
	if !started || result.Status != "success" {
		t.Fatalf("resolved await result=%+v started=%v", result, started)
	}
	for _, issue := range []int{42, 43} {
		path := daemon.RunSocketPath(o.layout.BatchDir(row.BatchID), buildRunID(issue, row.RunTS, row.RunShortID))
		conn, err := net.Dial("unix", path)
		if err == nil {
			_ = conn.Close()
		}
		if (err == nil) != (issue == 43) {
			t.Fatalf("endpoint ownership for %d: %v", issue, err)
		}
	}
}

func (f *lifecycleRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	if (issue != nil) != (f.kind == "issue") || branch != f.branch {
		f.t.Fatalf("mode policy: issue=%+v branch=%s", issue, branch)
	}
	return &lifecycleRunnable{factory: f}
}

type lifecycleRunnable struct{ factory *lifecycleRunnableFactory }

// The closing-reference guard reads PR state concurrently with the runnable.
type lifecycleGitHubClient struct {
	*fakeGitHubClient
	mu sync.Mutex
}

func (c *lifecycleGitHubClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fakeGitHubClient.FindPRByBranch(ctx, branch)
}

func (r *lifecycleRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, cfg prompt.RenderConfig) AgentRunResult {
	f := r.factory
	conn, err := net.Dial("unix", f.socketPath)
	if err != nil {
		f.t.Fatalf("live run endpoint: %v", err)
	}
	_ = conn.Close()
	status := f.outcomes[len(f.configs)]
	f.configs = append(f.configs, cfg)
	if status == "aborted" || status == "cancel" {
		f.cancel()
		<-ctx.Done()
		if status == "cancel" {
			status = "failure"
		}
	}
	if status == "success" && f.kind == "issue" {
		f.client.mu.Lock()
		f.client.prs[f.branch] = mergedPR(f.branch, "")
		f.client.mu.Unlock()
	}
	return AgentRunResult{Status: status, Branch: f.branch}
}
