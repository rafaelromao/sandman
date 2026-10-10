package batch

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

type managedLifecycleFactory struct {
	client        *reviewWaitSchedulerGitHubClient
	mu            sync.Mutex
	launch        map[int]int
	started       []int
	renewFeedback bool
}

func (f *managedLifecycleFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.mu.Lock()
	if f.launch == nil {
		f.launch = make(map[int]int)
	}
	f.launch[issue.Number]++
	attempt := f.launch[issue.Number]
	f.started = append(f.started, issue.Number)
	f.mu.Unlock()

	_ = os.MkdirAll(filepath.Join(sb.WorkDir(), ".sandman"), 0o755)
	_ = atomicfs.WriteAtomic(filepath.Join(sb.WorkDir(), ".sandman", "task.md"), []byte("# Task\n\nContinue the managed lifecycle.\n"), 0o600)
	if issue.Number == 42 {
		switch attempt {
		case 1:
			seedManagedReviewRequest(sb.WorkDir(), "1001")
		case 2:
			if f.renewFeedback {
				f.client.setPR(branch, func(pr *github.PR) {
					pr.StatusCheckRollup = "success"
					pr.ReviewDecision = "REVIEW_REQUIRED"
					pr.MergeStateStatus = "BLOCKED"
				})
				f.client.mu.Lock()
				now := time.Now().UTC()
				f.client.comments = []github.PRComment{
					{ID: "https://github.com/owner/repo/pull/17#issuecomment-1001", Body: "/sandman review", CreatedAt: now.Add(-time.Second)},
					{ID: "https://github.com/owner/repo/pull/17#issuecomment-1002", Body: "/sandman review follow-up", CreatedAt: now},
				}
				f.client.mu.Unlock()
				seedManagedReviewRequest(sb.WorkDir(), "1002")
			} else {
				f.client.setPR(branch, func(pr *github.PR) {
					pr.State = "merged"
					pr.Merged = true
					pr.Body = "Closes #42"
				})
				f.client.mu.Lock()
				f.client.issues[42].State = "closed"
				f.client.mu.Unlock()
			}
		case 3:
			f.client.setPR(branch, func(pr *github.PR) {
				pr.State = "merged"
				pr.Merged = true
				pr.Body = "Closes #42"
			})
			f.client.mu.Lock()
			f.client.issues[42].State = "closed"
			f.client.mu.Unlock()
		}
	}
	return &managedLifecycleRunnable{factory: f, issue: issue.Number, branch: branch, attempt: attempt}
}

type managedLifecycleRunnable struct {
	factory *managedLifecycleFactory
	issue   int
	branch  string
	attempt int
}

func (r *managedLifecycleRunnable) Run(context.Context, prompt.IssueRenderer, string, prompt.RenderConfig) AgentRunResult {
	return AgentRunResult{IssueNumber: r.issue, Status: "success", Branch: r.branch}
}

func (f *managedLifecycleFactory) launches(issue int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launch[issue]
}

func seedManagedReviewRequest(workDir, trigger string) {
	seed := &reviewRequestSeedingFactory{workDir: workDir, prNumber: 17, headSHA: "current-sha", results: []AgentRunResult{{Status: "success"}}}
	seed.NewRunnable(&github.Issue{Number: 42}, gateTestBranch, &fakeSandbox{workDir: workDir})
	for _, name := range []string{"17.review_request.json", "17.review_request.json.state"} {
		path := filepath.Join(workDir, ".sandman", "state", name)
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		data = []byte(strings.ReplaceAll(string(data), "issuecomment-2002", "issuecomment-"+trigger))
		_ = atomicfs.WriteAtomic(path, data, 0o600)
	}
	data, err := os.ReadFile(filepath.Join(workDir, ".sandman", "state", "17.review_request.json"))
	if err != nil {
		return
	}
	var request reviewRequestEnvelope
	if json.Unmarshal(data, &request) != nil {
		return
	}
	stateData, err := os.ReadFile(filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state"))
	if err != nil {
		return
	}
	var state reviewWaitState
	if json.Unmarshal(stateData, &state) != nil {
		return
	}
	request.TriggerIdentity = reviewTriggerIdentity(request.TriggerID)
	if atomicfs.WriteAtomicJSON(filepath.Join(workDir, ".sandman", "state", "17.review_registration.json"), reviewRequestRegistration{
		Protocol: reviewRegistrationProtocol,
		Request:  request,
		State:    state,
	}, 0o600) != nil {
		return
	}
}

func managedLifecycleOptions() runSessionOptions {
	return runSessionOptions{
		currentHead: func(string) (string, error) { return "current-sha", nil },
	}
}

func managedLifecycleOrchestrator(client *reviewWaitSchedulerGitHubClient, log events.EventLog, factory *managedLifecycleFactory) *Orchestrator {
	cfg := &config.Config{
		Agent:          "test-agent",
		Sandbox:        "worktree",
		WorktreeDir:    ".sandman/worktrees",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}
	return NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(managedLifecycleOptions()))
}

