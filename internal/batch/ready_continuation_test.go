package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestFindReadyContinuationsUsesLatestDurableCapacityPhase(t *testing.T) {
	t.Parallel()
	layout := paths.NewLayout(nil, t.TempDir())
	started := time.Now().UTC().Add(-2 * time.Minute)
	awaited := started.Add(time.Minute)
	queued := awaited.Add(time.Minute)

	eventsBefore := readyContinuationEvents("run-ready", "batch-ready", started, awaited, queued)
	ready := FindReadyContinuations(eventsBefore, layout)
	if len(ready) != 1 {
		t.Fatalf("ready continuations = %#v, want one", ready)
	}
	got := ready[0]
	if got.IssueNumber != 42 || got.RunID != "run-ready" || got.PreviousRunID != "run-ready" || got.PreviousRunBatchID != "batch-ready" || got.BatchID != "batch-ready" {
		t.Fatalf("ready continuation identity = %#v", got)
	}
	if got.Branch != "42-fix" || got.BaseBranch != "main" || got.IssueTitle != "Fix bug" {
		t.Fatalf("ready continuation worktree identity = %#v", got)
	}

	// A later external await means the operation is still pending; a later
	// continuation means the queued evidence has already been consumed.
	stillWaiting := append(append([]events.Event(nil), eventsBefore...), events.Event{
		Type: "run.await", Timestamp: queued.Add(time.Minute), RunID: "run-ready", Issue: 42,
		Payload: map[string]any{"await_reason": "pending"},
	})
	if got := FindReadyContinuations(stillWaiting, layout); len(got) != 0 {
		t.Fatalf("ready after newer await = %#v, want none", got)
	}
	continued := append(append([]events.Event(nil), eventsBefore...), events.Event{
		Type: "run.continued", Timestamp: queued.Add(time.Minute), RunID: "run-new", Issue: 42,
		Payload: map[string]any{"branch": "42-fix", "batch_id": "batch-new"},
	})
	if got := FindReadyContinuations(continued, layout); len(got) != 0 {
		t.Fatalf("ready after newer run = %#v, want none", got)
	}
}

func TestApplyReadyContinuationsRestoresPromptAndSchedulerIdentity(t *testing.T) {
	workDir := t.TempDir()
	layout := paths.NewLayout(nil, workDir)
	branch := "42-fix"
	taskPath := filepath.Join(layout.WorktreeDir, branch, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("create Task directory: %v", err)
	}
	const task = "# Task\n\nFinish the implementation.\n"
	if err := os.WriteFile(taskPath, []byte(task), 0o600); err != nil {
		t.Fatalf("write Task: %v", err)
	}
	request := Request{Issues: []int{43}}
	ready := []ReadyContinuation{{
		IssueNumber: 42, RunID: "original-run", PreviousRunID: "original-run",
		PreviousRunBatchID: "old-batch", BatchID: "old-batch", Branch: branch,
		BaseBranch: "main", IssueTitle: "Fix bug",
	}}
	if err := ApplyReadyContinuations(&request, ready, layout, 900); err != nil {
		t.Fatalf("ApplyReadyContinuations: %v", err)
	}
	if len(request.Issues) != 2 || request.Issues[1] != 42 {
		t.Fatalf("issues = %v, want existing issue plus ready continuation", request.Issues)
	}
	if request.IssueMode(42) != ModeContinue || request.RunIDs[42] != "original-run" || !request.ReadyContinuations[42] {
		t.Fatalf("restored scheduler identity = mode:%v run:%q ready:%v", request.IssueMode(42), request.RunIDs[42], request.ReadyContinuations[42])
	}
	if request.PreviousRunIDs[42] != "original-run" || request.PreviousRunBatchIDs[42] != "old-batch" || !request.ReuseSession[42] {
		t.Fatalf("restored previous identity = runs:%v batches:%v reuse:%v", request.PreviousRunIDs, request.PreviousRunBatchIDs, request.ReuseSession)
	}
	if request.Branches[42] != branch || request.BaseBranches[42] != "main" || request.IssueTitles[42] != "Fix bug" {
		t.Fatalf("restored worktree identity = branches:%v bases:%v titles:%v", request.Branches, request.BaseBranches, request.IssueTitles)
	}
	if !strings.Contains(request.TaskPrompts[42], task) || !strings.Contains(request.TaskPrompts[42], "Delegated review response timeout: `900` seconds") {
		t.Fatalf("restored prompt omitted Task or current review timeout:\n%s", request.TaskPrompts[42])
	}
}

