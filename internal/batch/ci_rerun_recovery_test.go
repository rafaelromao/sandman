package batch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/skill"
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
			observedLifecycle40 := session.observeLifecycleWithHostPathsForTest(context.Background(), root, "42-work", "", "row", true)
			status, extras, handled := observedLifecycle40.status(), observedLifecycle40.Evidence, observedLifecycle40.handled()
			if !handled || status != "await" || extras["gate"] != "pending" {
				t.Fatalf("fresh CI rerun forced repeated timeout remediation: status=%s extras=%+v handled=%v", status, extras, handled)
			}
			registered, err := readCIWaitRegistration(path)
			if err != nil || registered.ExecutionID != "rerun-checks" || registered.DeadlineUnixSeconds != now.Add(ciWaitTimeout).Unix() {
				t.Fatalf("new execution did not get its bounded wait: registration=%+v error=%v", registered, err)
			}
			now = now.Add(10 * time.Minute)
			observedLifecycle50 := session.observeLifecycleWithHostPathsForTest(context.Background(), root, "42-work", "", "row", true)
			status = observedLifecycle50.status()
			unchanged, err := readCIWaitRegistration(path)
			if err != nil || status != "await" || unchanged.DeadlineUnixSeconds != registered.DeadlineUnixSeconds {
				t.Fatalf("re-observation renewed current execution: status=%s registration=%+v error=%v", status, unchanged, err)
			}
			now = time.Unix(registered.DeadlineUnixSeconds, 0).UTC()
			observedLifecycle56 := session.observeLifecycleWithHostPathsForTest(context.Background(), root, "42-work", "", "row", true)
			status, extras = observedLifecycle56.status(), observedLifecycle56.Evidence
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
	legacy, err := s.ciWaitEvidence(context.Background(), root, pr, "head")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	pr.CIExecutionID = "new-execution"
	bound, err := s.ciWaitEvidence(context.Background(), root, pr, "head")
	if err != nil || bound["ci_wait"].(map[string]any)["deadline_unix_seconds"] == legacy["ci_wait"].(map[string]any)["deadline_unix_seconds"] {
		t.Fatalf("legacy wait poisoned first identified execution: evidence=%v error=%v", bound, err)
	}
	deadline := bound["ci_wait"].(map[string]any)["deadline_unix_seconds"]
	now = now.Add(time.Hour)
	for _, identity := range []string{"new-execution", ""} {
		pr.CIExecutionID = identity
		restart := &runSession{opts: s.opts}
		current, err := restart.ciWaitEvidence(context.Background(), root, pr, "head")
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
	_, err := (&runSession{}).ciWaitEvidence(context.Background(), root, &github.PR{Number: 17, HeadRefOid: "head", StatusCheckRollup: "pending", CIExecutionID: "rerun"}, "head")
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

func TestCIRerunPendingWaitFailureWithUnrelatedActivityRequiresRecordedRepairAndMergeActions(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-ci-action-recovery-")
	t.Chdir(root)
	initGitRepo(t, root)
	worktree := filepath.Join(root, "worktrees", "42-work")
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\nRepair CI and complete the pull request.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	client := &reviewWaitSchedulerGitHubClient{comments: []github.PRComment{{
		ID: "https://github.com/owner/repo/pull/17#issuecomment-1001", Body: "/sandman review", CreatedAt: now,
	}}, fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}},
		prs: map[string]*github.PR{"42-work": {
			Number: 17, State: "open", HeadRefName: "42-work", HeadRefOid: "head", Body: "Closes #42",
			StatusCheckRollup: "pending", ReviewDecision: "APPROVED", MergeStateStatus: "BLOCKED", CIExecutionID: "checks-first",
		}},
	}}
	launches, polls := 0, 0
	ciHelper := newRecordingCIRerunGH(t, root)
	installedHelper := installCIRerunHelperForManagedTest(t, root)
	helperCommand := `sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha head --tested-sha tested-merge-sha --run-id 12345 --job-id 987654321 --diagnosis transient-provider-timeout`
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		launches++
		var actionForResult *ciRerunActionEvidence
		switch launches {
		case 2:
			// Execute the installed helper against a recording fake GitHub CLI.
			// Only after its confirmed read-back do we advance the lifecycle fake.
			output := runCIRerunHelperForManagedTest(t, ciHelper, installedHelper, helperCommand)
			if !strings.Contains(output, `"state":"confirmed"`) {
				t.Fatalf("rerun helper did not confirm its action: %s", output)
			}
			actionForResult = captureCIRerunActionFromOpenCodeToolEvent(t, helperCommand, output)
			if actionForResult == nil {
				t.Fatalf("actual installed helper output was not captured from its structured shell tool result: %s", output)
			}
			client.setPR(branch, func(pr *github.PR) {
				// GitHub may keep the prior failure in its rollup while the
				// confirmed replacement attempt and aggregate job are queued.
				// The structured helper receipt must authorize this wait.
				pr.StatusCheckRollup, pr.CIExecutionID = "failure", "checks-rerun"
				pr.CIHasActiveChecks = true
				pr.CIActiveFailedWorkflowRunID = "12345"
				pr.CIActiveFailedWorkflowRunFailedCheckNames = []string{"opencode-canary", "quality-gate"}
				pr.CIActiveFailedWorkflowRunFailedChecks = failedCIChecks("12345", "opencode-canary", "quality-gate")
				pr.CIActiveFailedWorkflowRunCheckNames = []string{"build-macos", "opencode-canary", "quality-gate"}
				pr.MergeStateStatus = "BLOCKED"
			})
		case 3:
			merge := exec.Command(ciHelper.ghPath, "pr", "merge", "17", "--merge")
			merge.Env = ciHelper.env
			if output, err := merge.CombinedOutput(); err != nil {
				t.Fatalf("record merge command: %v: %s", err, output)
			}
			client.setPR(branch, func(pr *github.PR) {
				pr.State, pr.Merged, pr.StatusCheckRollup = "merged", true, "success"
				pr.ReviewDecision, pr.MergeStateStatus = "APPROVED", "CLEAN"
			})
		}
		return AgentRunResult{IssueNumber: issue.Number, Status: "success", Branch: branch, ciRerunAction: actionForResult}
	}}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	log := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard),
		WithRunnableFactory(factory),
		WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			now:                  func() time.Time { return now },
			currentHead:          func(string) (string, error) { return "head", nil },
			awaitWait: func(context.Context, time.Duration) error {
				polls++
				if polls == 1 {
					client.setPR("42-work", func(pr *github.PR) {
						pr.StatusCheckRollup = "failure"
						pr.CIHasActiveChecks = true
						// The only active check belongs to a different workflow;
						// no workflow-run identity authorizes deferring diagnosis.
						pr.CIActiveFailedWorkflowRunID = ""
						pr.MergeStateStatus = "BLOCKED"
					})
					writeRespondedApprovalForCanonicalRequest(t, worktree, 17)
				} else if polls == 2 {
					client.setPR("42-work", func(pr *github.PR) {
						pr.StatusCheckRollup, pr.CIHasActiveChecks = "pending", true
						pr.CIActiveFailedWorkflowRunID = ""
					})
				} else {
					client.setPR("42-work", func(pr *github.PR) {
						pr.StatusCheckRollup, pr.ReviewDecision, pr.MergeStateStatus = "success", "APPROVED", "CLEAN"
					})
				}
				return nil
			},
		}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{42}, Branches: map[int]string{42: "42-work"}, RunTS: "261028221500", RunShortID: "ci-action-recovery", Retries: 3, Parallel: 1, PromptConfig: prompt.RenderConfig{ReviewCommandSet: true}})
	if err != nil || result == nil || result.Runs[0].Status != "success" {
		t.Fatalf("managed CI recovery did not complete: result=%+v error=%v launches=%d polls=%d events=%+v", result, err, launches, polls, log.snapshot())
	}
	actions := ciHelper.actions(t)
	if launches != 3 || polls != 3 || len(actions) != 2 || actions[0] != "POST repos/owner/repo/actions/jobs/987654321/rerun" || actions[1] != "gh pr merge 17 --merge" {
		t.Fatalf("expected actual helper rerun then merge after three external polls: launches=%d polls=%d actions=%v", launches, polls, actions)
	}
	if countEventsByType(log.snapshot(), "run.retry") != 0 || countEventsByType(log.snapshot(), "run.await") < 2 {
		t.Fatalf("CI recovery consumed generic retries or skipped capacity-releasing waits: %+v", log.snapshot())
	}
	states := events.ProjectRunStates(log.snapshot())
	if len(states) != 1 || !states[0].IsTerminal() || states[0].Status() != "success" {
		t.Fatalf("completion was not independently projected from the verified merged PR: %+v", states)
	}
	registered, err := readCIWaitRegistration(filepath.Join(worktree, ".sandman", "state", "17.ci_wait.json"))
	if err != nil || registered.ExecutionID != "checks-rerun" {
		t.Fatalf("confirmed replacement execution did not become the observed CI generation: registration=%+v error=%v", registered, err)
	}
}

