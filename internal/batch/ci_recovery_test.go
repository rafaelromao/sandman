package batch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

type ciRecoveryFakeClient struct {
	*fakeGitHubClient
	eventLog      events.EventLog
	findCalls     int
	issueCalls    int
	jobCalls      int
	rerunCalls    []int64
	jobs          []github.CIWorkflowJob
	rerunJobs     []github.CIWorkflowJob
	rerunErr      error
	intentVisible bool
}

func (c *ciRecoveryFakeClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	c.issueCalls++
	return c.fakeGitHubClient.FetchIssue(ctx, number)
}

func (c *ciRecoveryFakeClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	c.findCalls++
	return c.fakeGitHubClient.FindPRByBranch(ctx, branch)
}

func (c *ciRecoveryFakeClient) ListWorkflowJobs(_ context.Context, runID string) ([]github.CIWorkflowJob, error) {
	c.jobCalls++
	if fmt.Sprint(c.jobs[0].RunID) != runID {
		return nil, fmt.Errorf("workflow run ID = %s, want %d", runID, c.jobs[0].RunID)
	}
	return append([]github.CIWorkflowJob(nil), c.jobs...), nil
}

func (c *ciRecoveryFakeClient) RerunWorkflowJob(_ context.Context, jobID int64) error {
	c.rerunCalls = append(c.rerunCalls, jobID)
	for _, event := range c.eventLog.(*spyEventLog).snapshot() {
		if event.Type == ciRecoveryIntentEvent {
			c.intentVisible = true
		}
	}
	if c.rerunJobs != nil {
		c.jobs = append([]github.CIWorkflowJob(nil), c.rerunJobs...)
	}
	return c.rerunErr
}

type intentWriteFailureLog struct {
	*spyEventLog
}

func (l *intentWriteFailureLog) Log(event events.Event) error {
	if event.Type == ciRecoveryIntentEvent {
		return errors.New("injected durable intent write failure")
	}
	return l.spyEventLog.Log(event)
}

func ciRecoveryFixture(t *testing.T, now time.Time, client *ciRecoveryFakeClient, log events.EventLog, status github.StatusChecker) (*runExecutor, RowSpec) {
	t.Helper()
	action := ciRerunEvidenceForDiagnosis("confirmed", "transient-provider-timeout")
	action.SourceHeadSHA = "head"
	action.TestedSHA = "tested"
	action.RunID = "101"
	action.JobName = "opencode-canary"
	action.JobID = "111"
	prerequisite := map[string]any{
		"protocol": "ci-rerun-prerequisite/v1", "repository": "owner/repo",
		"pull_request": 17, "head_sha": "head", "workflow_run_id": "101",
		"failed_checks": []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		"workflow_check_names": []string{"opencode-canary", "quality-gate"},
	}
	recovery := ciRecoveryState{
		Window:     recoveryWindow{WindowStarted: now.Add(-time.Minute), NextAttempt: now.Add(time.Hour)},
		Repository: "owner/repo", Issue: 42, Branch: "42-work",
		PullRequest: 17, HeadSHA: "head", WorkflowRunID: "101",
		JobName: action.JobName, Diagnosis: action.Diagnosis, Receipt: action,
		FailedChecks:       prerequisite["failed_checks"].([]github.CIFailedCheck),
		WorkflowCheckNames: prerequisite["workflow_check_names"].([]string),
	}
	await := map[string]any{
		"await": true, "await_reason": ciRecoveryGate, "gate": ciRecoveryGate,
		"reason": "CI_FAILURE", "branch": "42-work", "batch_id": "batch",
		"base_branch": "main", "pull_request": 17, "head_sha": "head",
		ciRecoveryKey: recovery.payload(), ciRerunActionKey: action.payload(),
		ciRecoveryPrerequisiteKey: prerequisite,
	}
	eventsLog, ok := log.(*spyEventLog)
	if !ok {
		if failure, ok := log.(*intentWriteFailureLog); ok {
			eventsLog = failure.spyEventLog
		}
	}
	if eventsLog == nil {
		t.Fatal("test event log does not expose seed events")
	}
	eventsLog.events = []events.Event{
		{Type: "run.started", RunID: "run", Issue: 42, Timestamp: now.Add(-2 * time.Hour), Payload: map[string]any{"branch": "42-work", "batch_id": "batch"}},
		{Type: "run.await", RunID: "run", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: await},
	}
	client.eventLog = log
	root := t.TempDir()
	e := &runExecutor{deps: runDeps{
		githubClient: client, eventLog: log, layout: paths.NewLayout(nil, root),
		errorLog: nil, githubStatus: status,
		runSessionOpts: runSessionOptions{now: func() time.Time { return now }, currentHead: func(string) (string, error) { return "head", nil }},
	}}
	row := RowSpec{RunID: "run", IssueNumber: 42, BatchID: "batch", BaseBranch: "main", Branches: map[int]string{42: "42-work"}}
	return e, row
}