func managedLifecycleRequest() Request {
	return Request{
		Issues:       []int{42, 43},
		RunTS:        "261010120000",
		RunShortID:   "managed",
		Parallel:     1,
		Branches:     map[int]string{42: gateTestBranch, 43: "43-dependent"},
		Dependencies: map[int][]int{43: {42}},
	}
}

func runByIssue(result *Result, issue int) AgentRunResult {
	for _, run := range result.Runs {
		if run.IssueNumber == issue {
			return run
		}
	}
	return AgentRunResult{IssueNumber: issue, Status: "missing"}
}

func lifecycleState(t *testing.T, log *events.JSONLLogger, issue int) events.RunState {
	t.Helper()
	states, err := events.ReadRunStates(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.IssueNumber() == issue {
			return state
		}
	}
	t.Fatalf("run state for issue %d not found", issue)
	return events.RunState{}
}

func runStateForIssue(t *testing.T, eventLog []events.Event, issue int) events.RunState {
	t.Helper()
	states := events.ProjectRunStates(eventLog)
	for _, state := range states {
		if state.IssueNumber() == issue {
			return state
		}
	}
	t.Fatalf("run state for issue %d not found", issue)
	return events.RunState{}
}

func TestManagedReviewLifecycle_ProductionPathRecoversAndReleasesDependency(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, State: "open", Title: "Parent"},
			43: {Number: 43, State: "open", Title: "Dependent"},
		},
		prs: map[string]*github.PR{
			gateTestBranch: {Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", Body: "Closes #42", StatusCheckRollup: "success", MergeStateStatus: "BLOCKED"},
			"43-dependent": {Number: 43, State: "merged", Merged: true, HeadRefName: "43-dependent", Body: "Closes #43"},
		},
	}}
	log := &events.JSONLLogger{Path: filepath.Join(root, ".sandman", "events.jsonl")}
	factory := &managedLifecycleFactory{client: client, renewFeedback: true}
	request := managedLifecycleRequest()

	first, err := managedLifecycleOrchestrator(client, log, factory).RunBatch(context.Background(), request)
	if err != nil {
		t.Fatalf("initial managed batch: %v", err)
	}
	if runByIssue(first, 42).Status != "await" || runByIssue(first, 43).Status != "queued" {
		t.Fatalf("initial statuses = %+v, want parent await and dependent queued", first.Runs)
	}
	parent := lifecycleState(t, log, 42)
	if parent.RunID == "" || factory.launches(42) != 1 {
		t.Fatalf("initial lifecycle = %+v launches=%d", parent, factory.launches(42))
	}
	registration, err := readFileReviewRegistration(filepath.Join(root, ".sandman", "worktrees", gateTestBranch, ".sandman", "state", "17.review_registration.json"))
	if err != nil || registration.Request.TriggerID != "https://github.com/owner/repo/pull/17#issuecomment-1001" {
		t.Fatalf("initial canonical review registration = %+v, err=%v", registration, err)
	}

	layout := paths.NewLayout(&config.Config{WorktreeDir: ".sandman/worktrees"}, root)
	eventsBefore, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	ready := FindReadyContinuations(eventsBefore, layout)
	if len(ready) != 2 {
		t.Fatalf("restart recovery found %d continuations, want parent and dependent: %+v", len(ready), ready)
	}
	continued := Request{}
	if err := ApplyReadyContinuations(&continued, ready, layout, 1800); err != nil {
		t.Fatalf("apply restart continuations: %v", err)
	}
	if continued.RunIDs[42] != parent.RunID || continued.PreviousRunIDs[42] != parent.RunID {
		t.Fatalf("restart changed parent RunID: %+v", continued)
	}

	workDir := filepath.Join(root, ".sandman", "worktrees", gateTestBranch)
	writeInformalRespondedClassification(t, workDir, "Please fix the race in internal/socketpath/socketpath.go.")
	syncCanonicalState(t, workDir)
	feedbackBatch, err := managedLifecycleOrchestrator(client, log, factory).RunBatch(context.Background(), continued)
	if err != nil {
		t.Fatalf("feedback continuation batch: %v result=%+v", err, feedbackBatch)
	}
	if runByIssue(feedbackBatch, 42).Status != "await" || factory.launches(42) != 2 {
		t.Fatalf("feedback continuation = %+v launches=%d", feedbackBatch, factory.launches(42))
	}
	registration, err = readFileReviewRegistration(filepath.Join(workDir, ".sandman", "state", "17.review_registration.json"))
	if err != nil || registration.Request.TriggerID != "https://github.com/owner/repo/pull/17#issuecomment-1002" {
		t.Fatalf("renewed canonical review registration = %+v, err=%v", registration, err)
	}
	renewedEvents, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	renewed := Request{}
	if err := ApplyReadyContinuations(&renewed, FindReadyContinuations(renewedEvents, layout), layout, 1800); err != nil {
		t.Fatalf("apply renewed continuation: %v", err)
	}
	writeCurrentHeadApprovalClassification(t, workDir)
	syncCanonicalState(t, workDir)
	approvalBatch, err := managedLifecycleOrchestrator(client, log, factory).RunBatch(context.Background(), renewed)
	if err != nil || runByIssue(approvalBatch, 42).Status != "success" || factory.launches(42) != 3 {
		t.Fatalf("approval continuation = %+v launches=%d err=%v", approvalBatch, factory.launches(42), err)
	}
	final, err := managedLifecycleOrchestrator(client, log, factory).RunBatch(context.Background(), continued)
	if err != nil || runByIssue(final, 42).Status != "success" || runByIssue(final, 43).Status != "success" {
		t.Fatalf("terminal statuses = %+v, err=%v, want parent and dependent success", final.Runs, err)
	}
	if factory.launches(42) != 3 || factory.launches(43) != 1 {
		t.Fatalf("launches = parent %d dependent %d, want parent work followed by one dependent release", factory.launches(42), factory.launches(43))
	}

	allEvents, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	parentEvents := make([]events.Event, 0)
	for _, event := range allEvents {
		if event.Issue == 42 {
			parentEvents = append(parentEvents, event)
		}
	}
	if got := countEventsByType(parentEvents, "run.await"); got != 4 {
		t.Fatalf("parent await events = %d, want initial, restart, feedback, and approval waits", got)
	}
	if got := countEventsByType(parentEvents, "run.continued"); got != 2 {
		t.Fatalf("parent continuation events = %d, want feedback and approval re-entry", got)
	}
	if got := countEventsByType(parentEvents, "run.retry"); got != 0 {
		t.Fatalf("parent retry events = %d, want zero lifecycle retries", got)
	}
	for _, event := range parentEvents {
		if event.Type != "run.started" && event.Type != "run.continued" && event.Type != "run.await" && event.Type != "run.resumed" && event.Type != "run.finished" {
			continue
		}
		if event.RunID != parent.RunID {
			t.Fatalf("parent event %s used RunID %q, want %q", event.Type, event.RunID, parent.RunID)
		}
	}
	parentFinished := findEvent(parentEvents, "run.finished")
	if parentFinished == nil || parentFinished.Payload["status"] != "success" {
		t.Fatalf("parent terminal event = %+v", parentFinished)
	}
	parentFinishedIndex := -1
	dependentStartedIndex := -1
	for index, event := range allEvents {
		if event.Issue == 42 && event.Type == "run.finished" && event.Payload["status"] == "success" {
			parentFinishedIndex = index
		}
		if event.Issue == 43 && (event.Type == "run.started" || event.Type == "run.finished") && dependentStartedIndex < 0 {
			dependentStartedIndex = index
		}
	}
	if parentFinishedIndex < 0 || dependentStartedIndex < parentFinishedIndex {
		t.Fatalf("dependent was released before parent success: parent=%d dependent=%d", parentFinishedIndex, dependentStartedIndex)
	}
}

