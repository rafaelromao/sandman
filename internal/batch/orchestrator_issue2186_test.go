package batch

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestRunBatch_InBatchBlockerOrdering_ResolverWired(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker", State: "closed"},
			100: {Number: 100, Title: "Dependent", State: "open", BlockedBy: []int{42}},
		},
		prs: map[string]*github.PR{
			"sandman/42-blocker":    mergedPR("sandman/42-blocker", ""),
			"sandman/100-dependent": mergedPR("sandman/100-dependent", ""),
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}
	resolved, err := resolver.Resolve(context.Background(), []int{42, 100}, true)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	if len(resolved.Deps[100]) != 1 || resolved.Deps[100][0] != 42 {
		t.Fatalf("expected resolver to wire in-batch blocker 100->[42], got %v", resolved.Deps[100])
	}

	spyLog := &spyEventLog{}
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	dependentStarted := make(chan struct{})

	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42:  &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}, started: blockerStarted, release: releaseBlocker},
			100: &controlledRunnable{result: AgentRunResult{IssueNumber: 100, Status: "success"}, started: dependentStarted},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog, WithRunnableFactory(factory))

	done := make(chan struct{})
	var result *Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = o.RunBatch(context.Background(), Request{
			Issues:       resolved.Issues,
			Dependencies: resolved.Deps,
			Parallel:     2,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")
	assertNoSignal(t, dependentStarted, "dependent started before blocker completed")
	close(releaseBlocker)
	waitForSignal(t, dependentStarted, "expected dependent to start after blocker completed")
	waitForSignal(t, done, "expected batch to complete")
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	if len(factory.created) != 2 {
		t.Fatalf("expected both runnables to be created, got %v", factory.created)
	}

	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(result.Runs))
	}

	statuses := make(map[int]string)
	for _, run := range result.Runs {
		statuses[run.IssueNumber] = run.Status
	}
	if statuses[42] != "success" {
		t.Fatalf("expected blocker success, got %q", statuses[42])
	}
	if statuses[100] != "success" {
		t.Fatalf("expected dependent success, got %q", statuses[100])
	}

	var queuedFound bool
	var started100 bool
	var blocked100 bool
	for _, e := range spyLog.events {
		switch e.Type {
		case "run.queued":
			if e.Issue == 100 {
				if blockedBy, ok := e.Payload["blocked_by"].([]int); !ok || !reflect.DeepEqual(blockedBy, []int{42}) {
					t.Fatalf("expected run.queued for 100 to declare blocked_by [42], got %#v", e.Payload)
				}
				queuedFound = true
			}
		case "run.started":
			if e.Issue == 100 {
				started100 = true
			}
		case "run.blocked":
			if e.Issue == 100 {
				blocked100 = true
			}
		}
	}
	if !queuedFound {
		t.Fatal("expected run.queued event for dependent with blocked_by payload")
	}
	if !started100 {
		t.Fatal("expected run.started event for dependent after blocker closed")
	}
	if blocked100 {
		t.Fatal("did not expect run.blocked event for dependent")
	}
}

func TestRunBatch_InBatchBlockerFailure_EmitsRunBlocked(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker", State: "open"},
			100: {Number: 100, Title: "Dependent", State: "open", BlockedBy: []int{42}},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}
	resolved, err := resolver.Resolve(context.Background(), []int{42, 100}, true)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	spyLog := &spyEventLog{}
	blockerStarted := make(chan struct{})
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "failure"}, started: blockerStarted},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog, WithRunnableFactory(factory))

	done := make(chan struct{})
	var result *Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = o.RunBatch(context.Background(), Request{
			Issues:       resolved.Issues,
			Dependencies: resolved.Deps,
			Parallel:     2,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")
	waitForSignal(t, done, "expected batch to complete")
	if runErr == nil {
		t.Fatal("expected error when in-batch blocker fails")
	}

	if len(factory.created) != 1 || factory.created[0] != 42 {
		t.Fatalf("expected only blocker runnable to be created, got %v", factory.created)
	}

	var blockedEvent *events.Event
	for i := range spyLog.events {
		e := spyLog.events[i]
		if e.Type == "run.blocked" && e.Issue == 100 {
			blockedEvent = &e
		}
		if e.Type == "run.started" && e.Issue == 100 {
			t.Fatal("did not expect run.started for blocked dependent")
		}
	}
	if blockedEvent == nil {
		t.Fatal("expected run.blocked event for dependent")
	}
	blockedBy, ok := blockedEvent.Payload["blocked_by"].([]int)
	if !ok || !reflect.DeepEqual(blockedBy, []int{42}) {
		t.Fatalf("expected blocked_by [42], got %#v", blockedEvent.Payload["blocked_by"])
	}

	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(result.Runs))
	}
}