func makeCIRRecoveryDue(t *testing.T, log *spyEventLog, now time.Time) {
	t.Helper()
	latest := len(log.events) - 1
	state, ok := ciRecoveryFromEvidence(log.events[latest].Payload)
	if !ok {
		t.Fatal("CI recovery fixture did not decode")
	}
	state.Window.NextAttempt = now
	log.events[latest].Payload[ciRecoveryKey] = state.payload()
}

func TestCIRerunRecoveryRehydratesSchedulerOperationAndMissingCheckpoint(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	batchDir := filepath.Join(layout.SandmanDir, "batches", "batch-ci-recovery")
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{{
		ID: "batch-ci-recovery", Path: batchDir, Status: batchindex.StatusActive, Kind: batchindex.KindIssue,
	}}}).Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-2 * time.Hour)
	awaitedAt := now.Add(-time.Minute)
	nextAttempt := now.Add(40 * time.Minute)
	action := ciRerunEvidenceForDiagnosis("confirmed", "transient-provider-timeout")
	action.SourceHeadSHA, action.TestedSHA = "head", "tested"
	recovery := ciRecoveryState{
		Window:     recoveryWindow{WindowStarted: startedAt, NextAttempt: nextAttempt},
		Repository: "owner/repo", Issue: 42, Branch: "42-ci",
		PullRequest: 17, HeadSHA: "head", WorkflowRunID: "101",
		JobName: action.JobName, Diagnosis: action.Diagnosis, Receipt: action,
		FailedChecks: []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		WorkflowCheckNames: []string{"opencode-canary", "quality-gate"},
	}
	await := ciRecoveryAwait(nil, "42-ci", recovery).Evidence
	await["await"], await["base_branch"], await["batch_id"] = true, "main", "batch-ci-recovery"
	eventLog := []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(-time.Hour), RunID: "run-ci", Issue: 42,
			Payload: map[string]any{"branch": "42-ci", "base_branch": "main", "batch_id": "batch-ci-recovery"}},
		{Type: "run.await", Timestamp: awaitedAt, RunID: "run-ci", Issue: 42, Payload: await},
	}

	ready := FindReadyContinuationsAt(eventLog, layout, now)
	if len(ready) != 1 || ready[0].Wait == nil {
		t.Fatalf("missing-checkpoint recovery = %#v, want one recovered continuation", ready)
	}
	wait := ready[0].Wait
	wantOperationID := fmt.Sprintf("ci-recovery:%d", startedAt.Unix())
	if wait.Ready || wait.OperationID != wantOperationID ||
		!wait.OperationDeadline.Equal(startedAt.Add(ciRecoveryBound)) ||
		!wait.NextPollAt.Equal(nextAttempt) ||
		!wait.LeaseExpiresAt.Equal(awaitedAt.Add(daemon.RunRecoveryGrace)) ||
		!wait.RecoverableAt(now) {
		t.Fatalf("reconstructed scheduler wait = %+v, want operation %q, fixed bound, and hourly schedule", wait, wantOperationID)
	}

	states := events.ProjectRunStates(eventLog)
	stale := daemon.RunWait{
		Protocol: "run-wait/v1", RunID: "run-ci", BatchID: "batch-ci-recovery",
		LeaseExpiresAt: now.Add(daemon.RunRecoveryGrace), OperationID: "old-operation",
		NextPollAt: now.Add(5 * time.Minute),
	}
	reconciled, changed, valid := reconcileRecoveryWait(stale, states[0])
	if !valid || !changed || reconciled.OperationID != wantOperationID ||
		!reconciled.OperationDeadline.Equal(startedAt.Add(ciRecoveryBound)) ||
		!reconciled.NextPollAt.Equal(nextAttempt) {
		t.Fatalf("checkpoint reconciliation = %+v changed=%v valid=%v", reconciled, changed, valid)
	}
}

