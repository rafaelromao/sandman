//go:build e2e

package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
func (badgeE2EIssueGitHubClient) ListSubIssues(_ context.Context, _ int) ([]int, error) {
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
// The hook is silent (issue #2195) — callers assert against the
// optional controlWriter seam instead of the writer stream.
func badgeE2EBuildOrchestrator(t *testing.T, lister PRLister, runner SandmanRunner, controlReader BadgeControlFileReader, controlWriter BadgeControlFileWriter, runResults []AgentRunResult) *Orchestrator {
	t.Helper()
	cfgStore := &fakeConfigStore{config: badgeE2EConfig()}

	o := NewOrchestrator(
		badgeE2EIssueGitHubClient{},
		badgeE2ENoopRenderer{},
		cfgStore,
		nil,
		WithBadgeHooker(NewBadgeHookerWith(runner, lister, controlReader, controlWriter)),
		WithRunnableFactory(&fakeRunnableFactory{results: runResults}),
		WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{}}),
	)
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

	controlReader := &fakeBadgeControlFileReader{present: false}
	controlWriter := &fakeBadgeControlFileWriter{}
	seedPRs := []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix-bug", Title: "Fix failing test"}}
	lister := &fakePRLister{mergedPRs: seedPRs, hasBadge: false}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, controlReader, controlWriter, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt == "" {
		t.Fatalf("expected RunPrompt invocation")
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
	if controlWriter.calls != 1 {
		t.Errorf("expected control file written exactly once after PR creation (issue #2195), got %d", controlWriter.calls)
	}
}

func TestBadgeE2E_Idempotency_MarkerPRPresent(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	controlReader := &fakeBadgeControlFileReader{present: false}
	controlWriter := &fakeBadgeControlFileWriter{}
	seedPRs := []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix-bug", Title: "Fix failing test"}}
	lister := &fakePRLister{mergedPRs: seedPRs, hasBadge: true}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, controlReader, controlWriter, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt != "" {
		t.Errorf("expected no RunPrompt when marker PR is present, got: %s", runner.capturedPrompt)
	}
	if controlWriter.calls != 0 {
		t.Errorf("expected no control-file write when marker PR is present, got %d write(s)", controlWriter.calls)
	}
}

func TestBadgeE2E_NoMergedSandmanPRs(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	controlReader := &fakeBadgeControlFileReader{present: false}
	controlWriter := &fakeBadgeControlFileWriter{}
	lister := &fakePRLister{mergedPRs: nil, hasBadge: false}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, controlReader, controlWriter, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt != "" {
		t.Errorf("expected no RunPrompt when no merged sandman/* PRs, got: %s", runner.capturedPrompt)
	}
	if controlWriter.calls != 0 {
		t.Errorf("expected no control-file write when no merged sandman/* PRs, got %d write(s)", controlWriter.calls)
	}
}

func TestBadgeE2E_TriggerCheckFailure_StaysSilent(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	controlReader := &fakeBadgeControlFileReader{present: false}
	controlWriter := &fakeBadgeControlFileWriter{}
	lister := &fakePRLister{mergedErr: errFakeNetwork}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, controlReader, controlWriter, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt != "" {
		t.Errorf("expected no RunPrompt on trigger check failure, got: %s", runner.capturedPrompt)
	}
	if controlWriter.calls != 0 {
		t.Errorf("expected no control-file write on trigger check failure, got %d write(s)", controlWriter.calls)
	}
}

