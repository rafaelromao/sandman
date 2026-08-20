package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// runLifecycleCaseForIssue runs one issue-driven session through the
// production RunExecutor path with the given mode, mirroring
// runCleanGateCaseForIssue but parameterizing continuation so a fresh run and
// a --continue can be compared with the identical PR facts.
func runLifecycleCaseForIssue(t *testing.T, pr *github.PR, mode IssueMode, prevRunID string) (AgentRunResult, []events.Event, int) {
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
	worktreePath := sb.WorkDir()
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
	}}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs:    map[string]*github.PR{gateTestBranch: pr},
	}
	runOpts := gateTestRunOptions()
	if mode == ModeContinue {
		runOpts.awaitResumeMax = 1
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runOpts),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: gateTestBranch},
		BaseBranch:  "main",
	}
	if mode == ModeContinue {
		row.Mode = ModeContinue
		row.PreviousRunIDs = map[int]string{42: prevRunID}
	}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), row)
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return result, logs, len(factory.created)
}

// finishedStatus is the terminal-status extraction helper: it looks up the last
// run.finished event and asserts it exists before returning its status.
func finishedStatus(t *testing.T, logs []events.Event) string {
	t.Helper()
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatalf("run.finished event not found: %v", logs)
	}
	status, _ := finished.Payload["status"].(string)
	return status
}

// Slice 1 of the implementation-PR lifecycle consolidation (issue #2596):
// every merged-PR outcome funnels through decideImplementationPRLifecycle so
// the run session has one decision point to fold the remaining gates into in
// later slices.

func TestDecideImplementationPRLifecycle_MergedWithClosingIntent(t *testing.T) {
	d := decideImplementationPRLifecycle(implementationPRFacts{
		pr:      &github.PR{Number: 42, State: "merged", Merged: true, Body: "Closes #42"},
		headSHA: "current-sha",
		mergeFacts: &mergedMergeFacts{
			mergedWithClosingIntent: true,
		},
	})
	if d.action != lifecycleSuccess || !d.handled {
		t.Fatalf("decision = %+v, want handled lifecycleSuccess", d)
	}
	if got := lifecycleStatusRepr(d); got != "success" {
		t.Fatalf("status = %q, want success", got)
	}
	if d.completionFailure || d.needMergeFacts {
		t.Fatalf("decision carries unexpected flags: %+v", d)
	}
	if extras := lifecycleFailureExtras(d, 42); extras != nil {
		t.Fatalf("success extras = %#v, want nil", extras)
	}
}

func TestDecideImplementationPRLifecycle_MergedStateWithoutMergedFlag(t *testing.T) {
	d := decideImplementationPRLifecycle(implementationPRFacts{
		pr:      &github.PR{Number: 42, State: "merged", Merged: false, Body: "Closes #42"},
		headSHA: "current-sha",
		mergeFacts: &mergedMergeFacts{
			mergedWithClosingIntent: true,
		},
	})
	if d.action != lifecycleSuccess || !d.handled {
		t.Fatalf("decision = %+v, want handled lifecycleSuccess (State=merged gate resolves)", d)
	}
}

func TestDecideImplementationPRLifecycle_MergedMissingClosingRefRequestsFacts(t *testing.T) {
	d := decideImplementationPRLifecycle(implementationPRFacts{
		pr:      &github.PR{Number: 42, State: "merged", Merged: true, Body: "Refs #42"},
		headSHA: "current-sha",
	})
	if !d.needMergeFacts || d.handled {
		t.Fatalf("decision = %+v, want needMergeFacts without a terminal action", d)
	}

	withRef := decideImplementationPRLifecycle(implementationPRFacts{
		pr:      &github.PR{Number: 42, State: "merged", Merged: true, Body: "Refs #42"},
		headSHA: "current-sha",
		mergeFacts: &mergedMergeFacts{
			mergedWithoutClosingRef: true,
		},
	})
	if withRef.action != lifecycleFailure || !withRef.handled || !withRef.completionFailure {
		t.Fatalf("decision = %+v, want handled lifecycleFailure with completionFailure", withRef)
	}
	if got := lifecycleStatusRepr(withRef); got != "failure" {
		t.Fatalf("status = %q, want failure", got)
	}
	extras := lifecycleFailureExtras(withRef, 42)
	completion, ok := extras["completion"].(map[string]any)
	if !ok || completion["reason"] != "merged-pr-missing-closing-reference" {
		t.Fatalf("failure extras = %#v, want completion missing-closing-reference", extras)
	}
}