func TestManagedReviewLifecycle_FeedbackResumesAndRenewsRequest(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Parent"}},
		prs: map[string]*github.PR{
			gateTestBranch: {Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", Body: "Closes #42", StatusCheckRollup: "success", MergeStateStatus: "BLOCKED"},
		},
	}}
	log := &events.JSONLLogger{Path: filepath.Join(root, ".sandman", "events.jsonl")}
	factory := &managedLifecycleFactory{client: client, renewFeedback: true}
	orchestrator := managedLifecycleOrchestrator(client, log, factory)

	first, err := orchestrator.RunBatch(context.Background(), Request{
		Issues: []int{42}, RunTS: "261010120002", RunShortID: "feedback", Branches: map[int]string{42: gateTestBranch},
	})
	if err != nil || runByIssue(first, 42).Status != "await" {
		t.Fatalf("initial feedback batch = %+v, err=%v", first, err)
	}
	initialState := lifecycleState(t, log, 42)

	layout := paths.NewLayout(&config.Config{WorktreeDir: ".sandman/worktrees"}, root)
	initialEvents, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	ready := FindReadyContinuations(initialEvents, layout)
	continued := Request{}
	if err := ApplyReadyContinuations(&continued, ready, layout, 1800); err != nil {
		t.Fatalf("apply feedback continuation: %v", err)
	}
	workDir := filepath.Join(root, ".sandman", "worktrees", gateTestBranch)
	writeInformalRespondedClassification(t, workDir, "Please fix the race in internal/socketpath/socketpath.go.")
	syncCanonicalState(t, workDir)

	feedback, err := orchestrator.RunBatch(context.Background(), continued)
	if err != nil || runByIssue(feedback, 42).Status != "await" || factory.launches(42) != 2 {
		t.Fatalf("feedback continuation = %+v launches=%d err=%v", feedback, factory.launches(42), err)
	}
	feedbackState := lifecycleState(t, log, 42)
	if feedbackState.RunID != initialState.RunID {
		t.Fatalf("feedback continuation changed RunID: initial=%q feedback=%q", initialState.RunID, feedbackState.RunID)
	}
	registration, err := readFileReviewRegistration(filepath.Join(workDir, ".sandman", "state", "17.review_registration.json"))
	if err != nil || registration.Request.TriggerID != "https://github.com/owner/repo/pull/17#issuecomment-1002" {
		t.Fatalf("renewed canonical registration = %+v, err=%v", registration, err)
	}

	renewedEvents, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	renewed := Request{}
	if err := ApplyReadyContinuations(&renewed, FindReadyContinuations(renewedEvents, layout), layout, 1800); err != nil {
		t.Fatalf("apply renewed continuation: %v", err)
	}
	writeCurrentHeadApprovalClassification(t, workDir)
	syncCanonicalState(t, workDir)
	final, err := orchestrator.RunBatch(context.Background(), renewed)
	if err != nil || runByIssue(final, 42).Status != "success" || factory.launches(42) != 3 {
		t.Fatalf("renewed approval continuation = %+v launches=%d err=%v", final, factory.launches(42), err)
	}
	finalState := lifecycleState(t, log, 42)
	if finalState.RunID != initialState.RunID {
		t.Fatalf("approval continuation changed RunID: initial=%q final=%q", initialState.RunID, finalState.RunID)
	}

	allEvents, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	parentEvents := make([]events.Event, 0)
	for _, event := range allEvents {
		if event.Issue == 42 {
			parentEvents = append(parentEvents, event)
		}
	}
	if countEventsByType(parentEvents, "run.continued") != 2 || countEventsByType(parentEvents, "run.retry") != 0 {
		t.Fatalf("feedback lifecycle events = %+v", parentEvents)
	}
}

