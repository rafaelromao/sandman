package batch

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

func TestAFKContinuityContract_OpenCodeLimitWithHistoricalClosedPR(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	const branch = "42-historical-limit"
	historical := &github.PR{
		Number: 7, State: "closed", HeadRefName: branch, HeadRefOid: "historical-head",
		Body: "Closes #42", UpdatedAt: time.Now().Add(-24 * time.Hour),
	}
	client := &reviewWaitSchedulerGitHubClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Recover from quota", State: "open"}},
		prs:    map[string]*github.PR{branch: historical},
	}}
	assertHistorical := func() {
		t.Helper()
		pr, err := client.FindPRByBranch(context.Background(), branch)
		if err != nil || pr == nil || pr.Number != 7 || pr.State != "closed" || pr.Merged {
			t.Errorf("quota recovery changed historical PR: pr=%+v err=%v", pr, err)
		}
	}
	sb := &usageLimitRetrySandbox{workDir: filepath.Join(root, "worktree"), failures: 1}
	sb.onAttempt = func(int) { assertHistorical() }
	sb.onSuccess = func() {
		// Publish a different PR; the historical object remains closed forever.
		client.mu.Lock()
		defer client.mu.Unlock()
		client.prs[branch] = &github.PR{
			Number: 8, State: "merged", Merged: true, HeadRefName: branch,
			HeadRefOid: "current-head", Body: "Closes #42",
		}
	}
	log := &spyEventLog{}
	var waits []time.Duration
	cfg := &config.Config{
		Agent: "opencode", DefaultAgent: "opencode", Sandbox: "worktree",
		WorktreeDir: filepath.Join(root, "worktrees"), Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")},
	}
	o := NewOrchestrator(client, &retryRenderer{result: "# Task\n\nRecover and publish a new PR."}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&usageLimitRetrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(&usageLimitRetryRunnableFactory{}),
		WithRunSessionOpts(runSessionOptions{
			currentHead:          func(string) (string, error) { return "current-head", nil },
			releaseAwaitCapacity: true,
			awaitWait: func(_ context.Context, delay time.Duration) error {
				assertHistorical()
				waits = append(waits, delay)
				return nil
			},
		}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := o.RunBatch(ctx, Request{
		Issues: []int{42}, Branches: map[int]string{42: branch}, Agent: "opencode",
		Retries: 1, Parallel: 1, RunIdleTimeout: 0, RunIdleTimeoutSet: true,
		RunTS: "261029151914", RunShortID: "oldpr",
	})
	if err != nil || result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		t.Fatalf("historical PR prevented quota recovery: result=%+v err=%v events=%+v", result, err, log.snapshot())
	}
	if len(waits) != 1 || waits[0] != usageLimitPollInterval {
		t.Fatalf("waits = %v, want one quota poll", waits)
	}
	commands := sb.commandsSnapshot()
	if len(commands) != 2 || strings.Contains(commands[0], "--session") || !strings.Contains(commands[1], "--session 'usage-limit-session'") {
		t.Fatalf("commands = %q, want fresh entry then exact-session recovery", commands)
	}
	all := log.snapshot()
	for kind, want := range map[string]int{"run.started": 1, "run.await": 1, "run.continued": 1, "run.retry": 0, "run.resumed": 0, "run.finished": 1} {
		if got := countEventsByType(all, kind); got != want {
			t.Errorf("%s count = %d, want %d; events=%+v", kind, got, want, all)
		}
	}
	var runID string
	for _, event := range all {
		if event.Type == "run.started" {
			runID = event.RunID
		}
		if event.Type == "run.await" && event.Payload["await_reason"] != "usage-limit" {
			t.Errorf("await lost quota classification: %+v", event)
		}
		if event.Type == "run.await" || event.Type == "run.continued" || event.Type == "run.finished" {
			if runID == "" || event.RunID != runID {
				t.Errorf("recovery changed Run identity: started=%q event=%+v", runID, event)
			}
		}
	}
	if historical.State != "closed" || historical.Merged || historical.Number != 7 {
		t.Fatalf("historical PR was reopened or merged: %+v", historical)
	}
}

