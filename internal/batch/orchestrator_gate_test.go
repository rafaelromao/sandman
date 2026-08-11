package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const gateTestBranch = "42-fix-bug"

func gateTestRunOptions() runSessionOptions {
	return runSessionOptions{
		currentHead:      func(string) (string, error) { return "current-sha", nil },
		gatePollInitial:  time.Millisecond,
		gatePollMaxSleep: time.Millisecond,
		gatePollBudget:   5 * time.Millisecond,
	}
}

func runCleanGateCase(t *testing.T, pr *github.PR) (AgentRunResult, []events.Event, int) {
	return runCleanGateCaseForIssue(t, "open", pr)
}

func runCleanGateCaseForIssue(t *testing.T, issueState string, pr *github.PR) (AgentRunResult, []events.Event, int) {
	t.Helper()
	if pr != nil && pr.HeadRefOid == "" {
		pr.HeadRefOid = "current-sha"
	}
	workDir := testenv.MkdirShort(t, "sm-orch-")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{
		IssueNumber: 42,
		Status:      "success",
		Branch:      gateTestBranch,
	}}}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: issueState, Title: "Fix bug"}},
		prs:    map[string]*github.PR{gateTestBranch: pr},
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(gateTestRunOptions()),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: sb}, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: gateTestBranch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return result, logs, len(factory.created)
}

func assertExternalGateTerminal(t *testing.T, logs []events.Event, gate string) {
	t.Helper()
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatalf("run.finished event not found: %v", logs)
	}
	if got := finished.Payload["status"]; got != "blocked" {
		t.Fatalf("terminal status = %v, want blocked", got)
	}
	if got, _ := finished.Payload["blocker"].(string); got != "external-gate" {
		t.Fatalf("terminal blocker = %q, want external-gate", got)
	}
	if got, _ := finished.Payload["gate"].(string); got != gate {
		t.Fatalf("terminal gate = %q, want %q", got, gate)
	}
	if got := finished.Payload["retries_total"]; got != float64(3) {
		t.Fatalf("terminal retries_total = %v, want configured ceiling 3", got)
	}
	if got := finished.Payload["retries_done"]; got != float64(0) {
		t.Fatalf("terminal retries_done = %v, want 0", got)
	}

	states := events.ProjectRunStates(logs)
	if len(states) != 1 {
		t.Fatalf("projected states = %d, want 1", len(states))
	}
	if got := states[0].Status(); got != "blocked" {
		t.Fatalf("projected status = %q, want blocked", got)
	}
	if states[0].Finished == nil {
		t.Fatal("projected finished event is nil")
	}
	if states[0].Finished.Payload["gate"] != gate {
		t.Fatalf("projected gate = %v, want %q", states[0].Finished.Payload["gate"], gate)
	}
}

func TestRunSingle_PendingCIGateDoesNotConsumeRetries(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "pending",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "BLOCKED",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "pending")
}

func TestRunSingle_PendingDelegatedReviewDoesNotConsumeRetries(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "success",
		MergeStateStatus:  "BLOCKED",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "pending")
}

func TestRunSingle_ApprovedCleanOpenPRIsReadyToMergeWithoutRetry(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "ready-to-merge")
}

func TestRunSingle_ApprovedCleanOpenPRWithoutChecksIsReadyToMerge(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:           17,
		State:            "open",
		HeadRefName:      gateTestBranch,
		ReviewDecision:   "APPROVED",
		MergeStateStatus: "CLEAN",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, gateReadyToMerge)
}

func TestRunSingle_RestoresHostPathsBeforeExternalGateHeadCheck(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{
		IssueNumber: 42,
		Status:      "success",
		Branch:      branch,
	}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:            17,
			State:             "open",
			HeadRefName:       branch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			currentHead: func(string) (string, error) {
				if !sb.restoreHostPathsCalled {
					t.Error("current-head resolver ran before host paths were restored")
				}
				return "current-sha", nil
			},
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   5 * time.Millisecond,
		}),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if !sb.restoreHostPathsCalled {
		t.Fatal("expected RestoreHostPaths before external gate check")
	}
}

