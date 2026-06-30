//go:build e2e

package batch

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func init() {
	// Unix domain sockets have a ~108 char path limit. The
	// orchestrator's command-server socket is rooted at the cwd, so
	// tests must run with TMPDIR=/tmp to keep the resolved socket
	// path short enough for bind(2).
	_ = os.Setenv("TMPDIR", "/tmp")
}

// errFakeNetwork is the sentinel returned by the seeded PRLister when
// the trigger check fails. The post-batch hook must observe this,
// emit a warn-line, and stay silent.
var errFakeNetwork = errors.New("fake network failure: gh pr list unavailable")

const badgeE2EMarker = "<!-- sandman-badge-pr -->"

const badgeE2EPlacementSection = "## Instructions"

// badgeE2EConfig returns a static config that the orchestrator can
// consume via fakeConfigStore. AgentProviders uses a non-preset
// `test-agent` name so the orchestrator never reaches the host
// opencode binary.
func badgeE2EConfig() *config.Config {
	return &config.Config{
		Agent:        "test-agent",
		DefaultAgent: "test-agent",
		Sandbox:      "worktree",
		WorktreeDir:  ".sandman/worktrees",
		Git:          config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"test-agent": {Name: "test-agent", Command: "true"},
		},
	}
}

// badgeE2EIssueGitHubClient is a minimal GitHub client stub. The
// orchestrator's RunBatch fetches the issue via FetchIssue before
// invoking the runnable, so we hand back a valid issue. FindPRByBranch
// returns a merged PR so the issue-driven run is treated as successful
// after the fake runnable returns.
type badgeE2EIssueGitHubClient struct{}

func (badgeE2EIssueGitHubClient) FetchIssue(num int) (*github.Issue, error) {
	return &github.Issue{Number: num, Title: "Fix bug"}, nil
}
func (badgeE2EIssueGitHubClient) FetchIssueDependencies(_ int) ([]int, error) { return nil, nil }
func (badgeE2EIssueGitHubClient) FetchPR(_ int) (*github.PR, error) {
	return &github.PR{Number: 1, State: "closed", Merged: true}, nil
}
func (badgeE2EIssueGitHubClient) SearchIssues(_ string) ([]github.Issue, error) {
	return nil, nil
}
func (badgeE2EIssueGitHubClient) FindPRByBranch(branch string) (*github.PR, error) {
	return &github.PR{Number: 1, State: "closed", Merged: true, HeadRefName: branch}, nil
}
func (badgeE2EIssueGitHubClient) ListOpenPRs() ([]github.PR, error)                { return nil, nil }
func (badgeE2EIssueGitHubClient) ListPRComments(_ int) ([]github.PRComment, error) { return nil, nil }
func (badgeE2EIssueGitHubClient) ListIssueComments(_ int) ([]github.IssueComment, error) {
	return nil, nil
}
func (badgeE2EIssueGitHubClient) RepoName() (string, error)                        { return "owner/repo", nil }
func (badgeE2EIssueGitHubClient) EditComment(_, _ string) error                    { return nil }
func (badgeE2EIssueGitHubClient) EditPRBody(_ int, _ string) error                 { return nil }
func (badgeE2EIssueGitHubClient) AddCommentReaction(_, _ string) (string, error)   { return "", nil }
func (badgeE2EIssueGitHubClient) AddIssueReaction(_ int, _ string) (string, error) { return "", nil }
func (badgeE2EIssueGitHubClient) RemoveCommentReaction(_, _ string) error          { return nil }
func (badgeE2EIssueGitHubClient) RemoveIssueReaction(_ int, _ string) error        { return nil }
func (badgeE2EIssueGitHubClient) CloseIssue(_ int, _ string) error                 { return nil }

// badgeE2ENoopRenderer is a renderer that hands back the empty
// prompt. It satisfies prompt.IssueRenderer so the orchestrator can
// call Render without panicking.
type badgeE2ENoopRenderer struct{}

func (badgeE2ENoopRenderer) Render(_ prompt.RenderConfig, _ prompt.IssueData) (string, error) {
	return "", nil
}
func (badgeE2ENoopRenderer) RenderReview(_ prompt.RenderConfig, _ prompt.PRData) (string, error) {
	return "", nil
}

// badgeE2EBuildOrchestrator wires an Orchestrator with the production
// post-batch badge hook path (WithBadgeHooker + NewBadgeHookerWith)
// and the test-package fakes for RunnableFactory + SandboxFactory so
// RunBatch completes without shelling out to a real agent or worktree.
func badgeE2EBuildOrchestrator(t *testing.T, lister PRLister, runner SandmanRunner, stderr *bytes.Buffer, runResults []AgentRunResult) *Orchestrator {
	t.Helper()
	cfgStore := &fakeConfigStore{config: badgeE2EConfig()}

	o := NewOrchestrator(
		badgeE2EIssueGitHubClient{},
		badgeE2ENoopRenderer{},
		cfgStore,
		nil,
		WithBadgeHooker(NewBadgeHookerWith(stderr, runner, lister)),
	)
	o.runnableFactory = &fakeRunnableFactory{results: runResults}
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	return o
}

