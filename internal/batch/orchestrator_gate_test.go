package batch

import (
	"context"
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
		gatePollInitial:  time.Millisecond,
		gatePollMaxSleep: time.Millisecond,
		gatePollBudget:   5 * time.Millisecond,
	}
}

func runCleanGateCase(t *testing.T, pr *github.PR) (AgentRunResult, []events.Event, int) {
	t.Helper()
	workDir := testenv.MkdirShort(t, "gate-")
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
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
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

	states := events.ProjectRunStates(logs)
	if len(states) != 1 {
		t.Fatalf("projected states = %d, want 1", len(states))
	}
	if got := states[0].Status(); got != "blocked" {
		t.Fatalf("projected status = %q, want blocked", got)
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