type perRunGateSequenceClient struct {
	fakeGitHubClient
	responses []*github.PR
	calls     int
}

func (c *perRunGateSequenceClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	index := c.calls
	c.calls++
	if index >= len(c.responses) {
		index = len(c.responses) - 1
	}
	return c.responses[index], nil
}

func TestRunSingle_PendingGateTransitionToReadyToMergeIsTerminal(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: branch}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &perRunGateSequenceClient{
		fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}}},
		responses: []*github.PR{
			{Number: 17, State: "open", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			{Number: 17, State: "open", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"},
		},
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(gateTestRunOptions()),
	)
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if client.calls != 3 {
		t.Fatalf("PR lookups = %d, want pending lookup, ready poll, and terminal conflict check", client.calls)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	assertExternalGateTerminal(t, logs, gateReadyToMerge)
	if launches := len(factory.created); launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
}

func TestRunSingle_ClosedIssuePendingPRIsExternalGateBlocked(t *testing.T) {
	result, logs, launches := runCleanGateCaseForIssue(t, "closed", &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "pending",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "BLOCKED",
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "pending")
}

func TestRunSingle_ClosedUnmergedPRDoesNotConsumeRetries(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:      17,
		State:       "closed",
		HeadRefName: gateTestBranch,
	})

	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "unavailable")
}

func TestRunSingle_FailedExternalGateIsActionableWithoutRetry(t *testing.T) {
	tests := []struct {
		name string
		pr   *github.PR
	}{
		{
			name: "failed CI",
			pr: &github.PR{
				Number:            17,
				State:             "open",
				HeadRefName:       gateTestBranch,
				StatusCheckRollup: "failure",
			},
		},
		{
			name: "rejected review",
			pr: &github.PR{
				Number:            17,
				State:             "open",
				HeadRefName:       gateTestBranch,
				StatusCheckRollup: "success",
				ReviewDecision:    "CHANGES_REQUESTED",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, logs, launches := runCleanGateCase(t, tt.pr)
			if result.Status != "blocked" {
				t.Fatalf("status = %q, want blocked", result.Status)
			}
			if result.RetriesTotal != 1 {
				t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
			}
			if launches != 1 {
				t.Fatalf("agent launches = %d, want 1", launches)
			}
			assertExternalGateTerminal(t, logs, "failed")
		})
	}
}

type gateSequenceClient struct {
	fakeGitHubClient
	branch string
	pr     *github.PR
	calls  int32
}

func (c *gateSequenceClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	call := atomic.AddInt32(&c.calls, 1)
	if branch == c.branch && call >= 2 {
		merged := *c.pr
		merged.State = "merged"
		merged.Merged = true
		merged.Body = "Closes #42"
		return &merged, nil
	}
	return c.pr, nil
}

func TestPollPRGateStopsWhenPRMerges(t *testing.T) {
	client := &gateSequenceClient{
		branch: gateTestBranch,
		pr: &github.PR{
			Number:            17,
			State:             "open",
			HeadRefName:       gateTestBranch,
			StatusCheckRollup: "pending",
		},
	}

	got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions())
	if got != gateResolved {
		t.Fatalf("poll result = %v, want gateResolved", got)
	}
	if calls := atomic.LoadInt32(&client.calls); calls < 2 {
		t.Fatalf("PR lookups = %d, want at least 2", calls)
	}
}

func TestPollPRGateStopsWhenOpenPRGateIsReadyToMerge(t *testing.T) {
	client := &gateAvailabilityClient{responses: []*github.PR{
		{
			State:             "open",
			StatusCheckRollup: "pending",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "BLOCKED",
		},
		{
			State:             "open",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		},
	}}

	if got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions()); got != gatePollReadyToMerge {
		t.Fatalf("poll result = %v, want gatePollReadyToMerge", got)
	}
}

