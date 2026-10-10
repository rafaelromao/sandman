package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestCIRerunRecoversExpiredWaitWithoutPoisonedTask(t *testing.T) {
	for _, task := range []string{"# Task\nContinue implementation.\n", "# Task\nCI_WAIT_TIMEOUT recorded earlier; give up.\n"} {
		t.Run(task, func(t *testing.T) {
			root := t.TempDir()
			now := time.Now().UTC().Truncate(time.Second)
			old := ciWaitRegistration{Protocol: ciWaitProtocol, PullRequest: 17, HeadSHA: "head", ExecutionID: "old-checks", StartedUnixSeconds: now.Add(-4 * time.Hour).Unix(), DeadlineUnixSeconds: now.Add(-4*time.Hour + ciWaitTimeout).Unix(), EffectiveTimeoutSecs: int64(ciWaitTimeout / time.Second)}
			stateDir := filepath.Join(root, ".sandman", "state")
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".sandman", "task.md"), []byte(task), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(stateDir, "17.ci_wait.json")
			if err := atomicfs.WriteAtomicJSON(path, old, 0o600); err != nil {
				t.Fatal(err)
			}
			pr := &github.PR{Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "pending", CIExecutionID: "rerun-checks"}
			session := &runSession{issueNumber: 42, deps: runDeps{githubClient: &fakeGitHubClient{prs: map[string]*github.PR{"42-work": pr}}, errorLog: io.Discard}, opts: runSessionOptions{now: func() time.Time { return now }, currentHead: func(string) (string, error) { return "head", nil }}}
			status, extras, handled := session.handleLifecycleDecision(context.Background(), root, "42-work", "", "row", true)
			if !handled || status != "await" || extras["gate"] != "pending" {
				t.Fatalf("fresh CI rerun forced repeated timeout remediation: status=%s extras=%+v handled=%v", status, extras, handled)
			}
			registered, err := readCIWaitRegistration(path)
			if err != nil || registered.ExecutionID != "rerun-checks" || registered.DeadlineUnixSeconds != now.Add(ciWaitTimeout).Unix() {
				t.Fatalf("new execution did not get its bounded wait: registration=%+v error=%v", registered, err)
			}
			now = now.Add(10 * time.Minute)
			status, _, _ = session.handleLifecycleDecision(context.Background(), root, "42-work", "", "row", true)
			unchanged, err := readCIWaitRegistration(path)
			if err != nil || status != "await" || unchanged.DeadlineUnixSeconds != registered.DeadlineUnixSeconds {
				t.Fatalf("re-observation renewed current execution: status=%s registration=%+v error=%v", status, unchanged, err)
			}
			now = time.Unix(registered.DeadlineUnixSeconds, 0).UTC()
			status, extras, _ = session.handleLifecycleDecision(context.Background(), root, "42-work", "", "row", true)
			if status != "resume" || extras["gate"] != gateCIWaitTimeout {
				t.Fatalf("same execution escaped its hard bound: status=%s extras=%+v", status, extras)
			}
		})
	}
}

func TestCIExecutionWaitPreservesBoundsAndLegacyBinding(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	s := &runSession{opts: runSessionOptions{now: func() time.Time { return now }}}
	pr := &github.PR{Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "pending"}
	legacy, err := s.ciWaitEvidence(root, pr, "head")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	pr.CIExecutionID = "new-execution"
	bound, err := s.ciWaitEvidence(root, pr, "head")
	if err != nil || bound["ci_wait"].(map[string]any)["deadline_unix_seconds"] == legacy["ci_wait"].(map[string]any)["deadline_unix_seconds"] {
		t.Fatalf("legacy wait poisoned first identified execution: evidence=%v error=%v", bound, err)
	}
	deadline := bound["ci_wait"].(map[string]any)["deadline_unix_seconds"]
	now = now.Add(time.Hour)
	for _, identity := range []string{"new-execution", ""} {
		pr.CIExecutionID = identity
		restart := &runSession{opts: s.opts}
		current, err := restart.ciWaitEvidence(root, pr, "head")
		if err != nil || current["ci_wait"].(map[string]any)["deadline_unix_seconds"] != deadline {
			t.Fatalf("restart or unknown identity renewed expired execution: evidence=%v error=%v", current, err)
		}
	}
}