func TestCIRerunRecoveryQualificationUsesOnlyConfirmedPredecessorReceipts(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	action := ciRerunEvidenceForDiagnosis("confirmed", "transient-provider-timeout")
	action.SourceHeadSHA, action.TestedSHA, action.RunID, action.JobName = "head", "tested", "101", "opencode-canary"
	prerequisite := map[string]any{
		"protocol": "ci-rerun-prerequisite/v1", "repository": "owner/repo",
		"pull_request": 17, "head_sha": "head", "workflow_run_id": "101",
		"failed_checks": []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		"workflow_check_names": []string{"opencode-canary", "quality-gate"},
	}
	oldRun := events.Event{
		Type: "run.await", RunID: "old-run", Issue: 42, Timestamp: now.Add(-time.Hour),
		Payload: map[string]any{
			"await": true, "branch": "42-work", ciRerunActionKey: action.payload(),
		},
	}
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "old-run", Issue: 42, Timestamp: now.Add(-2 * time.Hour), Payload: map[string]any{"branch": "42-work"}},
		oldRun,
		{Type: "run.continued", RunID: "current-run", Issue: 42, Timestamp: now, Payload: map[string]any{
			"branch": "42-work", "previous_run_id": "old-run",
		}},
	}}
	client := &ciRecoveryFakeClient{fakeGitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{
		"42-work": {Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "failure",
			CIFailedWorkflow: &github.CIFailedWorkflow{RunID: "101",
				FailedChecks: prerequisite["failed_checks"].([]github.CIFailedCheck),
				CheckNames:   prerequisite["workflow_check_names"].([]string)}},
	}}}
	e := &runExecutor{deps: runDeps{
		githubClient: client, eventLog: log, errorLog: nil,
		runSessionOpts: runSessionOptions{now: func() time.Time { return now }, currentHead: func(string) (string, error) { return "head", nil }},
	}}
	row := RowSpec{RunID: "current-run", IssueNumber: 42, Branches: map[int]string{42: "42-work"}}
	session := newRunSession(e, row)
	observer := newLifecycleObserver(session)
	evidence := map[string]any{
		"gate": "ci-failure", "reason": "CI_FAILURE", "pull_request": 17, "head_sha": "head",
		ciRecoveryPrerequisiteKey: prerequisite,
	}
	wait := observer.Observe(t.Context(), lifecycleObservationRequest{
		Branch: "42-work", RunID: "current-run", HostPathsReady: true, AttemptStatus: "success",
	})
	if wait.Action != lifecycleAwait || stringEvidence(wait.Evidence, "gate") != ciRecoveryGate {
		t.Fatalf("matching predecessor receipt = %s/%v, want a CI recovery wait at entry", wait.status(), wait.Evidence)
	}
	state, ok := ciRecoveryFromEvidence(wait.Evidence)
	if !ok || state.Window.WindowStarted != now || state.Window.NextAttempt != now ||
		!state.bound().Equal(now.Add(ciRecoveryBound)) {
		t.Fatalf("new Run did not start an immediate fresh recovery window: %#v", wait.Evidence[ciRecoveryKey])
	}

	sameRunLog := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "same-run", Issue: 42, Timestamp: now.Add(-time.Hour), Payload: map[string]any{"branch": "42-work"}},
		{Type: "run.resumed", RunID: "same-run", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{
			"branch": "42-work", ciRerunActionKey: action.payload(),
		}},
	}}
	sameRunExecutor := &runExecutor{deps: runDeps{
		githubClient: client, eventLog: sameRunLog,
		runSessionOpts: runSessionOptions{now: func() time.Time { return now }},
	}}
	sameRunRow := RowSpec{RunID: "same-run", IssueNumber: 42, Branches: map[int]string{42: "42-work"}}
	sameRunObserver := newLifecycleObserver(newRunSession(sameRunExecutor, sameRunRow))
	sameRunWait, qualified := sameRunObserver.beginCIRerunRecovery("42-work", "same-run", evidence, false)
	sameRunState, valid := ciRecoveryFromEvidence(sameRunWait.Evidence)
	if !qualified || !valid || !sameRunState.Window.NextAttempt.Equal(now.Add(ciRecoveryInterval)) {
		t.Fatalf("same-RunID stall schedule = qualified %t, evidence %#v; want first recovery in one hour", qualified, sameRunWait.Evidence)
	}

	for name, mutate := range map[string]func(*ciRerunActionEvidence, *events.Event){
		"waiting receipt": func(action *ciRerunActionEvidence, _ *events.Event) {
			*action = ciRerunEvidenceForDiagnosis("waiting", "transient-provider-timeout")
		},
		"foreign branch": func(_ *ciRerunActionEvidence, event *events.Event) {
			event.Payload["branch"] = "other-branch"
		},
		"foreign issue": func(_ *ciRerunActionEvidence, event *events.Event) {
			event.Issue = 43
		},
	} {
		t.Run(name, func(t *testing.T) {
			copyAction := action
			copyEvent := oldRun
			copyEvent.Payload = cloneLifecycleExtras(oldRun.Payload)
			mutate(&copyAction, &copyEvent)
			copyEvent.Payload[ciRerunActionKey] = copyAction.payload()
			testLog := &spyEventLog{events: []events.Event{
				{Type: "run.started", RunID: "old-run", Issue: 42, Timestamp: now.Add(-2 * time.Hour), Payload: map[string]any{"branch": "42-work"}},
				copyEvent,
				{Type: "run.continued", RunID: "current-run", Issue: 42, Timestamp: now, Payload: map[string]any{
					"branch": "42-work", "previous_run_id": "old-run",
				}},
			}}
			testExecutor := &runExecutor{deps: runDeps{
				githubClient: client, eventLog: testLog,
				runSessionOpts: runSessionOptions{now: func() time.Time { return now }},
			}}
			testObserver := newLifecycleObserver(newRunSession(testExecutor, row))
			verdict, qualified := testObserver.beginCIRerunRecovery("42-work", "current-run", evidence, true)
			if qualified {
				t.Fatalf("unqualified predecessor receipt opened recovery: %s/%v", verdict.status(), verdict.Evidence)
			}
		})
	}
}