func TestPollPRGateContinuesPastStaleReadyState(t *testing.T) {
	client := &gateAvailabilityClient{responses: []*github.PR{
		{
			State:             "open",
			StatusCheckRollup: "pending",
			MergeStateStatus:  "BLOCKED",
		},
		{
			State:             "open",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
			HeadRefOid:        "stale-sha",
		},
		{
			State:             "open",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
			HeadRefOid:        "current-sha",
		},
	}}

	got := pollPRGateAtHead(context.Background(), client, gateTestBranch, "current-sha", gateTestRunOptions())
	if got != gatePollReadyToMerge {
		t.Fatalf("poll result = %v, want gatePollReadyToMerge", got)
	}
	if client.calls != 3 {
		t.Fatalf("PR lookups = %d, want 3 after stale ready state", client.calls)
	}
}

type gateAvailabilityClient struct {
	fakeGitHubClient
	responses []*github.PR
	lookupErr error
	calls     int
}

func (c *gateAvailabilityClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	_ = ctx
	_ = branch
	index := c.calls
	c.calls++
	if c.lookupErr != nil {
		return nil, c.lookupErr
	}
	if index < len(c.responses) {
		return c.responses[index], nil
	}
	return nil, nil
}

type staleHeadGateClient struct {
	fakeGitHubClient
	calls int
}

func (c *staleHeadGateClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	c.calls++
	return &github.PR{
		State:             "open",
		HeadRefOid:        "stale-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}, nil
}

func TestHandleExternalGateKeepsPersistentStaleApprovalPending(t *testing.T) {
	client := &staleHeadGateClient{}
	resolverCalls := 0
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) {
				resolverCalls++
				return "current-sha", nil
			},
			gatePollInitial:  time.Millisecond,
			gatePollMaxSleep: time.Millisecond,
			gatePollBudget:   5 * time.Millisecond,
		},
	}

	status, extras, handled := session.handleExternalGate(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	if !handled || status != "blocked" {
		t.Fatalf("stale gate = (%q, %#v, %t), want blocked", status, extras, handled)
	}
	if got := extras["gate"]; got != "pending" {
		t.Fatalf("stale gate reason = %v, want pending", got)
	}
	if resolverCalls != 1 {
		t.Fatalf("current-head resolver calls = %d, want 1 snapshot", resolverCalls)
	}
	if client.calls < 2 {
		t.Fatalf("PR lookups = %d, want initial lookup and polling", client.calls)
	}
}

func TestHandleExternalGateHostPathRestoreFailureRemainsPending(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		State:             "open",
		HeadRefOid:        "current-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}

	status, extras, handled := session.handleExternalGateWithHostPaths(context.Background(), t.TempDir(), gateTestBranch, "", "run-test", false)
	if !handled || status != "blocked" {
		t.Fatalf("restore failure gate = (%q, %#v, %t), want blocked", status, extras, handled)
	}
	if got := extras["gate"]; got != "pending" {
		t.Fatalf("restore failure gate reason = %v, want pending", got)
	}
}

func TestHandleExternalGateFailsClosedWhenHeadCannotBeValidated(t *testing.T) {
	for _, tt := range []struct {
		name        string
		currentHead func(string) (string, error)
		prHead      string
	}{
		{
			name: "current head resolver fails",
			currentHead: func(string) (string, error) {
				return "", context.DeadlineExceeded
			},
			prHead: "current-sha",
		},
		{
			name: "pull request head is unavailable",
			currentHead: func(string) (string, error) {
				return "current-sha", nil
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				State:             "open",
				HeadRefOid:        tt.prHead,
				StatusCheckRollup: "success",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "CLEAN",
			}}}
			session := &runSession{
				deps: runDeps{githubClient: client, errorLog: io.Discard},
				opts: runSessionOptions{
					currentHead:      tt.currentHead,
					gatePollInitial:  time.Millisecond,
					gatePollMaxSleep: time.Millisecond,
					gatePollBudget:   5 * time.Millisecond,
				},
			}

			status, extras, handled := session.handleExternalGate(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
			if !handled || status != "blocked" {
				t.Fatalf("head validation gate = (%q, %#v, %t), want blocked", status, extras, handled)
			}
			if got := extras["gate"]; got != "pending" {
				t.Fatalf("head validation gate reason = %v, want pending", got)
			}
		})
	}
}