func syncCanonicalState(t *testing.T, workDir string) {
	t.Helper()
	layout := paths.NewLayout(nil, workDir)
	registrationPath := layout.PRReviewRegistrationPath(17)
	registration, err := readFileReviewRegistration(registrationPath)
	if err != nil {
		t.Fatalf("read canonical registration: %v", err)
	}
	stateData, err := os.ReadFile(layout.PRReviewRequestStatePath(17))
	if err != nil {
		t.Fatalf("read observed review state: %v", err)
	}
	var state reviewWaitState
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("decode observed review state: %v", err)
	}
	state.TriggerID = registration.Request.TriggerID
	state.Protocol = registration.Request.Protocol
	state.Repository = registration.Request.Repository
	state.PullRequest = registration.Request.PullRequest
	state.TriggerPrefix = registration.Request.TriggerPrefix
	state.TriggerCreatedAt = registration.Request.TriggerCreatedAt
	state.ConfirmedAt = registration.Request.ConfirmedAt
	state.StartedAt = registration.Request.StartedAt
	state.DeadlineAt = registration.Request.DeadlineAt
	state.StartedUnixSeconds = registration.Request.StartedUnixSeconds
	state.EffectiveTimeout = registration.Request.EffectiveTimeout
	state.DeadlineUnixSeconds = registration.Request.DeadlineUnixSeconds
	state.PollPlan = append([]int(nil), registration.Request.PollPlan...)
	state.HeadSHA = registration.Request.HeadSHA
	state.ObservedHeadSHA = registration.Request.HeadSHA
	state.State = "pending"
	state.Lifecycle = "started"
	state.Reason = "pending"
	elapsed := 0
	state.ElapsedSeconds = &elapsed
	if state.Evidence != nil {
		state.ObservedState = "responded"
		state.ObservedReason = "responded"
		state.ObservedAt = registration.Request.ConfirmedAt
		state.Evidence.Classification = normalizeReviewClassification(t, state.Evidence.Classification, registration.Request)
	}
	registration.State = state
	if err := atomicfs.WriteAtomicJSON(registrationPath, registration, 0o600); err != nil {
		t.Fatalf("persist canonical observed review state: %v", err)
	}
}