func TestCIExecutionCannotReplaceInvalidWaitState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".sandman", "state", "17.ci_wait.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	invalid := ciWaitRegistration{Protocol: "invalid", PullRequest: 17, HeadSHA: "head"}
	if err := atomicfs.WriteAtomicJSON(path, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (&runSession{}).ciWaitEvidence(root, &github.PR{Number: 17, HeadRefOid: "head", StatusCheckRollup: "pending", CIExecutionID: "rerun"}, "head")
	if err == nil {
		t.Fatal("new execution hid invalid durable CI state")
	}
	retained, err := readCIWaitRegistration(path)
	if err != nil || retained.Protocol != "invalid" {
		t.Fatalf("invalid state silently replaced: %+v %v", retained, err)
	}
}

func TestCIRerunRemediationYieldsOnceThenCompletes(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-ci-rerun-")
	t.Chdir(root)
	initGitRepo(t, root)
	worktree := filepath.Join(root, "worktrees", "42-work")
	stateDir := filepath.Join(worktree, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\nRepair CI and complete the pull request.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := ciWaitRegistration{Protocol: ciWaitProtocol, PullRequest: 17, HeadSHA: "head", ExecutionID: "old-checks", StartedUnixSeconds: now.Add(-4 * time.Hour).Unix(), DeadlineUnixSeconds: now.Add(-4*time.Hour + ciWaitTimeout).Unix(), EffectiveTimeoutSecs: int64(ciWaitTimeout / time.Second)}
	if err := atomicfs.WriteAtomicJSON(filepath.Join(stateDir, "17.ci_wait.json"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}}, prs: map[string]*github.PR{"42-work": {Number: 17, State: "open", HeadRefName: "42-work", HeadRefOid: "head", Body: "Closes #42", StatusCheckRollup: "pending", CIExecutionID: "old-checks"}}}}
	launches, polls := 0, 0
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		launches++
		client.setPR(branch, func(pr *github.PR) { pr.CIExecutionID = "rerun-checks" })
		return AgentRunResult{IssueNumber: issue.Number, Status: "success", Branch: branch}
	}}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	log := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}), WithRunSessionOpts(runSessionOptions{
		releaseAwaitCapacity: true, now: func() time.Time { return now }, currentHead: func(string) (string, error) { return "head", nil },
		awaitWait: func(context.Context, time.Duration) error {
			polls++
			client.setPR("42-work", func(pr *github.PR) { pr.StatusCheckRollup, pr.State, pr.Merged = "success", "merged", true })
			return nil
		},
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{42}, Branches: map[int]string{42: "42-work"}, RunTS: "261008174547", RunShortID: "ci-rerun", Retries: 3, Parallel: 1})
	if err != nil || result == nil || result.Runs[0].Status != "success" || launches != 1 || polls != 1 {
		t.Fatalf("CI repair did not reach completion: result=%+v error=%v launches=%d polls=%d events=%+v", result, err, launches, polls, log.snapshot())
	}
	if countEventsByType(log.snapshot(), "run.resumed") != 0 || countEventsByType(log.snapshot(), "run.await") != 1 {
		t.Fatalf("fresh checks spent remediation allowance instead of waiting: %+v", log.snapshot())
	}
	registered, err := readCIWaitRegistration(filepath.Join(stateDir, "17.ci_wait.json"))
	if err != nil || registered.ExecutionID != "rerun-checks" || registered.DeadlineUnixSeconds != now.Add(ciWaitTimeout).Unix() {
		t.Fatalf("repair did not replace the stale CI operation: registration=%+v error=%v", registered, err)
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) != 1 || !states[0].IsTerminal() || states[0].Status() != "success" {
		t.Fatalf("completion projection=%+v", states)
	}
}