func TestBadgeE2E_PRCreateFailure_StaysSilentAndDoesNotMarkFile(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	controlReader := &fakeBadgeControlFileReader{present: false}
	controlWriter := &fakeBadgeControlFileWriter{}
	seedPRs := []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix-bug", Title: "Fix failing test"}}
	lister := &fakePRLister{mergedPRs: seedPRs, hasBadge: false}
	runner := &fakeSandmanRunner{err: errFakeNetwork}

	o := badgeE2EBuildOrchestrator(t, lister, runner, controlReader, controlWriter, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if runner.capturedPrompt == "" {
		t.Fatalf("expected exactly one RunPrompt attempt on PR create failure path")
	}
	if controlWriter.calls != 0 {
		t.Errorf("expected no control-file write when child runner fails, got %d write(s) — failing to write means the next batch retries harmlessly", controlWriter.calls)
	}
}

// TestBadgeE2E_ControlFilePresent_SkipsAPIScan exercises the cold-start
// short-circuit: when the local control file is present, neither the
// API scan nor the spawn runs. The hook is silent (issue #2195):
// nothing is written to any operator-visible stream.
//
// See https://github.com/rafaelromao/sandman/issues/2195.
func TestBadgeE2E_ControlFilePresent_SkipsAPIScan(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	badgeE2EPrimeRepo(t)

	controlReader := &fakeBadgeControlFileReader{present: true}
	controlWriter := &fakeBadgeControlFileWriter{}
	seedPRs := []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix-bug", Title: "Fix failing test"}}
	lister := &fakePRLister{mergedPRs: seedPRs, hasBadge: false}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	o := badgeE2EBuildOrchestrator(t, lister, runner, controlReader, controlWriter, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if lister.hasBadgeCallCount != 0 {
		t.Errorf("expected HasBadgePR NOT to be called when control file is present (tracking file is the API gate, see issue #2195), got %d call(s)", lister.hasBadgeCallCount)
	}
	if runner.capturedPrompt != "" {
		t.Errorf("expected no spawn when control file is present, got prompt=%q", runner.capturedPrompt)
	}
	if controlWriter.calls != 0 {
		t.Errorf("expected no control-file write when control file is already present, got %d write(s)", controlWriter.calls)
	}
}

// badgeE2EPaginatedGhCommander is a ghCommander that distinguishes
// between the two calls the production defaultPRLister makes. The
// merged-PR call is served from `mergedPayload` (a single response).
// The marker-scan call is served from `markerStream` as one
// concatenated `gh api --paginate` stream that the lister walks
// sequentially — emulating what `gh` itself returns when it
// concatenates pages of `pulls?state=all` into a single JSON-Lines
// payload via `--paginate`.
//
// After issue #2195 the lister uses `gh api --paginate` (which honours
// REST `Link` headers internally) instead of hand-rolling the cursor.
// The Go code now sees a single stream — the fake returns that single
// stream verbatim.
type badgeE2EPaginatedGhCommander struct {
	mergedPayload []byte
	markerStream  []byte

	mergedCalls int
	markerCalls int
}

func (c *badgeE2EPaginatedGhCommander) runGh(_ context.Context, args ...string) ([]byte, error) {
	if isBadgeMarkerScanArgs(args) {
		c.markerCalls++
		return c.markerStream, nil
	}
	c.mergedCalls++
	return c.mergedPayload, nil
}

// isBadgeMarkerScanArgs returns true when the args signature matches
// the marker-scan call (`gh api --paginate`). The production
// defaultPRLister makes exactly one other call from this path:
// ListMergedSandmanPRs, which uses `--state merged`. We disambiguate
// by the unique `--paginate` flag the marker-scan call now carries.
func isBadgeMarkerScanArgs(args []string) bool {
	for _, a := range args {
		if a == "--paginate" {
			return true
		}
	}
	return false
}

// TestBadgeE2E_PaginatedSearch_FindsMarkerOnSecondPage_NoSpawn exercises
// the production defaultPRLister.HasBadgePR pagination path through
// the full post-batch hook + orchestrator wiring. The fake gh shim
// serves a 100-PR stream with no marker followed by a marker PR —
// emulating the multi-page output of `gh api --paginate`. The hook
// must walk past the 100th entry, find the marker, and short-circuit
// the spawn. The test also pins the writer-silent invariant on the
// success path: the tracking-file writer remains untouched.
//
// This is the e2e counterpart of the unit-level
// TestHasBadgePR_FindsMarkerOnSecondPage: it drives the same code
// path through NewBadgeHookerWith + the orchestrator's post-batch
// hook trigger so a regression in the wiring that bypasses the
// lister (or reintroduces per-page stderr noise) breaks the e2e
// gate.
func TestBadgeE2E_PaginatedSearch_FindsMarkerOnSecondPage_NoSpawn(t *testing.T) {
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

	markerStream := append(encodeJSONL(page1), encodeJSONL(page2)...)

	gh := &badgeE2EPaginatedGhCommander{
		mergedPayload: mergedJSON,
		markerStream:  markerStream,
	}
	lister := &defaultPRLister{gh: gh}
	runner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}

	controlReader := &fakeBadgeControlFileReader{present: false}
	controlWriter := &fakeBadgeControlFileWriter{}
	o := badgeE2EBuildOrchestrator(t, lister, runner, controlReader, controlWriter, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if gh.mergedCalls != 1 {
		t.Errorf("expected exactly one ListMergedSandmanPRs call, got %d", gh.mergedCalls)
	}
	if gh.markerCalls != 1 {
		t.Errorf("expected exactly one paginated marker-scan call, got %d", gh.markerCalls)
	}
	if runner.capturedPrompt != "" {
		t.Errorf("expected no spawn when marker is found beyond the first 100 entries, got prompt=%q", runner.capturedPrompt)
	}
	if controlWriter.calls != 0 {
		t.Errorf("expected no control-file write when marker is present, got %d write(s)", controlWriter.calls)
	}
}

// TestBadgeE2E_PaginatedSearch_MarkerAbsent_TriggersCreation drives
// the full post-batch hook + orchestrator wiring with the production
// defaultPRLister finding no marker across many entries, then
// verifies the operator-visible PR-creation contract end-to-end:
//
//   - The paginated scan consumes the full stream without finding a
//     marker, so the hook reaches the spawn step.
//   - The runner is invoked exactly once with the stable
//     sandman/built-with-sandman branch and a prompt that contains
//     the marker, the placement instructions, and the merged PR
//     rationale.
//   - The tracking file is written exactly once after the runner
//     reports success.
//
// This complements the unit-level
// TestHasBadgePR_MarkerAbsent_TraversesFullStream and
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
	markerStream := append(encodeJSONL(page1), encodeJSONL(page2)...)

	gh := &badgeE2EPaginatedGhCommander{
		mergedPayload: mergedJSON,
		markerStream:  markerStream,
	}
	lister := &defaultPRLister{gh: gh}
	runner := &fakeSandmanRunner{prURL: prURL}

	controlReader := &fakeBadgeControlFileReader{present: false}
	controlWriter := &fakeBadgeControlFileWriter{}
	o := badgeE2EBuildOrchestrator(t, lister, runner, controlReader, controlWriter, badgeE2ESuccessResults())

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{1}, Sandbox: "worktree"})

	if gh.markerCalls != 1 {
		t.Errorf("expected exactly one paginated marker-scan call, got %d", gh.markerCalls)
	}
	if runner.capturedPrompt == "" {
		t.Fatalf("expected exactly one RunPrompt attempt on PR creation path")
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
	if controlWriter.calls != 1 {
		t.Errorf("expected exactly one control-file write after PR creation (issue #2195), got %d write(s)", controlWriter.calls)
	}
}

// encodeJSONL marshals a slice of prPayloadBody entries into the
// newline-delimited JSON shape that `gh api --paginate` emits: one
// valid JSON document per line, no surrounding array.
func encodeJSONL(entries []prPayloadBody) []byte {
	var buf []byte
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	return buf
}
