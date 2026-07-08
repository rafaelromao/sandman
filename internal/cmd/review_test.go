package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/runid"
)

// fakePRGitHubClient is a test double that satisfies the FetchPR
// and ListOpenPRs surface area used by the review command tests.
type fakePRGitHubClient struct {
	*fakeGitHubClient
	pr         *github.PR
	prErr      error
	openPRs    []github.PR
	openPRErr  error
	prByNumber map[int]*github.PR
}

func (f *fakePRGitHubClient) FetchPR(ctx context.Context, number int) (*github.PR, error) {
	if f.prByNumber != nil {
		if pr, ok := f.prByNumber[number]; ok {
			return pr, nil
		}
	}
	return f.pr, f.prErr
}

func (f *fakePRGitHubClient) ListOpenPRs(ctx context.Context) ([]github.PR, error) {
	return f.openPRs, f.openPRErr
}

func (f *fakePRGitHubClient) ListPRComments(ctx context.Context, number int) ([]github.PRComment, error) {
	return nil, nil
}

// spyBatchRunnerWithCapture records the batch.Request passed in.
type spyBatchRunnerWithCapture struct {
	spyBatchRunner
	captured batch.Request
}

func (s *spyBatchRunnerWithCapture) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	s.captured = req
	return s.result, s.err
}

// spyBatchRunnerMultiCapture records all batch.Requests passed in.
type spyBatchRunnerMultiCapture struct {
	spyBatchRunner
	captured []batch.Request
}

func (s *spyBatchRunnerMultiCapture) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	s.captured = append(s.captured, req)
	return s.result, s.err
}

func (s *spyBatchRunnerMultiCapture) requests() []batch.Request {
	return s.captured
}

// newReviewDeps returns Dependencies for a review command test, isolated
// from the real repo via a fresh temp dir that is git-init'd and
// chdir'd into. The supplied cfg is wrapped in a fakeStore and a
// fresh fakeEventLog/Renderer/IssuePicker are wired. All 32 callers
// inherit isolation automatically. Tests that need a different
// review guard (issue #383) should build their own Dependencies.
func newReviewDeps(t *testing.T, gh github.Client, cfg *config.Config, runner batch.Runner) Dependencies {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0o755); err != nil {
		t.Fatalf("mkdir .sandman: %v", err)
	}
	initCmd := exec.Command("git", "init", "-q", dir)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	t.Chdir(dir)
	return Dependencies{
		BatchRunner:  runner,
		ConfigStore:  &fakeStore{config: cfg},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IssuePicker:  &fakeIssuePicker{},
		IsTTY:        func() bool { return false },
		RepoRoot:     ".",
	}
}

func TestReviewCmd_NoArgsStartsDaemon(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 42, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
		return fmt.Errorf("daemon reached")
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from injected daemon runner")
	}
	if !strings.Contains(err.Error(), "daemon reached") {
		t.Errorf("expected daemon branch to be reached, got: %v", err)
	}
}

func TestReviewCmd_DaemonModeCreatesReviewSock(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
		if err := os.MkdirAll(".sandman/reviews", 0755); err != nil {
			return err
		}
		broadcaster := daemon.NewBroadcaster()
		sock := daemon.NewControlSocketWithName(".sandman/reviews", "review.sock", broadcaster)
		if err := sock.Start(); err != nil {
			return err
		}
		defer sock.Stop()
		<-ctx.Done()
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, ".sandman", "reviews", "review.sock")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "reviews", "review.sock")); err != nil {
		t.Fatalf("review.sock not created: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cmd did not return after cancel")
	}
}

func TestReviewCmd_DaemonSocketAcceptsConnections(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runReviewDaemon(ctx, deps, cfg, "", 0, false, 0, false, "", "", 0, false) }()

	sockPath := filepath.Join(dir, ".sandman", "reviews", "review.sock")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("review.sock not created: %v", err)
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("connect to review.sock: %v", err)
	}
	defer conn.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error from runReviewDaemon: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runReviewDaemon did not return after cancel")
	}
}

