package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// The ordinary managed observation cadence is 30 seconds from the first poll.
func TestAwaitPollInterval_DefaultIsThirtySecondsFromFirstPoll(t *testing.T) {
	for poll := 0; poll < 8; poll++ {
		if got := awaitPollInterval(runSessionOptions{}, poll); got != 30*time.Second {
			t.Fatalf("default poll %d interval = %v, want 30s", poll, got)
		}
	}
}

func TestAwaitPollInterval_ExplicitOverrideStillWins(t *testing.T) {
	opts := runSessionOptions{lifecyclePollPlan: []time.Duration{7 * time.Second, 3 * time.Second}}
	for poll, want := range []time.Duration{7 * time.Second, 3 * time.Second, 3 * time.Second} {
		if got := awaitPollInterval(opts, poll); got != want {
			t.Fatalf("override poll %d interval = %v, want %v", poll, got, want)
		}
	}
}

func TestLifecyclePollIntervals_DefaultIsThirtySecondsFromFirstPoll(t *testing.T) {
	s := &runSession{opts: runSessionOptions{}}
	for name, extras := range map[string]map[string]any{
		"no evidence":           nil,
		"no retained plan":      {"review_request": map[string]any{"deadline_unix_seconds": int64(1)}},
		"unrelated ci evidence": {"ci_wait": map[string]any{"deadline_unix_seconds": int64(1)}},
	} {
		if got := s.lifecyclePollIntervals(extras); !reflect.DeepEqual(got, []time.Duration{30 * time.Second}) {
			t.Fatalf("%s: default intervals = %v, want a single 30s plan repeated for every poll", name, got)
		}
	}
}

// A registration created before the 30-second default carries an explicit
// plan. It stays valid, is still honoured by the foreground path, and a
// default or new registration never rewrites it.
func TestReviewRegistration_RetainedLegacyPollPlanStaysValidAndHonoured(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-legacy-plan-")
	registeredAt := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	triggeredAt := registeredAt.Add(-time.Minute)
	client := &registrationGitHubClient{comments: []github.PRComment{{ID: "trigger-1001", Body: "/sandman review", CreatedAt: triggeredAt}}}
	session := &runSession{
		deps:                   runDeps{githubClient: client, layout: paths.NewLayout(nil, workDir)},
		renderCfg:              prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 600},
		reviewRegistrationNow:  func() time.Time { return registeredAt },
		reviewAttemptStartedAt: triggeredAt.Add(-time.Minute),
	}
	pr := &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("register review request: %v", err)
	}
	path := session.deps.layout.PRReviewRegistrationPath(pr.Number)
	record, err := readFileReviewRegistration(path)
	if err != nil {
		t.Fatalf("read registration: %v", err)
	}
	if got := record.Request.PollPlan; !slices.Equal(got, []int{30}) {
		t.Fatalf("new registration plan = %v, want [30]", got)
	}
	legacy := []int{120, 60, 60, 30}
	record.Request.PollPlan = slices.Clone(legacy)
	record.State.PollPlan = slices.Clone(legacy)
	if err := atomicfs.WriteAtomicJSON(path, record, 0o600); err != nil {
		t.Fatalf("write legacy registration: %v", err)
	}
	read, err := readReviewRegistration(path, "owner/repo", pr, "current-sha")
	if err != nil {
		t.Fatalf("legacy plan registration rejected: %v", err)
	}
	got := session.lifecyclePollIntervals(map[string]any{"review_request": map[string]any{"poll_plan": read.Request.PollPlan}})
	want := []time.Duration{120 * time.Second, 60 * time.Second, 60 * time.Second, 30 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("retained plan intervals = %v, want %v", got, want)
	}
}