func TestCIRerunCancellationBeforeMutationPreventsHelperPost(t *testing.T) {
	root := t.TempDir()
	gh := newRecordingCIRerunGH(t, root)
	installCIRerunHelperForManagedTest(t, root)
	commandText := `sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha head --tested-sha tested-merge-sha --run-id 12345 --job-id 987654321 --diagnosis transient-provider-timeout`
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := exec.CommandContext(ctx, "sh", "-c", commandText)
	command.Env = gh.env
	if output, err := command.CombinedOutput(); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled helper command = (%v, %s), want context cancellation", err, output)
	}
	if actions := gh.actions(t); len(actions) != 0 {
		t.Fatalf("pre-cancelled recovery mutated GitHub: %v", actions)
	}
}

func TestManagedCIRerunCancellationBeforePostAbortsWithoutRelaunch(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-ci-cancel-recovery-")
	t.Chdir(root)
	initGitRepo(t, root)
	worktree := filepath.Join(root, "worktrees", "42-work")
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\nRepair current-head CI.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gh := newRecordingCIRerunGH(t, root)
	installCIRerunHelperForManagedTest(t, root)
	readyPath := filepath.Join(root, "gh-jobs-ready")
	cancelPath := filepath.Join(root, "gh-cancel")
	configureRecordingCIRerunGHBlock(t, gh, readyPath, cancelPath)
	helperCommand := `sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha head --tested-sha tested-merge-sha --run-id 12345 --job-id 987654321 --diagnosis transient-provider-timeout`
	factory := &cancellableCIRerunRunnableFactory{gh: gh, helperCommand: helperCommand}
	now := time.Now().UTC().Truncate(time.Second)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}},
		prs: map[string]*github.PR{"42-work": {
			Number: 17, State: "open", HeadRefName: "42-work", HeadRefOid: "head", Body: "Closes #42",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, CIActiveFailedWorkflowRunID: "12345", CIExecutionID: "checks-current",
			CIActiveFailedWorkflowRunFailedChecks: []github.CIFailedCheck{{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/12345/job/11"}},
			CIActiveFailedWorkflowRunCheckNames:   []string{"opencode-canary", "build-macos"},
		}},
	}}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	log := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard),
		WithRunnableFactory(factory),
		WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}),
		WithRunSessionOpts(runSessionOptions{now: func() time.Time { return now }, currentHead: func(string) (string, error) { return "head", nil }}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type batchOutcome struct {
		result *Result
		err    error
	}
	done := make(chan batchOutcome, 1)
	go func() {
		result, err := o.RunBatch(ctx, Request{Issues: []int{42}, Branches: map[int]string{42: "42-work"}, RunTS: "261029112000", RunShortID: "cancel-ci-rerun", Retries: 3, Parallel: 1})
		done <- batchOutcome{result: result, err: err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("helper did not reach the pre-mutation job lookup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := os.WriteFile(cancelPath, []byte("cancelled"), 0o600); err != nil {
		t.Fatal(err)
	}
	var outcome batchOutcome
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled CI recovery did not stop the managed run")
	}
	if outcome.result == nil || len(outcome.result.Runs) != 1 || outcome.result.Runs[0].Status != "aborted" {
		t.Fatalf("cancelled CI recovery result = %+v (error %v), want an aborted run", outcome.result, outcome.err)
	}
	if factory.created != 1 {
		t.Fatalf("cancellation launched another implementation attempt: launches=%d", factory.created)
	}
	if actions := gh.actions(t); len(actions) != 0 {
		t.Fatalf("cancellation before the rerun POST allowed a GitHub mutation: %v", actions)
	}
	if countEventsByType(log.snapshot(), "run.retry") != 0 || countEventsByType(log.snapshot(), "run.continued") != 0 || countEventsByType(log.snapshot(), "run.aborted") != 1 {
		t.Fatalf("cancellation did not prevent later retry/continuation: %+v", log.snapshot())
	}
}

func TestCIRerunObservedWhileParkedPersistsLatestAwaitEvidence(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-ci-parked-rerun-")
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
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}}, prs: map[string]*github.PR{
		"42-work": {Number: 17, State: "open", HeadRefName: "42-work", HeadRefOid: "head", Body: "Closes #42", StatusCheckRollup: "pending", CIExecutionID: "old-checks"},
	}}}
	launches, polls := 0, 0
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		launches++
		return AgentRunResult{IssueNumber: issue.Number, Status: "success", Branch: branch}
	}}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	log := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}), WithRunSessionOpts(runSessionOptions{
		releaseAwaitCapacity: true, now: func() time.Time { return now }, currentHead: func(string) (string, error) { return "head", nil },
		awaitWait: func(context.Context, time.Duration) error {
			polls++
			if polls == 1 {
				client.setPR("42-work", func(pr *github.PR) { pr.CIExecutionID = "rerun-checks" })
			} else {
				client.setPR("42-work", func(pr *github.PR) { pr.StatusCheckRollup, pr.State, pr.Merged = "success", "merged", true })
			}
			return nil
		},
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{42}, Branches: map[int]string{42: "42-work"}, RunTS: "261008174547", RunShortID: "parked-rerun", Retries: 3, Parallel: 1})
	if err != nil || result == nil || result.Runs[0].Status != "success" || launches != 1 || polls != 2 {
		t.Fatalf("parked CI rerun did not reach completion: result=%+v error=%v launches=%d polls=%d events=%+v", result, err, launches, polls, log.snapshot())
	}
	var awaits []events.Event
	for _, event := range log.snapshot() {
		if event.Type == "run.await" {
			awaits = append(awaits, event)
		}
	}
	if len(awaits) < 2 {
		t.Fatalf("parked rerun did not persist a fresh await observation: %+v", log.snapshot())
	}
	latest := awaits[len(awaits)-1]
	ciWait, ok := latest.Payload["ci_wait"].(map[string]any)
	if !ok || ciWait["execution_id"] != "rerun-checks" || ciWait["deadline_unix_seconds"] != now.Add(ciWaitTimeout).Unix() {
		t.Fatalf("latest await lost rerun evidence: %#v", latest.Payload)
	}
}

