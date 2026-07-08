//go:build e2e

package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func (badgeE2EIssueGitHubClient) FetchIssue(_ context.Context, num int) (*github.Issue, error) {
	return &github.Issue{Number: num, Title: "Fix bug"}, nil
}
func (badgeE2EIssueGitHubClient) FetchIssueDependencies(_ context.Context, _ int) ([]int, error) {
	return nil, nil
}
func (badgeE2EIssueGitHubClient) FetchPR(_ context.Context, _ int) (*github.PR, error) {
	return &github.PR{Number: 1, State: "closed", Merged: true}, nil
}
func (badgeE2EIssueGitHubClient) SearchIssues(_ context.Context, _ string) ([]github.Issue, error) {
	return nil, nil
}
func (badgeE2EIssueGitHubClient) FindPRByBranch(_ context.Context, branch string) (*github.PR, error) {
	return &github.PR{Number: 1, State: "closed", Merged: true, HeadRefName: branch}, nil
}
func (badgeE2EIssueGitHubClient) ListOpenPRs(_ context.Context) ([]github.PR, error) { return nil, nil }
func (badgeE2EIssueGitHubClient) ListPRComments(_ context.Context, _ int) ([]github.PRComment, error) {
	return nil, nil
}
func (badgeE2EIssueGitHubClient) ListIssueComments(_ context.Context, _ int) ([]github.IssueComment, error) {
	return nil, nil
}
func (badgeE2EIssueGitHubClient) RepoName(_ context.Context) (string, error) {
	return "owner/repo", nil
}
func (badgeE2EIssueGitHubClient) EditComment(_ context.Context, _, _ string) error {
	return nil
}
func (badgeE2EIssueGitHubClient) EditPRBody(_ context.Context, _ int, _ string) error {
	return nil
}
func (badgeE2EIssueGitHubClient) AddCommentReaction(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (badgeE2EIssueGitHubClient) AddIssueReaction(_ context.Context, _ int, _ string) (string, error) {
	return "", nil
}
func (badgeE2EIssueGitHubClient) RemoveCommentReaction(_ context.Context, _, _ string) error {
	return nil
}
func (badgeE2EIssueGitHubClient) RemoveIssueReaction(_ context.Context, _ int, _ string) error {
	return nil
}
func (badgeE2EIssueGitHubClient) CloseIssue(_ context.Context, _ int, _ string) error {
	return nil
}

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

// TestBadgeE2E_ControlFileAbsent_MarkerPRFound_NoSpawn exercises the
// cold-start checkout path where the local control file has not been
// written yet but a previously-proposed marker PR is still visible on
// the remote. Under the new hook ordering, HasBadgePR is the
// authoritative gate and runs before the control-file fast path, so
// the runner must be skipped even with no sentinel file on disk.
//
// The fakePRLister seam models the marker-found outcome as a boolean
// (it does not carry per-PR state). The state-aggregation behavior
// (open / closed / merged marker PRs all suppress the spawn) is
// already pinned by the unit test
// TestMaybeSuggestBadge_HasBadgePR_AnyState_SkipsSpawn in
// badge_hook_test.go, which uses the production defaultPRLister over
// a wrappingPRLister + fakeGhCommander. This e2e focuses on the
// orchestrator seam and the absent-on-disk invariant.
//
// See https://github.com/rafaelromao/sandman/issues/1929.
func TestBadgeE2E_ControlFileAbsent_MarkerPRFound_NoSpawn(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	repoDir := badgeE2EPrimeRepo(t)

	controlPath := filepath.Join(repoDir, ".sandman", "state", ".built_with_sandman")
	if _, err := os.Stat(controlPath); !os.IsNotExist(err) {
		t.Fatalf("expected control file to be absent on fresh checkout, got stat err=%v", err)
	}

	stderr := &bytes.Buffer{}
	seedPRs := []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix-bug", Title: "Fix failing test"}}
	lister := &fakePRLister{mergedPRs: seedPRs, hasBadge: true}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, stderr, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if lister.hasBadgeCallCount != 1 {
		t.Errorf("expected HasBadgePR to be invoked exactly once (authoritative gate runs before control-file fast path), got %d call(s)", lister.hasBadgeCallCount)
	}
	if runner.capturedPrompt != "" {
		t.Errorf("expected no spawn when control file is absent but marker PR is present, got prompt=%q", runner.capturedPrompt)
	}
	if strings.Contains(stderr.String(), "Sandman suggested a Built with Sandman badge PR") {
		t.Errorf("expected no summary line on stderr when marker PR is present, got: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "Badge PR suggestion skipped") {
		t.Errorf("expected no warn-line on stderr when marker PR is present, got: %s", stderr.String())
	}
}

// badgeE2EPaginatedGhCommander is a ghCommander that distinguishes
// between the two calls the production defaultPRLister makes and
// serves a sequenced set of responses for the marker-scan call. It
// drives the real findBadgeMarkerPR pagination path so the e2e test
// can pin the operator-visible behavior end-to-end without shelling
// out to the real `gh` binary.
//
// The merged-PR call is served from `mergedPayload` (a single-page
// response). The marker-scan call walks `markerCalls` page by page,
// appending a `Link: <url>; rel="next"` header on every page that has
// a successor so the production parser picks up the cursor. A page
// with no linkNext terminates the scan as the API's last page.
type badgeE2EPaginatedGhCommander struct {
	mergedPayload []byte
	markerCalls   []badgeE2EMarkerPage

	mergedCalls  int
	markerCallNo int
	markerArgs   [][]string
}

type badgeE2EMarkerPage struct {
	payload  []prPayloadBody
	linkNext string
}

func (c *badgeE2EPaginatedGhCommander) runGh(_ context.Context, args ...string) ([]byte, error) {
	captured := append([]string(nil), args...)
	if isBadgeMarkerScanArgs(captured) {
		c.markerCallNo++
		c.markerArgs = append(c.markerArgs, captured)
		idx := c.markerCallNo - 1
		if idx >= len(c.markerCalls) {
			return []byte("[]"), nil
		}
		page := c.markerCalls[idx]
		out, err := json.Marshal(page.payload)
		if err != nil {
			return nil, err
		}
		if page.linkNext != "" {
			out = append(out, []byte(fmt.Sprintf("\n<%s>; rel=\"next\"", page.linkNext))...)
		}
		return out, nil
	}
	c.mergedCalls++
	return c.mergedPayload, nil
}

// isBadgeMarkerScanArgs returns true when the args signature matches
// the marker-scan call (--state all, --json number,body). The
// production defaultPRLister makes exactly one other call from this
// path: ListMergedSandmanPRs, which uses --state merged. We use the
// combination of state + json keys to disambiguate without coupling
// to argument order.
func isBadgeMarkerScanArgs(args []string) bool {
	hasStateAll := false
	hasNumberBody := false
	for i, a := range args {
		if a == "--state" && i+1 < len(args) && args[i+1] == "all" {
			hasStateAll = true
		}
		if a == "--json" && i+1 < len(args) && args[i+1] == "number,body" {
			hasNumberBody = true
		}
	}
	return hasStateAll && hasNumberBody
}

// TestBadgeE2E_PaginatedSearch_FindsMarkerOnPage2_NoSpawn exercises
// the production defaultPRLister.findBadgeMarkerPR pagination path
// through the full post-batch hook + orchestrator wiring. The fake gh
// shim serves a 100-PR page with no marker followed by a page-2 page
// whose body contains the marker comment. The hook must walk both
// pages, find the marker, and short-circuit the spawn. The test also
// pins the operator-visible "no stray scan logs" invariant on the
// success path: the writer must contain nothing for the marker-scan
// stage (the lister writes nothing on success).
//
// This is the e2e counterpart of the unit-level
// TestHasBadgePR_FindsMarkerOnSecondPage: it drives the same code path
// through NewBadgeHookerWith + the orchestrator's post-batch hook
// trigger so a regression in the wiring that bypasses the lister (or
// re-introduces per-page stderr noise) breaks the e2e gate.
func TestBadgeE2E_PaginatedSearch_FindsMarkerOnPage2_NoSpawn(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	mergedJSON, err := json.Marshal([]prPayloadList{
		{Number: 1, HeadRefName: "sandman/1-fix-bug", Title: "Fix failing test"},
	})
	if err != nil {
		t.Fatalf("marshal merged prs: %v", err)
	}

	page1 := make([]prPayloadBody, 100)
	for i := range page1 {
		page1[i] = prPayloadBody{Number: i + 1, Body: fmt.Sprintf("Plain PR body #%d", i+1)}
	}
	page2 := []prPayloadBody{{Number: 101, Body: "<!-- sandman-badge-pr -->\nProposed by agent."}}

	gh := &badgeE2EPaginatedGhCommander{
		mergedPayload: mergedJSON,
		markerCalls: []badgeE2EMarkerPage{
			{payload: page1, linkNext: "https://github.com/owner/repo/pulls?state=all&limit=100&after=PAGE1"},
			{payload: page2},
		},
	}
	lister := &defaultPRLister{gh: gh}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	stderr := &bytes.Buffer{}
	o := badgeE2EBuildOrchestrator(t, lister, runner, stderr, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if gh.mergedCalls != 1 {
		t.Errorf("expected exactly one ListMergedSandmanPRs call, got %d", gh.mergedCalls)
	}
	if gh.markerCallNo != 2 {
		t.Errorf("expected the marker scan to walk 2 pages (find on page 2), got %d gh call(s)", gh.markerCallNo)
	}
	if runner.capturedPrompt != "" {
		t.Errorf("expected no spawn when marker is found on page 2 of the paginated scan, got prompt=%q", runner.capturedPrompt)
	}
	if strings.Contains(stderr.String(), "badge marker scan:") {
		t.Errorf("expected no per-page marker scan log lines on writer, got stderr: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "Sandman suggested a Built with Sandman badge PR") {
		t.Errorf("expected no summary line when marker is present, got stderr: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "Badge PR suggestion skipped") {
		t.Errorf("expected no warn-line on stderr when marker is present, got: %s", stderr.String())
	}
}

// TestBadgeE2E_PaginatedSearch_MarkerAbsent_TriggersCreation drives
// the full post-batch hook + orchestrator wiring with the production
// defaultPRLister finding no marker across multiple pages, then
// verifies the operator-visible PR-creation contract end-to-end:
//
//   - The paginated scan exhausts the API's pages without finding a
//     marker, so the hook reaches the spawn step.
//   - The runner is invoked exactly once with the stable
//     sandman/built-with-sandman branch and a prompt that contains
//     the marker, the placement instructions, and the merged PR
//     rationale.
//   - The returned PR URL is propagated into the operator-visible
//     summary line on the writer (not lost, not re-rendered).
//   - The writer is silent during the paginated scan (no
//     `badge marker scan:` per-page log lines) — the quiet-scan
//     invariant the user requested.
//
// This complements the unit-level
// TestFindBadgeMarkerPR_MarkerAbsent_ExhaustsPages and
// TestMaybeSuggestBadge_PromptContainsMergedPRs by proving the
// behavior end-to-end through the production lister + hook + writer
// stack, so a regression in any of those seams breaks the e2e gate.
func TestBadgeE2E_PaginatedSearch_MarkerAbsent_TriggersCreation(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	mergedJSON, err := json.Marshal([]prPayloadList{
		{Number: 7, HeadRefName: "sandman/7-add-login", Title: "Add login"},
		{Number: 8, HeadRefName: "sandman/8-refactor", Title: "Refactor auth"},
	})
	if err != nil {
		t.Fatalf("marshal merged prs: %v", err)
	}

	page1 := make([]prPayloadBody, 100)
	for i := range page1 {
		page1[i] = prPayloadBody{Number: i + 1, Body: fmt.Sprintf("Plain PR body #%d", i+1)}
	}
	page2 := []prPayloadBody{{Number: 101, Body: "Another plain body"}, {Number: 102, Body: "Yet another plain body"}}

	const prURL = "https://github.com/owner/repo/pull/4242"
	gh := &badgeE2EPaginatedGhCommander{
		mergedPayload: mergedJSON,
		markerCalls: []badgeE2EMarkerPage{
			{payload: page1, linkNext: "https://github.com/owner/repo/pulls?state=all&limit=100&after=PAGE1"},
			{payload: page2},
		},
	}
	lister := &defaultPRLister{gh: gh}
	runner := &fakeSandmanRunner{prURL: prURL}

	stderr := &bytes.Buffer{}
	o := badgeE2EBuildOrchestrator(t, lister, runner, stderr, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if gh.markerCallNo != 2 {
		t.Errorf("expected the marker scan to walk 2 pages before declaring marker absent, got %d gh call(s)", gh.markerCallNo)
	}
	if runner.capturedPrompt == "" {
		t.Fatalf("expected exactly one RunPrompt attempt on PR creation path, stderr=%q", stderr.String())
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
	if !strings.Contains(runner.capturedPrompt, "Add login (#7)") {
		t.Errorf("expected prompt to contain merged PR #7 rationale, got: %s", runner.capturedPrompt)
	}
	if !strings.Contains(runner.capturedPrompt, "Refactor auth (#8)") {
		t.Errorf("expected prompt to contain merged PR #8 rationale, got: %s", runner.capturedPrompt)
	}

	wantSummary := fmt.Sprintf("Sandman suggested a Built with Sandman badge PR: %s (close it to dismiss)", prURL)
	if !strings.Contains(stderr.String(), wantSummary) {
		t.Errorf("expected stderr to contain summary line %q, got: %s", wantSummary, stderr.String())
	}
	if strings.Contains(stderr.String(), "badge marker scan:") {
		t.Errorf("expected no per-page marker scan log lines on writer, got stderr: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "Badge PR suggestion skipped") {
		t.Errorf("expected no warn-line on stderr on success path, got: %s", stderr.String())
	}
}