func TestDecideImplementationPRLifecycle_UnverifiableMergedFails(t *testing.T) {
	d := decideImplementationPRLifecycle(implementationPRFacts{
		pr:      &github.PR{Number: 42, State: "merged", Merged: true, Body: "Refs #42"},
		headSHA: "current-sha",
		mergeFacts: &mergedMergeFacts{
			mergedWithClosingIntent: false,
			mergedWithoutClosingRef: false,
		},
	})
	if !d.handled || d.needMergeFacts || d.action != lifecycleFailure || !d.completionFailure {
		t.Fatalf("decision = %+v, want handled completion failure for unverifiable merge", d)
	}
	if d.gate != lifecycleGateResolved {
		t.Fatalf("gate = %q, want resolved failure", d.gate)
	}
}

func TestDecideImplementationPRLifecycle_NonResolvedGates(t *testing.T) {
	cases := []struct {
		name       string
		pr         *github.PR
		wantGate   lifecycleGate
		wantAction lifecycleAction
	}{
		{
			name: "pending ci",
			pr: &github.PR{Number: 42, State: "open", StatusCheckRollup: "pending",
				ReviewDecision: "APPROVED", MergeStateStatus: "BLOCKED", HeadRefOid: "current-sha"},
			wantGate: lifecycleGatePending, wantAction: lifecycleAwait,
		},
		{
			name: "ci failure",
			pr: &github.PR{Number: 42, State: "open", StatusCheckRollup: "failure",
				ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN", HeadRefOid: "current-sha"},
			wantGate: lifecycleGateFailed, wantAction: lifecycleAwait,
		},
		{
			name: "changes requested",
			pr: &github.PR{Number: 42, State: "open", StatusCheckRollup: "success",
				ReviewDecision: "CHANGES_REQUESTED", MergeStateStatus: "CLEAN", HeadRefOid: "current-sha"},
			wantGate: lifecycleGateFailed, wantAction: lifecycleAwait,
		},
		{
			name: "ready to merge",
			pr: &github.PR{Number: 42, State: "open", MergeStateStatus: "CLEAN",
				HeadRefOid: "current-sha"},
			wantGate: lifecycleGateReady, wantAction: lifecycleAwait,
		},
		{
			name: "head drifted",
			pr: &github.PR{Number: 42, State: "open", MergeStateStatus: "CLEAN",
				HeadRefOid: "other-sha"},
			wantGate: lifecycleGatePending, wantAction: lifecycleAwait,
		},
		{
			name:     "unavailable state",
			pr:       &github.PR{Number: 42, State: "closed", Merged: false},
			wantGate: lifecycleGateUnavailable, wantAction: lifecycleFailure,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decideImplementationPRLifecycle(implementationPRFacts{
				pr:      tc.pr,
				headSHA: "current-sha",
			})
			if !d.handled || d.needMergeFacts || d.action != tc.wantAction {
				t.Fatalf("decision = %+v, want handled %v", d, tc.wantAction)
			}
			if d.gate != tc.wantGate {
				t.Fatalf("gate = %q, want %q", d.gate, tc.wantGate)
			}
		})
	}
}

func TestDecideImplementationPRLifecycle_EmptyPRHeadAwaits(t *testing.T) {
	d := decideImplementationPRLifecycle(implementationPRFacts{
		pr:      &github.PR{Number: 42, State: "open", MergeStateStatus: "CLEAN"},
		headSHA: "",
	})
	if !d.handled || d.action != lifecycleAwait || d.gate != lifecycleGatePending {
		t.Fatalf("empty head decision = %+v, want handled pending await", d)
	}
}