func TestConfirmedCIRerunReceiptSurvivesOrdinaryPendingWait(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "worktrees", "42-work")
	if err := os.MkdirAll(filepath.Join(workDir, ".sandman", "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}},
		prs: map[string]*github.PR{"42-work": {
			Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "pending", CIExecutionID: "checks-rerun-attempt-2",
		}},
	}
	log := &spyEventLog{events: []events.Event{{
		Type: "run.started", RunID: "run-confirmed-rerun-retained", Issue: 42, Timestamp: now.Add(-time.Minute),
		Payload: map[string]any{"branch": "42-work", "batch_id": "batch"},
	}}}
	e := &runExecutor{deps: runDeps{
		githubClient: client, eventLog: log, layout: paths.NewLayout(nil, root), errorLog: io.Discard,
		runSessionOpts: runSessionOptions{now: func() time.Time { return now }, currentHead: func(string) (string, error) { return "head", nil }},
	}}
	row := RowSpec{RunID: "run-confirmed-rerun-retained", IssueNumber: 42, BatchID: "batch", BaseBranch: "main", Branches: map[int]string{42: "42-work"}}
	action := &ciRerunActionEvidence{
		Protocol: "ci-rerun/v1", State: "confirmed", Reason: "selected-job-and-quality-gate-rerun-observed",
		Repository: "owner/repo", PullRequest: 17, SourceHeadSHA: "head", TestedSHA: "tested-sha",
		Diagnosis: "transient-provider-timeout", RunID: "101", JobID: "987654321", JobName: "opencode-canary", ObservedAttempt: 2,
	}
	request := lifecycleObservationRequest{
		WorkDir: workDir, Branch: "42-work", RunID: row.RunID, HostPathsReady: true,
		AttemptStatus: "success", AfterAgentAttempt: true, CIRerunAction: action,
	}
	if !action.matchesPullRequest(17, "owner/repo", "head") {
		t.Fatalf("test receipt does not match expected live PR identity: %+v", action)
	}
	first := newLifecycleObserver(newRunSession(e, row)).Observe(context.Background(), request)
	if first.Action != lifecycleAwait || first.Evidence["gate"] != string(lifecycleGatePending) {
		t.Fatalf("verified rerun with pending checks should await: %+v", first)
	}
	if _, ok := first.Evidence[ciRerunActionKey].(map[string]any); !ok {
		t.Fatalf("ordinary pending-CI await dropped the confirmed rerun receipt: verdict=%+v evidence=%#v", first, first.Evidence)
	}
	if err := e.persistObservedAwait(context.Background(), row, first.Evidence); err != nil {
		t.Fatal(err)
	}

	client.prs["42-work"].StatusCheckRollup = "failure"
	client.prs["42-work"].CIHasActiveChecks = true
	client.prs["42-work"].CIActiveFailedWorkflowRunID = "101"
	client.prs["42-work"].CIActiveFailedWorkflowRunFailedCheckNames = []string{"opencode-canary", "quality-gate"}
	client.prs["42-work"].CIActiveFailedWorkflowRunFailedChecks = failedCIChecks("101", "opencode-canary", "quality-gate")
	client.prs["42-work"].CIActiveFailedWorkflowRunCheckNames = []string{"build-macos", "opencode-canary", "quality-gate"}
	client.prs["42-work"].MergeStateStatus = "BLOCKED"
	reobserved := e.observeAwaitingRowLifecycle(context.Background(), row)
	if reobserved.Action != lifecycleAwait || reobserved.Evidence["gate"] != string(lifecycleGatePending) || reobserved.ciRepairRequired {
		t.Fatalf("retained receipt should authorize only its matching failed-workflow prerequisite: %+v", reobserved)
	}
	prerequisite, ok := reobserved.Evidence[ciRerunPrerequisiteKey].(map[string]any)
	if !ok || prerequisite["workflow_run_id"] != "101" || prerequisite["head_sha"] != "head" {
		t.Fatalf("re-observation lost the matching workflow prerequisite: %#v", reobserved.Evidence)
	}
}