// With no override the foreground path observes after 30 seconds from the
// first poll and repeats that interval, through the production plan.
func TestRunExecutor_ForegroundLifecycleDefaultCadenceIsThirtySecondsFromFirstPoll(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := "42-fix-bug"
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: branch}}}
	pending := func() *github.PR {
		return &github.PR{Number: 17, State: "open", Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"}
	}
	client := &perRunGateSequenceClient{
		fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}}},
		responses: []*github.PR{
			pending(), // closing-reference guard
			pending(), // initial lifecycle decision
			pending(), // first observation
			pending(), // second observation
			{Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: branch, HeadRefOid: "current-sha"},
		},
	}
	var waits []time.Duration
	options := gateTestRunOptions()
	options.lifecyclePollPlan = nil // production default cadence
	options.lifecycleWait = func(_ context.Context, interval time.Duration) error {
		waits = append(waits, interval)
		return nil
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(options))
	bc := BatchConfig{
		Cfg:       &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName: "opencode", AgentCfg: config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(), Retries: 3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: branch}, BaseBranch: "main",
	})
	if !started || result.Status != "success" {
		t.Fatalf("run = (%t, %q), want started success after CI resolves merged", started, result.Status)
	}
	want := []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Second}
	if !reflect.DeepEqual(waits, want) {
		t.Fatalf("foreground waits = %v, want %v", waits, want)
	}
	if len(factory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1", len(factory.created))
	}
}

// A driver started without its own interval falls back to the same default,
// and a cancelled wait aborts the row without another observation.
func TestRowDriverDefaultPollIntervalIsTheManagedObservationInterval(t *testing.T) {
	run := RunSpec{RunID: "run-42", IssueNumber: 42, BatchID: "batch-a", Branch: "42-fix", BaseBranch: "main"}
	var waits []time.Duration
	gates := &rowDriverWaitRecorder{rowDriverGateFake: &rowDriverGateFake{}, waits: &waits}
	executor := &rowDriverExecutorFake{observations: []lifecycleVerdict{
		{Action: lifecycleAwait, Evidence: map[string]any{"gate": "ci"}},
		{Action: lifecycleAwait, Evidence: map[string]any{"gate": "ci"}},
		{Action: lifecycleFailure},
	}}
	got := newRowDriver(executor, gates, &rowDriverClockFake{}).Drive(context.Background(), run, ContinuationCursor{}, rowDriverOptions{
		Awaiting: true, ReleaseAwaitCapacity: true,
	})
	if got.Result.Status != "failure" || len(waits) != 2 || waits[0] != 30*time.Second || waits[1] != 30*time.Second {
		t.Fatalf("Drive = %+v waits = %v, want two 30s waits (one per re-observation)", got.Result, waits)
	}
	if executor.observeCalls != 3 || executor.executeCalls != 0 || gates.acquires != 0 {
		t.Fatalf("observations=%d executions=%d admissions=%d, want one observation per wait and no execution", executor.observeCalls, executor.executeCalls, gates.acquires)
	}
}

// The faster ordinary cadence is not a general polling change: provider-quota
// polls, GitHub recovery attempts and the explicit-plan contract keep their own
// schedules.
func TestManagedObservationCadenceLeavesOtherSchedulesUnchanged(t *testing.T) {
	if usageLimitPollInterval != 10*time.Minute {
		t.Fatalf("provider-quota poll interval = %v, want 10m", usageLimitPollInterval)
	}
	if githubRecoveryInterval != time.Hour {
		t.Fatalf("GitHub recovery interval = %v, want 1h", githubRecoveryInterval)
	}
	if managedObservationInterval != 30*time.Second {
		t.Fatalf("managed observation interval = %v, want 30s", managedObservationInterval)
	}
}