func normalizeReviewClassification(t *testing.T, raw json.RawMessage, request reviewRequestEnvelope) json.RawMessage {
	t.Helper()
	var classification map[string]any
	if err := json.Unmarshal(raw, &classification); err != nil {
		t.Fatalf("decode review classification: %v", err)
	}
	var visit func(map[string]any)
	visit = func(values map[string]any) {
		for key, value := range values {
			switch key {
			case "repository":
				values[key] = request.Repository
			case "pull_request":
				values[key] = request.PullRequest
			case "head_sha":
				values[key] = request.HeadSHA
			case "trigger_id":
				values[key] = request.TriggerID
			case "trigger_prefix":
				values[key] = request.TriggerPrefix
			case "trigger_created_at", "start":
				values[key] = request.TriggerCreatedAt
			case "confirmed_at", "started_at":
				values[key] = request.ConfirmedAt
			case "deadline_at":
				values[key] = request.DeadlineAt
			case "deadline_unix_seconds":
				values[key] = request.DeadlineUnixSeconds
			case "started_unix_seconds":
				values[key] = request.StartedUnixSeconds
			case "response_timestamp":
				values[key] = request.ConfirmedAt
			}
			if nested, ok := value.(map[string]any); ok {
				visit(nested)
			}
			if nested, ok := value.([]any); ok {
				for _, item := range nested {
					if nestedMap, ok := item.(map[string]any); ok {
						visit(nestedMap)
					}
				}
			}
		}
	}
	visit(classification)
	encoded, err := json.Marshal(classification)
	if err != nil {
		t.Fatalf("encode review classification: %v", err)
	}
	return encoded
}

func TestManagedReviewLifecycle_CancellationRevokesRecoveryIntent(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "open"}},
		prs: map[string]*github.PR{
			gateTestBranch: {Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", StatusCheckRollup: "pending"},
			"43-dependent": {Number: 43, State: "open", HeadRefName: "43-dependent", HeadRefOid: "dependent-sha", StatusCheckRollup: "success", MergeStateStatus: "CLEAN"},
		},
	}}
	observationsAtCancel := -1
	log := &cancelOnAwaitLog{
		cancel:  cancel,
		onAwait: func() { observationsAtCancel = client.observationCount() },
	}
	factory := &managedLifecycleFactory{client: client}
	result, err := managedLifecycleOrchestrator(client, log, factory).RunBatch(ctx, Request{
		Issues: []int{42, 43}, RunTS: "261010120001", RunShortID: "cancel",
		Branches: map[int]string{42: gateTestBranch, 43: "43-dependent"}, Dependencies: map[int][]int{43: {42}},
	})
	if result == nil || runByIssue(result, 42).Status != "aborted" {
		t.Fatalf("cancelled managed run = %+v, err=%v", result, err)
	}
	if factory.launches(42) != 1 || factory.launches(43) != 0 || countEventsByType(log.snapshot(), "run.retry") != 0 {
		t.Fatalf("cancellation caused extra work: parent launches=%d dependent launches=%d retries=%d", factory.launches(42), factory.launches(43), countEventsByType(log.snapshot(), "run.retry"))
	}
	if observationsAtCancel < 0 || client.observationCount() != observationsAtCancel {
		t.Fatalf("cancellation performed a later external probe: at-cancel=%d final=%d", observationsAtCancel, client.observationCount())
	}
	state := runStateForIssue(t, log.snapshot(), 42)
	if !state.IsTerminal() || state.Status() != "aborted" {
		t.Fatalf("cancelled state = %+v, want terminal aborted", state)
	}
	if ready := FindReadyContinuations(log.snapshot(), paths.NewLayout(&config.Config{WorktreeDir: ".sandman/worktrees"}, root)); len(ready) != 0 {
		t.Fatalf("cancelled run remained recoverable: %+v", ready)
	}
	for _, event := range log.snapshot() {
		if event.Issue == 43 && event.Type == "run.started" {
			t.Fatalf("dependent was admitted after cancellation: %+v", event)
		}
	}
}