func TestCIRerunRecoveryUsesPersistedGenerationAfterTransportFailure(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	now := time.Now().UTC().Truncate(time.Second)
	old := ciWaitRegistration{Protocol: ciWaitProtocol, PullRequest: 17, HeadSHA: "head", ExecutionID: "old-checks", StartedUnixSeconds: now.Unix(), DeadlineUnixSeconds: now.Add(ciWaitTimeout).Unix(), EffectiveTimeoutSecs: int64(ciWaitTimeout / time.Second)}
	oldEvidence, err := ciWaitEvidenceFromRegistration(old, 17)
	if err != nil {
		t.Fatal(err)
	}
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"branch": "42-fix", "batch_id": "batch"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Second), Payload: func() map[string]any {
			payload := cloneLifecycleExtras(oldEvidence)
			for key, value := range map[string]any{"await": true, "await_reason": "pending", "branch": "42-fix", "base_branch": "main", "batch_id": "batch"} {
				payload[key] = value
			}
			return payload
		}()},
	}}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{"42-fix": {Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "pending", CIExecutionID: "old-checks"}}}
	e := &runExecutor{deps: runDeps{eventLog: log, githubClient: client, layout: layout, errorLog: io.Discard, runSessionOpts: runSessionOptions{now: func() time.Time { return now }, currentHead: func(string) (string, error) { return "head", nil }}}}
	row := RowSpec{RunID: "row", IssueNumber: 42, BatchID: "batch", BaseBranch: "main", Branches: map[int]string{42: "42-fix"}}
	claim, err := daemon.ClaimRun(layout.SandmanDir, "row")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	owner, err := newWaitOwner(layout, layout.LocateBatch("batch", ""), daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: "42-fix", BaseBranch: "main", OperationID: "pending:" + old.ExecutionID, OperationDeadline: time.Unix(old.DeadlineUnixSeconds, 0)}, func() time.Time { return now }, log)
	if err != nil {
		t.Fatal(err)
	}
	client.prs["42-fix"].CIExecutionID = "rerun-checks"
	observedLifecycle252 := e.observeRowLifecycle(context.Background(), row)
	status, extras, handled := observedLifecycle252.status(), observedLifecycle252.Evidence, observedLifecycle252.handled()
	if !handled || status != "await" {
		t.Fatalf("rerun observation status=%q handled=%v extras=%#v", status, handled, extras)
	}
	if err := e.persistObservedAwait(context.Background(), row, extras); err != nil {
		t.Fatal(err)
	}
	if err := owner.checkpoint(row, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	owner.close()
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
	ready := FindReadyContinuations(log.snapshot(), layout)
	if len(ready) != 1 || ready[0].Wait == nil || ready[0].Wait.Ready || ready[0].Wait.OperationDeadline.Unix() != now.Add(ciWaitTimeout).Unix() {
		t.Fatalf("recovery schedule did not retain the new operation: %+v", ready)
	}
	client.findPRErr = errors.New("temporary transport failure")
	restart := newRunSession(e, row)
	observedLifecycle272 := restart.observeLifecycleWithHostPathsForTest(context.Background(), root, "42-fix", "", "row", true)
	status, extras, handled = observedLifecycle272.status(), observedLifecycle272.Evidence, observedLifecycle272.handled()
	if !handled || status != "await" {
		t.Fatalf("transport recovery status=%q handled=%v extras=%#v", status, handled, extras)
	}
	ciWait, ok := extras["ci_wait"].(map[string]any)
	if !ok || ciWait["execution_id"] != "rerun-checks" || ciWait["deadline_unix_seconds"] != now.Add(ciWaitTimeout).Unix() {
		t.Fatalf("transport recovery used stale CI generation: %#v", extras)
	}
}

