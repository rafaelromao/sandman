package batch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// TestRunBatch_StartSandboxFailurePreservesUnderlyingError is the regression
// test for run 260721104202-24a8-2316 (and sibling rows 2317, 2318, 2319 in
// batch 260721104202-24a8-2318+11). The user's symptom was:
//
//	events.jsonl: run.finished with early_failure=true, error="start sandbox"
//	stderr:       error: start sandbox for issue 2316: <underlying git error>
//
// The underlying git error string is the operator's only debug breadcrumb,
// but it lives in the live stderr at run time — not on disk anywhere. The
// fix preserves it in the persisted event payload under `error_message` so
// a future operator can read it from .sandman/events.jsonl after the fact.
//
// This test pins both contracts at the orchestrator seam:
//
//  1. The persisted run.finished event for issue 2316 must carry the user's
//     exact symptom (early_failure=true, error="start sandbox").
//  2. The persisted run.finished event must ALSO carry the underlying error
//     string (the one the operator sees in their live stderr) so it can be
//     read back later without rerunning.
//
// If either contract breaks, the operator loses the ability to diagnose the
// failure without rerunning.
func TestRunBatch_StartSandboxFailurePreservesUnderlyingError(t *testing.T) {
	underlying := errors.New("git worktree add: fatal: a branch named 'sandman/2316-...' already exists")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{2316: {Number: 2316, Title: "stale issue"}},
	}
	var errBuf bytes.Buffer
	cfg := &config.Config{
		Agent:       "test-agent",
		Sandbox:     "worktree",
		WorktreeDir: ".sandman/worktrees",
		Git:         config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"test-agent": {Command: "true"},
		},
	}
	spyLog := &spyEventLog{}

	failingSandbox := &fakeSandbox{startErr: underlying}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, spyLog,
		WithErrorLog(&errBuf),
		WithSandboxFactory(&fakeSandboxFactory{sandbox: failingSandbox}),
	)

	_, err := o.RunBatch(context.Background(), Request{
		Issues:     []int{2316},
		Mode:       map[int]IssueMode{2316: ModeOverride},
		Parallel:   1,
		RunTS:      orchTestRunTS,
		RunShortID: orchTestRunShortID,
	})
	if err == nil {
		t.Fatal("expected batch to surface a failure when the sandbox cannot start")
	}

	// Contract 1 — the persisted event reproduces the user's exact symptom.
	var finished *events.Event
	for i := range spyLog.events {
		e := &spyLog.events[i]
		if e.Type == "run.finished" && e.Issue == 2316 {
			finished = e
			break
		}
	}
	if finished == nil {
		t.Fatal("expected run.finished event for issue 2316 to be persisted")
	}
	if finished.Payload["status"] != "failure" {
		t.Fatalf("expected status=failure, got %v", finished.Payload["status"])
	}
	if v, _ := finished.Payload["early_failure"].(bool); !v {
		t.Fatalf("expected early_failure=true in run.finished payload, got %+v", finished.Payload)
	}
	if finished.Payload["error"] != "start sandbox" {
		t.Fatalf("expected error='start sandbox' in run.finished payload, got %v", finished.Payload["error"])
	}

	// Contract 2 — the operator's live stderr carries the underlying error.
	stderr := errBuf.String()
	if !strings.Contains(stderr, "start sandbox") {
		t.Fatalf("expected stderr to carry the orchestrator's start-sandbox error line, got: %s", stderr)
	}
	if !strings.Contains(stderr, underlying.Error()) {
		t.Fatalf("expected stderr to carry the underlying git error so the operator can debug, got: %s", stderr)
	}

	// Contract 3 — the persisted event payload ALSO carries the underlying
	// error message so a future operator can read it from .sandman/events.jsonl
	// without rerunning. This is the contract the original incident violates:
	// the user can only see the underlying error in their live terminal, and
	// once the run finishes that breadcrumb is gone forever.
	if msg, _ := finished.Payload["error_message"].(string); !strings.Contains(msg, underlying.Error()) {
		t.Fatalf("expected run.finished payload to carry the underlying error under error_message, got %+v\nstderr (live): %s", finished.Payload, stderr)
	}
}

// TestWorktreeSandboxStart_OverrideStaleBranchSucceeds documents a
// non-reproduction: against a clean stale-branch state (the prior aborted
// batch's leftover branches with no worktree dir or registration), the
// orchestrator's WorktreeSandbox.Start succeeds. This is the exact
// configuration that would have existed at 10:42:47 on 2026-07-21 if the
// prior aborted batch had cleanly torn down its worktrees.
//
// We keep this test as documentation of what we tried and ruled out, since
// the user's failure on run 260721104202-24a8-2316 cannot be reproduced from
// the bare stale-branch state alone — the host had some other artifact
// (likely a stale .git/worktrees/<basename> registration, a stale
// .git/index.lock, or another transient state) that we cannot reconstruct
// after the fact. The persisted diagnostic gap (Contract 3 above) is the
// actionable fix regardless of which exact git error fired.
func TestWorktreeSandboxStart_OverrideStaleBranchSucceeds(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	// Stale branch + no worktree dir + no registration.
	const branch = "2316-slice-2-stale"
	runGit(t, dir, "branch", branch, "main")

	wt := sandbox.NewWorktreeSandbox(dir, dir+"/.sandman/worktrees", branch, "main")
	if err := wt.Start(sandbox.SandboxStart{Override: true, StrandedReconcile: true}); err != nil {
		t.Fatalf("WorktreeSandbox.Start on a bare stale-branch state succeeded in our repro but the live host failed on 2026-07-21; this documents the non-reproduction: %v", err)
	}
}

// stopRunnableFactory returns a Runnable that exits cleanly so the orchestrator
// can complete the per-issue lifecycle after wt.Start succeeds. In the failing
// case the orchestrator short-circuits at wt.Start before reaching the runnable
// at all, so this is mostly defensive.
type stopRunnableFactory struct{}

func (stopRunnableFactory) NewRunnable(_ *github.Issue, _ string, _ sandbox.Sandbox) Runnable {
	return &stopRunnable{}
}

type stopRunnable struct{}

func (s *stopRunnable) Run(_ context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	return AgentRunResult{Status: "success"}
}
