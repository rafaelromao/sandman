package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

// CI recovery window (issue #3011): the fixtures here follow the GitHub
// recovery ones. Time is a fake clock, health a fake status checker and the
// rerun a fake GitHub client: nothing touches the network.

const (
	ciTestRepo   = "owner/repo" // what fakeGitHubClient.RepoName reports
	ciTestHead   = "current-sha"
	ciTestTested = "tested-sha"
	ciTestRun    = "101"
	ciTestJob    = "opencode-canary"
	ciTestPR     = 33
)

// fakeRerunClient is a GitHub client that also reruns workflow jobs.
type fakeRerunClient struct {
	fakeGitHubClient
	requests []github.WorkflowRerunRequest
	results  []github.WorkflowRerunResult
	err      error
	rerunned func()
}

func (f *fakeRerunClient) RerunWorkflowJob(_ context.Context, request github.WorkflowRerunRequest) (github.WorkflowRerunResult, error) {
	f.requests = append(f.requests, request)
	if f.rerunned != nil {
		f.rerunned()
	}
	if f.err != nil {
		return github.WorkflowRerunResult{}, f.err
	}
	if len(f.results) == 0 {
		return github.WorkflowRerunResult{State: github.WorkflowRerunConfirmed, Reason: "selected-job-and-quality-gate-rerun-observed", ObservedAttempt: 3}, nil
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result, nil
}

func ciTestReceipt() ciRerunActionEvidence {
	return ciRerunActionEvidence{
		Protocol: "ci-rerun/v1", State: "confirmed", Reason: "selected-job-and-quality-gate-rerun-observed",
		Repository: ciTestRepo, PullRequest: ciTestPR, SourceHeadSHA: ciTestHead, TestedSHA: ciTestTested,
		Diagnosis: "transient-provider-quota", RunID: ciTestRun, JobID: "11", JobName: ciTestJob, ObservedAttempt: 2,
	}
}

// ciFailedPR is a pull request whose settled current-head CI failed in the
// diagnosed job and its quality gate, both in workflow run 101.
func ciFailedPR(executionID string) *github.PR {
	return &github.PR{
		Number: ciTestPR, State: "open", HeadRefName: gateTestBranch, HeadRefOid: ciTestHead, Body: "Closes #42",
		StatusCheckRollup: "failure", MergeStateStatus: "CLEAN", CIExecutionID: executionID,
		CIFailedWorkflowRunID: ciTestRun,
		CIFailedWorkflowRunFailedChecks: []github.CIFailedCheck{
			{Name: ciTestJob, DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/11"},
			{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/15"},
		},
		CIFailedWorkflowRunCheckNames: []string{"build-macos", ciTestJob, "quality-gate"},
	}
}

type ciRecoveryFixture struct {
	t       *testing.T
	now     time.Time
	log     *spyEventLog
	client  *fakeRerunClient
	plain   *fakeGitHubClient // set to run without the rerun capability
	status  *fakeGitHubStatus
	runID   string
	lookups int
	// exhausted makes the observer's relaunch allowance used up, the state in
	// which a CI repair stalls.
	exhausted bool
	// head is the worktree's head; reviewCommand selects delegated review.
	head          string
	reviewCommand string
	persisted     bool
}

func newCIRecoveryFixture(t *testing.T) *ciRecoveryFixture {
	f := &ciRecoveryFixture{
		t: t, now: time.Unix(1_800_000_000, 0).UTC(), log: &spyEventLog{},
		status: &fakeGitHubStatus{report: github.StatusReport{Indicator: "none"}},
		runID:  "261030010000-ci-42", exhausted: true, head: ciTestHead,
	}
	f.client = &fakeRerunClient{fakeGitHubClient: fakeGitHubClient{
		findPRHook: func() { f.lookups++ },
		prs:        map[string]*github.PR{gateTestBranch: ciFailedPR("exec-1")},
	}}
	_ = f.log.Log(events.Event{Type: "run.started", RunID: f.runID, Issue: 42, Timestamp: f.now.Add(-2 * time.Hour), Payload: map[string]any{"branch": gateTestBranch}})
	f.record(ciTestReceipt())
	return f
}

// start records the Run's admission in a fresh event log.
func (f *ciRecoveryFixture) start() {
	_ = f.log.Log(events.Event{Type: "run.started", RunID: f.runID, Issue: 42, Timestamp: f.now.Add(-2 * time.Hour), Payload: map[string]any{"branch": gateTestBranch}})
}

// replaceReceipt swaps the recorded receipt for one the arrangement changed.
func (f *ciRecoveryFixture) replaceReceipt(change func(*ciRerunActionEvidence)) {
	receipt := ciTestReceipt()
	change(&receipt)
	f.log = &spyEventLog{}
	f.start()
	f.record(receipt)
}

// record persists a rerun receipt the way the lifecycle does: in a wait.
func (f *ciRecoveryFixture) record(receipt ciRerunActionEvidence) {
	_ = f.log.Log(events.Event{Type: "run.await", RunID: f.runID, Issue: 42, Timestamp: f.now.Add(-time.Hour), Payload: map[string]any{
		"await": true, "gate": "pending", "await_reason": "pending", "branch": gateTestBranch, ciRerunActionKey: receipt.payload(),
	}})
}

func (f *ciRecoveryFixture) pr() *github.PR { return f.client.prs[gateTestBranch] }

// checker is the injected status checker; nil means no checker at all.
func (f *ciRecoveryFixture) checker() github.StatusChecker {
	if f.status == nil {
		return nil
	}
	return f.status
}

func (f *ciRecoveryFixture) session() *runSession {
	var client github.Client = f.client
	if f.plain != nil {
		client = f.plain
	}
	s := &runSession{
		issueNumber: 42, issueState: "open", runID: f.runID,
		deps: runDeps{githubClient: client, eventLog: f.log, errorLog: io.Discard, githubStatus: f.checker()},
		opts: runSessionOptions{
			now:         func() time.Time { return f.now },
			currentHead: func(string) (string, error) { return f.head, nil },
		},
	}
	s.renderCfg.ReviewCommand = f.reviewCommand
	if f.exhausted {
		s.resumeCount = s.resumeCapFor()
	}
	return s
}

// observe runs one observation after an agent attempt and persists a resulting
// wait, as both the row driver and the foreground observer do.
func (f *ciRecoveryFixture) observe(ctx context.Context) lifecycleVerdict {
	f.t.Helper()
	s := f.session()
	verdict := newLifecycleObserver(s).Observe(ctx, lifecycleObservationRequest{
		WorkDir: f.t.TempDir(), Branch: gateTestBranch, RunID: f.runID, HostPathsReady: true,
		AttemptStatus: "success", AfterAgentAttempt: true,
	})
	if verdict.Action == lifecycleAwait {
		s.emitAwait(ctx, f.runID, AgentRunResult{Branch: gateTestBranch}, verdict.Evidence)
	}
	return verdict
}

func ciRecoveryOf(t *testing.T, verdict lifecycleVerdict) ciRecovery {
	t.Helper()
	if verdict.Action != lifecycleAwait || verdict.Evidence["gate"] != gateCIRecovery || verdict.Evidence["reason"] != ciRecoveryReason ||
		verdict.Evidence["await_reason"] != gateCIRecovery {
		t.Fatalf("verdict = %s/%v, want a ci-recovery wait", verdict.status(), verdict.Evidence)
	}
	recovery, ok := ciRecoveryFromEvidence(verdict.Evidence)
	if !ok {
		t.Fatalf("ci-recovery wait carries no readable window: %v", verdict.Evidence)
	}
	return recovery
}

// Tracer bullet. A CI repair whose relaunch allowance is used up, on a failure
// the implementor diagnosed as transient and whose single rerun failed the same
// way, waits in a CI recovery window instead of parking in a hold.
func TestCIRecovery_QualifyingStallEntersTheWindow(t *testing.T) {
	f := newCIRecoveryFixture(t)
	recovery := ciRecoveryOf(t, f.observe(t.Context()))

	if !recovery.WindowStarted.Equal(f.now) || !recovery.NextAttempt.Equal(f.now) {
		t.Fatalf("window = %+v, want it to start now with its first attempt due at once", recovery)
	}
	if want := f.now.Add(24 * time.Hour); !recovery.bound().Equal(want) {
		t.Fatalf("bound = %v, want 24 hours from the start (%v)", recovery.bound(), want)
	}
	if recovery.Target != ciTestReceipt() {
		t.Fatalf("target = %+v, want the diagnosed rerun receipt %+v", recovery.Target, ciTestReceipt())
	}
	if len(f.client.requests) != 0 {
		t.Fatalf("entering the window reran %d times, want the first attempt at the next observation", len(f.client.requests))
	}
}

// Only failures backed by typed transient rerun evidence that matches the
// failed execution qualify. Every other stalled CI repair keeps the outcome the
// continuity table gives it.
func TestCIRecovery_OnlyMatchingTransientEvidenceQualifies(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(f *ciRecoveryFixture)
	}{
		"no receipt was ever recorded": {arrange: func(f *ciRecoveryFixture) { f.log = &spyEventLog{}; f.start() }},
		"receipt bound to another head": {arrange: func(f *ciRecoveryFixture) {
			f.replaceReceipt(func(r *ciRerunActionEvidence) { r.SourceHeadSHA = "older-head" })
		}},
		"receipt bound to another workflow run": {arrange: func(f *ciRecoveryFixture) {
			f.replaceReceipt(func(r *ciRerunActionEvidence) { r.RunID = "999" })
		}},
		"receipt bound to another pull request": {arrange: func(f *ciRecoveryFixture) {
			f.replaceReceipt(func(r *ciRerunActionEvidence) { r.PullRequest = 77 })
		}},
		"receipt bound to another repository": {arrange: func(f *ciRecoveryFixture) {
			f.replaceReceipt(func(r *ciRerunActionEvidence) { r.Repository = "other/repo" })
		}},
		"receipt diagnosing another job": {arrange: func(f *ciRecoveryFixture) {
			f.replaceReceipt(func(r *ciRerunActionEvidence) { r.JobName = "source-tests" })
		}},
		"a deterministic diagnosis": {arrange: func(f *ciRecoveryFixture) {
			f.replaceReceipt(func(r *ciRerunActionEvidence) { r.Diagnosis = "deterministic-failure" })
		}},
		"a refused rerun": {arrange: func(f *ciRecoveryFixture) {
			f.replaceReceipt(func(r *ciRerunActionEvidence) {
				r.State, r.Reason, r.ObservedAttempt = "refused", "rerun-authorization-denied", 1
			})
		}},
		"another check failed beside the diagnosed job": {arrange: func(f *ciRecoveryFixture) {
			pr := f.pr()
			pr.CIFailedWorkflowRunFailedChecks = append(pr.CIFailedWorkflowRunFailedChecks,
				github.CIFailedCheck{Name: "source-tests", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/16"})
			pr.CIFailedWorkflowRunCheckNames = append(pr.CIFailedWorkflowRunCheckNames, "source-tests")
		}},
		"the failure cannot be pinned to one workflow run": {arrange: func(f *ciRecoveryFixture) {
			f.pr().CIFailedWorkflowRunID, f.pr().CIFailedWorkflowRunFailedChecks, f.pr().CIFailedWorkflowRunCheckNames = "", nil, nil
		}},
		"the client cannot rerun workflow jobs": {arrange: func(f *ciRecoveryFixture) {
			f.plain = &fakeGitHubClient{findPRHook: func() { f.lookups++ }, prs: f.client.prs}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCIRecoveryFixture(t)
			tc.arrange(f)
			verdict := f.observe(t.Context())
			if verdict.Evidence["gate"] == gateCIRecovery || verdict.Evidence["reason"] == ciRecoveryReason {
				t.Fatalf("verdict = %s/%v, want the ordinary outcome of a stalled CI repair", verdict.status(), verdict.Evidence)
			}
			if !verdict.handled() {
				t.Fatalf("verdict = %s, want the existing CI repair outcome", verdict.status())
			}
			if len(f.client.requests) != 0 {
				t.Fatalf("reran %d times without qualifying evidence", len(f.client.requests))
			}
		})
	}
}

// A used-up allowance is not the only way CI repair stalls: a relaunch that
// would repeat a signature the Run's wait already recorded stalls too.
func TestCIRecovery_ARepeatedResumeSignatureQualifies(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.exhausted = false
	// The first observation relaunches the implementor and records the
	// signature the hold would remember.
	first := newLifecycleObserver(f.session()).Observe(t.Context(), lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: f.runID, HostPathsReady: true, AttemptStatus: "success", AfterAgentAttempt: true,
	})
	if first.Action != lifecycleResume || first.Evidence["gate"] != "ci-failure" {
		t.Fatalf("first observation = %s/%v, want the CI repair relaunch", first.status(), first.Evidence)
	}
	signature := resumeSignature(map[string]any{"gate": "ci-failure", "reason": "CI_FAILURE", "ci_execution_id": "exec-1"}, ciTestHead)
	_ = f.log.Log(events.Event{Type: "run.await", RunID: f.runID, Issue: 42, Timestamp: f.now.Add(-time.Minute), Payload: map[string]any{
		"await": true, "gate": "ci-failure", "await_reason": "ci-failure", "branch": gateTestBranch, stalledResumesKey: []any{signature},
	}})
	ciRecoveryOf(t, f.observe(t.Context()))
}

// `--continue` is a new RunID with no receipt of its own: the predecessor's
// receipt, found through run.continued, still qualifies, and the window is a
// fresh one that starts now.
func TestCIRecovery_ContinuedRunFindsItsPredecessorsReceipt(t *testing.T) {
	f := newCIRecoveryFixture(t)
	previous := f.runID
	_ = f.log.Log(events.Event{Type: "run.finished", RunID: previous, Issue: 42, Timestamp: f.now.Add(-time.Minute), Payload: map[string]any{"status": "failure", "reason": ciRecoveryExhaustedReason}})
	f.now = f.now.Add(48 * time.Hour)
	f.runID = "261101010000-ci-42"
	_ = f.log.Log(events.Event{Type: "run.continued", RunID: f.runID, Issue: 42, Timestamp: f.now, Payload: map[string]any{"branch": gateTestBranch, "previous_run_id": previous}})
	// The new RunID starts with a fresh allowance, so its window opens at the
	// first stall of its own relaunches; the fixture models that stall.
	recovery := ciRecoveryOf(t, f.observe(t.Context()))
	if !recovery.WindowStarted.Equal(f.now) || !recovery.NextAttempt.Equal(f.now) {
		t.Fatalf("continued window = %+v, want a fresh window whose first attempt is due at once", recovery)
	}
	attempted := ciRecoveryOf(t, f.poll(t.Context()))
	if len(f.client.requests) != 1 || attempted.LookupAttempts != 1 || !attempted.WindowStarted.Equal(f.now) {
		t.Fatalf("reruns=%d window=%+v, want the next observation to attempt at once in the fresh window", len(f.client.requests), attempted)
	}
}

// poll is one observation of the awaiting Run, as the row driver makes it every
// interval: not an agent attempt. A resulting wait is persisted only when its
// recovery state changed.
func (f *ciRecoveryFixture) poll(ctx context.Context) lifecycleVerdict {
	f.t.Helper()
	s := f.session()
	verdict := newLifecycleObserver(s).Observe(ctx, lifecycleObservationRequest{
		WorkDir: f.t.TempDir(), Branch: gateTestBranch, RunID: f.runID, HostPathsReady: true, AttemptStatus: "success",
	})
	if verdict.Action == lifecycleAwait {
		e := &runExecutor{deps: s.deps}
		row := RowSpec{RunID: f.runID, IssueNumber: 42, BaseBranch: "main", Branches: map[int]string{42: gateTestBranch}}
		if err := e.persistObservedAwait(ctx, row, verdict.Evidence); err != nil {
			f.t.Fatal(err)
		}
	}
	return verdict
}

// open enters the window and returns it.
func (f *ciRecoveryFixture) open() ciRecovery {
	f.t.Helper()
	return ciRecoveryOf(f.t, f.observe(f.t.Context()))
}

func (f *ciRecoveryFixture) awaits() int { return countEventsByType(f.log.snapshot(), "run.await") }

// Observation before the next attempt is due makes no GitHub call, no status
// check and no rerun, and returns the identical window, so it is not recorded
// again. After an attempt the next one is an hour away and nothing runs
// between.
func TestCIRecovery_NothingRunsBetweenAttempts(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.open()
	first := ciRecoveryOf(t, f.poll(t.Context())) // the first attempt, due at once
	if len(f.client.requests) != 1 || f.status.calls != 1 {
		t.Fatalf("first attempt: reruns=%d status checks=%d, want one of each", len(f.client.requests), f.status.calls)
	}
	if !first.NextAttempt.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("next attempt = %v, want an hour after the attempt", first.NextAttempt)
	}
	lookups, awaits := f.lookups, f.awaits()
	for _, wait := range []time.Duration{time.Minute, 30 * time.Minute, 59*time.Minute + 59*time.Second} {
		f.now = f.now.Add(wait)
		again := ciRecoveryOf(t, f.poll(t.Context()))
		if again != first {
			t.Fatalf("after %v the window = %+v, want it unchanged %+v", wait, again, first)
		}
		f.now = f.now.Add(-wait)
	}
	f.now = f.now.Add(59 * time.Minute)
	f.poll(t.Context())
	if f.lookups != lookups || f.status.calls != 1 || len(f.client.requests) != 1 || f.awaits() != awaits {
		t.Fatalf("between attempts: lookups %d->%d status=%d reruns=%d awaits %d->%d, want nothing to run or be recorded",
			lookups, f.lookups, f.status.calls, len(f.client.requests), awaits, f.awaits())
	}
}

// An attempt checks the Actions health, looks the pull request up, and reruns
// the diagnosed job of the identified workflow run through the client seam,
// then records the check and the rerun outcome.
func TestCIRecovery_AttemptRerunsTheDiagnosedJobAndRecordsIt(t *testing.T) {
	f := newCIRecoveryFixture(t)
	opened := f.open()
	f.status.report = github.StatusReport{Indicator: "none"}
	recovery := ciRecoveryOf(t, f.poll(t.Context()))

	want := github.WorkflowRerunRequest{
		Repository: ciTestRepo, PullRequest: ciTestPR, HeadSHA: ciTestHead, TestedSHA: ciTestTested, RunID: ciTestRun, JobName: ciTestJob,
	}
	if len(f.client.requests) != 1 || f.client.requests[0] != want {
		t.Fatalf("rerun requests = %+v, want one for %+v", f.client.requests, want)
	}
	if recovery.LookupAttempts != 1 || recovery.StatusChecks != 1 || recovery.LastStatus != "operational" ||
		recovery.Rerun.State != "confirmed" || recovery.Rerun.ObservedAttempt != 3 || !recovery.Rerun.At.Equal(f.now) {
		t.Fatalf("recovery = %+v, want one confirmed attempt recorded after one operational status check", recovery)
	}
	if !recovery.WindowStarted.Equal(opened.WindowStarted) || !recovery.NextAttempt.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("recovery = %+v, want the same window and the next attempt an hour later", recovery)
	}
}

// A reported Actions outage (or a major incident) skips the rerun, makes no
// GitHub call, and schedules the next hour. The check is recorded.
func TestCIRecovery_OutageSkipsTheRerun(t *testing.T) {
	f := newCIRecoveryFixture(t)
	opened := f.open()
	f.status.report = github.StatusReport{Indicator: "major", Outage: true, Description: "Partial System Outage", Components: []string{"Actions: partial_outage"}}
	lookups := f.lookups
	recovery := ciRecoveryOf(t, f.poll(t.Context()))
	if f.lookups != lookups || len(f.client.requests) != 0 || f.status.calls != 1 {
		t.Fatalf("outage: lookups %d->%d reruns=%d status=%d, want one status check and nothing else", lookups, f.lookups, len(f.client.requests), f.status.calls)
	}
	if recovery.LastStatus != "outage" || recovery.LookupAttempts != 0 || !recovery.WindowStarted.Equal(opened.WindowStarted) ||
		!recovery.NextAttempt.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("recovery = %+v, want an outage record in the same window and the next attempt in an hour", recovery)
	}
	if got := f.status.scopes; len(got) != 1 || len(got[0]) != 1 || got[0][0] != github.ComponentActions {
		t.Fatalf("status scopes = %v, want the Actions component only", got)
	}
}