func TestObservedAwaitPersistsCIGenerationWhenReviewDeadlineIsEarlier(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	review := map[string]any{
		"head_sha":              "head",
		"pull_request":          17,
		"trigger_id":            "review-1",
		"deadline_unix_seconds": now.Add(5 * time.Minute).Unix(),
	}
	oldCI := map[string]any{
		"head_sha":              "head",
		"pull_request":          17,
		"execution_id":          "old-checks",
		"deadline_unix_seconds": now.Add(ciWaitTimeout).Unix(),
	}
	newCI := cloneLifecycleExtras(map[string]any{
		"head_sha":              "head",
		"pull_request":          17,
		"execution_id":          "rerun-checks",
		"deadline_unix_seconds": now.Add(2 * ciWaitTimeout).Unix(),
	})
	previous := map[string]any{"ci_wait": oldCI, "review_request": review, "await": true}
	observed := map[string]any{"ci_wait": newCI, "review_request": review, "await": true}
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"branch": "42-fix", "batch_id": "batch"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Second), Payload: previous},
	}}
	e := &runExecutor{deps: runDeps{eventLog: log, layout: paths.NewLayout(nil, t.TempDir()), runSessionOpts: runSessionOptions{now: func() time.Time { return now }}}}
	row := RowSpec{RunID: "row", IssueNumber: 42, BatchID: "batch", BaseBranch: "main", Branches: map[int]string{42: "42-fix"}}
	if err := e.persistObservedAwait(context.Background(), row, observed); err != nil {
		t.Fatal(err)
	}

	var awaits []events.Event
	for _, event := range log.snapshot() {
		if event.Type == "run.await" {
			awaits = append(awaits, event)
		}
	}
	if len(awaits) != 2 {
		t.Fatalf("changed CI generation was deduplicated under earlier review deadline: %+v", log.snapshot())
	}
	latestCI, ok := awaits[1].Payload["ci_wait"].(map[string]any)
	if !ok || latestCI["execution_id"] != "rerun-checks" || latestCI["deadline_unix_seconds"] != now.Add(2*ciWaitTimeout).Unix() {
		t.Fatalf("latest await lost changed CI generation: %#v", awaits[1].Payload)
	}
}

