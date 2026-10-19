package batch

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

type informalApprovalMergeFactory struct {
	fakeRunnableFactory
	client *registrationGitHubClient
	pr     *github.PR
}

type readyApprovalReadRunner struct{ calls int }

func (r *readyApprovalReadRunner) Run(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	r.calls++
	if r.calls == 1 {
		return exec.CommandContext(ctx, "sleep", "60")
	}
	return exec.CommandContext(ctx, "sh", "-c", `printf '%s' '[{"number":17,"state":"OPEN","headRefName":"42-fix-bug","headRefOid":"current-sha","body":"Closes #42","statusCheckRollup":"success","mergeStateStatus":"CLEAN"}]'`)
}

type readyApprovalGitHubClient struct {
	*registrationGitHubClient
	cli *github.CLIClient
	pr  *github.PR
}

func (c *readyApprovalGitHubClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	if c.pr.Merged {
		return c.registrationGitHubClient.FindPRByBranch(ctx, branch)
	}
	return c.cli.FindPRByBranch(ctx, branch)
}

func TestInformalApprovalExecutor_ReadyContinuationSurvivesGitHubTimeout(t *testing.T) {
	body := capturedApprovalWithCIAdvice(t)
	s, client, pr, workDir, now := informalApprovalLifecycleFixture(t, body)
	pr.StatusCheckRollup = "success"
	status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	if !handled || status != "resume" || extras["reason"] != "REVIEW_APPROVED" {
		t.Fatalf("initial ready decision = %s/%v", status, extras)
	}
	t.Chdir(workDir)
	if err := atomicfs.WriteAtomic(filepath.Join(workDir, ".sandman", "task.md"), []byte("# Task\nContinue approved merge work.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client.issues = map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}}
	log := &events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")}
	row := RowSpec{IssueNumber: 42, Mode: ModeContinue, RunID: "same-run", BatchID: "batch", BaseBranch: "main",
		Branches: map[int]string{42: gateTestBranch}, PreviousRunIDs: map[int]string{42: "same-run"},
		RenderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 1800}}
	if err := log.Log(events.Event{Type: "run.started", RunID: row.RunID, Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"branch": gateTestBranch}}); err != nil {
		t.Fatal(err)
	}
	if err := logCapacityQueuedContinuationAt(log, *now, row.RunID, 42, row.BatchID, row, extras, "Fix bug"); err != nil {
		t.Fatal(err)
	}
	runner := &readyApprovalReadRunner{}
	recovering := &readyApprovalGitHubClient{registrationGitHubClient: client, pr: pr,
		cli: github.NewCLIClient(github.WithRunner(runner), github.WithTimeout(50*time.Millisecond))}
	factory := &informalApprovalMergeFactory{client: client, pr: pr,
		fakeRunnableFactory: fakeRunnableFactory{results: []AgentRunResult{{Status: "success", Branch: gateTestBranch}, {Status: "success", Branch: gateTestBranch}}}}
	sbFactory := &retrySandboxFactory{sandbox: &retrySandbox{workDir: workDir}}
	opts := gateTestRunOptions()
	opts.now = func() time.Time { return *now }
	o := NewOrchestrator(recovering, &retryRenderer{result: "task"}, nil, log,
		WithRunnableFactory(factory), WithSandboxFactory(sbFactory), WithErrorLog(io.Discard), WithRunSessionOpts(opts))
	bc := BatchConfig{Cfg: &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver(), Retries: 3}
	result, _ := o.newRunExecutor(t.Context(), bc, sbFactory, nil).Execute(t.Context(), row)
	if result.Status != "success" || len(factory.created) != 2 || runner.calls < 2 {
		t.Fatalf("approved ready continuation terminalized on a recoverable GitHub timeout: result=%+v launches=%d reads=%d", result, len(factory.created), runner.calls)
	}
	raw, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	states := events.ProjectRunStates(raw)
	if len(states) != 1 || states[0].Status() != "success" || countEventsByType(raw, "run.retry") != 0 {
		t.Fatalf("read recovery spent implementation retries or lost the selected lifecycle: %+v", raw)
	}
}

func (f *informalApprovalMergeFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	run := f.fakeRunnableFactory.NewRunnable(issue, branch, sb)
	if len(f.created) == 2 {
		f.pr.State, f.pr.Merged, f.pr.Body = "merged", true, "Closes #42"
		f.client.issues[42].State = "closed"
	}
	return run
}