func TestApplyReadyContinuationsDoesNotOverrideExplicitOverride(t *testing.T) {
	request := Request{Issues: []int{42}, Mode: map[int]IssueMode{42: ModeOverride}}
	ready := []ReadyContinuation{{IssueNumber: 42, RunID: "old", PreviousRunID: "old", PreviousRunBatchID: "batch", Branch: "42-fix", BaseBranch: "main"}}
	if err := ApplyReadyContinuations(&request, ready, paths.NewLayout(nil, t.TempDir()), 900); err != nil {
		t.Fatalf("ApplyReadyContinuations: %v", err)
	}
	if request.IssueMode(42) != ModeOverride || request.ReadyContinuations[42] {
		t.Fatalf("override mode was replaced: mode=%v ready=%v", request.IssueMode(42), request.ReadyContinuations[42])
	}
}

func TestInitialContinuationRecoveryRetainsPreviousIdentity(t *testing.T) {
	request := Request{}
	wait := daemon.RunWait{InitialAdmission: true, AdmissionMode: int(ModeContinue), PreviousRunID: "previous", PreviousBatchID: "previous-batch", ReuseSession: true}
	if err := ApplyReadyContinuations(&request, []ReadyContinuation{{IssueNumber: 42, RunID: "fresh-continuation", Branch: "42-fix", BaseBranch: "main", Wait: &wait}}, paths.NewLayout(nil, t.TempDir()), 1800); err != nil {
		t.Fatal(err)
	}
	if request.RunIDs[42] != "fresh-continuation" || request.IssueMode(42) != ModeContinue || request.PreviousRunIDs[42] != "previous" || request.PreviousRunBatchIDs[42] != "previous-batch" || !request.ReuseSession[42] {
		t.Fatalf("initial continuation lost its artifact handoff: %+v", request)
	}
}

func TestReadyHandoffRecoveryUsesRenewedCurrentBatch(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	now := time.Now().UTC()
	claim, err := daemon.ClaimRun(layout.SandmanDir, "row")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	record := daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42, Branch: "42-fix", BaseBranch: "main", Ready: true, OperationID: "capacity"}
	if err := daemon.RenewRunWait(layout.BatchDir("old"), record, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	moved, err := daemon.TransferRunWait(layout.BatchDir("old"), layout.BatchDir("new"), "row", now.Add(-9*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.RenewRunWait(layout.BatchDir("new"), moved, now); err != nil {
		t.Fatal(err)
	}
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
	log := &spyEventLog{events: []events.Event{{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-20 * time.Minute), Payload: map[string]any{"branch": "42-fix", "batch_id": "old"}}}}
	if err := logCapacityQueuedContinuation(log, "row", 42, "new", RowSpec{Branches: map[int]string{42: "42-fix"}, BaseBranch: "main"}, nil, "Ready"); err != nil {
		t.Fatal(err)
	}
	ready := FindReadyContinuations(log.snapshot(), layout)
	if len(ready) != 1 || ready[0].BatchID != "new" || ready[0].Wait == nil || !ready[0].Wait.RecoverableAt(now) {
		t.Fatalf("ready recovery selected obsolete expired owner: %+v", ready)
	}
}

func TestProductionReadyTakeoverPublishesOwnerBeforeAdmission(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	layout := paths.NewLayout(nil, root)
	now := time.Now().UTC()
	log := &spyEventLog{events: readyContinuationEvents("ready", "old", now.Add(-time.Minute), now.Add(-30*time.Second), now)}
	if err := daemon.RenewRunWait(layout.BatchDir("old"), daemon.RunWait{Protocol: "run-wait/v1", RunID: "ready", BatchID: "old", Issue: 42, Branch: "42-fix", BaseBranch: "main", Ready: true, OperationID: "capacity"}, now); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(layout.WorktreeDir, "42-fix", ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(task), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(task, []byte("# Task\nPreserved work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := Request{Issues: []int{43}, RunTS: "261004120000", RunShortID: "owner", Parallel: 1, Branches: map[int]string{43: "43-busy"}}
	if err := ApplyReadyContinuations(&request, FindReadyContinuations(log.snapshot(), layout), layout, 1800); err != nil {
		t.Fatal(err)
	}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "closed"}}, prs: map[string]*github.PR{
		"42-fix": {Number: 17, State: "open", HeadRefName: "42-fix", HeadRefOid: "head", StatusCheckRollup: "failure"}, "43-busy": {Number: 18, State: "closed", Merged: true, HeadRefName: "43-busy", Body: "Closes #43"},
	}}
	factory := &readyRecoveryRunnableFactory{busyStarted: make(chan struct{}), allowBusyFinish: make(chan struct{})}
	queued := make(chan struct{})
	var once sync.Once
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}}, log,
		WithErrorLog(io.Discard), WithRunnableFactory(factory), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }, startWaiterQueued: func(priority bool) {
			if priority {
				once.Do(func() { close(queued) })
			}
		}}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = o.RunBatch(ctx, request) }()
	select {
	case <-queued:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("ready takeover did not reach occupied admission")
	}
	select {
	case <-factory.busyStarted:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("ordinary row did not occupy admission")
	}
	states, err := events.ReadRunStates(log)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if state := states["ready"]; state.BatchID() != issueBatchIDForRequest(request) || state.Status() != "waiting" {
		cancel()
		t.Fatalf("production takeover failed to publish current ownership: %+v", state)
	}
	if starts := factory.startsSnapshot(); !equalPriorityInts(starts, []int{43}) {
		cancel()
		t.Fatalf("ready row executed before admission: %v", starts)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled takeover did not stop")
	}
}