func TestObservedAwaitRetainsOmittedCIWhenReviewChanges(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous := map[string]any{
		"ci_wait": map[string]any{
			"head_sha":              "head",
			"pull_request":          17,
			"execution_id":          "checks-1",
			"deadline_unix_seconds": now.Add(ciWaitTimeout).Unix(),
		},
		"review_request": map[string]any{
			"head_sha":              "head",
			"pull_request":          17,
			"trigger_id":            "review-1",
			"deadline_unix_seconds": now.Add(5 * time.Minute).Unix(),
		},
		"await": true,
	}
	observed := map[string]any{
		"review_request": map[string]any{
			"head_sha":              "head",
			"pull_request":          17,
			"trigger_id":            "review-2",
			"deadline_unix_seconds": now.Add(10 * time.Minute).Unix(),
		},
		"await": true,
	}
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"branch": "42-fix", "batch_id": "batch"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Second), Payload: previous},
	}}
	e := &runExecutor{deps: runDeps{eventLog: log, layout: paths.NewLayout(nil, t.TempDir()), runSessionOpts: runSessionOptions{now: func() time.Time { return now }}}}
	row := RowSpec{RunID: "row", IssueNumber: 42, BatchID: "batch", BaseBranch: "main", Branches: map[int]string{42: "42-fix"}}
	if err := e.persistObservedAwait(context.Background(), row, observed); err != nil {
		t.Fatal(err)
	}

	awaits := make([]events.Event, 0, 2)
	for _, event := range log.snapshot() {
		if event.Type == "run.await" {
			awaits = append(awaits, event)
		}
	}
	if len(awaits) != 2 {
		t.Fatalf("changed review observation was not persisted: %+v", log.snapshot())
	}
	latest := awaits[1].Payload
	ciWait, ok := latest["ci_wait"].(map[string]any)
	if !ok || ciWait["execution_id"] != "checks-1" || ciWait["deadline_unix_seconds"] != now.Add(ciWaitTimeout).Unix() {
		t.Fatalf("omitted CI evidence was not retained: %#v", latest)
	}
	review, ok := latest["review_request"].(map[string]any)
	if !ok || review["trigger_id"] != "review-2" || review["deadline_unix_seconds"] != now.Add(10*time.Minute).Unix() {
		t.Fatalf("changed review evidence was not persisted: %#v", latest)
	}
}

