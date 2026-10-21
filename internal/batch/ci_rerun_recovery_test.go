package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
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
	status, extras, handled := e.observeLifecycle(context.Background(), row)
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
	status, extras, handled = restart.handleLifecycleDecision(context.Background(), root, "42-fix", "", "row", true)
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