// An unreachable (or absent) status page is unknown health: the rerun runs.
func TestCIRecovery_UnreachableStatusStillReruns(t *testing.T) {
	for name, arrange := range map[string]func(*ciRecoveryFixture){
		"status error": func(f *ciRecoveryFixture) { f.status.err = errors.New("dial tcp: i/o timeout") },
		"no checker":   func(f *ciRecoveryFixture) { f.status = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newCIRecoveryFixture(t)
			f.open()
			arrange(f)
			recovery := ciRecoveryOf(t, f.poll(t.Context()))
			if len(f.client.requests) != 1 || recovery.LastStatus != "unreachable" || recovery.LookupAttempts != 1 {
				t.Fatalf("reruns=%d recovery=%+v, want the rerun attempted on unknown health", len(f.client.requests), recovery)
			}
		})
	}
}

// attempt advances to the next attempt and observes it.
func (f *ciRecoveryFixture) attempt() lifecycleVerdict {
	f.t.Helper()
	f.now = f.now.Add(time.Hour)
	return f.poll(f.t.Context())
}

// settle opens the window and runs its first attempt, whose rerun failed to
// fix anything: it returns the window after that attempt.
func (f *ciRecoveryFixture) settle() ciRecovery {
	f.t.Helper()
	f.open()
	return ciRecoveryOf(f.t, f.poll(f.t.Context()))
}