func TestCIRerunRecoveryStaysQuietUntilItsScheduledAttempt(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	client := &ciRecoveryFakeClient{fakeGitHubClient: &fakeGitHubClient{}, jobs: []github.CIWorkflowJob{{RunID: 101, ID: 1}}}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	verdict := e.observeAwaitingRowLifecycle(context.Background(), row)
	if verdict.Action != lifecycleAwait || stringEvidence(verdict.Evidence, "gate") != ciRecoveryGate {
		t.Fatalf("verdict = %s/%v, want the retained CI recovery wait", verdict.status(), verdict.Evidence)
	}
	if client.issueCalls != 0 || client.findCalls != 0 || client.jobCalls != 0 || len(client.rerunCalls) != 0 || len(status.requested) != 0 {
		t.Fatalf("pre-due observation made external calls: issue=%d PR=%d jobs=%d reruns=%v status=%v",
			client.issueCalls, client.findCalls, client.jobCalls, client.rerunCalls, status.requested)
	}
}

func TestCIRerunRecoveryQuietObservationDoesNotRerecordItsAwait(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	client := &ciRecoveryFakeClient{fakeGitHubClient: &fakeGitHubClient{}, jobs: []github.CIWorkflowJob{{RunID: 101, ID: 1}}}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	verdict := e.observeAwaitingRowLifecycle(context.Background(), row)
	if err := e.persistObservedAwait(context.Background(), row, verdict.Evidence); err != nil {
		t.Fatal(err)
	}
	if got := len(log.snapshot()); got != 2 {
		t.Fatalf("quiet observation appended an unchanged await: event count=%d, want the original 2", got)
	}
}

func TestCIRerunRecoveryUnreadableHistoryFailsClosedFromItsWaitLease(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	layout := paths.NewLayout(nil, t.TempDir())
	batchLocation := layout.LocateBatch("batch", "")
	wait := daemon.RunWait{
		Protocol: "run-wait/v1", RunID: "run", BatchID: "batch", Issue: 42,
		Branch: "42-work", BaseBranch: "main", OperationID: "ci-recovery:" + fmt.Sprint(now.Unix()),
		OperationDeadline: now.Add(ciRecoveryBound), LeaseExpiresAt: now.Add(daemon.RunRecoveryGrace),
		NextPollAt: now.Add(time.Hour),
	}
	if err := daemon.RenewRunWait(layout, batchLocation, wait, now); err != nil {
		t.Fatal(err)
	}
	client := &ciRecoveryFakeClient{fakeGitHubClient: &fakeGitHubClient{}}
	e := &runExecutor{deps: runDeps{
		githubClient: client, eventLog: unreadableEventLog{}, layout: layout,
		runSessionOpts: runSessionOptions{now: func() time.Time { return now }},
	}}
	row := RowSpec{RunID: "run", IssueNumber: 42, BatchID: "batch", BaseBranch: "main", Branches: map[int]string{42: "42-work"}}
	verdict := e.observeAwaitingRowLifecycle(context.Background(), row)
	if verdict.Action != lifecycleAwait || stringEvidence(verdict.Evidence, "reason") != ciRecoveryStateUnavailableReason {
		t.Fatalf("unreadable recovery history = %s/%v, want a fail-closed wait", verdict.status(), verdict.Evidence)
	}
	if client.issueCalls != 0 || client.findCalls != 0 || client.jobCalls != 0 || len(client.rerunCalls) != 0 {
		t.Fatalf("unreadable history reached GitHub: issue=%d PR=%d jobs=%d reruns=%v",
			client.issueCalls, client.findCalls, client.jobCalls, client.rerunCalls)
	}
}