func TestReviewCmd_OneShotRendersPromptAndInvokesBatch(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "openai/gpt-5",
		Agent:              "opencode",
		Sandbox:            "podman",
		WorktreeDir:        ".sandman/worktrees",
		Agents:             map[string]config.Agent{},
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr: &github.PR{
			Number: 17,
			Title:  "Refactor daemon",
			Body:   "Splits the orchestrator.",
		},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"17"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "repo=owner/repo agent=opencode model=openai/gpt-5") {
		t.Errorf("expected repo/agent/model info line, got %q", buf.String())
	}
	if len(runner.captured.Issues) != 0 {
		t.Errorf("expected empty Issues (prompt-only), got %v", runner.captured.Issues)
	}
	if runner.captured.PromptConfig.PromptFlag == "" {
		t.Fatal("expected rendered prompt in PromptConfig.PromptFlag")
	}
	want := []string{
		"Review pull request #17: Refactor daemon",
		"Splits the orchestrator.",
		"gh pr diff 17",
		"decision.md",
		"RunDir: ",
	}
	for _, w := range want {
		if !strings.Contains(runner.captured.PromptConfig.PromptFlag, w) {
			t.Errorf("rendered prompt missing %q\nprompt:\n%s", w, runner.captured.PromptConfig.PromptFlag)
		}
	}
	if strings.Contains(runner.captured.PromptConfig.PromptFlag, "gh pr comment 17") {
		t.Errorf("rendered prompt must not retain the old `gh pr comment 17` posting instruction; the agent writes <RUN_DIR>/decision.md and the daemon posts (issue #1845)\nprompt:\n%s", runner.captured.PromptConfig.PromptFlag)
	}
	if strings.Contains(runner.captured.PromptConfig.PromptFlag, "{{RUN_DIR}}") {
		t.Errorf("rendered prompt must not retain the unfilled {{RUN_DIR}} placeholder, got prompt:\n%s", runner.captured.PromptConfig.PromptFlag)
	}
	if runner.captured.Agent != "opencode" {
		t.Errorf("expected review agent 'opencode', got %q", runner.captured.Agent)
	}
	if runner.captured.Model != "openai/gpt-5" {
		t.Errorf("expected review model 'openai/gpt-5', got %q", runner.captured.Model)
	}
	if runner.captured.Sandbox != "podman" {
		t.Errorf("expected default sandbox from config 'podman', got %q", runner.captured.Sandbox)
	}
	if !runner.captured.Review {
		t.Errorf("expected Review=true on one-shot review batch request, got false")
	}
	if runner.captured.PRNumber != 17 {
		t.Errorf("expected PRNumber=17 on one-shot review batch request, got %d", runner.captured.PRNumber)
	}
	if runner.captured.IssueNumber != 0 {
		t.Errorf("expected IssueNumber=0 (no linked issue) on one-shot review batch request, got %d", runner.captured.IssueNumber)
	}
	if runner.captured.ReviewFocus != "" {
		t.Errorf("expected empty ReviewFocus on one-shot review batch request, got %q", runner.captured.ReviewFocus)
	}
	if !strings.HasSuffix(runner.captured.RunID, "-PR17") {
		t.Errorf("expected RunID to end with '-PR17' on one-shot review batch request, got %q", runner.captured.RunID)
	}
	if !strings.Contains(runner.captured.RunDir, "PR17") {
		t.Errorf("expected RunDir to contain PR17, got %q", runner.captured.RunDir)
	}
	if !filepath.IsAbs(runner.captured.RunDir) {
		t.Errorf("expected one-shot review RunDir to be absolute (issue #1845), got %q", runner.captured.RunDir)
	}
	// Pin the per-row folder shape: the agent must write to
	// <batchDir>/runs/<perRowRunID>/decision.md, not the batch dir.
	// This matches the daemon's prepareReviewRun contract
	// (internal/review/daemon.go:1137-1138) and the parent PRD
	// "Review decision file" decision (issue #1843).
	if !strings.HasSuffix(runner.captured.RunDir, string(filepath.Separator)+"runs"+string(filepath.Separator)+runner.captured.RunID) {
		t.Errorf("expected one-shot review RunDir to be the per-row folder under <batchDir>/runs/<runID>, got %q (runID=%q)", runner.captured.RunDir, runner.captured.RunID)
	}
	// The rendered prompt's {{RUN_DIR}} substitution must match the
	// per-row worktree (issue #1953), not the run folder, so the
	// agent's <RUN_DIR>/decision.md lands in the daemon-readable
	// worktree path. For container sandboxes the path is
	// translated to /workspace/<rel>; for host sandboxes it stays
	// host-absolute.
	wantWorktree := runner.captured.WorktreeDir
	if wantWorktree == "" {
		t.Fatalf("expected WorktreeDir to be set on one-shot review batch request, got empty")
	}
	wantPromptRunDir := wantWorktree
	if runner.captured.Sandbox == "podman" || runner.captured.Sandbox == "docker" {
		// Container sandbox: the cmd resolves repoRoot to an
		// absolute path and ContainerVisiblePath rebases to
		// /workspace. We don't have repoRoot here; assert the
		// prompt contains the worktree basename and the
		// /workspace prefix instead.
		if !strings.Contains(runner.captured.PromptConfig.PromptFlag, "/workspace/.sandman/worktrees/") {
			t.Errorf("rendered prompt's {{RUN_DIR}} must use /workspace prefix for container sandbox %q, got prompt:\n%s", runner.captured.Sandbox, runner.captured.PromptConfig.PromptFlag)
		}
	} else if !strings.Contains(runner.captured.PromptConfig.PromptFlag, "RunDir: "+wantPromptRunDir) {
		t.Errorf("rendered prompt's {{RUN_DIR}} substitution must equal the per-row worktree, got prompt:\n%s\nWorktreeDir: %q", runner.captured.PromptConfig.PromptFlag, wantPromptRunDir)
	}
	if runner.captured.Parallel != 1 {
		t.Errorf("expected default review parallel 1, got %d", runner.captured.Parallel)
	}
	if runner.captured.OutputWriter == nil {
		t.Error("expected OutputWriter to be set (non-nil) for one-shot review batch request")
	}
}

