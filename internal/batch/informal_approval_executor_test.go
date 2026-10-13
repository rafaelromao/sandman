package batch

import (
	"context"
	"io"
	"os"
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
		{"metrics review", approvingMetricsReview, "top_level"},
		{"captured approval with CI wait advice", capturedApprovalWithCIWaitAdvice, "top_level"},
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