func TestCIRerunRecoveryAbortPreventsDueStatusAndMutation(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	client := &ciRecoveryFakeClient{fakeGitHubClient: &fakeGitHubClient{}, jobs: []github.CIWorkflowJob{{RunID: 101, ID: 1}}}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	makeCIRRecoveryDue(t, log, now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	verdict := e.observeAwaitingRowLifecycle(ctx, row)
	if verdict.Action != lifecycleAborted {
		t.Fatalf("aborted recovery observation = %s/%v, want abort", verdict.status(), verdict.Evidence)
	}
	if len(status.requested) != 0 || client.issueCalls != 0 || client.findCalls != 0 ||
		client.jobCalls != 0 || len(client.rerunCalls) != 0 {
		t.Fatalf("abort did not fence due recovery work: status=%v issue=%d PR=%d jobs=%d reruns=%v",
			status.requested, client.issueCalls, client.findCalls, client.jobCalls, client.rerunCalls)
	}
}

func TestCIRerunRecoverySkipsMutationDuringActionsOutage(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{
		Indicator: "minor", Outage: true, Description: "Actions degraded", Components: []string{"Actions: degraded_performance"},
	}}
	client := &ciRecoveryFakeClient{fakeGitHubClient: &fakeGitHubClient{}, jobs: []github.CIWorkflowJob{{RunID: 101, ID: 1}}}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	makeCIRRecoveryDue(t, log, now)
	verdict := e.observeAwaitingRowLifecycle(context.Background(), row)
	if verdict.Action != lifecycleAwait {
		t.Fatalf("verdict = %s/%v, want an await", verdict.status(), verdict.Evidence)
	}
	recovery, ok := ciRecoveryFromEvidence(verdict.Evidence)
	if !ok || recovery.Window.NextAttempt != now.Add(time.Hour) || recovery.Window.StatusChecks != 1 ||
		recovery.Window.LastStatus != "outage" || recovery.LastRerunOutcome != "skipped-github-actions-outage" {
		t.Fatalf("outage wait evidence = %#v, want a recorded one-hour skip", verdict.Evidence[ciRecoveryKey])
	}
	if client.findCalls != 0 || client.jobCalls != 0 || len(client.rerunCalls) != 0 {
		t.Fatalf("outage observation reached GitHub mutation path: PR=%d jobs=%d reruns=%v", client.findCalls, client.jobCalls, client.rerunCalls)
	}
	if len(status.requested) != 1 || len(status.requested[0]) != 1 || status.requested[0][0] != github.ComponentActions {
		t.Fatalf("status components = %v, want Actions only", status.requested)
	}
}

func TestCIRerunRecoveryPersistsIntentBeforeTargetedRerun(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	pr := &github.PR{
		Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "failure",
		CIActiveFailedWorkflowRunID: "101",
		CIActiveFailedWorkflowRunFailedChecks: []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "quality-gate"},
	}
	initialJobs := []github.CIWorkflowJob{
		{RunID: 101, RunAttempt: 2, ID: 21, Name: "opencode-canary", Status: "completed", Conclusion: "failure"},
		{RunID: 101, RunAttempt: 2, ID: 22, Name: "quality-gate", Status: "completed", Conclusion: "failure"},
	}
	rerunJobs := []github.CIWorkflowJob{
		{RunID: 101, RunAttempt: 3, ID: 31, Name: "opencode-canary", Status: "in_progress"},
		{RunID: 101, RunAttempt: 3, ID: 32, Name: "quality-gate", Status: "queued"},
	}
	client := &ciRecoveryFakeClient{
		fakeGitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{"42-work": pr}},
		jobs:             initialJobs, rerunJobs: rerunJobs,
	}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	makeCIRRecoveryDue(t, log, now)
	verdict := e.observeAwaitingRowLifecycle(context.Background(), row)
	if verdict.Action != lifecycleAwait {
		t.Fatalf("verdict = %s/%v, want a recovery await", verdict.status(), verdict.Evidence)
	}
	recovery, ok := ciRecoveryFromEvidence(verdict.Evidence)
	if !ok || recovery.RerunAttempts != 1 || recovery.LastRerunOutcome != "confirmed" ||
		recovery.LastWorkflowAttempt != 2 || recovery.MutationIntent != nil ||
		!recovery.Window.NextAttempt.Equal(now.Add(time.Hour)) {
		t.Fatalf("recovery evidence = %#v", verdict.Evidence[ciRecoveryKey])
	}
	if len(client.rerunCalls) != 1 || client.rerunCalls[0] != 21 || !client.intentVisible {
		t.Fatalf("reruns=%v durable intent visible=%v; want one targeted rerun after intent append", client.rerunCalls, client.intentVisible)
	}
	if client.issueCalls != 0 || client.findCalls != 2 || client.jobCalls != 3 || len(status.requested) != 1 {
		t.Fatalf("due attempt calls: issue=%d PR=%d jobs=%d status=%v", client.issueCalls, client.findCalls, client.jobCalls, status.requested)
	}
}