// TestReviewRun_EndToEnd_TimestampFirstIdentity is the end-to-end
// regression for issue #1946: from the cmd layer to the on-disk
// manifest and per-row folder, every identity that surfaces for a
// review batch must use the canonical `<ts>-<sid>-...` shape. The cmd
// layer routes through review.ReviewRunIDFor (the shared helper) so
// the daemon and one-shot paths mint the same identity. The captured
// `batch.Request.RunID` is the canonical row RunID, the on-disk
// `batch.json.batchId` agrees with it, the per-row folder under
// `.sandman/batches/<batchID>/runs/<runID>/` matches, and the manifest
// carries the canonical (RunTS, RunShortID) primitives.
//
// The daemon launch path (review/daemon.go::prepareReviewRun →
// reviewLaunchRequest.RunID) is covered separately in
// internal/review/daemon_canonical_test.go (TestDaemon_ReviewRunIDAndFolder_AreCanonical
// for orphan reviews, TestDaemon_ReviewRunIDAndFolder_AreCanonicalWithLinkedIssue
// for linked). The portal layer's per-row RunID derivation is covered
// in internal/cmd/portal_review_canonical_test.go (TestPortal_ReviewGrouping_OrphanReviewStaysOrphan,
// TestPortal_ReviewGrouping_LinkedReviewGroupsUnderIssue). This test
// is the cmd-side end-to-end piece that closes the loop with
// acceptance criterion "Review launch paths should derive the same
// names from the shared identity helpers" (issue #1946).
//
// The test exercises both the orphan shape (`<ts>-<sid>-PR<pr>`,
// no linked issue) and the linked shape (`<ts>-<sid>-<linkedIssue>-PR<pr>`,
// PR body closes `#<n>`) so both branches of review.ReviewRunIDFor
// are pinned end to end (acceptance criterion "Tests cover orphan and
// linked-issue review flows with the new shape").
func TestReviewRun_EndToEnd_TimestampFirstIdentity(t *testing.T) {
	t.Run("orphan review", func(t *testing.T) {
		runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
		gh := &fakePRGitHubClient{
			fakeGitHubClient: &fakeGitHubClient{},
			pr: &github.PR{
				Number: 17,
				Title:  "Refactor daemon",
				Body:   "No linked issue here.",
			},
		}
		deps := newReviewDeps(t, gh, &config.Config{
			DefaultAgent:       "opencode",
			DefaultModel:       "opencode/big-pickle",
			DefaultReviewAgent: "opencode",
			DefaultReviewModel: "openai/gpt-5",
			Agent:              "opencode",
			Sandbox:            "podman",
			WorktreeDir:        ".sandman/worktrees",
			Agents:             map[string]config.Agent{},
			AgentProviders: map[string]config.Agent{
				"opencode": {Preset: "opencode", Command: "opencode"},
			},
		}, runner)

		var buf bytes.Buffer
		cmd := NewReviewCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"17"})

		if err := cmd.Execute(); err != nil {
			t.Fatalf("unexpected error: %v\noutput: %s", err, buf.String())
		}

		// Canonical (ts, sid) mint: the cmd layer routes through
		// review.ReviewRunIDFor so the (ts, sid) pair and the
		// subject are stitched in one helper.
		wantTS, wantSID, err := runidFromRequest(t, runner.captured, "PR17")
		if err != nil {
			t.Fatal(err)
		}

		rowID := runner.captured.RunID
		wantPublicBatchID := rowID // orphan review is single-row, BatchId == RunID

		// Portal-side derivation cross-check. The portal derives the
		// per-row RunID from the manifest primitives (RunTS,
		// RunShortID, PRNumber) via perRowRunIDForManifest at
		// internal/cmd/portal_runs_view.go. Mirror that derivation
		// here so the cmd-side mint and the portal-side derivation
		// agree on the canonical <ts>-<sid>-PR<n> string
		// (acceptance criterion "Manifest and portal consumers
		// resolve review runs correctly").
		portalDerived := runid.NewRunID(runid.KindReview, "PR17", wantTS, wantSID)
		if portalDerived != rowID {
			t.Errorf("portal-derived per-row RunID = %q, want %q (must agree with cmd-minted RunID)", portalDerived, rowID)
		}

		// On-disk batch.json manifest agrees: BatchId == per-row
		// RunID, and RunTS / RunShortID round-trip through
		// runid.NewRunID. Same shape as the issue-driven
		// regression test (TestRun_IssueBatch_EndToEnd_...).
		manifest, err := daemon.ReadManifest(filepath.Join(deps.RepoRoot, ".sandman", "batches", wantPublicBatchID))
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if manifest.BatchId != wantPublicBatchID {
			t.Errorf("batch.json.batchId = %q, want %q", manifest.BatchId, wantPublicBatchID)
		}
		if manifest.RunTS != wantTS {
			t.Errorf("batch.json.runTs = %q, want %q", manifest.RunTS, wantTS)
		}
		if manifest.RunShortID != wantSID {
			t.Errorf("batch.json.runShortId = %q, want %q", manifest.RunShortID, wantSID)
		}

		// Timestamp-first shape: per-row RunID begins with RunTS.
		if !strings.HasPrefix(rowID, wantTS) {
			t.Errorf("per-row RunID %q must start with RunTS %q (timestamp-first)", rowID, wantTS)
		}

		// Per-row folder under <batchesDir>/<batchID>/runs/<runID>/
		// is exactly the captured RunDir.
		wantRunDir, err := filepath.Abs(filepath.Join(deps.RepoRoot, ".sandman", "batches", wantPublicBatchID, "runs", rowID))
		if err != nil {
			t.Fatalf("abs run dir: %v", err)
		}
		if runner.captured.RunDir != wantRunDir {
			t.Errorf("captured RunDir = %q, want %q (per-row folder under public BatchId)", runner.captured.RunDir, wantRunDir)
		}
	})

	t.Run("linked review", func(t *testing.T) {
		const linkedIssue = 1066
		runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
		gh := &fakePRGitHubClient{
			fakeGitHubClient: &fakeGitHubClient{},
			pr: &github.PR{
				Number: 42,
				Title:  "Linked review",
				Body:   "This PR fixes #1066.",
			},
		}
		deps := newReviewDeps(t, gh, &config.Config{
			DefaultAgent:       "opencode",
			DefaultModel:       "opencode/big-pickle",
			DefaultReviewAgent: "opencode",
			DefaultReviewModel: "openai/gpt-5",
			Agent:              "opencode",
			Sandbox:            "podman",
			WorktreeDir:        ".sandman/worktrees",
			Agents:             map[string]config.Agent{},
			AgentProviders: map[string]config.Agent{
				"opencode": {Preset: "opencode", Command: "opencode"},
			},
		}, runner)

		var buf bytes.Buffer
		cmd := NewReviewCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"42"})

		if err := cmd.Execute(); err != nil {
			t.Fatalf("unexpected error: %v\noutput: %s", err, buf.String())
		}

		// Linked subject: <linkedIssue>-PR<pr>. The cmd layer must
		// hand the linked issue to review.ReviewRunIDFor so the
		// per-row RunID carries the segment.
		wantTS, wantSID, err := runidFromRequest(t, runner.captured, "1066-PR42")
		if err != nil {
			t.Fatal(err)
		}

		rowID := runner.captured.RunID
		wantPublicBatchID := rowID

		// Portal-side derivation cross-check (acceptance criterion
		// "Manifest and portal consumers resolve review runs
		// correctly"). For a linked review the portal's
		// perRowRunIDForManifest folds in the linked issue between
		// the prefix and the -PR<n> tail, so mirror the call sites
		// here.
		portalDerivedLinked := runid.NewRunID(runid.KindReview, "1066-PR42", wantTS, wantSID)
		if portalDerivedLinked != rowID {
			t.Errorf("portal-derived per-row RunID (linked) = %q, want %q (must agree with cmd-minted RunID)", portalDerivedLinked, rowID)
		}

		// On-disk batch.json manifest agrees.
		manifest, err := daemon.ReadManifest(filepath.Join(deps.RepoRoot, ".sandman", "batches", wantPublicBatchID))
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if manifest.BatchId != wantPublicBatchID {
			t.Errorf("batch.json.batchId = %q, want %q (linked per-row RunID)", manifest.BatchId, wantPublicBatchID)
		}
		if manifest.RunTS != wantTS {
			t.Errorf("batch.json.runTs = %q, want %q", manifest.RunTS, wantTS)
		}
		if manifest.RunShortID != wantSID {
			t.Errorf("batch.json.runShortId = %q, want %q", manifest.RunShortID, wantSID)
		}

		// Segments are stitched in the canonical timestamp-first
		// order. The PR-42 tail must come after the linked-issue
		// segment; a regression that drops the linked issue from
		// the subject would surface here.
		if !strings.HasPrefix(rowID, wantTS) {
			t.Errorf("RunID = %q, must start with RunTS %q (timestamp-first)", rowID, wantTS)
		}
		if !strings.HasSuffix(rowID, "-PR42") {
			t.Errorf("RunID = %q, want -PR42 suffix", rowID)
		}
		if !strings.Contains(rowID, "-1066-PR42") {
			t.Errorf("RunID = %q, want -1066-PR42 segment for linked issue", rowID)
		}
		if strings.Contains(rowID, "PR42-1066") {
			t.Errorf("RunID = %q, must not permute linked issue and PR (legacy shape)", rowID)
		}

		// Per-row folder under <batchesDir>/<batchID>/runs/<runID>/
		// is exactly the captured RunDir.
		wantRunDir, err := filepath.Abs(filepath.Join(deps.RepoRoot, ".sandman", "batches", wantPublicBatchID, "runs", rowID))
		if err != nil {
			t.Fatalf("abs run dir: %v", err)
		}
		if runner.captured.RunDir != wantRunDir {
			t.Errorf("captured RunDir = %q, want %q (per-row folder under public BatchId)", runner.captured.RunDir, wantRunDir)
		}
	})
}