func TestRecoveryReconcilesChangedExternalOperation(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	now := time.Now().UTC()
	record := daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: "42-fix", BaseBranch: "main", OperationID: "ci:old", OperationDeadline: now.Add(time.Minute)}
	if err := daemon.RenewRunWait(layout.BatchDir("batch"), record, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		payload  map[string]any
		probe    bool
		deadline time.Time
	}{
		{"quota", map[string]any{"await_reason": "usage-limit", "usage_limit_deadline_unix_seconds": now.Add(5 * time.Hour).Unix()}, true, now.Add(5 * time.Hour).Truncate(time.Second)},
		{"review", map[string]any{"review_request": map[string]any{"deadline_unix_seconds": now.Add(30 * time.Minute).Unix()}}, false, now.Add(30 * time.Minute).Truncate(time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := []events.Event{{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"batch_id": "batch", "branch": "42-fix"}}, {Type: "run.await", RunID: "row", Issue: 42, Timestamp: now, Payload: tc.payload}}
			ready := FindReadyContinuations(log, layout)
			if len(ready) != 1 || ready[0].Wait == nil || ready[0].Wait.UsageLimitProbe != tc.probe || !ready[0].Wait.OperationDeadline.Equal(tc.deadline) || ready[0].Wait.OperationID == record.OperationID || ready[0].Wait.LeaseExpiresAt.After(record.OperationDeadline) {
				t.Fatalf("event-before-checkpoint retained obsolete operation: %+v", ready)
			}
		})
	}
}