// badgeE2ESuccessResults returns a single success result used by the
// badge e2e tests. The orchestrator's RunBatch sees one successful
// run and fires the post-batch hook.
func badgeE2ESuccessResults() []AgentRunResult {
	return []AgentRunResult{{IssueNumber: 1, Status: "success", Branch: "sandman/1-fix"}}
}

// badgeE2EPrimeRepo primes a temp dir as a git repo and chdir's into
// it. The orchestrator resolves the .sandman layout from the cwd, so
// the test must operate inside a real git working tree.
func badgeE2EPrimeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)
	return dir
}

func TestBadgeE2E_HappyPath_ProductionWiringFiresBadgeHook(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	stderr := &bytes.Buffer{}
	seedPRs := []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix-bug", Title: "Fix failing test"}}
	lister := &fakePRLister{mergedPRs: seedPRs, hasBadge: false}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, stderr, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt == "" {
		t.Fatalf("expected RunPrompt invocation, stderr=%q", stderr.String())
	}
	if runner.capturedBranch != "sandman/built-with-sandman" {
		t.Errorf("expected branch=sandman/built-with-sandman, got %q", runner.capturedBranch)
	}
	if !strings.Contains(runner.capturedPrompt, badgeE2EMarker) {
		t.Errorf("expected prompt to contain marker, got: %s", runner.capturedPrompt)
	}
	if !strings.Contains(runner.capturedPrompt, badgeE2EPlacementSection) {
		t.Errorf("expected prompt to contain placement rules, got: %s", runner.capturedPrompt)
	}
	if !strings.Contains(runner.capturedPrompt, "Fix failing test (#1)") {
		t.Errorf("expected prompt to contain merged PR rationale, got: %s", runner.capturedPrompt)
	}
	if !strings.Contains(stderr.String(), "Sandman suggested a Built with Sandman badge PR: https://github.com/owner/repo/pull/99 (close it to dismiss)") {
		t.Errorf("expected stderr to contain summary line, got: %s", stderr.String())
	}
}

func TestBadgeE2E_Idempotency_MarkerPRPresent(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	stderr := &bytes.Buffer{}
	seedPRs := []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix-bug", Title: "Fix failing test"}}
	lister := &fakePRLister{mergedPRs: seedPRs, hasBadge: true}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, stderr, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt != "" {
		t.Errorf("expected no RunPrompt when marker PR is present, got: %s", runner.capturedPrompt)
	}
	if strings.Contains(stderr.String(), "Sandman suggested a Built with Sandman badge PR:") {
		t.Errorf("expected no summary line, got stderr: %s", stderr.String())
	}
}

func TestBadgeE2E_NoMergedSandmanPRs(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	stderr := &bytes.Buffer{}
	lister := &fakePRLister{mergedPRs: nil, hasBadge: false}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, stderr, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt != "" {
		t.Errorf("expected no RunPrompt when no merged sandman/* PRs, got: %s", runner.capturedPrompt)
	}
	if strings.Contains(stderr.String(), "Sandman suggested a Built with Sandman badge PR:") {
		t.Errorf("expected no summary line, got stderr: %s", stderr.String())
	}
}

func TestBadgeE2E_TriggerCheckFailure_WarnsAndStaysSilent(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	stderr := &bytes.Buffer{}
	lister := &fakePRLister{mergedErr: errFakeNetwork}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, stderr, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt != "" {
		t.Errorf("expected no RunPrompt on trigger check failure, got: %s", runner.capturedPrompt)
	}
	if !strings.Contains(stderr.String(), "Badge PR suggestion skipped:") {
		t.Errorf("expected warn-line on stderr, got: %s", stderr.String())
	}
}

func TestBadgeE2E_PRCreateFailure_WarnsAndStaysSilent(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	stderr := &bytes.Buffer{}
	seedPRs := []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix-bug", Title: "Fix failing test"}}
	lister := &fakePRLister{mergedPRs: seedPRs, hasBadge: false}
	runner := &fakeSandmanRunner{err: errFakeNetwork}

	o := badgeE2EBuildOrchestrator(t, lister, runner, stderr, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt == "" {
		t.Fatalf("expected exactly one RunPrompt attempt on PR create failure path, stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Badge PR suggestion skipped:") {
		t.Errorf("expected warn-line on stderr, got: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "Sandman suggested a Built with Sandman badge PR:") {
		t.Errorf("expected no success summary line when child runner fails, got stderr: %s", stderr.String())
	}
}