// runidFromRequest verifies that the captured batch.Request's RunID
// is the canonical review RunID for the given subject, and returns
// the (ts, sid) prefix pair it carries so downstream helpers can
// re-derive the same id without trusting the segment order. The
// canonical `<ts>-<sid>-...` shape is enforced via runid's
// KindFromDirName prefix regex; the recovered (ts, sid) is the
// RunID's leading two dash-separated segments after stripping the
// trailing subject.
func runidFromRequest(t *testing.T, req batch.Request, wantSubject string) (ts, sid string, err error) {
	t.Helper()
	rowID := req.RunID
	if rowID == "" {
		return "", "", fmt.Errorf("captured batch.Request is missing RunID")
	}
	if !strings.HasSuffix(rowID, "-"+wantSubject) {
		return "", "", fmt.Errorf("captured RunID %q does not carry the expected subject %q", rowID, wantSubject)
	}
	prefix := strings.TrimSuffix(rowID, "-"+wantSubject)
	if _, ok := runid.KindFromDirName(prefix + "-marker"); !ok {
		return "", "", fmt.Errorf("captured RunID prefix %q does not match the canonical <ts>-<sid> shape", prefix)
	}
	parts := strings.SplitN(prefix, "-", 2)
	if len(parts) != 2 || len(parts[0]) != 12 || len(parts[1]) != 4 {
		return "", "", fmt.Errorf("captured RunID prefix %q does not have the canonical <ts>-<sid> shape", prefix)
	}
	ts = parts[0]
	sid = parts[1]
	// Cross-check against runid.NewRunID so any drift in the helper
	// is caught at this seam.
	got := runid.NewRunID(runid.KindReview, wantSubject, ts, sid)
	if got != rowID {
		return "", "", fmt.Errorf("captured RunID = %q, want %q (canonical <ts>-<sid>-%s)", rowID, got, wantSubject)
	}
	return ts, sid, nil
}

func TestReviewCmd_OneShotParallelFlagOverridesConfig(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:          "opencode",
		DefaultModel:          "opencode/big-pickle",
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "openai/gpt-5",
		DefaultReviewParallel: 4,
		Agent:                 "opencode",
		Sandbox:               "podman",
		Agents:                map[string]config.Agent{},
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr: &github.PR{
			Number: 17,
			Title:  "Refactor daemon",
			Body:   "Splits the orchestrator.",
		},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"17", "--parallel", "7"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if runner.captured.Parallel != 7 {
		t.Fatalf("expected --parallel to set request parallel to 7, got %d", runner.captured.Parallel)
	}
}

func TestReviewCmd_AgentFlagOverridesReviewAgent(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "openai/gpt-5",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--agent", "opencode"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Agent != "opencode" {
		t.Errorf("expected --agent to specify review agent, got %q", runner.captured.Agent)
	}
}

func TestReviewCmd_ModelFlagOverridesReviewModel(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--model", "openai/gpt-4.1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Model != "openai/gpt-4.1" {
		t.Errorf("expected --model to override review model, got %q", runner.captured.Model)
	}
}