// A green rerun ends the window in the very observation that sees it: the
// normal lifecycle decides, here by requesting the delegated review.
func TestCIRecovery_AGreenRerunResumesTheNormalLifecycle(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.reviewCommand = "/sandman review"
	f.settle()
	f.client.prs[gateTestBranch] = &github.PR{
		Number: ciTestPR, State: "open", HeadRefName: gateTestBranch, HeadRefOid: ciTestHead, Body: "Closes #42",
		StatusCheckRollup: "success", MergeStateStatus: "CLEAN", CIExecutionID: "exec-2",
	}
	verdict := f.attempt()
	if verdict.Evidence["gate"] == gateCIRecovery || verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "REVIEW_REQUEST_REQUIRED" {
		t.Fatalf("verdict = %s/%v, want the review-request delivery of the normal lifecycle", verdict.status(), verdict.Evidence)
	}
	if len(f.client.requests) != 1 {
		t.Fatalf("reruns = %d, want only the first attempt's", len(f.client.requests))
	}
}

// The diagnosed job failing again keeps the window open and reruns it, even
// though the failed execution is a new one; the window keeps its start.
func TestCIRecovery_TheSameJobFailingAgainKeepsTheWindow(t *testing.T) {
	f := newCIRecoveryFixture(t)
	first := f.settle()
	// The rerun's execution failed the same way, in new job executions: a rerun
	// mints new job IDs and links, so only the names identify the diagnosed job.
	f.pr().CIExecutionID = "exec-2"
	f.pr().CIFailedWorkflowRunFailedChecks = []github.CIFailedCheck{
		{Name: ciTestJob, DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/31"},
		{Name: "quality-gate", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/35"},
	}
	verdict := f.attempt()
	recovery := ciRecoveryOf(t, verdict)
	if len(f.client.requests) != 2 || recovery.LookupAttempts != 2 || !recovery.WindowStarted.Equal(first.WindowStarted) {
		t.Fatalf("reruns=%d recovery=%+v, want a second rerun in the same window", len(f.client.requests), recovery)
	}
	if !recovery.NextAttempt.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("next attempt = %v, want an hour after this one", recovery.NextAttempt)
	}
}

// A new head is new work for the implementor: the window ends and CI repair
// resumes under the ordinary rules.
func TestCIRecovery_ANewHeadEndsTheWindow(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.settle()
	f.exhausted = false
	f.head = "new-head"
	f.pr().HeadRefOid = "new-head"
	verdict := f.attempt()
	if verdict.Evidence["gate"] == gateCIRecovery || verdict.Action != lifecycleResume || verdict.Evidence["gate"] != "ci-failure" {
		t.Fatalf("verdict = %s/%v, want implementor CI repair on the new head", verdict.status(), verdict.Evidence)
	}
	if len(f.client.requests) != 1 {
		t.Fatalf("reruns = %d, want none for the new head", len(f.client.requests))
	}
}

// A different failing check is not the diagnosed failure either.
func TestCIRecovery_ADifferentFailingCheckEndsTheWindow(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.settle()
	f.exhausted = false
	pr := f.pr()
	pr.CIFailedWorkflowRunFailedChecks = append(pr.CIFailedWorkflowRunFailedChecks,
		github.CIFailedCheck{Name: "source-tests", DetailsURL: "https://github.com/owner/repo/actions/runs/101/job/16"})
	pr.CIFailedWorkflowRunCheckNames = append(pr.CIFailedWorkflowRunCheckNames, "source-tests")
	pr.CIExecutionID = "exec-2" // other check executions are other work
	verdict := f.attempt()
	if verdict.Evidence["gate"] == gateCIRecovery || verdict.Action != lifecycleResume || verdict.Evidence["gate"] != "ci-failure" {
		t.Fatalf("verdict = %s/%v, want implementor CI repair", verdict.status(), verdict.Evidence)
	}
	if len(f.client.requests) != 1 {
		t.Fatalf("reruns = %d, want none for another failing check", len(f.client.requests))
	}
}

// A rerun that is still running leaves nothing to rerun: the window waits for
// the next hour without submitting another.
func TestCIRecovery_AStillRunningRerunWaitsWithoutAnotherRerun(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.settle()
	f.client.prs[gateTestBranch] = &github.PR{
		Number: ciTestPR, State: "open", HeadRefName: gateTestBranch, HeadRefOid: ciTestHead, Body: "Closes #42",
		StatusCheckRollup: "pending", CIHasActiveChecks: true, MergeStateStatus: "BLOCKED", CIExecutionID: "exec-2",
	}
	recovery := ciRecoveryOf(t, f.attempt())
	if len(f.client.requests) != 1 || recovery.Rerun.State != "waiting" || recovery.Rerun.Reason != "ci-still-running" ||
		!recovery.NextAttempt.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("reruns=%d recovery=%+v, want a waiting record and the next attempt in an hour", len(f.client.requests), recovery)
	}
}

