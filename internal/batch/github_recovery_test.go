package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// An unreadable event log hides the window. The Run fails closed into a wait
// and does not open a new window that would restart the 24-hour bound.
func TestGitHubRecovery_UnreadableEventLogWaitsWithoutNewWindow(t *testing.T) {
	session := &runSession{
		issueNumber: 42, issueState: "open", runID: "row",
		deps: runDeps{githubClient: &fakeGitHubClient{findPRErr: errors.New("gh pr list: exit status 1")}, eventLog: unreadableEventLog{}, errorLog: io.Discard},
		opts: runSessionOptions{currentHead: func(string) (string, error) { return "current-sha", nil }},
	}
	verdict := newLifecycleObserver(session).Observe(t.Context(), lifecycleObservationRequest{
		WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: "row", HostPathsReady: true, AttemptStatus: "success",
	})
	if verdict.Action != lifecycleAwait || verdict.Evidence["gate"] != gateGitHubUnavailable {
		t.Fatalf("verdict = %s/%v, want a GitHub recovery wait", verdict.status(), verdict.Evidence)
	}
	if _, ok := verdict.Evidence[githubRecoveryKey]; ok {
		t.Fatalf("unreadable log opened a recovery window: %v", verdict.Evidence)
	}
}

// A typed nil status client reports unknown health instead of panicking.
func TestGitHubRecovery_NilStatusPageClientIsUnreachable(t *testing.T) {
	var client *github.StatusPageClient
	if _, err := client.GitHubStatus(t.Context()); err == nil {
		t.Fatal("nil status client reported health")
	}
}

// fakeGitHubStatus is the injected githubstatus.com health check.
type fakeGitHubStatus struct {
	report github.StatusReport
	err    error
	calls  int
}

func (f *fakeGitHubStatus) GitHubStatus(context.Context) (github.StatusReport, error) {
	f.calls++
	return f.report, f.err
}

type githubRecoveryFixture struct {
	t       *testing.T
	now     time.Time
	log     *spyEventLog
	client  *fakeGitHubClient
	status  *fakeGitHubStatus
	lookups int
	runID   string
}

func newGitHubRecoveryFixture(t *testing.T) *githubRecoveryFixture {
	f := &githubRecoveryFixture{
		t: t, now: time.Unix(1_800_000_000, 0).UTC(), log: &spyEventLog{},
		status: &fakeGitHubStatus{report: github.StatusReport{Indicator: "none"}},
		runID:  "261030010000-gh-42",
	}
	f.client = &fakeGitHubClient{
		findPRErr:  errors.New("gh pr list: exit status 1"),
		findPRHook: func() { f.lookups++ },
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 33, State: "merged", Merged: true, HeadRefName: gateTestBranch, HeadRefOid: "current-sha", Body: "Closes #42",
		}},
	}
	_ = f.log.Log(events.Event{Type: "run.started", RunID: f.runID, Issue: 42, Timestamp: f.now.Add(-time.Hour), Payload: map[string]any{"branch": gateTestBranch}})
	return f
}

func (f *githubRecoveryFixture) checker() github.StatusChecker {
	if f.status == nil {
		return nil
	}
	return f.status
}