func TestRunBatch_InBatchBlockerSuccessButIssueOpen_EmitsRunBlocked(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker", State: "open"},
			100: {Number: 100, Title: "Dependent", State: "open", BlockedBy: []int{42}},
		},
		prs: map[string]*github.PR{
			"sandman/42-blocker":    mergedPR("sandman/42-blocker", ""),
			"sandman/100-dependent": mergedPR("sandman/100-dependent", ""),
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}
	resolved, err := resolver.Resolve(context.Background(), []int{42, 100}, true)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	spyLog := &spyEventLog{}
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}, started: blockerStarted, release: releaseBlocker},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog, WithRunnableFactory(factory))

	done := make(chan struct{})
	var result *Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = o.RunBatch(context.Background(), Request{
			Issues:       resolved.Issues,
			Dependencies: resolved.Deps,
			Parallel:     2,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")
	close(releaseBlocker)
	waitForSignal(t, done, "expected batch to complete")
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	var blockedEvent *events.Event
	for i := range spyLog.events {
		e := spyLog.events[i]
		if e.Type == "run.blocked" && e.Issue == 100 {
			blockedEvent = &e
		}
		if e.Type == "run.started" && e.Issue == 100 {
			t.Fatal("did not expect run.started for dependent when blocker issue remains open")
		}
	}
	if blockedEvent == nil {
		t.Fatal("expected run.blocked event for dependent when blocker issue remains open")
	}
	blockedBy, ok := blockedEvent.Payload["blocked_by"].([]int)
	if !ok || !reflect.DeepEqual(blockedBy, []int{42}) {
		t.Fatalf("expected blocked_by [42], got %#v", blockedEvent.Payload["blocked_by"])
	}
	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(result.Runs))
	}
}

func TestRunBatch_InBatchBlockerStaysQueuedUntilBlockerTerminal(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker", State: "closed"},
			100: {Number: 100, Title: "Dependent", State: "open", BlockedBy: []int{42}},
		},
		prs: map[string]*github.PR{
			"sandman/42-blocker":    mergedPR("sandman/42-blocker", ""),
			"sandman/100-dependent": mergedPR("sandman/100-dependent", ""),
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}
	resolved, err := resolver.Resolve(context.Background(), []int{42, 100}, true)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	spyLog := &spyEventLog{}
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	dependentStarted := make(chan struct{})

	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42:  &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}, started: blockerStarted, release: releaseBlocker},
			100: &controlledRunnable{result: AgentRunResult{IssueNumber: 100, Status: "success"}, started: dependentStarted},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog, WithRunnableFactory(factory))

	done := make(chan struct{})
	var result *Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = o.RunBatch(context.Background(), Request{
			Issues:       resolved.Issues,
			Dependencies: resolved.Deps,
			Parallel:     2,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")

	var queuedSeenFor100 bool
	for _, e := range spyLog.events {
		if e.Type == "run.queued" && e.Issue == 100 {
			queuedSeenFor100 = true
		}
		if e.Type == "run.started" && e.Issue == 100 {
			t.Fatal("dependent started while blocker was still running")
		}
	}
	if !queuedSeenFor100 {
		t.Fatal("expected run.queued event for dependent at batch start")
	}

	assertNoSignal(t, dependentStarted, "dependent started while blocker was still running")

	close(releaseBlocker)
	waitForSignal(t, dependentStarted, "expected dependent to start after blocker completes")
	waitForSignal(t, done, "expected batch to complete")
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(result.Runs))
	}
}