func TestReviewCmd_InvalidContainerFlagsReturnError(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "container capacity less than one",
			args:    []string{"42", "--container-capacity", "-1"},
			wantErr: "container_capacity must be 0 or greater",
		},
		{
			name:    "negative max containers",
			args:    []string{"42", "--max-containers", "-1"},
			wantErr: "max_containers must be 0 or greater",
		},
		{
			name:    "container capacity negative in daemon mode",
			args:    []string{"--container-capacity", "-5"},
			wantErr: "container_capacity must be 0 or greater",
		},
		{
			name:    "negative parallel",
			args:    []string{"--parallel", "-3"},
			wantErr: "parallel must be 0 or greater",
		},
		{
			name:    "max containers negative in daemon mode",
			args:    []string{"--max-containers", "-5"},
			wantErr: "max_containers must be 0 or greater",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				DefaultAgent:       "opencode",
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/big-pickle",
				Agent:              "opencode",
				AgentProviders: map[string]config.Agent{
					"opencode": {Preset: "opencode", Command: "opencode"},
				},
			}
			gh := &fakePRGitHubClient{
				fakeGitHubClient: &fakeGitHubClient{},
				pr:               &github.PR{Number: 42, Title: "T", Body: "B"},
			}
			runner := &spyBatchRunner{result: &batch.Result{}}
			deps := newReviewDeps(t, gh, cfg, runner)

			prev := reviewDaemonRunner
			reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
				return nil
			}
			defer func() { reviewDaemonRunner = prev }()

			var buf bytes.Buffer
			cmd := NewReviewCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
			var target *UsageError
			if !errors.As(err, &target) {
				t.Fatalf("expected *UsageError, got %T: %v", err, err)
			}
		})
	}
}

func TestReviewCmd_SandboxFlagDefaultsToConfig(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		Sandbox:            "podman",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Sandbox != "podman" {
		t.Errorf("expected review sandbox default from config 'podman', got %q", runner.captured.Sandbox)
	}
}

func TestReviewCmd_ContainerCapacityFlag(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--container-capacity", "5"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.ContainerCapacity != 5 {
		t.Errorf("expected ContainerCapacity 5, got %d", runner.captured.ContainerCapacity)
	}
	if !runner.captured.ContainerCapacitySet {
		t.Errorf("expected ContainerCapacitySet=true")
	}
}

func TestReviewCmd_MaxContainersFlag(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--max-containers", "3"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.MaxContainers != 3 {
		t.Errorf("expected MaxContainers 3, got %d", runner.captured.MaxContainers)
	}
	if !runner.captured.MaxContainersSet {
		t.Errorf("expected MaxContainersSet=true")
	}
}

func TestReviewCmd_SandboxFlagOverride(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1", "--sandbox", "podman"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Sandbox != "podman" {
		t.Errorf("expected --sandbox override 'podman', got %q", runner.captured.Sandbox)
	}
}

func TestReviewCmd_DaemonFlagsCapture(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	var (
		capturedSandbox string
		capturedCC      int
		capturedCCSet   bool
		capturedMC      int
		capturedMCSet   bool
	)
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
		capturedSandbox = sandbox
		capturedCC = cc
		capturedCCSet = ccSet
		capturedMC = mc
		capturedMCSet = mcSet
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--sandbox", "podman", "--container-capacity", "5", "--max-containers", "3"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedSandbox != "podman" {
		t.Errorf("expected daemon to receive sandbox 'podman', got %q", capturedSandbox)
	}
	if capturedCC != 5 {
		t.Errorf("expected daemon to receive container-capacity 5, got %d", capturedCC)
	}
	if !capturedCCSet {
		t.Errorf("expected daemon to receive container-capacity-set=true")
	}
	if capturedMC != 3 {
		t.Errorf("expected daemon to receive max-containers 3, got %d", capturedMC)
	}
	if !capturedMCSet {
		t.Errorf("expected daemon to receive max-containers-set=true")
	}
}

func TestReviewCmd_DaemonModePropagatesAgentModelFlags(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	var capturedAgent string
	var capturedModel string
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
		capturedAgent = agent
		capturedModel = model
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--agent", "claude", "--model", "anthropic/claude"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedAgent != "claude" {
		t.Errorf("expected daemon to receive agent 'claude', got %q", capturedAgent)
	}
	if capturedModel != "anthropic/claude" {
		t.Errorf("expected daemon to receive model 'anthropic/claude', got %q", capturedModel)
	}
}

func TestReviewCmd_DaemonModePropagatesAgentFlag(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantAgent string
	}{
		{
			name:      "flag set",
			args:      []string{"--agent", "claude"},
			wantAgent: "claude",
		},
		{
			name:      "flag empty (not passed)",
			args:      []string{},
			wantAgent: "",
		},
		{
			name:      "flag zero (passed with empty value)",
			args:      []string{"--agent", ""},
			wantAgent: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				DefaultAgent:       "opencode",
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/big-pickle",
			}
			gh := &fakePRGitHubClient{
				fakeGitHubClient: &fakeGitHubClient{},
			}
			runner := &spyBatchRunner{result: &batch.Result{}}
			deps := newReviewDeps(t, gh, cfg, runner)

			var capturedAgent string
			prev := reviewDaemonRunner
			reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
				capturedAgent = agent
				return nil
			}
			defer func() { reviewDaemonRunner = prev }()

			cmd := NewReviewCmd(deps)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if capturedAgent != tt.wantAgent {
				t.Errorf("expected daemon to receive agent %q, got %q", tt.wantAgent, capturedAgent)
			}
		})
	}
}

