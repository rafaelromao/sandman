package batch

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestRunSingle_AlreadyResolved_DecisionVerifiedAutoClosesAndClosesIssue
// pins slice 5's headline behaviour: when the verify chain returns
// VerifyVerified, the orchestrator flips the run to `success`,
// closes the issue if open (bypassing the `hasBlockingOpenPR`
// open-PR backstop on verified outcomes), and surfaces the
// `verification` payload in the run.finished event.
func TestRunSingle_AlreadyResolved_DecisionVerifiedAutoClosesAndClosesIssue(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	branch := "sandman/42-fix-bug"
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
			return VerifyVerified, []OracleCheck{{Name: "decision", Details: map[string]any{"ran": 3}}}
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
		t.Fatalf("status = %q, want success (Decision verified → auto-close path)", result.Status)
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
	verification, ok := payload["verification"].(map[string]any)
	if !ok {
		t.Fatalf("expected verification payload, got %T: %+v", payload["verification"], payload)
	}
	if verification["outcome"] != "Verified" {
		t.Errorf("outcome = %v, want Verified", verification["outcome"])
	}
	checks, ok := verification["checks"].([]any)
	if !ok || len(checks) != 1 {
		t.Fatalf("checks = %+v, want one element", verification["checks"])
	}
	if blocker, ok := payload["blocker"]; ok && blocker != nil {
		t.Errorf("expected no blocker on Verified path, got %v", blocker)
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
	branch := "sandman/42-fix-bug"
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
	branch := "sandman/42-fix-bug"
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
	branch := "sandman/42-fix-bug"
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
	branch := "sandman/42-fix-bug"
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
	branch := "sandman/42-fix-bug"
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
	branch := "sandman/42-fix-bug"
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