func TestDecideImplementationPRLifecycle_NilPRUnhandled(t *testing.T) {
	d := decideImplementationPRLifecycle(implementationPRFacts{headSHA: "current-sha"})
	if d.handled || d.gate != lifecycleGateNone {
		t.Fatalf("nil PR decision = %+v, want unhandled none", d)
	}
}

// Characterization: terminal merged outcomes must not write an ## External
// Gate task section, a gate log line, or a blocker key on their finished
// event. These are the invariants the lifecycle adapter preserves when the
// merged arm stops consulting confirmExternalGate's blocked/unverified tail.
func TestLifecycle_MergedWithClosingIntentTerminalizesWithoutBlocker(t *testing.T) {
	result, logs, launches := runCleanGateCaseForIssue(t, "open", &github.PR{
		Number:      42,
		State:       "merged",
		Merged:      true,
		Body:        "Closes #42",
		HeadRefOid:  "current-sha",
		HeadRefName: gateTestBranch,
	})
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0", got)
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatal("run.finished event not found")
	}
	if got := finished.Payload["status"]; got != "success" {
		t.Fatalf("terminal status = %v, want success", got)
	}
	for _, key := range []string{"blocker", "gate", "completion"} {
		if _, ok := finished.Payload[key]; ok {
			t.Fatalf("finished payload carries unexpected terminal key %q: %#v", key, finished.Payload)
		}
	}
}

func TestLifecycle_MergedWithoutClosingReferenceTerminalizesFailure(t *testing.T) {
	result, logs, launches := runCleanGateCaseForIssue(t, "open", &github.PR{
		Number:      42,
		State:       "merged",
		Merged:      true,
		Body:        "Refs #42",
		HeadRefOid:  "current-sha",
		HeadRefName: gateTestBranch,
	})
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0", got)
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatal("run.finished event not found")
	}
	if got := finished.Payload["status"]; got != "failure" {
		t.Fatalf("terminal status = %v, want failure", got)
	}
	completion, ok := finished.Payload["completion"].(map[string]any)
	if !ok || completion["reason"] != "merged-pr-missing-closing-reference" {
		t.Fatalf("completion diagnostic = %#v, want merged-pr-missing-closing-reference", finished.Payload["completion"])
	}
	for _, key := range []string{"blocker", "gate"} {
		if _, ok := finished.Payload[key]; ok {
			t.Fatalf("finished payload carries unexpected terminal key %q: %#v", key, finished.Payload)
		}
	}
}

// Determinism (B1.3): a fresh run and a --continue must produce the identical
// lifecycle decision and terminal event for the same PR facts. Both merged
// arms (verified merge with closing intent, merge without closing reference)
// are exercised against the production RunExecutor path.
func TestLifecycle_MergedDeterministicAcrossFreshAndContinue(t *testing.T) {
	pr := func(body string) *github.PR {
		return &github.PR{
			Number:      42,
			State:       "merged",
			Merged:      true,
			Body:        body,
			HeadRefOid:  "current-sha",
			HeadRefName: gateTestBranch,
		}
	}

	// Arm 1: merged with closing intent terminalizes success, identically for
	// a fresh run and a continuation.
	freshResult, freshLogs, _ := runLifecycleCaseForIssue(t, pr("Closes #42"), ModeFresh, "")
	continueResult, continueLogs, _ := runLifecycleCaseForIssue(t, pr("Closes #42"), ModeContinue, "prior-run")
	if s := finishedStatus(t, freshLogs); s != "success" {
		t.Fatalf("fresh merged-with-closing status = %q, want success", s)
	}
	if s := finishedStatus(t, continueLogs); s != "success" {
		t.Fatalf("continue merged-with-closing status = %q, want success", s)
	}
	freshFinished := findEvent(freshLogs, "run.finished")
	continueFinished := findEvent(continueLogs, "run.finished")
	for _, key := range []string{"status", "blocker", "gate", "completion"} {
		if got := freshFinished.Payload[key]; got != continueFinished.Payload[key] {
			t.Fatalf("merged-with-closing payload key %q differs: fresh=%v continue=%v", key, got, continueFinished.Payload[key])
		}
	}
	if freshFinished.Payload["status"] != "success" {
		t.Fatalf("merged-with-closing terminal = %#v, want success", freshFinished.Payload)
	}

	// Arm 2: merged without closing reference terminalizes failure with the
	// completion diagnostic, identically for a fresh run and a continuation.
	freshResult2, freshLogs2, _ := runLifecycleCaseForIssue(t, pr("Refs #42"), ModeFresh, "")
	continueResult2, continueLogs2, _ := runLifecycleCaseForIssue(t, pr("Refs #42"), ModeContinue, "prior-run")
	if s := finishedStatus(t, freshLogs2); s != "failure" {
		t.Fatalf("fresh merged-without-closing status = %q, want failure", s)
	}
	if s := finishedStatus(t, continueLogs2); s != "failure" {
		t.Fatalf("continue merged-without-closing status = %q, want failure", s)
	}
	fresh2Finished := findEvent(freshLogs2, "run.finished")
	continue2Finished := findEvent(continueLogs2, "run.finished")
	fresh2Completion, _ := fresh2Finished.Payload["completion"].(map[string]any)
	continue2Completion, _ := continue2Finished.Payload["completion"].(map[string]any)
	if fresh2Completion["reason"] != "merged-pr-missing-closing-reference" || continue2Completion["reason"] != fresh2Completion["reason"] {
		t.Fatalf("merged-without-closing completion differs: fresh=%#v continue=%#v", fresh2Completion, continue2Completion)
	}

	_ = freshResult
	_ = continueResult
	_ = freshResult2
	_ = continueResult2
	if freshFinished.Payload["status"] != continueFinished.Payload["status"] {
		t.Fatal("fresh and continue terminal events differ")
	}
}