func TestReviewCmd_DaemonModePropagatesModelFlag(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantModel string
	}{
		{
			name:      "flag set",
			args:      []string{"--model", "anthropic/claude-sonnet-4"},
			wantModel: "anthropic/claude-sonnet-4",
		},
		{
			name:      "flag empty (not passed)",
			args:      []string{},
			wantModel: "",
		},
		{
			name:      "flag zero (passed with empty value)",
			args:      []string{"--model", ""},
			wantModel: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				DefaultAgent:       "opencode",
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/big-pickle",
			}
			gh := &fakePRGitHubClient{
				fakeGitHubClient: &fakeGitHubClient{},
			}
			runner := &spyBatchRunner{result: &batch.Result{}}
			deps := newReviewDeps(t, gh, cfg, runner)

			var capturedModel string
			prev := reviewDaemonRunner
			reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
				capturedModel = model
				return nil
			}
			defer func() { reviewDaemonRunner = prev }()

			cmd := NewReviewCmd(deps)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if capturedModel != tt.wantModel {
				t.Errorf("expected daemon to receive model %q, got %q", tt.wantModel, capturedModel)
			}
		})
	}
}

func TestReviewCmd_DaemonModePropagatesParallelFlag(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantParallel    int
		wantParallelSet bool
	}{
		{
			name:            "flag set",
			args:            []string{"--parallel", "4"},
			wantParallel:    4,
			wantParallelSet: true,
		},
		{
			name:            "flag empty (not passed)",
			args:            []string{},
			wantParallel:    0,
			wantParallelSet: false,
		},
		{
			name:            "flag zero",
			args:            []string{"--parallel", "0"},
			wantParallel:    0,
			wantParallelSet: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				DefaultAgent:          "opencode",
				DefaultReviewAgent:    "opencode",
				DefaultReviewModel:    "opencode/big-pickle",
				DefaultReviewParallel: 1,
			}
			gh := &fakePRGitHubClient{
				fakeGitHubClient: &fakeGitHubClient{},
			}
			runner := &spyBatchRunner{result: &batch.Result{}}
			deps := newReviewDeps(t, gh, cfg, runner)

			var capturedParallel int
			var capturedParallelSet bool
			prev := reviewDaemonRunner
			reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
				capturedParallel = parallel
				capturedParallelSet = parallelSet
				return nil
			}
			defer func() { reviewDaemonRunner = prev }()

			cmd := NewReviewCmd(deps)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if capturedParallel != tt.wantParallel {
				t.Errorf("expected daemon to receive parallel %d, got %d", tt.wantParallel, capturedParallel)
			}
			if capturedParallelSet != tt.wantParallelSet {
				t.Errorf("expected daemon to receive parallelSet=%v, got %v", tt.wantParallelSet, capturedParallelSet)
			}
		})
	}
}

func TestReviewCmd_DaemonParallelFlagOverridesConfig(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:          "opencode",
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/big-pickle",
		DefaultReviewParallel: 4,
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	var capturedParallel int
	var capturedParallelSet bool
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
		capturedParallel = parallel
		capturedParallelSet = parallelSet
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--parallel", "8"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedParallel != 8 {
		t.Fatalf("expected daemon to receive parallel 8, got %d", capturedParallel)
	}
	if !capturedParallelSet {
		t.Fatalf("expected daemon to receive parallelSet=true")
	}
	if cfg.DefaultReviewParallel != 4 {
		t.Fatalf("expected loaded config to remain at 4 (no cfg mutation), got %d", cfg.DefaultReviewParallel)
	}
}

func TestReviewCmd_DaemonParallelFlagUnsetLeavesConfigAlone(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:          "opencode",
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode/big-pickle",
		DefaultReviewParallel: 4,
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	var capturedParallel int
	var capturedParallelSet bool
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agent string, model string, parallel int, parallelSet bool) error {
		capturedParallel = parallel
		capturedParallelSet = parallelSet
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedParallel != 0 {
		t.Fatalf("expected daemon to receive parallel 0 when --parallel not passed, got %d", capturedParallel)
	}
	if capturedParallelSet {
		t.Fatalf("expected daemon to receive parallelSet=false when --parallel not passed")
	}
	if cfg.DefaultReviewParallel != 4 {
		t.Fatalf("expected loaded config to remain at 4 when --parallel not passed, got %d", cfg.DefaultReviewParallel)
	}
}

func TestReviewCmd_ZeroContainerFlagsForwarded(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	var buf bytes.Buffer
	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"1", "--container-capacity", "0", "--max-containers", "0"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !runner.captured.ContainerCapacitySet {
		t.Fatal("expected ContainerCapacitySet=true for --container-capacity=0")
	}
	if runner.captured.ContainerCapacity != 0 {
		t.Errorf("expected container_capacity=0, got %d", runner.captured.ContainerCapacity)
	}
	if !runner.captured.MaxContainersSet {
		t.Fatal("expected MaxContainersSet=true for --max-containers=0")
	}
	if runner.captured.MaxContainers != 0 {
		t.Errorf("expected max_containers=0, got %d", runner.captured.MaxContainers)
	}
}

func TestReviewCmd_FetchPRErrorBubblesUp(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prErr:            &testError{msg: "gh pr view failed"},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"9"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when FetchPR fails")
	}
	if !strings.Contains(err.Error(), "fetch PR") {
		t.Errorf("error should mention fetch PR, got: %v", err)
	}
}

func TestReviewCmd_FallsBackToDefaultAgent(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.captured.Agent != "opencode" {
		t.Errorf("expected fallback to default agent 'opencode', got %q", runner.captured.Agent)
	}
}

func TestReviewCmd_OneShotErrorsOnMissingModel(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when review model is not set")
	}
	if !strings.Contains(err.Error(), "review model is not set") {
		t.Errorf("expected error about missing review model, got: %v", err)
	}
}