// A rerun GitHub refuses or cannot confirm is recorded and retried at the next
// attempt, never submitted twice within one.
func TestCIRecovery_AFailedRerunIsRecordedAndRetriedNextHour(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.client.results = []github.WorkflowRerunResult{{State: github.WorkflowRerunUncertain, Reason: "rerun-submission-not-confirmed", ObservedAttempt: 2}}
	f.open()
	recovery := ciRecoveryOf(t, f.poll(t.Context()))
	if len(f.client.requests) != 1 || recovery.Rerun.State != "uncertain" || recovery.LookupError != "rerun-submission-not-confirmed" || recovery.LookupAttempts != 1 {
		t.Fatalf("reruns=%d recovery=%+v, want the unconfirmed submission recorded as a failed attempt", len(f.client.requests), recovery)
	}
	next := ciRecoveryOf(t, f.attempt())
	if len(f.client.requests) != 2 || next.Rerun.State != "confirmed" || next.LookupError != "" {
		t.Fatalf("reruns=%d recovery=%+v, want the next hour's attempt to rerun again", len(f.client.requests), next)
	}
}

// A lookup that fails during an attempt is a failed attempt of the window
// itself: it keeps the window and its bound instead of handing the Run to
// another wait.
func TestCIRecovery_AFailedLookupIsAFailedAttemptOfTheWindow(t *testing.T) {
	f := newCIRecoveryFixture(t)
	first := f.settle()
	f.client.findPRErr = errors.New("gh pr list: exit status 1")
	recovery := ciRecoveryOf(t, f.attempt())
	if recovery.LookupError != "lookup-failed" || recovery.LookupAttempts != 2 || !recovery.WindowStarted.Equal(first.WindowStarted) ||
		len(f.client.requests) != 1 {
		t.Fatalf("recovery=%+v reruns=%d, want a recorded lookup failure in the same window", recovery, len(f.client.requests))
	}
}