func TestReturnedWaitingIntentRenewsUntilBatchReleasesClaims(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	layout := paths.NewLayout(nil, root)
	now := time.Now().UTC()
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	var pulseMu sync.Mutex
	pulses := map[string]chan time.Time{}
	log := &spyEventLog{}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{41: {Number: 41, State: "open"}, 42: {Number: 42, State: "open"}, 43: {Number: 43, State: "closed"}}, prs: map[string]*github.PR{
		"42-parent": {Number: 17, State: "open", HeadRefName: "42-parent", HeadRefOid: "head", StatusCheckRollup: "pending"}, "43-busy": {Number: 18, State: "closed", Merged: true, HeadRefName: "43-busy", Body: "Closes #43"},
	}}
	factory := &readyRecoveryRunnableFactory{busyStarted: make(chan struct{}), allowBusyFinish: make(chan struct{})}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}}, log,
		WithErrorLog(io.Discard), WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}), WithRunnableFactory(factory), WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }, now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }, waitOwnerPulse: func(id string) <-chan time.Time {
			pulseMu.Lock()
			defer pulseMu.Unlock()
			ch := make(chan time.Time, 1)
			pulses[id] = ch
			return ch
		}}))
	request := Request{Issues: []int{42, 43, 41}, RunTS: "261004120000", RunShortID: "leases", Branches: map[int]string{42: "42-parent", 43: "43-busy", 41: "41-child"}, Dependencies: map[int][]int{41: {42}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = o.RunBatch(ctx, request) }()
	select {
	case <-factory.busyStarted:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("independent sibling did not start")
	}
	ids := []string{buildRunID(42, request.RunTS, request.RunShortID), buildRunID(41, request.RunTS, request.RunShortID)}
	deadline := time.Now().Add(3 * time.Second)
	for {
		pulseMu.Lock()
		parent, child := pulses[ids[0]], pulses[ids[1]]
		pulseMu.Unlock()
		if parent != nil && child != nil && countEventsByType(log.snapshot(), "run.await") > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("returned waiting rows never established owner renewers")
		}
		time.Sleep(time.Millisecond)
	}
	future := now.Add(6 * time.Minute)
	clock.Store(future.UnixNano())
	pulseMu.Lock()
	for _, id := range ids {
		pulses[id] <- future
	}
	pulseMu.Unlock()
	deadline = time.Now().Add(3 * time.Second)
	for {
		valid := true
		for _, id := range ids {
			record, err := daemon.ReadRunWait(layout.BatchDir(issueBatchIDForRequest(request)), id)
			if err != nil || !record.LeaseExpiresAt.After(future) {
				valid = false
			}
		}
		if valid {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("batch-owned returned intent lost renewal after original grace expired")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("batch cancellation did not stop lease owners")
	}
}

type failedRecoveredAwaitLog struct{ spyEventLog }

func (l *failedRecoveredAwaitLog) Log(event events.Event) error {
	if event.Type == "run.await" && event.Payload["recovered"] == true {
		return errors.New("recovered ownership append unavailable")
	}
	return l.spyEventLog.Log(event)
}

func TestExternalWaitTakeoverRejectsOwnershipAppendFailure(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	layout := paths.NewLayout(nil, root)
	now := time.Now().UTC()
	log := &failedRecoveredAwaitLog{spyEventLog: spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"batch_id": "old", "branch": "42-fix"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now, Payload: map[string]any{"ci_wait": map[string]any{"deadline_unix_seconds": now.Add(30 * time.Minute).Unix()}}},
	}}}
	if err := daemon.RenewRunWait(layout.BatchDir("old"), daemon.RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42, Branch: "42-fix", BaseBranch: "main", OperationID: "ci:old", OperationDeadline: now.Add(30 * time.Minute)}, now); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(layout.WorktreeDir, "42-fix", ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(task), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(task, []byte("# Task\nPreserved work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := Request{RunTS: "261004120000", RunShortID: "append"}
	if err := ApplyReadyContinuations(&request, FindReadyContinuations(log.snapshot(), layout), layout, 1800); err != nil {
		t.Fatal(err)
	}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{}}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory))
	result, _ := o.RunBatch(context.Background(), request)
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "aborted" || len(factory.created) != 0 || len(FindReadyContinuations(log.snapshot(), layout)) != 0 {
		t.Fatalf("failed ownership publication admitted execution: result=%+v launches=%v events=%v", result, factory.created, log.snapshot())
	}
}