func TestInformalApprovalExecutor_WaitsResumesMergesAndReleasesDependent(t *testing.T) {
	for _, tc := range []struct{ name, body, source string }{
		{"captured CI-wait review", capturedApprovalWithCIAdvice(t), "top_level"},
		{"metrics review", approvingMetricsReview, "top_level"},
		{"plain approval", "LGTM", "top_level"},
		{"COMMENTED approval", "Looks good", "COMMENTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, client, pr, seedDir, now := informalApprovalLifecycleFixture(t, tc.body)
			if tc.source == "COMMENTED" {
				client.comments = client.comments[:1]
				client.reviews = []github.PRReview{{ID: "commented", State: "COMMENTED", Body: tc.body, CreatedAt: now.Add(-time.Minute)}}
			}
			if err := s.registerReviewRequest(t.Context(), seedDir, pr, pr.HeadRefOid); err != nil {
				t.Fatal(err)
			}
			registration, err := readFileReviewRegistration(paths.NewLayout(nil, seedDir).PRReviewRegistrationPath(17))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			t.Chdir(dir)
			workDir := filepath.Join(dir, "worktree")
			if err := os.MkdirAll(filepath.Join(workDir, ".sandman", "state"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := atomicfs.WriteAtomicJSON(paths.NewLayout(nil, workDir).PRReviewRegistrationPath(17), registration, 0600); err != nil {
				t.Fatal(err)
			}
			if err := atomicfs.WriteAtomic(filepath.Join(workDir, ".sandman", "task.md"), []byte("# Task\n\n## Next Step\nWait for formal GitHub approval.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			client.issues = map[int]*github.Issue{
				42: {Number: 42, State: "open", Title: "Fix bug"},
				43: {Number: 43, State: "open", Title: "Dependent", BlockedBy: []int{42}},
			}
			resolver := NewDependencyResolver(client)
			resolver.warningWriter = io.Discard
			before, err := resolver.Resolve(t.Context(), []int{43}, false, nil)
			if err != nil || len(before.Blocked[43]) != 1 {
				t.Fatalf("dependent before merge: %+v %v", before, err)
			}
			factory := &informalApprovalMergeFactory{client: client, pr: pr,
				fakeRunnableFactory: fakeRunnableFactory{results: []AgentRunResult{
					{Status: "success", Branch: gateTestBranch}, {Status: "success", Branch: gateTestBranch},
					{Status: "success", Branch: gateTestBranch}, {Status: "success", Branch: gateTestBranch},
				}},
			}
			log := &events.JSONLLogger{Path: filepath.Join(dir, "events.jsonl")}
			runID := "261013020000-abcd-42"
			if err := log.Log(events.Event{Type: "run.started", RunID: runID, Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"branch": gateTestBranch}}); err != nil {
				t.Fatal(err)
			}
			opts := gateTestRunOptions()
			opts.now = func() time.Time { return *now }
			polls := 0
			opts.lifecycleWait = func(context.Context, time.Duration) error {
				polls++
				if polls <= defaultAwaitResumeMax+1 && len(factory.created) != 0 {
					t.Fatal("pending CI launched an agent or consumed a relaunch")
				}
				*now = now.Add(time.Minute)
				if polls == defaultAwaitResumeMax+2 {
					pr.StatusCheckRollup = "success"
				}
				return nil
			}
			sb := &retrySandbox{workDir: workDir}
			sbFactory := &retrySandboxFactory{sandbox: sb}
			o := NewOrchestrator(client, &retryRenderer{result: "rendered task"}, nil, log,
				WithRunnableFactory(factory), WithSandboxFactory(sbFactory), WithErrorLog(io.Discard), WithRunSessionOpts(opts))
			bc := BatchConfig{Cfg: &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
				AgentName: "test-agent", AgentCfg: config.Agent{Command: "true"}, IdentityResolver: noopIdentityResolver(), Retries: 3}
			result, started := o.newRunExecutor(t.Context(), bc, sbFactory, nil).Execute(t.Context(), RowSpec{
				IssueNumber: 42, Mode: ModeContinue, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
				RunTS: "261013020000", RunShortID: "abcd", RunID: runID,
				PreviousRunIDs: map[int]string{42: runID}, RenderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 1800},
			})
			if !started || result.Status != "success" || len(factory.created) != 2 || polls != defaultAwaitResumeMax+2 {
				t.Fatalf("execution = %+v started=%t launches=%d polls=%d", result, started, len(factory.created), polls)
			}
			for _, cfg := range factory.configs {
				if !strings.Contains(cfg.TaskPrompt, "informal approval is sufficient") || !strings.Contains(cfg.TaskPrompt, "formal GitHub approval is not required") {
					t.Fatalf("merge continuation lost its accepted informal approval: %s", cfg.TaskPrompt)
				}
			}
			raw, err := log.Read()
			if err != nil {
				t.Fatal(err)
			}
			resumed := findEvent(raw, "run.resumed")
			if resumed == nil || resumed.Payload["reason"] != "approval" || countEventsByType(raw, "run.retry") != 0 {
				t.Fatalf("events did not preserve approval without retry: %+v", raw)
			}
			states := events.ProjectRunStates(raw)
			if len(states) != 1 || states[0].RunID != runID || states[0].Status() != "success" || !states[0].IsTerminal() {
				t.Fatalf("same-RunID terminal projection: %+v", states)
			}
			after, err := resolver.Resolve(t.Context(), []int{43}, false, nil)
			if err != nil || len(after.Blocked[43]) != 0 {
				t.Fatalf("dependent after verified merge: %+v %v", after, err)
			}
		})
	}
}