// Only 24 hours from the window's start end the Run, skipped hours included,
// and the Run does not rerun at the bound.
func TestCIRecovery_TwentyFourHoursEndTheRun(t *testing.T) {
	f := newCIRecoveryFixture(t)
	opened := f.open()
	f.poll(t.Context())
	for hour := 1; hour < 24; hour++ {
		f.status.report = github.StatusReport{Indicator: "none", Outage: hour%2 == 0}
		ciRecoveryOf(t, f.attempt())
	}
	reruns := len(f.client.requests)
	f.status.report = github.StatusReport{Indicator: "none"}
	verdict := f.attempt()
	if verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != ciRecoveryExhaustedReason {
		t.Fatalf("after 24 hours = %s/%v, want CI_RECOVERY_EXHAUSTED", verdict.status(), verdict.Evidence)
	}
	if _, ok := verdict.Evidence["gate"]; ok {
		t.Fatalf("terminal failure carries a wait gate: %v", verdict.Evidence)
	}
	if _, ok := verdict.Evidence["await"]; ok {
		t.Fatalf("terminal failure carries a wait marker: %v", verdict.Evidence)
	}
	final, ok := ciRecoveryFromEvidence(verdict.Evidence)
	if !ok || !final.WindowStarted.Equal(opened.WindowStarted) || final.Target != ciTestReceipt() {
		t.Fatalf("terminal evidence = %v, want the window it carries", verdict.Evidence)
	}
	if len(f.client.requests) != reruns {
		t.Fatalf("reruns %d->%d, want no rerun at the bound", reruns, len(f.client.requests))
	}
	if lifecycleFailureRecoverable(ciRecoveryExhaustedReason) || !lifecycleFailureRecoverable(ciRecoveryReason) {
		t.Fatal("CI_RECOVERY_EXHAUSTED must be permanent and CI_RECOVERY a wait")
	}
}