func TestRecoverableWaitingAdmissionRequiresNoInventedTask(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	now := time.Now().UTC()
	log := []events.Event{{Type: "run.queued", RunID: "initial", Issue: 42, Timestamp: now, Payload: map[string]any{"batch_id": "old"}}}
	if err := daemon.RenewRunWait(layout.BatchDir("old"), daemon.RunWait{
		Protocol: "run-wait/v1", RunID: "initial", BatchID: "old", Issue: 42, BaseBranch: "main", InitialAdmission: true, Ready: true, OperationID: "admission", Dependencies: []int{43},
	}, now); err != nil {
		t.Fatal(err)
	}
	ready := FindReadyContinuations(log, layout)
	if len(ready) != 1 {
		t.Fatalf("ownerless initial admission not discovered: %v", ready)
	}
	request := Request{}
	if err := ApplyReadyContinuations(&request, ready, layout, 1800); err != nil {
		t.Fatal(err)
	}
	if request.IssueMode(42) != ModeFresh || request.RunIDs[42] != "initial" || len(request.TaskPrompts) != 0 || !equalPriorityInts(request.Dependencies[42], []int{43}) {
		t.Fatalf("initial admission invented continuation artifacts: %#v", request)
	}
	log = append(log, events.Event{Type: "run.aborted", RunID: "initial", Issue: 42, Timestamp: now.Add(time.Second)})
	if got := FindReadyContinuations(log, layout); len(got) != 0 {
		t.Fatalf("explicit abort rehydrated: %v", got)
	}
}

func TestExternalWaitRehydratesWithinFixedGrace(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	now := time.Now().UTC()
	log := []events.Event{
		{Type: "run.started", RunID: "waiting", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"batch_id": "old", "branch": "42-wait"}},
		{Type: "run.await", RunID: "waiting", Issue: 42, Timestamp: now, Payload: map[string]any{"await_reason": "pending", "ci_wait": map[string]any{"deadline_unix_seconds": now.Add(time.Hour).Unix()}}},
	}
	if err := daemon.RenewRunWait(layout.BatchDir("old"), daemon.RunWait{
		Protocol: "run-wait/v1", RunID: "waiting", BatchID: "old", Issue: 42, Branch: "42-wait", BaseBranch: "main",
		OperationID: "ci:17:head", OperationDeadline: now.Add(time.Hour),
	}, now); err != nil {
		t.Fatal(err)
	}
	ready := FindReadyContinuations(log, layout)
	if len(ready) != 1 || ready[0].Wait == nil || ready[0].Wait.Ready {
		t.Fatalf("external wait lost readiness distinction: %v", ready)
	}
	if err := daemon.RenewRunWait(layout.BatchDir("old"), *ready[0].Wait, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := FindReadyContinuations(log, layout); len(got) != 0 {
		t.Fatalf("expired grace was renewed by discovery: %v", got)
	}
}