// A GitHub recovery wait sleeps until its hourly attempt even when the driver
// falls back to the default 30-second cadence.
func TestRowDriverGitHubRecoveryWaitKeepsHourlyScheduleUnderDefaultCadence(t *testing.T) {
	now := &rowDriverClockFake{now: time.Date(2026, time.October, 30, 3, 0, 0, 0, time.UTC)}
	recovery := githubRecovery{WindowStarted: now.now, NextAttempt: now.now.Add(githubRecoveryInterval)}
	var waits []time.Duration
	gates := &rowDriverWaitRecorder{rowDriverGateFake: &rowDriverGateFake{clock: now}, waits: &waits}
	executor := &rowDriverExecutorFake{observations: []lifecycleVerdict{githubRecoveryAwait(nil, "42-fix", recovery), {Action: lifecycleFailure}}}
	run := RunSpec{RunID: "run-42", IssueNumber: 42, BatchID: "batch-a", Branch: "42-fix", BaseBranch: "main"}
	got := newRowDriver(executor, gates, now).Drive(context.Background(), run, ContinuationCursor{}, rowDriverOptions{
		Awaiting: true, ReleaseAwaitCapacity: true,
	})
	if got.Result.Status != "failure" || len(waits) != 1 || waits[0] != githubRecoveryInterval {
		t.Fatalf("Drive = %+v waits = %v, want one sleep until the next hourly attempt", got.Result, waits)
	}
}

func TestAwaitWaitDefaultTimerIsCancellationAware(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := waitForAwaitPoll(ctx, managedObservationInterval); err == nil {
		t.Fatal("cancelled default wait returned nil")
	}
	if elapsed := time.Since(start); elapsed >= managedObservationInterval/2 {
		t.Fatalf("cancelled default wait blocked for %v", elapsed)
	}
}

// A row that yields for current-head CI and sees it finish right after its
// first observation waits exactly once, for the production default interval.
func TestBatchObservation_DefaultCadenceWaitsThirtySecondsOnceBeforeResolution(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-cadence-")
	t.Chdir(root)
	initGitRepo(t, root)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs:    map[string]*github.PR{"42-parent": {Number: 42, State: "open", HeadRefName: "42-parent", HeadRefOid: "current-sha", Body: "Closes #42", StatusCheckRollup: "pending"}},
	}}
	log := &spyEventLog{}
	var mu sync.Mutex
	var waits []time.Duration
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: waitingRunnableFunction(func(context.Context) AgentRunResult {
			return AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-parent"}
		}),
	}}
	cfg := &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}
	// No lifecyclePollPlan override: the production default cadence applies.
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunSessionOpts(runSessionOptions{
		releaseAwaitCapacity: true,
		currentHead:          func(string) (string, error) { return "current-sha", nil },
		awaitWait: func(ctx context.Context, interval time.Duration) error {
			mu.Lock()
			waits = append(waits, interval)
			mu.Unlock()
			// CI finishes and the pull request merges while the row waits.
			client.mu.Lock()
			client.issues[42] = &github.Issue{Number: 42, State: "closed"}
			client.prs["42-parent"] = mergedPR("42-parent", "Closes #42")
			client.mu.Unlock()
			return ctx.Err()
		},
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{Issues: []int{42}, Parallel: 1, RunTS: "261006090000", RunShortID: "cadence", Branches: map[int]string{42: "42-parent"}})
	if err != nil || result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		t.Fatalf("batch did not resolve after the first poll: result=%+v err=%v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 1 || waits[0] != 30*time.Second {
		t.Fatalf("external waits = %v, want exactly one 30s wait before the first re-observation", waits)
	}
}

// cadenceCIScenario drives one row through the production cadence while its
// current-head checks stay pending. The injected clock advances by exactly the
// interval the runtime asked to wait, so deadlines are judged against the real
// 30-second cadence rather than a zero-duration override.
type cadenceCIScenario struct {
	launches      int
	waits         []time.Duration
	deadlines     []int64
	executions    []string
	log           *spyEventLog
	result        *Result
	err           error
	finalDeadline int64
}