// A green result still counts at the bound; only a failure that is still the
// diagnosed one ends the Run.
func TestCIRecovery_GreenAtTheBoundStillCounts(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.reviewCommand = "/sandman review"
	f.settle()
	f.now = f.now.Add(24*time.Hour - time.Hour)
	f.client.prs[gateTestBranch] = &github.PR{
		Number: ciTestPR, State: "open", HeadRefName: gateTestBranch, HeadRefOid: ciTestHead, Body: "Closes #42",
		StatusCheckRollup: "success", MergeStateStatus: "CLEAN",
	}
	verdict := f.attempt()
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "REVIEW_REQUEST_REQUIRED" {
		t.Fatalf("green at the bound = %s/%v, want the normal lifecycle", verdict.status(), verdict.Evidence)
	}
}

// Explicit abort ends the wait at once with no lookup and no rerun.
func TestCIRecovery_AbortEndsTheWaitWithoutARerun(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.open()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	lookups := f.lookups
	if verdict := f.poll(ctx); verdict.Action != lifecycleAborted {
		t.Fatalf("aborted wait = %s, want aborted", verdict.status())
	}
	if len(f.client.requests) != 0 || f.lookups != lookups || f.status.calls != 0 {
		t.Fatalf("an aborted wait made calls: reruns=%d lookups %d->%d status=%d", len(f.client.requests), lookups, f.lookups, f.status.calls)
	}
}