func TestCIRerunRecoveryAttemptsWhenActionsStatusIsUnreachable(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{err: errors.New("status page unavailable")}
	pr := &github.PR{
		Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "failure",
		CIActiveFailedWorkflowRunID: "101",
		CIActiveFailedWorkflowRunFailedChecks: []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "quality-gate"},
	}
	client := &ciRecoveryFakeClient{
		fakeGitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{"42-work": pr}},
		jobs: []github.CIWorkflowJob{
			{RunID: 101, RunAttempt: 2, ID: 21, Name: "opencode-canary", Status: "completed", Conclusion: "failure"},
			{RunID: 101, RunAttempt: 2, ID: 22, Name: "quality-gate", Status: "completed", Conclusion: "failure"},
		},
		rerunJobs: []github.CIWorkflowJob{
			{RunID: 101, RunAttempt: 3, ID: 31, Name: "opencode-canary", Status: "in_progress"},
			{RunID: 101, RunAttempt: 3, ID: 32, Name: "quality-gate", Status: "queued"},
		},
	}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	makeCIRRecoveryDue(t, log, now)
	verdict := e.observeAwaitingRowLifecycle(context.Background(), row)
	recovery, ok := ciRecoveryFromEvidence(verdict.Evidence)
	if verdict.Action != lifecycleAwait || !ok || recovery.LastRerunOutcome != "confirmed" ||
		recovery.Window.LastStatus != "unreachable" || len(client.rerunCalls) != 1 {
		t.Fatalf("unreachable status did not allow the targeted rerun: verdict=%s evidence=%v reruns=%v",
			verdict.status(), verdict.Evidence, client.rerunCalls)
	}
	if len(status.requested) != 1 || status.requested[0][0] != github.ComponentActions {
		t.Fatalf("status check components = %v, want the Actions component", status.requested)
	}
}

func TestCIRerunRecoveryReturnsToLifecycleWhenRerunTurnsGreen(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	pr := &github.PR{
		Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "failure",
		CIActiveFailedWorkflowRunID: "101",
		CIActiveFailedWorkflowRunFailedChecks: []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "quality-gate"},
	}
	client := &ciRecoveryFakeClient{
		fakeGitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{"42-work": pr}},
		jobs: []github.CIWorkflowJob{
			{RunID: 101, RunAttempt: 2, ID: 21, Name: "opencode-canary", Status: "completed", Conclusion: "failure"},
			{RunID: 101, RunAttempt: 2, ID: 22, Name: "quality-gate", Status: "completed", Conclusion: "failure"},
		},
		rerunJobs: []github.CIWorkflowJob{
			{RunID: 101, RunAttempt: 3, ID: 31, Name: "opencode-canary", Status: "in_progress"},
			{RunID: 101, RunAttempt: 3, ID: 32, Name: "quality-gate", Status: "queued"},
		},
	}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	makeCIRRecoveryDue(t, log, now)
	first := e.observeAwaitingRowLifecycle(context.Background(), row)
	if first.Action != lifecycleAwait || len(client.rerunCalls) != 1 {
		t.Fatalf("initial runtime rerun = %s/%v calls=%v", first.status(), first.Evidence, client.rerunCalls)
	}
	statusChecks, prReads, jobReads := len(status.requested), client.findCalls, client.jobCalls
	// Simulate a process exit after POST/outcome but before run.await was
	// recorded. The write-ahead schedule must still quiet a fresh observer.
	quiet := e.observeAwaitingRowLifecycle(t.Context(), row)
	if quiet.Action != lifecycleAwait || len(status.requested) != statusChecks ||
		client.findCalls != prReads || client.jobCalls != jobReads {
		t.Fatalf("restart before next due time repeated an observation: %s/%v", quiet.status(), quiet.Evidence)
	}
	now = now.Add(time.Hour)
	e.deps.runSessionOpts.now = func() time.Time { return now }
	pr.StatusCheckRollup = "success"
	second := e.observeAwaitingRowLifecycle(context.Background(), row)
	if stringEvidence(second.Evidence, "gate") == ciRecoveryGate || stringEvidence(second.Evidence, "reason") == "CI_FAILURE" {
		t.Fatalf("green rerun did not return to normal lifecycle: %s/%v", second.status(), second.Evidence)
	}
	if len(client.rerunCalls) != 1 {
		t.Fatalf("green rerun triggered another mutation: %v", client.rerunCalls)
	}
}