type recordingCIRerunGH struct {
	ghPath  string
	fixture string
	env     []string
}

func newRecordingCIRerunGH(t *testing.T, root string) recordingCIRerunGH {
	t.Helper()
	bin := filepath.Join(root, "fake-gh-bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(recordingCIRerunGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(root, "recording-gh-state.json")
	state := map[string]any{
		"actions": []string{},
		"pr": map[string]any{
			"number": 17, "state": "open", "merged_at": nil,
			"head": map[string]any{"sha": "head"},
			"base": map[string]any{"repo": map[string]any{"full_name": "owner/repo"}},
		},
		"run": map[string]any{
			"id": 12345, "run_attempt": 1, "status": "completed", "conclusion": "failure",
			"event": "pull_request", "head_sha": "tested-merge-sha",
			"repository":    map[string]any{"full_name": "owner/repo"},
			"pull_requests": []any{map[string]any{"number": 17, "head": map[string]any{"sha": "head"}}},
		},
		"jobs": []any{
			map[string]any{"id": 987654321, "run_id": 12345, "run_attempt": 1, "name": "opencode-canary", "workflow_name": "CI", "status": "completed", "conclusion": "failure"},
			map[string]any{"id": 987654320, "run_id": 12345, "run_attempt": 1, "name": "quality-gate", "workflow_name": "CI", "status": "completed", "conclusion": "failure"},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, data, 0o600); err != nil {
		t.Fatal(err)
	}
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "PATH=") && !strings.HasPrefix(item, "HOME=") && !strings.HasPrefix(item, "CI_RERUN_FIXTURE=") {
			env = append(env, item)
		}
	}
	env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CI_RERUN_FIXTURE="+fixture, "HOME="+filepath.Join(root, "agent-home"))
	return recordingCIRerunGH{ghPath: ghPath, fixture: fixture, env: env}
}

func installCIRerunHelperForManagedTest(t *testing.T, root string) string {
	t.Helper()
	home := filepath.Join(root, "agent-home")
	if err := skill.Sync(skill.SyncOptions{HomeDir: home, ReviewCommand: "/sandman review"}); err != nil {
		t.Fatalf("install shared CI recovery capability: %v", err)
	}
	return filepath.Join(home, ".agents", "skills", "sandman", "implement", "ci-rerun-v1.sh")
}

func runCIRerunHelperForManagedTest(t *testing.T, gh recordingCIRerunGH, helperPath, commandText string) string {
	t.Helper()
	if _, err := os.Stat(helperPath); err != nil {
		t.Fatalf("installed helper is unavailable: %v", err)
	}
	command := exec.Command("sh", "-c", commandText)
	command.Env = gh.env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("execute installed CI rerun helper: %v: %s", err, output)
	}
	return string(output)
}