// Terminal merged-policy arms must not write a legacy gate task section
// into the worktree task.md (B1.4). The terminal tests above already pin the
// payload cleanliness; this pins the filesystem side for both merged arms.
func TestLifecycle_MergedTerminalWritesNoExternalGateTaskSection(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	cases := []struct {
		name string
		pr   *github.PR
		want string
	}{
		{
			name: "merged with closing intent",
			pr:   &github.PR{Number: 42, State: "merged", Merged: true, Body: "Closes #42", HeadRefOid: "current-sha", HeadRefName: branch},
			want: "success",
		},
		{
			name: "merged without closing reference",
			pr:   &github.PR{Number: 42, State: "merged", Merged: true, Body: "Refs #42", HeadRefOid: "current-sha", HeadRefName: branch},
			want: "failure",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: branch}}}
			eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
			client := &fakeGitHubClient{
				issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
				prs:    map[string]*github.PR{branch: tc.pr},
			}
			o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
				WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
			bc := BatchConfig{
				Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
				AgentName:        "opencode",
				AgentCfg:         config.Agent{Command: "echo hi"},
				IdentityResolver: noopIdentityResolver(),
				Retries:          3,
			}
			result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
				IssueNumber: 42, Branches: map[int]string{42: branch}, BaseBranch: "main",
			})
			if !started || result.Status != tc.want {
				t.Fatalf("result = (%t, %q), want started %s", started, result.Status, tc.want)
			}
			if strings.Contains(result.Status, "blocked") {
				t.Fatalf("terminal %s arrived via blocked", tc.want)
			}
			task, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "task.md"))
			if err != nil {
				t.Fatalf("read task: %v", err)
			}
			if strings.Contains(string(task), "External Gate") || strings.Contains(string(task), "external gate") {
				t.Fatalf("task.md gained a legacy gate section:\n%s", task)
			}
			logs, err := eventLog.Read()
			if err != nil {
				t.Fatalf("read events: %v", err)
			}
			finished := findEvent(logs, "run.finished")
			if finished == nil {
				t.Fatalf("run.finished event not found: %v", logs)
			}
			if finished.Payload["status"] != tc.want {
				t.Fatalf("terminal status = %v, want %s", finished.Payload["status"], tc.want)
			}
			for _, key := range []string{"blocker", "gate"} {
				if _, ok := finished.Payload[key]; ok {
					t.Fatalf("finished payload carries %q: %#v", key, finished.Payload)
				}
			}
		})
	}
}