func TestCIRerunRecoveryDoesNotAuthorizeRerunForDifferentFailureOrHead(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*github.PR)
	}{
		{
			name: "new head",
			edit: func(pr *github.PR) { pr.HeadRefOid = "new-head" },
		},
		{
			name: "extra failed check",
			edit: func(pr *github.PR) {
				pr.CIActiveFailedWorkflowRunFailedChecks = append(pr.CIActiveFailedWorkflowRunFailedChecks,
					github.CIFailedCheck{Name: "build-linux", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/13"})
				pr.CIActiveFailedWorkflowRunCheckNames = append(pr.CIActiveFailedWorkflowRunCheckNames, "build-linux")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
			status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
			pr := &github.PR{
				Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "failure",
				CIActiveFailedWorkflowRunID: "101",
				CIActiveFailedWorkflowRunFailedChecks: []github.CIFailedCheck{
					{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
					{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
				},
				CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "quality-gate"},
			}
			tc.edit(pr)
			client := &ciRecoveryFakeClient{fakeGitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{"42-work": pr}}}
			log := &spyEventLog{}
			e, row := ciRecoveryFixture(t, now, client, log, status)
			makeCIRRecoveryDue(t, log, now)
			verdict := e.observeAwaitingRowLifecycle(context.Background(), row)
			if verdict.Action == lifecycleAwait && stringEvidence(verdict.Evidence, "gate") == ciRecoveryGate {
				t.Fatalf("unrelated evidence stayed in CI recovery: %s/%v", verdict.status(), verdict.Evidence)
			}
			if len(client.rerunCalls) != 0 {
				t.Fatalf("unrelated evidence authorized reruns: %v", client.rerunCalls)
			}
		})
	}
}

func TestCIRerunRecoveryDoesNotPostWhenMutationIntentCannotBePersisted(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	pr := &github.PR{
		Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "failure",
		CIActiveFailedWorkflowRunID: "101",
		CIActiveFailedWorkflowRunFailedChecks: []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "quality-gate"},
	}
	baseLog := &spyEventLog{}
	log := &intentWriteFailureLog{spyEventLog: baseLog}
	client := &ciRecoveryFakeClient{
		fakeGitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{"42-work": pr}},
		eventLog:         log,
		jobs: []github.CIWorkflowJob{
			{RunID: 101, RunAttempt: 2, ID: 21, Name: "opencode-canary", Status: "completed", Conclusion: "failure"},
			{RunID: 101, RunAttempt: 2, ID: 22, Name: "quality-gate", Status: "completed", Conclusion: "failure"},
		},
	}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	makeCIRRecoveryDue(t, baseLog, now)
	verdict := e.observeAwaitingRowLifecycle(context.Background(), row)
	if verdict.Action != lifecycleAwait {
		t.Fatalf("verdict = %s/%v, want fail-closed recovery wait", verdict.status(), verdict.Evidence)
	}
	if len(client.rerunCalls) != 0 {
		t.Fatalf("rerun calls = %v, mutation must not happen when intent append fails", client.rerunCalls)
	}
}

func TestCIRerunRecoveryReconcilesAmbiguousPostWithoutReplaying(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	pr := &github.PR{
		Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "failure",
		CIActiveFailedWorkflowRunID: "101",
		CIActiveFailedWorkflowRunFailedChecks: []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "quality-gate"},
	}
	client := &ciRecoveryFakeClient{
		fakeGitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{"42-work": pr}},
		jobs: []github.CIWorkflowJob{
			{RunID: 101, RunAttempt: 2, ID: 21, Name: "opencode-canary", Status: "completed", Conclusion: "failure"},
			{RunID: 101, RunAttempt: 2, ID: 22, Name: "quality-gate", Status: "completed", Conclusion: "failure"},
		},
		rerunErr: errors.New("POST outcome is ambiguous"),
	}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	makeCIRRecoveryDue(t, log, now)
	first := e.observeAwaitingRowLifecycle(context.Background(), row)
	firstState, ok := ciRecoveryFromEvidence(first.Evidence)
	if first.Action != lifecycleAwait || !ok || firstState.MutationIntent == nil || len(client.rerunCalls) != 1 {
		t.Fatalf("ambiguous POST state = %s/%v reruns=%v", first.status(), first.Evidence, client.rerunCalls)
	}
	if err := e.persistObservedAwait(context.Background(), row, first.Evidence); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	e.deps.runSessionOpts.now = func() time.Time { return now }
	second := e.observeAwaitingRowLifecycle(context.Background(), row)
	secondState, ok := ciRecoveryFromEvidence(second.Evidence)
	if second.Action != lifecycleAwait || !ok || secondState.MutationIntent == nil ||
		secondState.LastRerunOutcome != "intent-unresolved-no-replay" || len(client.rerunCalls) != 1 {
		t.Fatalf("restart reconciliation = %s/%v reruns=%v, want retained intent and no second POST",
			second.status(), second.Evidence, client.rerunCalls)
	}
}