func TestReviewCmd_OneShotErrorsOnInvalidAgent(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "nonexistent-agent",
		DefaultReviewModel: "m",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 1, Title: "T", Body: "B"},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid review agent")
	}
	if !strings.Contains(err.Error(), "nonexistent-agent") {
		t.Errorf("expected error to mention agent name, got: %v", err)
	}
}

func TestReviewCmd_MultiplePRs(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			42: {Number: 42, Title: "PR 42", Body: "B"},
			43: {Number: 43, Title: "PR 43", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 batch requests, got %d", len(reqs))
	}
	if reqs[0].PRNumber != 42 {
		t.Errorf("expected first request PRNumber=42, got %d", reqs[0].PRNumber)
	}
	if reqs[1].PRNumber != 43 {
		t.Errorf("expected second request PRNumber=43, got %d", reqs[1].PRNumber)
	}
}

func TestReviewCmd_RangeSyntax(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			42: {Number: 42, Title: "PR 42", Body: "B"},
			43: {Number: 43, Title: "PR 43", Body: "B"},
			44: {Number: 44, Title: "PR 44", Body: "B"},
			45: {Number: 45, Title: "PR 45", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42:45"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 4 {
		t.Fatalf("expected 4 batch requests (42,43,44,45), got %d", len(reqs))
	}
	for i, n := range []int{42, 43, 44, 45} {
		if reqs[i].PRNumber != n {
			t.Errorf("request %d: expected PRNumber=%d, got %d", i, n, reqs[i].PRNumber)
		}
	}
}

func TestReviewCmd_UnboundedRangeEnd(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			100: {Number: 100, Title: "PR 100", Body: "B"},
			101: {Number: 101, Title: "PR 101", Body: "B"},
			102: {Number: 102, Title: "PR 102", Body: "B"},
		},
		openPRs: []github.PR{
			{Number: 100, Title: "PR 100", Body: "B"},
			{Number: 101, Title: "PR 101", Body: "B"},
			{Number: 102, Title: "PR 102", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"100:"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 batch requests (100,101,102), got %d", len(reqs))
	}
	for i, n := range []int{100, 101, 102} {
		if reqs[i].PRNumber != n {
			t.Errorf("request %d: expected PRNumber=%d, got %d", i, n, reqs[i].PRNumber)
		}
	}
}

func TestReviewCmd_UnboundedRangeStart(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			2: {Number: 2, Title: "PR 2", Body: "B"},
			4: {Number: 4, Title: "PR 4", Body: "B"},
			5: {Number: 5, Title: "PR 5", Body: "B"},
		},
		openPRs: []github.PR{
			{Number: 2, Title: "PR 2", Body: "B"},
			{Number: 4, Title: "PR 4", Body: "B"},
			{Number: 5, Title: "PR 5", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{":5"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 batch requests (2,4,5), got %d", len(reqs))
	}
	for i, n := range []int{2, 4, 5} {
		if reqs[i].PRNumber != n {
			t.Errorf("request %d: expected PRNumber=%d, got %d", i, n, reqs[i].PRNumber)
		}
	}
}

func TestReviewCmd_PRFlagRemoved(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--pr", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when using removed --pr flag")
	}
	if !strings.Contains(err.Error(), "--pr") {
		t.Errorf("expected error mentioning --pr, got: %v", err)
	}
}

func TestReviewCmd_InvalidRangeError(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	tests := []struct {
		name string
		args []string
	}{
		{"negative number", []string{"-1"}},
		{"bare colon", []string{":"}},
		{"reversed range", []string{"5:3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewReviewCmd(deps)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil {
				t.Error("expected error for invalid range, got nil")
			}
		})
	}
}

func TestReviewCmd_UnboundedRange_ListOpenPRError(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber:       map[int]*github.PR{},
		openPRErr:        errors.New("boom"),
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"100:"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when ListOpenPRs fails")
	}
	if !strings.Contains(err.Error(), "list open PRs: boom") {
		t.Errorf("expected error about list open PRs: boom, got: %v", err)
	}
}

func TestReviewCmd_MixedPlainAndUnboundedRange(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber: map[int]*github.PR{
			42:  {Number: 42, Title: "PR 42", Body: "B"},
			7:   {Number: 7, Title: "PR 7", Body: "B"},
			100: {Number: 100, Title: "PR 100", Body: "B"},
			101: {Number: 101, Title: "PR 101", Body: "B"},
		},
		openPRs: []github.PR{
			{Number: 100, Title: "PR 100", Body: "B"},
			{Number: 101, Title: "PR 101", Body: "B"},
		},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "100:"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 batch requests (42,100,101), got %d", len(reqs))
	}
	if reqs[0].PRNumber != 42 {
		t.Errorf("expected first request PRNumber=42, got %d", reqs[0].PRNumber)
	}
	if reqs[1].PRNumber != 100 {
		t.Errorf("expected second request PRNumber=100, got %d", reqs[1].PRNumber)
	}
	if reqs[2].PRNumber != 101 {
		t.Errorf("expected third request PRNumber=101, got %d", reqs[2].PRNumber)
	}
}

func TestReviewCmd_RangeTooLarge(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber:       map[int]*github.PR{},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"1:1001"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for range > 1000")
	}
	if !strings.Contains(err.Error(), "more than 1000 pull requests") {
		t.Errorf("expected error about more than 1000 pull requests, got: %v", err)
	}
}