func runCadenceCIScenario(t *testing.T, polls int, mutate func(poll int, client *reviewWaitSchedulerGitHubClient)) *cadenceCIScenario {
	t.Helper()
	root := testenv.MkdirShort(t, "sm-cadence-ci-")
	t.Chdir(root)
	initGitRepo(t, root)
	worktree := filepath.Join(root, "worktrees", "42-work")
	statePath := filepath.Join(worktree, ".sandman", "state", "17.ci_wait.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\nCI is running.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 11, 0, 0, 0, time.UTC)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "closed"}},
		prs:    map[string]*github.PR{"42-work": {Number: 17, State: "open", HeadRefName: "42-work", HeadRefOid: "head", Body: "Closes #42", StatusCheckRollup: "pending", CIExecutionID: "checks-1"}},
	}}
	scenario := &cadenceCIScenario{log: &spyEventLog{}}
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		scenario.launches++
		return AgentRunResult{IssueNumber: issue.Number, Status: "success", Branch: branch}
	}}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	poll := 0
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, scenario.log, WithErrorLog(io.Discard), WithRunnableFactory(factory),
		WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			now:                  func() time.Time { return now },
			currentHead:          func(string) (string, error) { return "head", nil },
			awaitWait: func(ctx context.Context, interval time.Duration) error {
				poll++
				scenario.waits = append(scenario.waits, interval)
				now = now.Add(interval)
				if registration, err := readCIWaitRegistration(statePath); err == nil {
					scenario.deadlines = append(scenario.deadlines, registration.DeadlineUnixSeconds)
					scenario.executions = append(scenario.executions, registration.ExecutionID)
				}
				if mutate != nil {
					mutate(poll, client)
				}
				if poll >= polls {
					client.setPR("42-work", func(pr *github.PR) { pr.StatusCheckRollup, pr.State, pr.Merged = "success", "merged", true })
				}
				return ctx.Err()
			},
		}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	scenario.result, scenario.err = o.RunBatch(ctx, Request{Issues: []int{42}, Branches: map[int]string{42: "42-work"}, RunTS: "261112110000", RunShortID: "cadence-ci", Retries: 3, Parallel: 1})
	if registration, err := readCIWaitRegistration(statePath); err == nil {
		scenario.finalDeadline = registration.DeadlineUnixSeconds
	}
	return scenario
}

// Many 30-second polls of one identified execution never renew its 30-minute
// bound, never add await records, and never spend relaunch or retry accounting.
func TestBatchObservation_ThirtySecondPollsDoNotRenewCIWaitOrSpendAccounting(t *testing.T) {
	scenario := runCadenceCIScenario(t, 20, nil)
	if scenario.err != nil || scenario.result == nil || scenario.result.Runs[0].Status != "success" {
		t.Fatalf("scenario did not complete: result=%+v err=%v", scenario.result, scenario.err)
	}
	if len(scenario.waits) != 20 {
		t.Fatalf("waits = %d, want one 30s wait per observation (20)", len(scenario.waits))
	}
	for i, interval := range scenario.waits {
		if interval != 30*time.Second {
			t.Fatalf("wait %d = %v, want 30s", i, interval)
		}
	}
	want := time.Date(2026, 11, 12, 11, 0, 0, 0, time.UTC).Add(ciWaitTimeout).Unix()
	for i, deadline := range scenario.deadlines {
		if deadline != want {
			t.Fatalf("observation %d deadline = %d, want the original %d (faster polling must not renew it)", i, deadline, want)
		}
	}
	events := scenario.log.snapshot()
	if got := countEventsByType(events, "run.await"); got != 1 {
		t.Fatalf("run.await events = %d, want one persisted wait for an unchanged operation identity", got)
	}
	if scenario.launches != 1 || countEventsByType(events, "run.resumed") != 0 || countEventsByType(events, "run.retry") != 0 {
		t.Fatalf("launches=%d resumed=%d retries=%d, want no relaunch or retry spent by polling", scenario.launches, countEventsByType(events, "run.resumed"), countEventsByType(events, "run.retry"))
	}
}