func TestPollPRGatePreservesLookupErrors(t *testing.T) {
	client := &gateAvailabilityClient{
		lookupErr: context.DeadlineExceeded,
	}

	got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions())
	if got != gatePollUnavailable {
		t.Fatalf("poll result = %v, want gatePollUnavailable", got)
	}
}

func TestPollPRGateDetectsDisappearedPR(t *testing.T) {
	client := &gateAvailabilityClient{
		responses: []*github.PR{
			{
				State:             "open",
				StatusCheckRollup: "pending",
			},
			nil,
		},
	}

	got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions())
	if got != gatePollPRMissing {
		t.Fatalf("poll result = %v, want gatePollPRMissing", got)
	}
}

func TestPollPRGateStopsWhenPendingPRCloses(t *testing.T) {
	client := &gateAvailabilityClient{responses: []*github.PR{
		{State: "open", StatusCheckRollup: "pending"},
		{State: "closed"},
	}}

	if got := pollPRGate(context.Background(), client, gateTestBranch, gateTestRunOptions()); got != gatePollUnavailable {
		t.Fatalf("poll result = %v, want gatePollUnavailable", got)
	}
	if client.calls != 2 {
		t.Fatalf("PR lookups = %d, want 2 without exhausting the poll budget", client.calls)
	}
}

type recoveringGateClient struct {
	fakeGitHubClient
	calls int
}

func (c *recoveringGateClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	c.calls++
	if c.calls == 1 {
		return nil, context.DeadlineExceeded
	}
	return &github.PR{State: "open", StatusCheckRollup: "pending"}, nil
}

func TestHandleExternalGateInitialLookupErrorRecoversToPending(t *testing.T) {
	client := &recoveringGateClient{}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}

	status, extras, handled := session.handleExternalGate(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	if !handled || status != "blocked" {
		t.Fatalf("recovered gate = (%q, %#v, %t), want blocked", status, extras, handled)
	}
	if got := extras["gate"]; got != "pending" {
		t.Fatalf("recovered gate reason = %v, want pending", got)
	}
	if client.calls < 2 {
		t.Fatalf("PR lookups = %d, want recovery polling", client.calls)
	}
}

func TestCheckPRExternalGateRecognizesFullyGreenOpenPRAsReadyToMerge(t *testing.T) {
	for _, tt := range []struct {
		name   string
		rollup string
		review string
	}{
		{name: "approved with checks", rollup: "success", review: "APPROVED"},
		{name: "approved without checks", review: "APPROVED"},
		{name: "no required review", rollup: "success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number:            17,
				State:             "open",
				StatusCheckRollup: tt.rollup,
				ReviewDecision:    tt.review,
				MergeStateStatus:  "CLEAN",
			}}}

			got, err := checkPRExternalGate(context.Background(), client, gateTestBranch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != gateReadyToMerge {
				t.Fatalf("fully green PR gate = %q, want %s", got, gateReadyToMerge)
			}
		})
	}
}

