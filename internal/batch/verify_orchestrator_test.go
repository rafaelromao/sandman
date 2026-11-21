package batch

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// alreadyResolvedRun drives one issue-driven Run whose Task carries
// `## Status: already resolved` through the executor in mode. It returns the
// Run's result, its terminal `run.finished` payload and the fake tracker, so a
// test can assert both the Run's outcome and the state of the work item.
func alreadyResolvedRun(t *testing.T, mode IssueMode, openPR bool, outcome VerifyOutcome, checks []OracleCheck) (AgentRunResult, map[string]any, *fakeGitHubClient) {
	t.Helper()
	workDir := t.TempDir()
	t.Chdir(workDir)
	branch := "42-fix-bug"
	wtDir := filepath.Join(workDir, "worktree")
	rtSandbox := &retrySandbox{workDir: wtDir}
	if mode == ModeContinue {
		seedContinuationTask(t, wtDir)
	}

	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		if openPR {
			return true, 17, "MERGEABLE", nil
		}
		return false, 0, "", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	pr := (*github.PR)(nil)
	if openPR {
		pr = &github.PR{Number: 17, State: "open", Merged: false, HeadRefName: branch}
	}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", State: "open"}},
		prs:    map[string]*github.PR{branch: pr},
	}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(wtDir, ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		}),
		WithVerifyPath(VerifyPathFunc(func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
			return outcome, checks
		})),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Mode:        mode,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	if mode == ModeContinue {
		row.PreviousRunIDs = map[int]string{42: "prior-run"}
	}
	result, started := o.newRunExecutor(t.Context(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(t.Context(), row)
	if !started {
		t.Fatal("expected run to start")
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var payload map[string]any
	for _, e := range logs {
		if e.Type == "run.finished" {
			payload = e.Payload
		}
	}
	if payload == nil {
		t.Fatalf("run.finished event not found")
	}
	return result, payload, client
}

// assertWorkItemStillOpen pins that Sandman never writes the work item's
// state: the tracker still reports it open after the Run finished.
func assertWorkItemStillOpen(t *testing.T, client *fakeGitHubClient, number int) {
	t.Helper()
	issue, err := client.FetchIssue(t.Context(), number)
	if err != nil {
		t.Fatalf("fetch issue %d: %v", number, err)
	}
	if github.IsIssueClosed(issue) {
		t.Fatalf("issue #%d state = %q, want the work item left open by Sandman", number, issue.State)
	}
}

// TestRunSingle_AlreadyResolved_KeepsOutcomeAndLeavesWorkItemOpen pins that an
// already-resolved Run keeps exactly the outcome it has always had (verified
// success, verification failure, the conservative open-PR backstop, or a clean
// no-signal success) and never writes the work item's state. Fresh, override
// and --continue Runs take different branches of the executor, so every
// outcome is asserted in every mode.
func TestRunSingle_AlreadyResolved_KeepsOutcomeAndLeavesWorkItemOpen(t *testing.T) {
	outcomes := []struct {
		name             string
		openPR           bool
		verify           VerifyOutcome
		checks           []OracleCheck
		wantStatus       string
		wantVerification string // "" = no verification payload
		wantBlocker      string // "" = no blocker
	}{
		{
			name: "verified", openPR: true, verify: VerifyVerified,
			checks:     []OracleCheck{{Name: "decision", Details: map[string]any{"ran": 3}}},
			wantStatus: "success", wantVerification: "Verified",
		},
		{
			name: "failed", openPR: true, verify: VerifyFailed,
			checks:     []OracleCheck{{Name: "decision", Details: map[string]any{"failed": 1}}},
			wantStatus: "failure", wantVerification: "Failed",
		},
		{
			name: "no signal with open PR", openPR: true, verify: VerifyNoSignal,
			wantStatus: "failure", wantBlocker: "open-pr-blocks-already-resolved",
		},
		{
			name: "no signal without open PR", openPR: false, verify: VerifyNoSignal,
			wantStatus: "success",
		},
	}
	modes := []struct {
		name string
		mode IssueMode
	}{
		{name: "fresh", mode: ModeFresh},
		{name: "override", mode: ModeOverride},
		{name: "continue", mode: ModeContinue},
	}
	for _, oc := range outcomes {
		for _, m := range modes {
			t.Run(oc.name+"/"+m.name, func(t *testing.T) {
				result, payload, client := alreadyResolvedRun(t, m.mode, oc.openPR, oc.verify, oc.checks)
				if result.Status != oc.wantStatus {
					t.Fatalf("status = %q, want %q", result.Status, oc.wantStatus)
				}
				verification, hasVerification := payload["verification"].(map[string]any)
				if oc.wantVerification == "" {
					if hasVerification {
						t.Errorf("expected no verification payload, got %+v", payload["verification"])
					}
				} else {
					if !hasVerification {
						t.Fatalf("expected verification payload, got %T: %+v", payload["verification"], payload)
					}
					if verification["outcome"] != oc.wantVerification {
						t.Errorf("verification outcome = %v, want %s", verification["outcome"], oc.wantVerification)
					}
					if checks, ok := verification["checks"].([]any); !ok || len(checks) != len(oc.checks) {
						t.Fatalf("verification checks = %+v, want %d element(s)", verification["checks"], len(oc.checks))
					}
				}
				if oc.wantBlocker == "" {
					if blocker, ok := payload["blocker"]; ok && blocker != nil {
						t.Errorf("expected no blocker, got %v", blocker)
					}
				} else if payload["blocker"] != oc.wantBlocker {
					t.Errorf("blocker = %v, want %s", payload["blocker"], oc.wantBlocker)
				}
				assertWorkItemStillOpen(t, client, 42)
			})
		}
	}
}

// TestRunSingle_AlreadyResolved_AllAbstainFallsBackToBackstop pins
// the conservative-backstop is the LAST guard, not the first. When
// every oracle abstains and there is an open PR, the run ends in
// `failure` with the verbatim `open-pr-blocks-already-resolved`
// blocker payload (no `verification` payload attached because no
// oracle reported).
func TestRunSingle_AlreadyResolved_AllAbstainFallsBackToBackstop(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	branch := "42-fix-bug"
	wtDir := filepath.Join(workDir, "worktree")
	rtSandbox := &retrySandbox{workDir: wtDir}

	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 17, "MERGEABLE", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: false, HeadRefName: branch}},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(wtDir, ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		}),
		WithVerifyPath(VerifyPathFunc(func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
			return VerifyNoSignal, nil
		})),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(t.Context(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(t.Context(), row)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure (all abstain + open PR → conservative backstop)", result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var payload map[string]any
	for _, e := range logs {
		if e.Type == "run.finished" {
			payload = e.Payload
		}
	}
	if payload == nil {
		t.Fatalf("run.finished event not found")
	}
	if payload["blocker"] != "open-pr-blocks-already-resolved" {
		t.Errorf("blocker = %v, want open-pr-blocks-already-resolved", payload["blocker"])
	}
	if _, ok := payload["verification"]; ok {
		t.Errorf("expected no verification payload on all-abstain path, got %+v", payload["verification"])
	}
}

// TestRunSingle_AlreadyResolved_AllAbstainNoOpenPRSucceeds pins the
// other end of the spectrum: when every oracle abstains and the
// branch has no open PR, the run ends in `success` (the conservative
// backstop has nothing to block on).
func TestRunSingle_AlreadyResolved_AllAbstainNoOpenPRSucceeds(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	branch := "42-fix-bug"
	wtDir := filepath.Join(workDir, "worktree")
	rtSandbox := &retrySandbox{workDir: wtDir}

	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return false, 0, "", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: nil},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(wtDir, ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		}),
		WithVerifyPath(VerifyPathFunc(func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
			return VerifyNoSignal, nil
		})),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(t.Context(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(t.Context(), row)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success (all abstain + no open PR → clean alreadyResolved short-circuit)", result.Status)
	}
}