func TestCIRerunRecoveryExpiresWithoutStatusCheckOrMutation(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	status := &scopedProbeStatus{report: github.StatusReport{Indicator: "none"}}
	pr := &github.PR{
		Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "failure",
		CIActiveFailedWorkflowRunID: "101",
		CIActiveFailedWorkflowRunFailedChecks: []github.CIFailedCheck{
			{Name: "opencode-canary", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/12"},
		},
		CIActiveFailedWorkflowRunCheckNames: []string{"opencode-canary", "quality-gate"},
	}
	client := &ciRecoveryFakeClient{fakeGitHubClient: &fakeGitHubClient{prs: map[string]*github.PR{"42-work": pr}}}
	log := &spyEventLog{}
	e, row := ciRecoveryFixture(t, now, client, log, status)
	state, eventsBase, open, _ := newLifecycleObserver(newRunSession(e, row)).ciRecoveryWindow(row.RunID)
	if !open {
		t.Fatal("fixture CI recovery window did not rehydrate")
	}
	state.Window.WindowStarted = now.Add(-ciRecoveryBound)
	state.Window.NextAttempt = now.Add(-time.Minute)
	eventsBase[ciRecoveryKey] = state.payload()
	log.events[len(log.events)-1].Payload = eventsBase
	verdict := newLifecycleObserver(newRunSession(e, row)).Observe(context.Background(), lifecycleObservationRequest{
		Branch: "42-work", RunID: row.RunID, AttemptStatus: "success",
	})
	if verdict.Action != lifecycleFailure || stringEvidence(verdict.Evidence, "reason") != ciRecoveryExhaustedReason {
		t.Fatalf("expiry verdict = %s/%v, want terminal CI recovery exhaustion", verdict.status(), verdict.Evidence)
	}
	if len(status.requested) != 0 || client.findCalls != 1 || client.jobCalls != 0 || len(client.rerunCalls) != 0 {
		t.Fatalf("expiry calls: status=%v PR=%d jobs=%d reruns=%v", status.requested, client.findCalls, client.jobCalls, client.rerunCalls)
	}
	for _, key := range []string{"await", "await_reason", "continuity_hold", "resume_stalled"} {
		if _, present := verdict.Evidence[key]; present {
			t.Errorf("terminal expiry evidence retains active marker %q", key)
		}
	}
	if _, present := verdict.Evidence[ciRecoveryKey]; !present {
		t.Fatal("terminal expiry dropped its recovery evidence")
	}
}

func TestCIRerunRecoveryBoundaryReusesOnePRReadForNormalLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*github.PR)
		action lifecycleAction
		reason string
	}{
		{"merged", func(pr *github.PR) { pr.State, pr.Merged = "merged", true }, lifecycleSuccess, ""},
		{"different workflow failure", func(pr *github.PR) { pr.CIMultipleFailedWorkflows = true }, lifecycleResume, "CI_FAILURE"},
		{"merge conflict", func(pr *github.PR) { pr.MergeStateStatus = "DIRTY" }, lifecycleResume, "MERGE_CONFLICT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
			pr := &github.PR{Number: 17, State: "open", HeadRefOid: "head", Body: "Closes #42", StatusCheckRollup: "failure"}
			tc.edit(pr)
			status := &scopedProbeStatus{}
			client := &ciRecoveryFakeClient{fakeGitHubClient: &fakeGitHubClient{
				prs: map[string]*github.PR{"42-work": pr},
			}}
			log := &spyEventLog{}
			e, row := ciRecoveryFixture(t, now, client, log, status)
			state, _ := ciRecoveryFromEvidence(log.events[1].Payload)
			state.Window.WindowStarted, state.Window.NextAttempt = now.Add(-ciRecoveryBound), now
			log.events[1].Payload[ciRecoveryKey] = state.payload()
			verdict := e.observeAwaitingRowLifecycle(t.Context(), row)
			if verdict.Action != tc.action || (tc.reason != "" && stringEvidence(verdict.Evidence, "reason") != tc.reason) {
				t.Fatalf("boundary verdict = %s/%v, want %v/%q", verdict.status(), verdict.Evidence, tc.action, tc.reason)
			}
			if client.findCalls != 1 || client.jobCalls != 0 || len(client.rerunCalls) != 0 || len(status.requested) != 0 {
				t.Fatalf("boundary calls: PR=%d jobs=%d reruns=%v status=%v",
					client.findCalls, client.jobCalls, client.rerunCalls, status.requested)
			}
		})
	}
}