// An unreadable event log hides the window and the receipt. The Run fails
// closed into a wait without opening a window and submits no rerun.
func TestCIRecovery_UnreadableEventLogWaitsWithoutAWindowOrARerun(t *testing.T) {
	f := newCIRecoveryFixture(t)
	s := f.session()
	s.deps.eventLog = unreadableEventLog{}
	verdict := newLifecycleObserver(s).Observe(t.Context(), lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: f.runID, HostPathsReady: true, AttemptStatus: "success", AfterAgentAttempt: true,
	})
	if verdict.Action != lifecycleAwait || verdict.Evidence["gate"] == gateCIRecovery {
		t.Fatalf("verdict = %s/%v, want a wait that is not a CI recovery window", verdict.status(), verdict.Evidence)
	}
	if _, ok := verdict.Evidence[ciRecoveryKey]; ok || len(f.client.requests) != 0 {
		t.Fatalf("an unreadable log opened a window or reran: %v reruns=%d", verdict.Evidence, len(f.client.requests))
	}
}

// Restart rehydrates the window from the event fold, with its start, bound and
// next attempt, and a fresh process keeps waiting rather than restarting it.
func TestCIRecovery_RestartRehydratesTheWindow(t *testing.T) {
	f := newCIRecoveryFixture(t)
	first := f.settle()
	f.now = f.now.Add(30 * time.Minute)
	restarted := f.session() // a new process: nothing in memory
	verdict := newLifecycleObserver(restarted).Observe(t.Context(), lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: f.runID, HostPathsReady: true, AttemptStatus: "success",
	})
	if got := ciRecoveryOf(t, verdict); got != first {
		t.Fatalf("restarted window = %+v, want the persisted window %+v", got, first)
	}
	if next, ok := recoveryNextAttempt(verdict.Evidence); !ok || !next.Equal(first.NextAttempt) {
		t.Fatalf("next attempt = %v (ok=%v), want %v for the row driver's poll", next, ok, first.NextAttempt)
	}
}

// The scheduler treats the window as the wait's bounded operation: the 24-hour
// bound is its deadline and the next attempt its poll time, across restart
// discovery and on every lease checkpoint. Retained review or CI deadlines and
// hold markers cannot override that identity.
func TestCIRecovery_SchedulerBoundsTheWaitByTheWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	recovery := ciRecovery{
		recoveryWindow: recoveryWindow{WindowStarted: now.Add(-2 * time.Hour), NextAttempt: now.Add(40 * time.Minute)},
		Target:         ciTestReceipt(),
	}
	wantOperation := fmt.Sprintf("ci:%d", recovery.WindowStarted.Unix())
	wait := ciRecoveryAwait(map[string]any{}, gateTestBranch, recovery).Evidence
	// An unrelated expired CI deadline and a hold marker ride along.
	wait["ci_wait"] = map[string]any{
		"protocol": ciWaitProtocol, "pull_request": ciTestPR, "head_sha": ciTestHead,
		"deadline_unix_seconds": now.Add(-time.Hour).Unix(), "started_unix_seconds": now.Add(-90 * time.Minute).Unix(), "effective_timeout_seconds": 1800,
	}
	wait["continuity_hold"] = true
	wait["batch_id"], wait["base_branch"] = "batch", "main"

	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-3 * time.Hour), Payload: map[string]any{"branch": gateTestBranch, "batch_id": "batch"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now.Add(-20 * time.Minute), Payload: wait},
	}}
	states := events.ProjectRunStates(log.snapshot())
	record, _, valid := reconcileRecoveryWait(daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", LeaseExpiresAt: now.Add(5 * time.Minute)}, states[0])
	if !valid || record.OperationID != wantOperation || !record.OperationDeadline.Equal(recovery.bound()) ||
		!record.NextPollAt.Equal(recovery.NextAttempt) || !record.RecoverableAt(now) {
		t.Fatalf("recovered wait = %+v valid=%v, want the window's bounded operation and next attempt", record, valid)
	}

	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	row := RowSpec{RunID: "row", IssueNumber: 42, BatchID: "batch", BaseBranch: "main", Branches: map[int]string{42: gateTestBranch}}
	claim, err := daemon.ClaimRun(layout.SandmanDir, "row")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := newWaitOwner(layout, layout.LocateBatch("batch", ""), daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: gateTestBranch, BaseBranch: "main", OperationID: "admission"}, func() time.Time { return now }, log)
	if err != nil {
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
	if len(ready) != 1 || ready[0].Wait == nil || ready[0].Wait.OperationID != wantOperation || !ready[0].Wait.OperationDeadline.Equal(recovery.bound()) {
		t.Fatalf("checkpointed wait = %+v, want the window's operation, not a hold or a retained deadline", ready)
	}
}

