//go:build e2e

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// cmdBadgeRunner captures the branch + prompt that the post-batch
// badge hook would have spawned a child `sandman run --prompt` for.
// It stands in for the real sandman binary in this test environment
// so the production hook path can be exercised end-to-end.
type cmdBadgeRunner struct {
	branch         string
	prURL          string
	capturedBranch string
	capturedPrompt string
}

func (r *cmdBadgeRunner) RunPrompt(_ context.Context, promptText, branch string) (string, error) {
	r.capturedBranch = branch
	r.capturedPrompt = promptText
	return r.prURL, nil
}

// cmdBadgeLister is a deterministic PRLister for the badge e2e test.
// It returns a fixed list of merged sandman/* PRs and an explicit
// marker-PR-found flag so the trigger decision is exercised under
// controlled inputs.
type cmdBadgeLister struct {
	mergedPRs         []batch.MergedSandmanPR
	hasBadge          bool
	hasBadgeCallCount int
}

func (l *cmdBadgeLister) ListMergedSandmanPRs(_ context.Context) ([]batch.MergedSandmanPR, error) {
	return l.mergedPRs, nil
}

func (l *cmdBadgeLister) HasBadgePR(_ context.Context) (bool, error) {
	l.hasBadgeCallCount++
	return l.hasBadge, nil
}

// cmdBadgeControlFileReader is the badge e2e test's
// BadgeControlFileReader fake. It returns whatever presence flag the
// test wants so the test can model both fresh-checkout and
// already-marked-on-disk invocations of the badge hook.
type cmdBadgeControlFileReader struct {
	present bool
}

func (r *cmdBadgeControlFileReader) HasBadgeControlFile() bool {
	return r.present
}

// cmdBadgeControlFileWriter is the badge e2e test's
// BadgeControlFileWriter fake. It counts Write invocations and lets
// the test pin whether the post-batch hook actually persisted the
// "already proposed on this checkout" gate after a successful
// RunPrompt (see issue #2195).
type cmdBadgeControlFileWriter struct {
	calls int
	err   error
}

func (w *cmdBadgeControlFileWriter) Write() error {
	w.calls++
	return w.err
}

// TestBadge_E2E_HappyPath exercises the post-batch badge hook end-to-end
// through the production BatchRunner wiring using a fake BatchRunner that
// drives the badge hook directly from a synthetic AgentRunResult. This
// replaces the prior real-opencode-agent wiring, which did not complete
// in this test environment and caused the batch to abort after 3
// retries (https://github.com/rafaelromao/sandman/issues/1772). The fake
// matches the pattern in internal/batch/badge_e2e_test.go so the test
// verifies the badge hook without invoking the agent.
func TestBadge_E2E_HappyPath(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)
	seedBadgeTestRepo(t, repoDir)

	// Wire the production badge hook path: NewBadgeHookerWith wrapping
	// a defaultBadgeHooker that captures the branch + prompt it would
	// have spawned a child `sandman run --prompt` for. The recorder
	// stands in for the real sandman binary so this test exercises
	// the production hook end-to-end without shelling out.
	rec := &cmdBadgeRunner{branch: "sandman/built-with-sandman", prURL: "https://example.test/badge/pull/99"}
	lister := &cmdBadgeLister{mergedPRs: []batch.MergedSandmanPR{{Number: 1, HeadRefName: "1-fix", Title: "Fix failing test"}}, hasBadge: false}
	controlReader := &cmdBadgeControlFileReader{present: false}
	controlWriter := &cmdBadgeControlFileWriter{}
	badgeHook := batch.NewBadgeHookerWith(rec, lister, controlReader, controlWriter)

	deps := badgeTestDeps(repoDir, badgeHook)
	runRootCommand(t, deps, "init", "--agent", "opencode")
	runRootCommand(t, deps, "config", "set", "review_command", "/oc review")

	out, err := runRootCommand(t, deps, "run", "--agent", "opencode", "--sandbox", "worktree", "1")
	t.Logf("sandman run returned err=%v output=%s", err, out)

	if err != nil {
		t.Fatalf("sandman run failed: %v output=%s", err, out)
	}

	// Operator-visible assertions: the badge hook was invoked with the
	// stable sidecar branch and a prompt that contains the marker
	// comment, and the tracking file was written exactly once after
	// the child run reported success.
	if rec.capturedBranch != "sandman/built-with-sandman" {
		t.Errorf("expected badge hook branch=sandman/built-with-sandman, got %q", rec.capturedBranch)
	}
	if rec.capturedPrompt == "" {
		t.Errorf("expected badge hook to record a prompt, got empty")
	}
	if !strings.Contains(rec.capturedPrompt, "<!-- sandman-badge-pr -->") {
		t.Errorf("expected rendered prompt to contain marker comment, got: %s", rec.capturedPrompt)
	}
	if controlWriter.calls != 1 {
		t.Errorf("expected exactly one control-file write after PR creation (issue #2195), got %d", controlWriter.calls)
	}
}