func TestHistoricalClosedPRObservation_AdmissionBoundaries(t *testing.T) {
	admitted := time.Unix(2_000, 0).UTC()
	const runID = "historical-boundaries"
	tests := []struct {
		name         string
		updated      time.Time
		head         string
		prHead       string
		headErr      error
		hostNotReady bool
		started      time.Time
		noStarted    bool
		foreignRun   bool
		unreadable   bool
		noEventLog   bool
		merged       bool
		wantIgnored  bool
	}{
		{name: "closed before original admission", updated: admitted.Add(-time.Second), head: "new", prHead: "old", started: admitted, wantIgnored: true},
		{name: "closed between admission and continuation", updated: admitted.Add(time.Second), head: "new", prHead: "old", started: admitted},
		{name: "closed exactly at admission", updated: admitted, head: "new", prHead: "old", started: admitted},
		{name: "unknown PR timestamp", head: "new", prHead: "old", started: admitted},
		{name: "unknown admission timestamp", updated: admitted.Add(-time.Second), head: "new", prHead: "old"},
		{name: "continuation without original admission", updated: admitted.Add(-time.Second), head: "new", prHead: "old", noStarted: true},
		{name: "another Run admission", updated: admitted.Add(-time.Second), head: "new", prHead: "old", started: admitted, foreignRun: true},
		{name: "missing local HEAD", updated: admitted.Add(-time.Second), prHead: "old", started: admitted},
		{name: "missing PR HEAD", updated: admitted.Add(-time.Second), head: "new", started: admitted},
		{name: "HEAD lookup error", updated: admitted.Add(-time.Second), head: "new", prHead: "old", started: admitted, headErr: errors.New("HEAD unavailable")},
		{name: "host paths not restored", updated: admitted.Add(-time.Second), head: "new", prHead: "old", started: admitted, hostNotReady: true},
		{name: "unreadable admission evidence", updated: admitted.Add(-time.Second), head: "new", prHead: "old", started: admitted, unreadable: true},
		{name: "missing event log", updated: admitted.Add(-time.Second), head: "new", prHead: "old", started: admitted, noEventLog: true},
		{name: "current closed PR", updated: admitted.Add(-time.Second), head: " SHA ", prHead: "sha", started: admitted},
		{name: "confirmed merge wins despite old head", updated: admitted.Add(-time.Second), head: "new", prHead: "old", started: admitted, merged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &spyEventLog{}
			if !tt.noStarted {
				id := runID
				if tt.foreignRun {
					id = "another-run"
				}
				if err := log.Log(events.Event{Type: "run.started", RunID: id, Timestamp: tt.started}); err != nil {
					t.Fatal(err)
				}
			}
			var eventLog events.EventLog = log
			if tt.unreadable {
				eventLog = unreadableEventLog{}
			}
			if tt.noEventLog {
				eventLog = nil
			}
			pr := &github.PR{Number: 7, State: "closed", Merged: tt.merged, HeadRefOid: tt.prHead, UpdatedAt: tt.updated, Body: "Closes #42"}
			session := &runSession{
				issueNumber: 42, issueState: "open",
				deps: runDeps{
					githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: pr}},
					eventLog:     eventLog, errorLog: io.Discard,
				},
				opts: runSessionOptions{currentHead: func(string) (string, error) { return tt.head, tt.headErr }},
			}
			request := lifecycleObservationRequest{
				WorkDir: t.TempDir(), Branch: gateTestBranch, RunID: runID,
				HostPathsReady: !tt.hostNotReady, AttemptStatus: "failure", AfterAgentAttempt: true,
			}
			// Reconstruct the observer after continuation, as scheduler re-entry
			// does. The later event must never replace the original admission.
			for _, continued := range []bool{false, true} {
				if continued {
					if err := log.Log(events.Event{Type: "run.continued", RunID: runID, Timestamp: admitted.Add(time.Hour)}); err != nil {
						t.Fatal(err)
					}
				}
				verdict := newLifecycleObserver(session).Observe(t.Context(), request)
				wantAction, wantKind := lifecycleFailure, lifecycleVerdictClosedPR
				if tt.wantIgnored {
					wantAction, wantKind = lifecycleNone, lifecycleVerdictOrdinary
				}
				if tt.merged {
					wantAction, wantKind = lifecycleSuccess, lifecycleVerdictCompletion
				}
				if verdict.Action != wantAction || verdict.Kind != wantKind {
					t.Fatalf("continued=%v: verdict=%+v, want action=%v kind=%v", continued, verdict, wantAction, wantKind)
				}
			}
		})
	}
}