// A receipt the helper answered with `waiting` (the workflow was still running,
// so nothing was submitted) is still typed transient evidence: the runtime's
// rerun is then the first real one.
func TestCIRecovery_AWaitingReceiptQualifies(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.replaceReceipt(func(r *ciRerunActionEvidence) {
		r.State, r.Reason, r.ObservedAttempt = "waiting", "workflow-still-running", 1
	})
	recovery := ciRecoveryOf(t, f.observe(t.Context()))
	if recovery.Target.State != "waiting" {
		t.Fatalf("target = %+v, want the waiting receipt", recovery.Target)
	}
}

// A new head resets the relaunch allowance round, so the window's exit does not
// leave the implementor stranded behind a used-up allowance: CI repair resumes
// on the new head even in the session that exhausted the old one.
func TestCIRecovery_ANewHeadResumesRepairEvenWithAUsedUpAllowance(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.settle()
	if !f.exhausted {
		t.Fatal("fixture must keep the allowance used up")
	}
	f.head = "new-head"
	f.pr().HeadRefOid = "new-head"
	f.now = f.now.Add(time.Hour) // the next attempt is due and sees the new head
	session := f.session()
	session.lastResumeRound = repairRound(map[string]any{"gate": "ci-failure", "head_sha": ciTestHead}, ciTestHead)
	verdict := newLifecycleObserver(session).Observe(t.Context(), lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: f.runID, HostPathsReady: true, AttemptStatus: "success",
	})
	if verdict.Action != lifecycleResume || verdict.Evidence["gate"] != "ci-failure" {
		t.Fatalf("verdict = %s/%v, want implementor CI repair on the new head", verdict.status(), verdict.Evidence)
	}
}

// Another wait of the Run can supersede the window: a hold written while the
// event log could not be read, or a GitHub outage. The window must resume with
// its original start and schedule, never restart its 24-hour bound.
func TestCIRecovery_ASupersededWindowResumesWithItsOriginalBound(t *testing.T) {
	f := newCIRecoveryFixture(t)
	first := f.settle()
	_ = f.log.Log(events.Event{Type: "run.await", RunID: f.runID, Issue: 42, Timestamp: f.now.Add(10 * time.Minute), Payload: map[string]any{
		"await": true, "gate": "ci-failure", "await_reason": "ci-failure", "reason": resumeStalledReason, "resume_stalled": true, "branch": gateTestBranch,
	}})
	f.now = f.now.Add(30 * time.Minute)
	resumed := ciRecoveryOf(t, f.observe(t.Context()))
	if !resumed.WindowStarted.Equal(first.WindowStarted) || !resumed.bound().Equal(first.bound()) ||
		resumed.LookupAttempts != first.LookupAttempts || !resumed.NextAttempt.Equal(first.NextAttempt) {
		t.Fatalf("resumed window = %+v, want the superseded window %+v", resumed, first)
	}
	if len(f.client.requests) != 1 {
		t.Fatalf("reruns = %d, want none for the resumed window before its next attempt", len(f.client.requests))
	}
	// A superseded window that is past its bound ends the Run instead of
	// restarting.
	f.now = first.bound().Add(time.Minute)
	if verdict := f.observe(t.Context()); verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != ciRecoveryExhaustedReason {
		t.Fatalf("expired superseded window = %s/%v, want CI_RECOVERY_EXHAUSTED", verdict.status(), verdict.Evidence)
	}
}

// The wait records the window's own evidence keys and a next action naming the
// next attempt and the bound.
func TestCIRecovery_WaitEvidenceNamesTheScheduleAndTheBound(t *testing.T) {
	f := newCIRecoveryFixture(t)
	f.open()
	verdict := f.poll(t.Context())
	recovery := ciRecoveryOf(t, verdict)
	raw, _ := verdict.Evidence[ciRecoveryKey].(map[string]any)
	for _, key := range []string{
		"window_started_unix_seconds", "next_attempt_unix_seconds", "bound_unix_seconds", "last_status", "last_status_check_unix_seconds",
		"attempts", "last_rerun", "target", "status_checks",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("ci_recovery evidence lacks %q: %v", key, raw)
		}
	}
	next := verdict.Evidence["next_action"].(string)
	for _, want := range []string{recovery.NextAttempt.UTC().Format(time.RFC3339), recovery.bound().UTC().Format(time.RFC3339), ciRecoveryExhaustedReason} {
		if !strings.Contains(next, want) {
			t.Errorf("next action %q does not name %q", next, want)
		}
	}
	if verdict.Evidence["await_reason"] != gateCIRecovery {
		t.Errorf("await_reason = %v, want %s", verdict.Evidence["await_reason"], gateCIRecovery)
	}
}

// The batch runner receives the bare CLI client, so the type assertion that
// finds the rerun capability keeps working in production.
func TestProductionGitHubClientCanRerunWorkflowJobs(t *testing.T) {
	var client github.Client = github.NewCLIClient()
	if _, ok := client.(github.WorkflowJobRerunner); !ok {
		t.Fatal("the production GitHub client cannot rerun workflow jobs; the CI recovery window would be inert")
	}
}