func TestRunBatchRehydratesReadyContinuationAfterRestart(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	initGitRepo(t, workDir)
	layout := paths.NewLayout(&config.Config{WorktreeDir: ".sandman/worktrees"}, workDir)

	const (
		issueReady  = 42
		issueBusy   = 43
		readyID     = "260929120000-abcd-42"
		oldBatchID  = "260929120000-abcd-42"
		readyBranch = "42-ready"
		busyBranch  = "43-busy"
	)
	started := time.Now().UTC().Add(-2 * time.Minute)
	awaited := started.Add(time.Minute)
	queued := awaited.Add(time.Minute)
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", Timestamp: started, RunID: readyID, Issue: issueReady, Payload: map[string]any{
			"branch": readyBranch, "base_branch": "main", "batch_id": oldBatchID,
		}},
		{Type: "run.await", Timestamp: awaited, RunID: readyID, Issue: issueReady, Payload: map[string]any{"gate": "pending", "await": true}},
		{Type: "run.capacity_queued", Timestamp: queued, RunID: readyID, Issue: issueReady, Payload: map[string]any{
			"ready_continuation": true, "branch": readyBranch, "base_branch": "main", "batch_id": oldBatchID,
			"previous_run_id": readyID, "previous_run_batch_id": oldBatchID, "issue_title": "Ready issue",
		}},
	}}
	ready := FindReadyContinuations(log.snapshot(), layout)
	if len(ready) != 1 {
		t.Fatalf("discovered ready continuations = %#v, want one", ready)
	}

	readyTask := filepath.Join(layout.WorktreeDir, readyBranch, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(readyTask), 0o755); err != nil {
		t.Fatalf("create ready worktree: %v", err)
	}
	if err := os.WriteFile(readyTask, []byte("# Task\n\nMerge the approved PR.\n"), 0o600); err != nil {
		t.Fatalf("write ready Task: %v", err)
	}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			issueReady: {Number: issueReady, State: "open", Title: "Ready issue"},
			issueBusy:  {Number: issueBusy, State: "open", Title: "Busy issue"},
		},
		prs: map[string]*github.PR{
			readyBranch: {Number: 42, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: readyBranch},
			busyBranch:  {Number: 43, State: "merged", Merged: true, Body: "Closes #43", HeadRefName: busyBranch},
		},
	}
	factory := &readyRecoveryRunnableFactory{busyStarted: make(chan struct{}), allowBusyFinish: make(chan struct{})}
	request := Request{
		Issues:     []int{issueBusy},
		Branches:   map[int]string{issueBusy: busyBranch},
		RunTS:      "260929123000",
		RunShortID: "cafe",
		Parallel:   1,
		PromptConfig: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 1800,
		},
	}
	if err := ApplyReadyContinuations(&request, ready, layout, 1800); err != nil {
		t.Fatalf("rehydrate ready continuation: %v", err)
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(reviewWaitSchedulerSandboxFactory{}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			baseBranchSync: func(string, string) error { return nil },
			currentHead:    func(string) (string, error) { return "current-sha", nil },
		}),
	)

	done := make(chan struct{})
	var result *Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = o.RunBatch(context.Background(), request)
	}()
	select {
	case <-factory.busyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary row did not acquire capacity")
	}
	if starts := factory.startsSnapshot(); len(starts) != 1 || starts[0] != issueBusy {
		t.Fatalf("ready continuation started before the occupied slot freed: %v", starts)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		finished := false
		for _, state := range events.ProjectRunStates(log.snapshot()) {
			if state.RunID == readyID && state.IsTerminal() && state.Status() == "success" {
				finished = true
			}
		}
		if finished {
			break
		}
		if time.Now().After(deadline) {
			close(factory.allowBusyFinish)
			t.Fatal("merged ready continuation required the occupied execution slot to finish")
		}
		time.Sleep(time.Millisecond)
	}
	close(factory.allowBusyFinish)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rehydrated continuation did not start after capacity became available")
	}
	if runErr != nil {
		t.Fatalf("RunBatch: %v; events=%v", runErr, log.snapshot())
	}
	if result == nil || len(result.Runs) != 2 {
		t.Fatalf("result = %#v, want two runs", result)
	}
	if starts := factory.startsSnapshot(); len(starts) != 1 || starts[0] != issueBusy {
		t.Fatalf("runnable starts = %v, want only the busy row; merged ready row should revalidate without relaunch", starts)
	}
	var continued bool
	for _, event := range log.snapshot() {
		if event.Type == "run.continued" && event.RunID == readyID {
			continued = true
		}
	}
	if continued {
		t.Fatalf("terminal observation must not restart active time for %q; events=%v", readyID, log.snapshot())
	}
	states := events.ProjectRunStates(log.snapshot())
	for _, state := range states {
		if state.RunID == readyID && (state.IsAwaiting() || state.IsCapacityQueued() || state.IsActive()) {
			t.Fatalf("ready continuation did not reach verified terminal success: %#v", state)
		}
	}
}