func configureRecordingCIRerunGHBlock(t *testing.T, gh recordingCIRerunGH, readyPath, cancelPath string) {
	t.Helper()
	data, err := os.ReadFile(gh.fixture)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state["block_jobs"] = true
	state["jobs_ready_path"] = readyPath
	state["cancel_path"] = cancelPath
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gh.fixture, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type cancellableCIRerunRunnableFactory struct {
	gh            recordingCIRerunGH
	helperCommand string
	created       int
}

func (f *cancellableCIRerunRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.created++
	return &cancellableCIRerunRunnable{issue: issue, branch: branch, worktree: sb.WorkDir(), gh: f.gh, helperCommand: f.helperCommand}
}

type cancellableCIRerunRunnable struct {
	issue         *github.Issue
	branch        string
	worktree      string
	gh            recordingCIRerunGH
	helperCommand string
}

func (r *cancellableCIRerunRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	command := exec.CommandContext(ctx, "sh", "-c", r.helperCommand)
	command.Env = r.gh.env
	_, err := command.CombinedOutput()
	status := "failed"
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		status = "aborted"
	}
	return AgentRunResult{IssueNumber: r.issue.Number, Status: status, Branch: r.branch, WorktreePath: r.worktree}
}

func captureCIRerunActionFromOpenCodeToolEvent(t *testing.T, command, output string) *ciRerunActionEvidence {
	t.Helper()
	var parser outputParser = newOpenCodeOutput(io.Discard, io.Discard, false)
	reporter, ok := parser.(ciRerunActionReporter)
	if !ok {
		t.Fatal("OpenCode parser does not expose helper tool evidence")
	}
	event, err := json.Marshal(map[string]any{
		"type": "tool_use",
		"part": map[string]any{"tool": "bash", "state": map[string]any{
			"status": "completed", "input": map[string]any{"command": command}, "output": output,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Write(append(event, '\n')); err != nil {
		t.Fatal(err)
	}
	return reporter.CIRerunAction()
}

func (gh recordingCIRerunGH) actions(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(gh.fixture)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.Actions
}

const recordingCIRerunGHScript = `#!/usr/bin/env python3
import json, os, sys, time
path = os.environ['CI_RERUN_FIXTURE']
with open(path) as f: state = json.load(f)
args = sys.argv[1:]
if args[:1] == ['pr']:
    if args[1:3] == ['merge', '17']:
        state['actions'].append('gh ' + ' '.join(args))
        with open(path, 'w') as f: json.dump(state, f)
        print('merged')
        sys.exit(0)
    sys.exit(9)
if args[:1] != ['api']:
    sys.exit(9)
method, route = 'GET', None
i = 1
while i < len(args):
    if args[i] in ('--method', '-X'):
        method = args[i + 1].upper(); i += 2
    elif args[i] == '-H':
        i += 2
    elif not args[i].startswith('-'):
        route = args[i]; i += 1
    else:
        i += 1
if method == 'POST':
    state['actions'].append('POST ' + route)
    state['run']['run_attempt'] = 2
    state['run']['status'] = 'in_progress'
    state['run']['conclusion'] = None
    state['jobs'] = [
        {'id': 987654322, 'run_id': 12345, 'run_attempt': 2, 'name': 'opencode-canary', 'workflow_name': 'CI', 'status': 'queued', 'conclusion': None},
        {'id': 987654323, 'run_id': 12345, 'run_attempt': 2, 'name': 'quality-gate', 'workflow_name': 'CI', 'status': 'queued', 'conclusion': None},
    ]
    with open(path, 'w') as f: json.dump(state, f)
    print('{}')
    sys.exit(0)
if route == 'repos/owner/repo/pulls/17':
    print(json.dumps(state['pr']))
elif route == 'repos/owner/repo/actions/runs/12345':
    print(json.dumps(state['run']))
elif route == 'repos/owner/repo/actions/runs/12345/jobs?filter=latest&per_page=100':
    if state.get('block_jobs'):
        with open(state['jobs_ready_path'], 'w') as f: f.write('ready')
        while not os.path.exists(state['cancel_path']): time.sleep(0.01)
        sys.exit(130)
    print(json.dumps({'total_count': len(state['jobs']), 'jobs': state['jobs']}))
else:
    sys.exit(2)
with open(path, 'w') as f: json.dump(state, f)
`