func (f *githubRecoveryFixture) session() *runSession {
	return &runSession{
		issueNumber: 42, issueState: "open", runID: f.runID,
		deps: runDeps{githubClient: f.client, eventLog: f.log, errorLog: io.Discard, githubStatus: f.checker()},
		opts: runSessionOptions{
			now:         func() time.Time { return f.now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
}

// observe runs one runtime observation and persists a resulting wait, as
// both the row driver and the foreground observer do.
func (f *githubRecoveryFixture) observe(ctx context.Context) lifecycleVerdict {
	f.t.Helper()
	s := f.session()
	verdict := newLifecycleObserver(s).Observe(ctx, lifecycleObservationRequest{
		WorkDir: f.t.TempDir(), Branch: gateTestBranch, RunID: f.runID, HostPathsReady: true, AttemptStatus: "success",
	})
	if verdict.Action == lifecycleAwait {
		s.emitAwait(ctx, f.runID, AgentRunResult{Branch: gateTestBranch}, verdict.Evidence)
	}
	return verdict
}

func githubRecoveryOf(t *testing.T, verdict lifecycleVerdict) githubRecovery {
	t.Helper()
	if verdict.Action != lifecycleAwait || verdict.Evidence["gate"] != gateGitHubUnavailable || verdict.Evidence["reason"] != githubUnavailableReason {
		t.Fatalf("verdict = %s/%v, want a github-unavailable wait", verdict.status(), verdict.Evidence)
	}
	recovery, ok := githubRecoveryFromEvidence(verdict.Evidence)
	if !ok {
		t.Fatalf("wait carries no recovery window: %v", verdict.Evidence)
	}
	return recovery
}

// Issue #2938: a lookup failure waits for GitHub instead of failing the Run,
// with or without a prior bounded operation.
func TestGitHubRecovery_LookupFailureWaits(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	recovery := githubRecoveryOf(t, f.observe(t.Context()))
	if !recovery.WindowStarted.Equal(f.now) || !recovery.NextAttempt.Equal(f.now.Add(githubRecoveryInterval)) {
		t.Fatalf("recovery = %+v, want a window opened now with the next attempt in an hour", recovery)
	}
	if f.lookups != 1 || f.status.calls != 0 {
		t.Fatalf("lookups=%d status checks=%d, want the failed lookup only", f.lookups, f.status.calls)
	}
}

// Before the next hourly attempt, observation neither looks up the pull
// request nor checks GitHub's status, and the wait evidence is unchanged.
func TestGitHubRecovery_NoAttemptBeforeNextHour(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	first := githubRecoveryOf(t, f.observe(t.Context()))
	f.now = f.now.Add(59 * time.Minute)
	second := githubRecoveryOf(t, f.observe(t.Context()))
	if f.lookups != 1 || f.status.calls != 0 || second != first {
		t.Fatalf("lookups=%d status=%d recovery=%+v, want no attempt and the same window %+v", f.lookups, f.status.calls, second, first)
	}
}

// A status page reporting an outage skips the lookup and keeps waiting.
func TestGitHubRecovery_OutageSkipsLookup(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	first := githubRecoveryOf(t, f.observe(t.Context()))
	f.status.report = github.StatusReport{Indicator: "major", Outage: true, Description: "Partial System Outage"}
	f.now = f.now.Add(time.Hour)
	recovery := githubRecoveryOf(t, f.observe(t.Context()))
	if f.lookups != 1 || f.status.calls != 1 {
		t.Fatalf("lookups=%d status=%d, want one status check and no lookup", f.lookups, f.status.calls)
	}
	if !recovery.WindowStarted.Equal(first.WindowStarted) || !recovery.NextAttempt.Equal(f.now.Add(githubRecoveryInterval)) || recovery.LastStatus != "outage" {
		t.Fatalf("recovery = %+v, want the same window, an outage record and the next attempt in an hour", recovery)
	}
}

// A healthy status page lets the lookup run; its success resumes the normal
// lifecycle decision.
func TestGitHubRecovery_HealthyStatusAttemptsAndRecovers(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	githubRecoveryOf(t, f.observe(t.Context()))
	f.client.findPRErr = nil
	f.now = f.now.Add(time.Hour)
	verdict := f.observe(t.Context())
	if f.status.calls != 1 || f.lookups != 2 {
		t.Fatalf("lookups=%d status=%d, want a status check then a lookup", f.lookups, f.status.calls)
	}
	if verdict.Action != lifecycleSuccess {
		t.Fatalf("recovered observation = %s/%v, want the merged pull request's success", verdict.status(), verdict.Evidence)
	}
}

// An unreachable status page is unknown health: the lookup is attempted.
func TestGitHubRecovery_UnreachableStatusAttempts(t *testing.T) {
	for name, status := range map[string]*fakeGitHubStatus{
		"status error": {err: errors.New("dial tcp: i/o timeout")},
		"no checker":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newGitHubRecoveryFixture(t)
			if status == nil {
				f.status = nil
			} else {
				f.status = status
			}
			githubRecoveryOf(t, f.observe(t.Context()))
			f.now = f.now.Add(time.Hour)
			recovery := githubRecoveryOf(t, f.observe(t.Context()))
			if f.lookups != 2 || recovery.LookupAttempts != 2 || recovery.LastStatus != "unreachable" {
				t.Fatalf("lookups=%d recovery=%+v, want the lookup attempted on unknown health", f.lookups, recovery)
			}
		})
	}
}

// Only 24 hours of continuous failed recovery ends the Run, and outage hours
// count towards it.
func TestGitHubRecovery_TwentyFourHoursFail(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	githubRecoveryOf(t, f.observe(t.Context()))
	for hour := 1; hour < 24; hour++ {
		f.now = f.now.Add(time.Hour)
		f.status.report.Outage = hour%2 == 0
		githubRecoveryOf(t, f.observe(t.Context()))
	}
	f.now = f.now.Add(time.Hour)
	f.status.report.Outage = false
	verdict := f.observe(t.Context())
	if verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != lookupGateReason || verdict.Evidence["lookup_error"] != "lookup-failed" {
		t.Fatalf("after 24 hours = %s/%v, want GATE_LOOKUP_FAILED", verdict.status(), verdict.Evidence)
	}
	if _, ok := verdict.Evidence["gate"]; ok {
		t.Fatalf("terminal failure carries a wait gate: %v", verdict.Evidence)
	}
	if lifecycleFailureRecoverable(githubUnavailableReason) != true {
		t.Fatal("GITHUB_UNAVAILABLE must be classified as a recoverable wait")
	}
}

// `sandman run --continue` after GATE_LOOKUP_FAILED is a new RunID: it
// attempts observation immediately and opens a fresh 24-hour window.
func TestGitHubRecovery_ContinueStartsFreshWindow(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	githubRecoveryOf(t, f.observe(t.Context()))
	f.now = f.now.Add(25 * time.Hour)
	if verdict := f.observe(t.Context()); verdict.Action != lifecycleFailure {
		t.Fatalf("expired window = %s, want failure", verdict.status())
	}
	_ = f.log.Log(events.Event{Type: "run.finished", RunID: f.runID, Issue: 42, Timestamp: f.now, Payload: map[string]any{"status": "failure", "reason": lookupGateReason}})
	previous := f.runID
	f.runID = "261031020000-gh-42"
	_ = f.log.Log(events.Event{Type: "run.continued", RunID: f.runID, Issue: 42, Timestamp: f.now, Payload: map[string]any{"branch": gateTestBranch, "previous_run_id": previous}})
	lookups, checks := f.lookups, f.status.calls
	recovery := githubRecoveryOf(t, f.observe(t.Context()))
	if f.lookups != lookups+1 || f.status.calls != checks {
		t.Fatalf("continuation did not attempt immediately: lookups %d->%d status %d->%d", lookups, f.lookups, checks, f.status.calls)
	}
	if !recovery.WindowStarted.Equal(f.now) {
		t.Fatalf("continuation window = %+v, want a fresh window", recovery)
	}
}

// A wait that recovered and failed again opens a new window instead of
// reusing a superseded one.
func TestGitHubRecovery_RecoveredWindowIsNotReused(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	f.client.prs[gateTestBranch] = &github.PR{Number: 33, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"}
	githubRecoveryOf(t, f.observe(t.Context()))
	f.client.findPRErr = nil
	f.now = f.now.Add(20 * time.Hour)
	if verdict := f.observe(t.Context()); verdict.Evidence["gate"] == gateGitHubUnavailable || verdict.Action == lifecycleFailure {
		t.Fatalf("recovered observation = %s/%v", verdict.status(), verdict.Evidence)
	}
	f.client.findPRErr = errors.New("gh pr list: exit status 1")
	f.now = f.now.Add(10 * time.Minute)
	recovery := githubRecoveryOf(t, f.observe(t.Context()))
	if !recovery.WindowStarted.Equal(f.now) {
		t.Fatalf("new outage window = %+v, want it to start now", recovery)
	}
}

// The wait takes precedence over an established review or CI deadline: an
// expired deadline is not a relaunch while GitHub cannot be observed. The
// deadline is retained unchanged for the decision after recovery.
func TestGitHubRecovery_EstablishedDeadlineIsRetainedNotEnforced(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	deadline := f.now.Add(-time.Minute).Unix()
	_ = f.log.Log(events.Event{Type: "run.await", RunID: f.runID, Issue: 42, Timestamp: f.now.Add(-30 * time.Minute), Payload: map[string]any{
		"await_reason": "pending", "gate": "pending", "branch": gateTestBranch, "ci_wait": map[string]any{
			"protocol": ciWaitProtocol, "pull_request": 33, "head_sha": "current-sha",
			"deadline_unix_seconds": deadline, "started_unix_seconds": deadline - 1800, "effective_timeout_seconds": 1800,
		},
	}})
	verdict := f.observe(t.Context())
	githubRecoveryOf(t, verdict)
	ci, ok := verdict.Evidence["ci_wait"].(map[string]any)
	if !ok || ci["deadline_unix_seconds"] != deadline {
		t.Fatalf("established CI operation not retained unchanged: %v", verdict.Evidence)
	}
}

// A restarted scheduler recovers the wait as the window's bounded operation,
// at its next hourly attempt, even when a retained CI deadline has expired.
func TestGitHubRecovery_RestartRecoversWaitSchedule(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	recovery := githubRecovery{WindowStarted: now.Add(-2 * time.Hour), NextAttempt: now.Add(40 * time.Minute)}
	wait := githubRecoveryAwait(map[string]any{"ci_wait": map[string]any{
		"protocol": ciWaitProtocol, "pull_request": 33, "head_sha": "current-sha",
		"deadline_unix_seconds": now.Add(-time.Hour).Unix(), "started_unix_seconds": now.Add(-90 * time.Minute).Unix(), "effective_timeout_seconds": 1800,
	}}, gateTestBranch, recovery)
	states := events.ProjectRunStates([]events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-3 * time.Hour), Payload: map[string]any{"branch": gateTestBranch, "batch_id": "batch"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now.Add(-20 * time.Minute), Payload: wait.Evidence},
	})
	if len(states) != 1 {
		t.Fatalf("states = %+v", states)
	}
	record, _, valid := reconcileRecoveryWait(daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", LeaseExpiresAt: now.Add(5 * time.Minute)}, states[0])
	if !valid || record.OperationID != fmt.Sprintf("github:%d", recovery.WindowStarted.Unix()) ||
		!record.OperationDeadline.Equal(recovery.bound()) || !record.NextPollAt.Equal(recovery.NextAttempt) || !record.RecoverableAt(now) {
		t.Fatalf("recovered wait = %+v valid=%v, want the GitHub window bound and next attempt", record, valid)
	}
}

// Explicit abort ends the wait at once.
func TestGitHubRecovery_AbortEndsWait(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	githubRecoveryOf(t, f.observe(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.now = f.now.Add(time.Hour)
	if verdict := f.observe(ctx); verdict.Action != lifecycleAborted {
		t.Fatalf("aborted wait = %s, want aborted", verdict.status())
	}
}

// Restart rehydrates the window from the event fold, and persistence records
// a new wait only when the recovery state changes.
func TestGitHubRecovery_PersistenceAndRestart(t *testing.T) {
	f := newGitHubRecoveryFixture(t)
	e := &runExecutor{deps: runDeps{githubClient: f.client, eventLog: f.log, errorLog: io.Discard, githubStatus: f.status,
		runSessionOpts: runSessionOptions{now: func() time.Time { return f.now }, currentHead: func(string) (string, error) { return "current-sha", nil }}}}
	row := RowSpec{RunID: f.runID, IssueNumber: 42, BaseBranch: "main", Branches: map[int]string{42: gateTestBranch}}
	observe := func() lifecycleVerdict {
		verdict := newLifecycleObserver(newRunSession(e, row)).Observe(t.Context(), lifecycleObservationRequest{
			WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: f.runID, HostPathsReady: true, AttemptStatus: "success",
		})
		if verdict.Action == lifecycleAwait {
			if err := e.persistObservedAwait(t.Context(), row, verdict.Evidence); err != nil {
				t.Fatal(err)
			}
		}
		return verdict
	}
	awaits := func() int { return countEventsByType(f.log.snapshot(), "run.await") }
	first := githubRecoveryOf(t, observe())
	if awaits() != 1 {
		t.Fatalf("awaits = %d, want the opened window recorded", awaits())
	}
	f.now = f.now.Add(30 * time.Minute)
	githubRecoveryOf(t, observe())
	if awaits() != 1 {
		t.Fatalf("awaits = %d, want an unchanged skip not recorded", awaits())
	}
	// A restarted process reads the same window from the event log.
	f.now = f.now.Add(30 * time.Minute)
	f.status.report.Outage = true
	restarted := githubRecoveryOf(t, observe())
	if !restarted.WindowStarted.Equal(first.WindowStarted) || awaits() != 2 {
		t.Fatalf("restart recovery = %+v (awaits %d), want the preserved window and a recorded check", restarted, awaits())
	}
	if next, ok := githubRecoveryNextAttempt(map[string]any{"gate": gateGitHubUnavailable, githubRecoveryKey: restarted.payload()}); !ok || !next.Equal(restarted.NextAttempt) {
		t.Fatalf("next attempt = %v, want %v for the row driver's poll", next, restarted.NextAttempt)
	}
}