// TestBadge_E2E_MarkerPRSeeded_ShortCircuitsBadgeHook shares the
// same fake-BatchRunner wiring as TestBadge_E2E_HappyPath and verifies
// that the marker-comment PR check is the authoritative short-circuit:
// under the new ordering (see internal/batch/badge_hook.go), HasBadgePR
// is always called and the runner is skipped because the marker is
// found. The test seeds the marker PR via the fake lister and does not
// touch the .built_with_sandman control file, so it pins the new
// contract and prevents regressions to the old "control file first,
// never call lister" ordering
// (https://github.com/rafaelromao/sandman/issues/1772,
// https://github.com/rafaelromao/sandman/issues/1929).
func TestBadge_E2E_MarkerPRSeeded_ShortCircuitsBadgeHook(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)
	seedBadgeTestRepo(t, repoDir)

	rec := &cmdBadgeRunner{branch: "sandman/built-with-sandman", prURL: "https://example.test/badge/pull/99"}
	lister := &cmdBadgeLister{mergedPRs: []batch.MergedSandmanPR{{Number: 1, HeadRefName: "1-fix", Title: "Fix failing test"}}, hasBadge: true}
	controlReader := &cmdBadgeControlFileReader{present: false}
	controlWriter := &cmdBadgeControlFileWriter{}
	badgeHook := batch.NewBadgeHookerWith(rec, lister, controlReader, controlWriter)

	deps := badgeTestDeps(repoDir, badgeHook)
	runRootCommand(t, deps, "init", "--agent", "opencode")
	runRootCommand(t, deps, "config", "set", "review_command", "/oc review")

	out, err := runRootCommand(t, deps, "run", "--agent", "opencode", "--sandbox", "worktree", "1")
	t.Logf("sandman run returned err=%v output=%s", err, out)

	if err != nil {
		t.Fatalf("sandman run failed: %v output=%s", err, out)
	}

	if lister.hasBadgeCallCount != 1 {
		t.Errorf("expected HasBadgePR to be invoked exactly once under the new ordering (authoritative gate), got %d call(s)", lister.hasBadgeCallCount)
	}
	if rec.capturedPrompt != "" {
		t.Errorf("expected badge hook NOT to spawn when marker PR is present, got prompt=%q", rec.capturedPrompt)
	}
	if controlWriter.calls != 0 {
		t.Errorf("expected no control-file write when marker PR is present, got %d write(s)", controlWriter.calls)
	}
}

func seedBadgeTestRepo(t *testing.T, dir string) {
	t.Helper()

	files := map[string]string{
		"go.mod": `module example.com/badge

go 1.24
`,
		"double.go": `package badge

func Double(n int) int {
	return n * 2
}
`,
		"double_test.go": `package badge

import "testing"

func TestDouble(t *testing.T) {
	if got := Double(2); got != 4 {
		t.Fatalf("Double(2) = %d, want 4", got)
	}
}
`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "feat: seed failing test")
}

// fakeBadgeBatchRunner is the batch.Runner used by the badge e2e tests.
// It skips the real orchestrator and drives the post-batch badge hook
// directly from a synthetic AgentRunResult so the tests verify the
// operator-visible badge hook without ever shelling out to opencode.
// This is the seam that fixes
// https://github.com/rafaelromao/sandman/issues/1772 — the prior wiring
// of batch.NewOrchestrator drove the real agent against a synthetic gh
// shim, which never reached the PR-merge state and caused the batch to
// abort after 3 retries.
type fakeBadgeBatchRunner struct {
	hook batch.BadgeHooker
}

func (f *fakeBadgeBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	results := make([]batch.AgentRunResult, len(req.Issues))
	for i, issue := range req.Issues {
		results[i] = batch.AgentRunResult{
			IssueNumber: issue,
			Status:      "success",
			Branch:      "1-fix",
		}
	}
	if f.hook != nil {
		f.hook.MaybeSuggestBadge(ctx, results)
	}
	return &batch.Result{Runs: results}, nil
}

func badgeTestDeps(repoDir string, badgeHook batch.BadgeHooker) Dependencies {
	cfgStore := &config.FileStore{Path: filepath.Join(repoDir, ".sandman", "config.yaml")}
	eventLog := &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")}

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, State: "open", Title: "Fix failing test"},
		},
	}

	return Dependencies{
		BatchRunner:  &fakeBadgeBatchRunner{hook: badgeHook},
		ConfigStore:  cfgStore,
		EventLog:     eventLog,
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IssuePicker:  &SimpleIssuePicker{},
		IsTTY:        isStdoutTTY,
	}
}