func TestRecoveryRevalidatesTerminalityAndGraceUnderClaim(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		initial, abort, expire bool
		legacy                 bool
	}{{"initial-abort-after-discovery", true, true, false, false}, {"external-abort-after-discovery", false, true, false, false}, {"grace-expires-after-discovery", true, false, true, false}, {"legacy-grace-expires-after-discovery", false, false, true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			initGitRepo(t, root)
			layout := paths.NewLayout(nil, root)
			now := time.Now().UTC()
			wait := daemon.RunWait{Protocol: "run-wait/v1", RunID: "recovery", BatchID: "old", Issue: 42, Branch: "42-fix", BaseBranch: "main", InitialAdmission: tc.initial, Ready: tc.initial, OperationID: "admission"}
			if err := daemon.RenewRunWait(layout.BatchDir("old"), wait, now); err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				if err := os.Remove(filepath.Join(layout.BatchDir("old"), "runs", wait.RunID, "wait.json")); err != nil {
					t.Fatal(err)
				}
			}
			kind := "run.started"
			if tc.initial {
				kind = "run.queued"
			}
			log := &spyEventLog{events: []events.Event{{Type: kind, Timestamp: now, RunID: wait.RunID, Issue: 42, Payload: map[string]any{"batch_id": "old", "branch": wait.Branch, "base_branch": "main"}}}}
			if !tc.initial {
				_ = log.Log(events.Event{Type: "run.await", Timestamp: now, RunID: wait.RunID, Issue: 42, Payload: map[string]any{"await_reason": "usage-limit", "usage_limit_deadline_unix_seconds": now.Add(5 * time.Hour).Unix()}})
			}
			if tc.legacy {
				if err := logCapacityQueuedContinuation(log, wait.RunID, 42, "old", RowSpec{Branches: map[int]string{42: wait.Branch}, BaseBranch: "main"}, nil, "Legacy"); err != nil {
					t.Fatal(err)
				}
			}
			ready := FindReadyContinuations(log.snapshot(), layout)
			if len(ready) != 1 {
				t.Fatalf("discovery=%+v", ready)
			}
			req := Request{RunTS: "261004120000", RunShortID: "new", Parallel: 1}
			if !tc.initial {
				path := filepath.Join(layout.WorktreeDir, wait.Branch, ".sandman", "task.md")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("# Task\nPreserved work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ApplyReadyContinuations(&req, ready, layout, 1800); err != nil {
				t.Fatal(err)
			}
			if tc.abort {
				_ = log.Log(events.Event{Type: "run.aborted", Timestamp: now.Add(time.Second), RunID: wait.RunID, Issue: 42})
			}
			if tc.expire && !tc.legacy {
				if err := daemon.RenewRunWait(layout.BatchDir("old"), wait, now.Add(-10*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			factory := &controlledRunnableFactory{runnables: map[int]Runnable{}}
			opts := runSessionOptions{}
			if tc.legacy {
				opts.now = func() time.Time { return now.Add(10 * time.Minute) }
			}
			o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test", Sandbox: "worktree", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory), WithRunSessionOpts(opts))
			result, err := o.RunBatch(context.Background(), req)
			if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "aborted" || len(factory.created) != 0 {
				t.Fatalf("recovery resurrected: result=%+v err=%v launches=%v", result, err, factory.created)
			}
		})
	}
}

func readyContinuationEvents(runID, batchID string, started, awaited, queued time.Time) []events.Event {
	return []events.Event{
		{Type: "run.started", Timestamp: started, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "base_branch": "main", "batch_id": batchID}},
		{Type: "run.await", Timestamp: awaited, RunID: runID, Issue: 42, Payload: map[string]any{"gate": "pending", "await": true}},
		{Type: "run.capacity_queued", Timestamp: queued, RunID: runID, Issue: 42, Payload: map[string]any{
			"ready_continuation": true, "branch": "42-fix", "base_branch": "main", "batch_id": batchID,
			"previous_run_id": runID, "previous_run_batch_id": batchID, "issue_title": "Fix bug",
		}},
	}
}

type readyRecoveryRunnableFactory struct {
	mu              sync.Mutex
	starts          []int
	busyStarted     chan struct{}
	allowBusyFinish chan struct{}
}

func (f *readyRecoveryRunnableFactory) NewRunnable(issue *github.Issue, _ string, _ sandbox.Sandbox) Runnable {
	return &readyRecoveryRunnable{factory: f, issue: issue.Number}
}

type readyRecoveryRunnable struct {
	factory *readyRecoveryRunnableFactory
	issue   int
}

func (r *readyRecoveryRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	f := r.factory
	f.mu.Lock()
	f.starts = append(f.starts, r.issue)
	f.mu.Unlock()
	if r.issue == 43 {
		select {
		case <-f.busyStarted:
		default:
			close(f.busyStarted)
		}
		select {
		case <-f.allowBusyFinish:
		case <-ctx.Done():
			return AgentRunResult{IssueNumber: r.issue, Status: "aborted"}
		}
	}
	return AgentRunResult{IssueNumber: r.issue, Status: "success"}
}

func (f *readyRecoveryRunnableFactory) startsSnapshot() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.starts...)
}