func TestReviewCmd_UnboundedRange_EmptyOpenPRs(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agent:              "opencode",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		prByNumber:       map[int]*github.PR{},
		openPRs:          []github.PR{},
	}
	runner := &spyBatchRunnerMultiCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"100:"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error for empty open PR list: %v", err)
	}

	reqs := runner.requests()
	if len(reqs) != 0 {
		t.Errorf("expected 0 batch requests for empty open PR list, got %d", len(reqs))
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// TestReviewCmd_OneShotWithLinkedIssueRegistersPerRowRunID verifies that
// `sandman review 42` for a PR with linked issue #42 registers the
// batches index entry with id `<ts>-<sid>-42-PR42`, matching the
// orchestrator's emitted per-row RunID for the review (acceptance
// criterion #1675 §review with linked issue).
func TestReviewCmd_OneShotWithLinkedIssueRegistersPerRowRunID(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "openai/gpt-5",
		Agent:              "opencode",
		Sandbox:            "podman",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	// Body contains "Fixes #42" so github.PR.LinkedIssueNumber() returns 42
	// via the body-fallback regex (issue #1675: subject `<issue>-PR<pr>`).
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr: &github.PR{
			Number: 42,
			Title:  "Implement feature",
			Body:   "Fixes #42",
		},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idx, err := batchindex.Load(filepath.Join(".", ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	if len(idx.Batches) != 1 {
		t.Fatalf("expected exactly 1 batch index entry, got %d (entries=%v)", len(idx.Batches), idx.Batches)
	}
	got := idx.Batches[0]
	wantSuffix := "-42-PR42"
	if !strings.HasSuffix(got.ID, wantSuffix) {
		t.Errorf("entry ID = %q, want suffix %q", got.ID, wantSuffix)
	}
	if got.Kind != batchindex.KindReview {
		t.Errorf("entry Kind = %v, want %v", got.Kind, batchindex.KindReview)
	}
}

// TestReviewCmd_OneShotOrphanRegistersPerRowRunID verifies that
// `sandman review 17` for an orphan PR (no linked issue) registers the
// batches index entry with id `<ts>-<sid>-PR17` — the same form the
// orchestrator emits in its review run event (acceptance criterion
// #1675 §orphan review).
func TestReviewCmd_OneShotOrphanRegistersPerRowRunID(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{
		DefaultAgent:       "opencode",
		DefaultModel:       "opencode/big-pickle",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "openai/gpt-5",
		Agent:              "opencode",
		Sandbox:            "podman",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "opencode"},
		},
	}
	// No linked issue: empty body and no ClosingIssuesReferences populated.
	gh := &fakePRGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr: &github.PR{
			Number: 17,
			Title:  "Refactor daemon",
			Body:   "Splits the orchestrator.",
		},
	}
	runner := &spyBatchRunner{result: &batch.Result{}}
	deps := newReviewDeps(t, gh, cfg, runner)

	cmd := NewReviewCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"17"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idx, err := batchindex.Load(filepath.Join(".", ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	if len(idx.Batches) != 1 {
		t.Fatalf("expected exactly 1 batch index entry, got %d (entries=%v)", len(idx.Batches), idx.Batches)
	}
	got := idx.Batches[0]
	wantSuffix := "-PR17"
	if !strings.HasSuffix(got.ID, wantSuffix) {
		t.Errorf("entry ID = %q, want suffix %q", got.ID, wantSuffix)
	}
	if got.Kind != batchindex.KindReview {
		t.Errorf("entry Kind = %v, want %v", got.Kind, batchindex.KindReview)
	}
}

// TestReviewCmd_OneShotBatchDirName_MatchesPerRowRunID pins the
// slice-3 invariant of issue #1919: the on-disk batch directory name
// equals the per-row RunID for both orphan and linked reviews. The
// linked form is the regression to fix. Mirrors
// TestDaemon_ReviewBatchDirName_MatchesPerRowRunID for the one-shot
// path.
func TestReviewCmd_OneShotBatchDirName_MatchesPerRowRunID(t *testing.T) {
	tests := []struct {
		name        string
		prNumber    int
		prBody      string
		wantSubject string
	}{
		{name: "orphan review", prNumber: 17, prBody: "no linked issue here", wantSubject: "PR17"},
		{name: "linked review", prNumber: 42, prBody: "Fixes #1234", wantSubject: "1234-PR42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				DefaultAgent:       "opencode",
				DefaultModel:       "opencode/big-pickle",
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "openai/gpt-5",
				Agent:              "opencode",
				Sandbox:            "podman",
				AgentProviders: map[string]config.Agent{
					"opencode": {Preset: "opencode", Command: "opencode"},
				},
			}
			gh := &fakePRGitHubClient{
				fakeGitHubClient: &fakeGitHubClient{},
				pr:               &github.PR{Number: tt.prNumber, Title: "T", Body: tt.prBody},
			}
			runner := &spyBatchRunnerWithCapture{spyBatchRunner: spyBatchRunner{result: &batch.Result{}}}
			deps := newReviewDeps(t, gh, cfg, runner)

			cmd := NewReviewCmd(deps)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{fmt.Sprintf("%d", tt.prNumber)})

			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			rowID := runner.captured.RunID
			if !strings.HasSuffix(rowID, "-"+tt.wantSubject) {
				t.Errorf("RunID = %q, want suffix -%s", rowID, tt.wantSubject)
			}
			runDir := runner.captured.RunDir
			if runDir == "" {
				t.Fatal("captured RunDir is empty")
			}
			batchDir := filepath.Dir(filepath.Dir(runDir))
			if filepath.Base(batchDir) != rowID {
				t.Errorf("on-disk batch dir name = %q, want %q (per-row RunID; slice 3 invariant)", filepath.Base(batchDir), rowID)
			}

			// Also check the batches index entry id agrees.
			idx, err := batchindex.Load(filepath.Join(".", ".sandman", "batches.json"))
			if err != nil {
				t.Fatalf("load batches index: %v", err)
			}
			if len(idx.Batches) != 1 {
				t.Fatalf("expected exactly 1 batch index entry, got %d", len(idx.Batches))
			}
			if idx.Batches[0].ID != rowID {
				t.Errorf("batches index entry id = %q, want %q", idx.Batches[0].ID, rowID)
			}
		})
	}
}