func TestCheckPRExternalGateDefersUnansweredReviewTrigger(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "older response", CreatedAt: time.Unix(20, 0)},
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("unanswered review trigger gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateDefersUnansweredTriggerAboveOlderApproval(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("unanswered trigger above approval gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateUsesSliceOrderForMissingTriggerTimestamp(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "older response", CreatedAt: time.Unix(20, 0)},
			{Body: "/review please"},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("missing-timestamp trigger gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateMatchesConfiguredTriggerAnywhereInComment(t *testing.T) {
	for _, body := range []string{
		"@sandman-bot /sandman review please",
		"hi\n/sandman review please",
	} {
		t.Run(body, func(t *testing.T) {
			client := &fakeGitHubClient{
				prs: map[string]*github.PR{gateTestBranch: {
					Number:            17,
					State:             "open",
					HeadRefOid:        "current-sha",
					StatusCheckRollup: "success",
					MergeStateStatus:  "CLEAN",
				}},
				prComments: map[int][]github.PRComment{17: {
					{Body: body, CreatedAt: time.Unix(30, 0)},
				}},
			}

			got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != "pending" {
				t.Fatalf("configured review trigger gate = %q, want pending", got)
			}
		})
	}
}

func TestCheckPRExternalGateOnlyMatchesConfiguredReviewCommand(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "context /sandman review", CreatedAt: time.Unix(30, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/custom review")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != gateReadyToMerge {
		t.Fatalf("different review command gate = %q, want %s", got, gateReadyToMerge)
	}
}

func TestCheckPRExternalGateTreatsLaterReviewSurfaceAsTriggerResponse(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
		prReviews: map[int][]github.PRReview{17: {
			{State: "COMMENTED", CreatedAt: time.Unix(31, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("commented review trigger gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateAcceptsLaterFormalApproval(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
		prReviews: map[int][]github.PRReview{17: {
			{State: "APPROVED", CreatedAt: time.Unix(31, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != gateReadyToMerge {
		t.Fatalf("formal approval gate = %q, want %s", got, gateReadyToMerge)
	}
}

func TestCheckPRExternalGateAcceptsInformalFormalReviewApproval(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
		prReviews: map[int][]github.PRReview{17: {
			{State: "COMMENTED", Body: "LGTM", CreatedAt: time.Unix(31, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != gateReadyToMerge {
		t.Fatalf("informal formal review gate = %q, want %s", got, gateReadyToMerge)
	}
}

func TestCheckPRExternalGateKeepsNonApprovalConversationResponsePending(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
			{Body: "please fix the failing test", CreatedAt: time.Unix(31, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("non-approval conversation gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateAcceptsInformalConversationApproval(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
			{Body: "looks good", CreatedAt: time.Unix(31, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != gateReadyToMerge {
		t.Fatalf("informal conversation approval gate = %q, want %s", got, gateReadyToMerge)
	}
}

func TestCheckPRExternalGateKeepsNoReviewConversationReady(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "thanks for the update", CreatedAt: time.Unix(31, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != gateReadyToMerge {
		t.Fatalf("no-review conversation gate = %q, want %s", got, gateReadyToMerge)
	}
}

func TestCheckPRExternalGateKeepsTimestamplessInformalApprovalPending(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
			{Body: "looks good"},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("timestampless informal approval gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateRejectsNegatedInformalApproval(t *testing.T) {
	for _, body := range []string{"not approved", "unapproved", "not all good", "not looks good", "looks good, but please fix the test"} {
		t.Run(body, func(t *testing.T) {
			client := &fakeGitHubClient{
				prs: map[string]*github.PR{gateTestBranch: {
					Number:            17,
					State:             "open",
					HeadRefOid:        "current-sha",
					StatusCheckRollup: "success",
					MergeStateStatus:  "CLEAN",
				}},
				prComments: map[int][]github.PRComment{17: {
					{Body: "/review please", CreatedAt: time.Unix(30, 0)},
					{Body: body, CreatedAt: time.Unix(31, 0)},
				}},
			}

			got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != "pending" {
				t.Fatalf("negated approval gate = %q, want pending", got)
			}
		})
	}
}

func TestCheckPRExternalGateKeepsPendingFormalReviewUnanswered(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
		prReviews: map[int][]github.PRReview{17: {
			{State: "PENDING", CreatedAt: time.Unix(31, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("pending formal review gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateTreatsMissingTimestampFormalResponseAsTriggerResponse(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
		prReviews: map[int][]github.PRReview{17: {
			{State: "COMMENTED"},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("missing-timestamp formal response gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateDoesNotAcceptMissingTimestampFormalApproval(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
		prReviews: map[int][]github.PRReview{17: {
			{State: "APPROVED"},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("missing-timestamp formal approval gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateTreatsMissingTimestampInlineResponseAsTriggerResponse(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
		prReviewComments: map[int][]github.PRReviewComment{17: {
			{Body: "looks good"},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("missing-timestamp inline response gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGatePreservesRequestedChangesAfterTrigger(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		prComments: map[int][]github.PRComment{17: {
			{Body: "/review please", CreatedAt: time.Unix(30, 0)},
		}},
		prReviews: map[int][]github.PRReview{17: {
			{State: "CHANGES_REQUESTED", CreatedAt: time.Unix(31, 0)},
		}},
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "failed" {
		t.Fatalf("requested changes after review trigger gate = %q, want failed", got)
	}
}

func TestCheckPRExternalGateCommentLookupFailureCannotReady(t *testing.T) {
	client := &fakeGitHubClient{
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "CLEAN",
		}},
		listPRCommentsErr: errors.New("comments unavailable"),
	}

	got, err := checkPRExternalGateAtHeadWithReviewCommand(context.Background(), client, gateTestBranch, "current-sha", "/review please")
	if err == nil {
		t.Fatal("comment lookup failure should be returned")
	}
	if got != "unavailable" {
		t.Fatalf("comment lookup failure gate = %q, want unavailable", got)
	}
}

func TestCheckPRExternalGateDefersStaleApproval(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		HeadRefOid:        "stale-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}

	got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("stale approved PR gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateHeadFreshnessPreservesPrecedence(t *testing.T) {
	tests := []struct {
		name string
		pr   *github.PR
		want string
	}{
		{
			name: "failed CI",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "failure",
				MergeStateStatus:  "CLEAN",
			},
			want: "failed",
		},
		{
			name: "requested changes",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "CHANGES_REQUESTED",
				MergeStateStatus:  "CLEAN",
			},
			want: "failed",
		},
		{
			name: "conflicting",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "CONFLICTING",
			},
			want: "failed",
		},
		{
			name: "pending review",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "REVIEW_REQUIRED",
				MergeStateStatus:  "BLOCKED",
			},
			want: "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: tt.pr}}
			got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("gate = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleExternalGateHeadLookupFailureRemainsPending(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		State:             "open",
		HeadRefOid:        "stale-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) {
				return "", context.DeadlineExceeded
			},
		},
	}

	status, extras, handled := session.handleExternalGate(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	if !handled || status != "blocked" {
		t.Fatalf("fallback gate = (%q, %#v, %t), want blocked", status, extras, handled)
	}
	if got := extras["gate"]; got != "pending" {
		t.Fatalf("fallback gate reason = %v, want pending", got)
	}
}

func TestCheckPRExternalGateMissingHeadMetadataRemainsPending(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		State:             "open",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}

	got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("missing PR head gate = %q, want pending", got)
	}
}

func TestConfirmExternalGateRejectsMergedPRWithoutClosingReference(t *testing.T) {
	branch := gateTestBranch
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{
				prs: map[string]*github.PR{branch: {
					Number:      17,
					State:       "closed",
					Merged:      true,
					HeadRefName: branch,
				}},
			},
			errorLog: io.Discard,
		},
	}

	status, extras, handled := session.confirmExternalGate(context.Background(), t.TempDir(), branch, "", "run-test")
	if !handled {
		t.Fatal("expected merged gate to be handled")
	}
	if status != "failure" {
		t.Fatalf("status = %q, want failure", status)
	}
	if _, ok := extras["completion"]; !ok {
		t.Fatalf("completion extras = %#v, want merged closing-reference diagnostic", extras)
	}
}

func TestHandleExternalGateCancellationDoesNotPersistBlocker(t *testing.T) {
	workDir := t.TempDir()
	taskPath := filepath.Join(workDir, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("create task directory: %v", err)
	}
	if err := os.WriteFile(taskPath, []byte("# Existing task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	logPath := filepath.Join(workDir, ".sandman", "run.log")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := &runSession{
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number:            17,
				State:             "open",
				StatusCheckRollup: "pending",
			}}},
		},
		opts: gateTestRunOptions(),
	}

	status, extras, handled := session.handleExternalGate(ctx, workDir, gateTestBranch, logPath, "run-test")
	if !handled || status != "aborted" || extras != nil {
		t.Fatalf("canceled gate = (%q, %#v, %t), want (aborted, nil, true)", status, extras, handled)
	}
	if task, err := os.ReadFile(taskPath); err != nil {
		t.Fatalf("read task: %v", err)
	} else if strings.Contains(string(task), "External Gate") {
		t.Fatalf("canceled gate persisted task blocker: %q", task)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("canceled gate run log exists, stat error = %v", err)
	}
}

func TestRecordExternalGateBlockerPersistsTaskAndLog(t *testing.T) {
	workDir := t.TempDir()
	taskPath := filepath.Join(workDir, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("create task directory: %v", err)
	}
	if err := os.WriteFile(taskPath, []byte("# Existing task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	logPath := filepath.Join(workDir, ".sandman", "run.log")
	session := &runSession{deps: runDeps{errorLog: io.Discard}}

	session.recordExternalGateBlocker(workDir, logPath, "run-test", "pending")

	task, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if !strings.Contains(string(task), "# Existing task") || !strings.Contains(string(task), "## External Gate") || !strings.Contains(string(task), "Next action:") {
		t.Fatalf("task blocker record = %q, want preserved task and durable next action", task)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	if !strings.Contains(string(log), "[run-test]") || !strings.Contains(string(log), "external gate pending") || !strings.Contains(string(log), "next action:") {
		t.Fatalf("run log blocker record = %q, want failure and next action", log)
	}

	session.recordExternalGateBlocker(workDir, logPath, "run-test", "failed")
	updatedTask, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read updated task: %v", err)
	}
	if strings.Count(string(updatedTask), "## External Gate") != 1 || strings.Contains(string(updatedTask), "gate is pending") || !strings.Contains(string(updatedTask), "gate is failed") {
		t.Fatalf("updated task blocker = %q, want one current failed-gate section", updatedTask)
	}
}

func TestRecordReadyToMergeExternalGatePersistsMergeAction(t *testing.T) {
	workDir := t.TempDir()
	taskPath := filepath.Join(workDir, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("create task directory: %v", err)
	}
	if err := os.WriteFile(taskPath, []byte("# Existing task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	logPath := filepath.Join(workDir, ".sandman", "run.log")
	session := &runSession{deps: runDeps{errorLog: io.Discard}}

	session.recordExternalGateBlocker(workDir, logPath, "run-test", gateReadyToMerge)

	task, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	taskText := string(task)
	if !strings.Contains(taskText, "State: pull request external gate is ready-to-merge.") {
		t.Fatalf("task blocker = %q, want ready-to-merge state", taskText)
	}
	const nextAction = "Next action: revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate."
	if !strings.Contains(taskText, nextAction) || strings.Contains(taskText, "- Failure:") {
		t.Fatalf("task blocker = %q, want ready-specific next action without generic failure", taskText)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	logText := string(log)
	if !strings.Contains(logText, "external gate ready-to-merge: pull request is ready to merge; next action: revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate") {
		t.Fatalf("run log = %q, want ready-specific next action", logText)
	}
}