// A verified same-head rerun is a new external operation: it starts its own
// bound, while earlier observations of the original execution kept theirs.
func TestBatchObservation_SameHeadRerunStartsNewCIWaitGeneration(t *testing.T) {
	scenario := runCadenceCIScenario(t, 12, func(poll int, client *reviewWaitSchedulerGitHubClient) {
		if poll == 5 {
			client.setPR("42-work", func(pr *github.PR) { pr.CIExecutionID = "checks-2" })
		}
	})
	if scenario.err != nil || scenario.result == nil || scenario.result.Runs[0].Status != "success" {
		t.Fatalf("scenario did not complete: result=%+v err=%v", scenario.result, scenario.err)
	}
	start := time.Date(2026, 11, 12, 11, 0, 0, 0, time.UTC)
	first := start.Add(ciWaitTimeout).Unix()
	// Observation i (0-based) follows wait i+1; the rerun is visible from the
	// observation after the 5th wait, at start + 5*30s.
	second := start.Add(5 * 30 * time.Second).Add(ciWaitTimeout).Unix()
	for i, deadline := range scenario.deadlines {
		want, execution := first, "checks-1"
		if i >= 5 {
			want, execution = second, "checks-2"
		}
		if deadline != want || scenario.executions[i] != execution {
			t.Fatalf("observation %d = (%s, %d), want (%s, %d)", i, scenario.executions[i], deadline, execution, want)
		}
	}
	events := scenario.log.snapshot()
	if got := countEventsByType(events, "run.await"); got != 2 {
		t.Fatalf("run.await events = %d, want one per CI wait generation", got)
	}
	if scenario.launches != 1 || countEventsByType(events, "run.resumed") != 0 {
		t.Fatalf("launches=%d resumed=%d, want a rerun wait without relaunch or spent allowance", scenario.launches, countEventsByType(events, "run.resumed"))
	}
}

// A ready row's live evidence is revalidated immediately before execution
// admission (the last Resume of each sequence is that revalidation). Whatever
// the observation selects, the default cadence only sets the wait between
// observations: a stale approval or changed head returns the row to waiting
// without a launch, and terminal observations finish without ever starting
// the agent.
func TestRowDriverAdmissionRevalidationUnderDefaultCadence(t *testing.T) {
	run := RunSpec{RunID: "run-42", IssueNumber: 42, BatchID: "batch-a", Branch: "42-fix", BaseBranch: "main"}
	pending := lifecycleVerdict{Action: lifecycleAwait, Evidence: map[string]any{"gate": "pending", "reason": "REVIEW_STALE_APPROVAL"}}
	cases := []struct {
		name         string
		observations []lifecycleVerdict
		results      []AgentRunResult
		wantStatus   string
		wantLaunches int
		wantWaits    []time.Duration
	}{
		{"stale approval or changed head waits again, then resumes", []lifecycleVerdict{pending, {Action: lifecycleResume}, {Action: lifecycleResume}}, []AgentRunResult{{Status: "success"}}, "success", 1, []time.Duration{30 * time.Second}},
		{"stale evidence twice waits once per observation", []lifecycleVerdict{pending, pending, {Action: lifecycleResume}, {Action: lifecycleResume}}, []AgentRunResult{{Status: "success"}}, "success", 1, []time.Duration{30 * time.Second, 30 * time.Second}},
		{"failed CI or conflict at admission is terminal without a launch", []lifecycleVerdict{{Action: lifecycleFailure}}, nil, "failure", 0, nil},
		{"merged observation finishes without execution capacity", []lifecycleVerdict{{Action: lifecycleSuccess}}, nil, "success", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var waits []time.Duration
			gates := &rowDriverWaitRecorder{rowDriverGateFake: &rowDriverGateFake{}, waits: &waits}
			executor := &rowDriverExecutorFake{started: true, observations: tc.observations, results: tc.results}
			got := newRowDriver(executor, gates, &rowDriverClockFake{}).Drive(context.Background(), run, ContinuationCursor{Mode: ModeContinue}, rowDriverOptions{
				Awaiting: true, Ready: true, ReleaseAwaitCapacity: true,
			})
			if got.Result.Status != tc.wantStatus || executor.executeCalls != tc.wantLaunches {
				t.Fatalf("Drive = %+v launches=%d, want status %q with %d launches", got.Result, executor.executeCalls, tc.wantStatus, tc.wantLaunches)
			}
			if !reflect.DeepEqual(waits, tc.wantWaits) {
				t.Fatalf("waits = %v, want %v", waits, tc.wantWaits)
			}
		})
	}
}