// TestRunSingle_AlreadyResolved_DecisionFailedFailsWithoutBackstop pins
// that a Decision `Failed` outcome (oracle proved the issue is NOT
// resolved) short-circuits with `failure` and does NOT consult the
// conservative backstop. The run ends in failure with a `verification`
// payload but no `blocker` (the blocker is reserved for the open-PR
// backstop).
func TestRunSingle_AlreadyResolved_DecisionFailedFailsWithoutBackstop(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	branch := "42-fix-bug"
	wtDir := filepath.Join(workDir, "worktree")
	rtSandbox := &retrySandbox{workDir: wtDir}

	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 17, "MERGEABLE", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: false, HeadRefName: branch}},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(wtDir, ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		}),
		WithVerifyPath(VerifyPathFunc(func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
			return VerifyFailed, []OracleCheck{{Name: "decision", Details: map[string]any{"failed": 1}}}
		})),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(t.Context(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(t.Context(), row)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure (Decision failed → orchestrator must respect the negative signal)", result.Status)
	}
	logs, _ := eventLog.Read()
	var payload map[string]any
	for _, e := range logs {
		if e.Type == "run.finished" {
			payload = e.Payload
		}
	}
	if payload == nil {
		t.Fatalf("run.finished event not found")
	}
	if _, ok := payload["verification"]; !ok {
		t.Errorf("expected verification payload on Decision-failed path")
	}
	if payload["blocker"] != nil {
		t.Errorf("expected no blocker on Decision-failed path, got %v", payload["blocker"])
	}
}

// TestRunSingle_VerifyPathReceivesPRSnapshot pins the slice-1
// integration: when the orchestrator's alreadyResolved arm runs the
// verify chain, the VerifyInput carries the PR snapshot fetched via
// FindPRByBranch. Without this, the CheapGate oracle would always
// abstain because its first guard is `if in.PR == nil`.
func TestRunSingle_VerifyPathReceivesPRSnapshot(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	branch := "42-fix-bug"
	wtDir := filepath.Join(workDir, "worktree")
	rtSandbox := &retrySandbox{workDir: wtDir}

	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 17, "MERGEABLE", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	var seenPR *github.PR
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs: map[string]*github.PR{
				branch: {
					Number:            17,
					State:             "open",
					Merged:            false,
					HeadRefName:       branch,
					ReviewDecision:    "APPROVED",
					MergeStateStatus:  "CLEAN",
					StatusCheckRollup: "success",
				},
			},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(wtDir, ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		}),
		WithVerifyPath(VerifyPathFunc(func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
			seenPR = in.PR
			return VerifyNoSignal, nil
		})),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	_, started := o.newRunExecutor(t.Context(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(t.Context(), row)
	if !started {
		t.Fatal("expected run to start")
	}
	if seenPR == nil {
		t.Fatal("verify path received nil PR — slice-1 fields never reach CheapGate in production")
	}
	if seenPR.ReviewDecision != "APPROVED" {
		t.Errorf("PR ReviewDecision = %q, want APPROVED", seenPR.ReviewDecision)
	}
	if seenPR.MergeStateStatus != "CLEAN" {
		t.Errorf("PR MergeStateStatus = %q, want CLEAN", seenPR.MergeStateStatus)
	}
	if seenPR.StatusCheckRollup != "success" {
		t.Errorf("PR StatusCheckRollup = %q, want success", seenPR.StatusCheckRollup)
	}
}

// TestRunSingle_AlreadyResolved_PreFilterRejectFallsBackToBackstop pins
// that a PreFilter reject outcome behaves like NoSignal: the
// conservative backstop runs. The PreFilter check is recorded in the
// verification payload so the operator can see why we abstained.
func TestRunSingle_AlreadyResolved_PreFilterRejectFallsBackToBackstop(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	branch := "42-fix-bug"
	wtDir := filepath.Join(workDir, "worktree")
	rtSandbox := &retrySandbox{workDir: wtDir}

	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 17, "MERGEABLE", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: false, HeadRefName: branch}},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(wtDir, ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "failure", Branch: branch},
			taskContent: "## Status: already resolved",
		}),
		WithVerifyPath(VerifyPathFunc(func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
			return VerifyNoSignal, []OracleCheck{{Name: "pre-filter", Details: map[string]any{"l1": false, "subset": false}}}
		})),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(t.Context(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(t.Context(), row)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure (PreFilter reject + open PR → conservative backstop)", result.Status)
	}
	logs, _ := eventLog.Read()
	var payload map[string]any
	for _, e := range logs {
		if e.Type == "run.finished" {
			payload = e.Payload
		}
	}
	if payload == nil {
		t.Fatalf("run.finished event not found")
	}
	if payload["blocker"] != "open-pr-blocks-already-resolved" {
		t.Errorf("blocker = %v, want open-pr-blocks-already-resolved", payload["blocker"])
	}
	verification, ok := payload["verification"].(map[string]any)
	if !ok {
		t.Fatalf("expected verification payload on PreFilter-reject path, got %+v", payload["verification"])
	}
	checks, ok := verification["checks"].([]any)
	if !ok || len(checks) != 1 {
		t.Fatalf("checks = %+v, want one element", verification["checks"])
	}
	entry, ok := checks[0].(map[string]any)
	if !ok || entry["name"] != "pre-filter" {
		t.Errorf("first check = %+v, want pre-filter", checks[0])
	}
}

// TestRunSingle_NonAlreadyResolvedPathUnchanged pins that the
// verify refactor does not change the non-alreadyResolved path.
// A run that finishes with `## Status: success` in task.md (not
// `## Status: already resolved`) never invokes the verify path; the
// orchestrator follows the existing branches untouched.
func TestRunSingle_NonAlreadyResolvedPathUnchanged(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	branch := "42-fix-bug"
	wtDir := filepath.Join(workDir, "worktree")
	rtSandbox := &retrySandbox{workDir: wtDir}

	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	oldLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 17, "MERGEABLE", nil
	}
	t.Cleanup(func() { lookupOpenPRFn = oldLookup })

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog := &events.JSONLLogger{Path: eventsPath}
	verifyCalled := false
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "merged", Merged: true, HeadRefName: branch}},
		},
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: rtSandbox}),
		WithRunnableFactory(&taskWritingRunnableFactory{
			taskPath:    filepath.Join(wtDir, ".sandman", "task.md"),
			result:      AgentRunResult{IssueNumber: 42, Status: "success", Branch: branch},
			taskContent: "## Status: success",
		}),
		WithVerifyPath(VerifyPathFunc(func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
			verifyCalled = true
			return VerifyNoSignal, nil
		})),
	)

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	bc := BatchConfig{
		Cfg:              cfg,
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	}
	result, started := o.newRunExecutor(t.Context(), bc, &retrySandboxFactory{sandbox: rtSandbox}, nil).Execute(t.Context(), row)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success (merged PR + non-alreadyResolved path unchanged)", result.Status)
	}
	if verifyCalled {
		t.Errorf("verify path should not be called when alreadyResolved is false")
	}
}
