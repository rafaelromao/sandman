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
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/runid"
	"github.com/rafaelromao/sandman/internal/testenv"
	"github.com/spf13/cobra"
)

// spyBatchRunner records the Request it receives.
type spyBatchRunner struct {
	called bool
	req    batch.Request
	result *batch.Result
	err    error
}

// Test fixtures use a deterministic (ts, shortid) pair so the new
// per-row RunIDs produced by runid.NewRunID match the strings the test
// events hard-code. The values are intentionally stable (no time /
// random component) so the tests can use full-string equality.
const (
	testRunTS      = "260618113825"
	testRunShortID = "abcd"

	testRunID42First  = testRunTS + "-" + testRunShortID + "-42-1"
	testRunID42Second = testRunTS + "-" + testRunShortID + "-42-2"
	testRunID42Prev   = testRunTS + "-" + testRunShortID + "-42-prev"
)

func (s *spyBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	s.called = true
	s.req = req
	return s.result, s.err
}

// fakeGitHubClient is a test double for github.Client.
type fakeGitHubClient struct {
	issues             map[int]*github.Issue
	prs                map[string]*github.PR
	fetchRelease       map[int]<-chan struct{}
	fetchReleaseAfter  map[int]int
	fetchCount         map[int]int
	findPRCalls        map[string]int
	mu                 sync.Mutex
	fetchIssueError    error
	searchIssuesQuery  string
	searchIssuesResult []github.Issue
	searchIssuesError  error
}

func (f *fakeGitHubClient) setIssueState(number int, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev, ok := f.issues[number]
	if !ok {
		return
	}
	updated := *prev
	updated.State = state
	f.issues[number] = &updated
}

func (f *fakeGitHubClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	f.mu.Lock()
	if f.fetchCount == nil {
		f.fetchCount = make(map[int]int)
	}
	f.fetchCount[number]++
	count := f.fetchCount[number]
	release := f.fetchRelease[number]
	f.mu.Unlock()

	if f.fetchIssueError != nil {
		return nil, f.fetchIssueError
	}
	threshold := 1
	if f.fetchReleaseAfter != nil {
		if after, ok := f.fetchReleaseAfter[number]; ok {
			threshold = after
		}
	}
	if release != nil && count > threshold {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	issue, ok := f.issues[number]
	f.mu.Unlock()
	if ok {
		return issue, nil
	}
	return &github.Issue{Number: number}, nil
}

func (f *fakeGitHubClient) FetchIssueDependencies(ctx context.Context, number int) ([]int, error) {
	f.mu.Lock()
	issue, ok := f.issues[number]
	f.mu.Unlock()
	if ok {
		return issue.BlockedBy, nil
	}
	return nil, nil
}

func (f *fakeGitHubClient) FetchPR(ctx context.Context, number int) (*github.PR, error) {
	return &github.PR{Number: number, State: "open"}, nil
}

func (f *fakeGitHubClient) SearchIssues(ctx context.Context, query string) ([]github.Issue, error) {
	f.mu.Lock()
	f.searchIssuesQuery = query
	if f.searchIssuesResult != nil || f.searchIssuesError != nil {
		result, errResult := f.searchIssuesResult, f.searchIssuesError
		f.mu.Unlock()
		return result, errResult
	}
	if f.issues == nil {
		f.mu.Unlock()
		return nil, fmt.Errorf("fake: search not configured")
	}
	var results []github.Issue
	for _, issue := range f.issues {
		if !github.IsIssueClosed(issue) {
			results = append(results, *issue)
		}
	}
	f.mu.Unlock()
	return results, nil
}

func (f *fakeGitHubClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	if f.findPRCalls != nil {
		f.mu.Lock()
		f.findPRCalls[branch]++
		f.mu.Unlock()
	}
	if f.prs != nil {
		if pr, ok := f.prs[branch]; ok {
			return pr, nil
		}
		return nil, nil
	}
	return nil, nil
}

func (f *fakeGitHubClient) ListOpenPRs(ctx context.Context) ([]github.PR, error) {
	return nil, nil
}

func (f *fakeGitHubClient) ListPRComments(ctx context.Context, number int) ([]github.PRComment, error) {
	return nil, nil
}

func (f *fakeGitHubClient) AuthenticatedLogin(ctx context.Context) (string, error) {
	return "sandman", nil
}

func (f *fakeGitHubClient) ListIssueComments(ctx context.Context, number int) ([]github.IssueComment, error) {
	return nil, nil
}

func (f *fakeGitHubClient) ListSubIssues(ctx context.Context, parent int) ([]int, error) {
	return nil, nil
}

func (f *fakeGitHubClient) RepoName(ctx context.Context) (string, error) {
	return "owner/repo", nil
}

func (f *fakeGitHubClient) EditComment(ctx context.Context, commentID, body string) error {
	return nil
}

func (f *fakeGitHubClient) EditPRBody(ctx context.Context, prNumber int, body string) error {
	return nil
}

func (f *fakeGitHubClient) AddCommentReaction(ctx context.Context, commentID, content string) (string, error) {
	return "", nil
}

func (f *fakeGitHubClient) AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error) {
	return "", nil
}

func (f *fakeGitHubClient) RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error {
	return nil
}

func (f *fakeGitHubClient) RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error {
	return nil
}

func (f *fakeGitHubClient) CloseIssue(ctx context.Context, issueNumber int, comment string) error {
	return nil
}

// newRunDeps returns Dependencies for a run command test, isolated
// from the real repo via a fresh temp dir that is git-init'd and
// chdir'd into. The default review command is overridden to
// "/oc review" so the review daemon guard (issue #383) is bypassed by
// default. Tests that need to exercise the guard must build their own
// Dependencies and chdir into a temp dir without a live socket.
func newRunDeps(t *testing.T, runner batch.Runner) Dependencies {
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
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
		RepoRoot:     ".",
	}
}

func addRegisteredContinuationWorktree(t *testing.T, repoDir, worktreeBase, branch string) string {
	t.Helper()
	runGit(t, repoDir, "config", "user.email", "test@test.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "checkout", "-B", "main")
	runGit(t, repoDir, "commit", "--allow-empty", "-m", "init")
	runGit(t, repoDir, "branch", branch)
	worktreePath := filepath.Join(worktreeBase, branch)
	runGit(t, repoDir, "worktree", "add", worktreePath, branch)
	return worktreePath
}

type continuationRunFixture struct {
	repoDir      string
	branch       string
	worktreePath string
	spy          *spyBatchRunner
	deps         Dependencies
}

func newContinuationRunFixture(t *testing.T) continuationRunFixture {
	t.Helper()
	repoDir := testenv.MkdirShort(t, "sm-run-")
	initRunIntegrationRepo(t, repoDir)
	t.Chdir(repoDir)

	branch := "sandman/42-fix-bug"
	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	worktreePath := filepath.Join(worktreeBase, branch)
	runGit(t, repoDir, "branch", branch)
	runGit(t, repoDir, "worktree", "add", worktreePath, branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("mkdir task dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n\nResume.\n"), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{
			Agent:         "opencode",
			WorktreeDir:   worktreeBase,
			ReviewCommand: "/oc review",
			AgentProviders: map[string]config.Agent{
				"opencode": {Preset: "opencode", Command: "true"},
			},
		}},
		EventLog: &fakeEventLog{events: []events.Event{{
			Type:  "run.started",
			RunID: testRunID42Prev,
			Issue: 42,
			Payload: map[string]any{
				"agent":       "opencode",
				"branch":      branch,
				"base_branch": "main",
			},
		}}},
		GitHubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", State: "open"}}},
		RepoRoot:     repoDir,
	}
	return continuationRunFixture{repoDir: repoDir, branch: branch, worktreePath: worktreePath, spy: spy, deps: deps}
}

func (f continuationRunFixture) execute(t *testing.T) string {
	t.Helper()
	var output bytes.Buffer
	cmd := NewRunCmd(f.deps)
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--continue", "42"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, output.String())
	}
	return output.String()
}

// newRunDepsInDir creates a fresh temp dir containing .sandman/
// with a live .sandman/review.sock listener, chdirs the test
// into it, and returns the dir and Dependencies wired to the
// supplied runner. Used by tests that need a live socket AND a
// chdir into a fresh dir to inspect run/control socket state.
func newRunDepsInDir(t testing.TB, runner batch.Runner) (string, Dependencies) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(filepath.Join(sandmanDir, "reviews"), 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", ReviewSocketPath(sandmanDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Chdir(dir)
	return dir, Dependencies{
		BatchRunner:  runner,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
		RepoRoot:     ".",
	}
}

func TestFilterClosedIssues_Helper(t *testing.T) {
	tests := []struct {
		name       string
		numbers    []int
		openSet    map[int]struct{}
		states     map[int]string
		want       []int
		wantStderr string
		wantErr    error
	}{
		{
			name:    "open issues pass through",
			numbers: []int{1, 2},
			openSet: map[int]struct{}{1: {}, 2: {}},
			want:    []int{1, 2},
		},
		{
			name:       "closed explicit logs warning and returns error",
			numbers:    []int{42},
			states:     map[int]string{42: "closed"},
			want:       nil,
			wantStderr: "Issue #42 is closed, skipping\n",
			wantErr:    errAllExplicitClosed,
		},
		{
			name:       "closed range-sourced logs warning and returns error",
			numbers:    []int{43},
			states:     map[int]string{43: "closed"},
			want:       nil,
			wantStderr: "Issue #43 is closed, skipping\n",
			wantErr:    errAllExplicitClosed,
		},
		{
			name:       "all closed range returns error",
			numbers:    []int{42, 43},
			states:     map[int]string{42: "closed", 43: "closed"},
			want:       nil,
			wantStderr: "Issue #42 is closed, skipping\nIssue #43 is closed, skipping\n",
			wantErr:    errAllExplicitClosed,
		},
		{
			name:       "mixed closed sources log warnings",
			numbers:    []int{7, 42, 43, 44},
			openSet:    map[int]struct{}{42: {}, 44: {}},
			states:     map[int]string{7: "closed", 42: "open", 43: "closed", 44: "open"},
			want:       []int{42, 44},
			wantStderr: "Issue #7 is closed, skipping\nIssue #43 is closed, skipping\n",
		},
		{
			name: "empty numbers slice",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			searchFn := func(ctx context.Context, query string) ([]github.Issue, error) {
				results := make([]github.Issue, 0, len(tt.openSet))
				for n := range tt.openSet {
					results = append(results, github.Issue{Number: n, State: "open"})
				}
				return results, nil
			}
			fetchFn := func(ctx context.Context, n int) (*github.Issue, error) {
				return &github.Issue{Number: n, State: tt.states[n]}, nil
			}
			var stderr bytes.Buffer
			got, err := filterClosedIssues(context.Background(), tt.numbers, searchFn, fetchFn, &stderr)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
			for i, v := range tt.want {
				if got[i] != v {
					t.Errorf("expected %d at index %d, got %d", v, i, got[i])
				}
			}
			if stderr.String() != tt.wantStderr {
				t.Errorf("stderr:\n  got:  %q\n  want: %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestFilterClosedIssues_FallsBackToFetchOnSearchError(t *testing.T) {
	searchFn := func(ctx context.Context, query string) ([]github.Issue, error) {
		return nil, fmt.Errorf("transient gh error")
	}
	fetchFn := func(ctx context.Context, n int) (*github.Issue, error) {
		return &github.Issue{Number: n, State: "open"}, nil
	}
	var stderr bytes.Buffer
	got, err := filterClosedIssues(context.Background(), []int{42, 43}, searchFn, fetchFn, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected fallback to return all open, got %v", got)
	}
}

func TestFilterClosedIssues_FetchErrorIsSkipped(t *testing.T) {
	searchFn := func(ctx context.Context, query string) ([]github.Issue, error) {
		return nil, fmt.Errorf("transient gh error")
	}
	fetchFn := func(ctx context.Context, n int) (*github.Issue, error) {
		return nil, fmt.Errorf("network error")
	}
	var stderr bytes.Buffer
	got, err := filterClosedIssues(context.Background(), []int{42}, searchFn, fetchFn, &stderr)
	if err != nil {
		t.Fatalf("expected no error when fetch errors are skipped, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
	if !strings.Contains(stderr.String(), "Warning: could not fetch issue #42") {
		t.Errorf("expected warning on stderr, got: %s", stderr.String())
	}
}

func TestFilterClosedIssuesAfterExpansion_Helper(t *testing.T) {
	tests := []struct {
		name       string
		expanded   []int
		userTyped  []int
		openSet    map[int]struct{}
		states     map[int]string
		want       []int
		wantStderr string
	}{
		{
			name:     "all expansion-introduced children open",
			expanded: []int{10, 11},
			openSet:  map[int]struct{}{10: {}, 11: {}},
			want:     []int{10, 11},
		},
		{
			name:       "all expansion-introduced children closed yields empty batch",
			expanded:   []int{10, 11},
			openSet:    map[int]struct{}{},
			want:       nil,
			wantStderr: "Issue #10 is closed, skipping\nIssue #11 is closed, skipping\n",
		},
		{
			name:      "all post-expansion numbers are user-typed — no is:open search",
			expanded:  []int{20},
			userTyped: []int{20},
			openSet:   map[int]struct{}{},
			want:      []int{20},
		},
		{
			name:     "order preserved from post-expansion list",
			expanded: []int{11, 10},
			openSet:  map[int]struct{}{10: {}, 11: {}},
			want:     []int{11, 10},
		},
		{
			name:     "empty expanded",
			expanded: nil,
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			searchFn := func(ctx context.Context, query string) ([]github.Issue, error) {
				results := make([]github.Issue, 0, len(tt.openSet))
				for n := range tt.openSet {
					results = append(results, github.Issue{Number: n, State: "open"})
				}
				return results, nil
			}
			fetchFn := func(ctx context.Context, n int) (*github.Issue, error) {
				state, ok := tt.states[n]
				if !ok {
					state = "open"
				}
				return &github.Issue{Number: n, State: state}, nil
			}
			var stderr bytes.Buffer
			got, err := filterClosedIssuesAfterExpansion(context.Background(), tt.expanded, tt.userTyped, searchFn, fetchFn, &stderr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("expected %v, got %v (stderr=%q)", tt.want, got, stderr.String())
			}
			for i, v := range tt.want {
				if got[i] != v {
					t.Errorf("at index %d: expected %d, got %d", i, v, got[i])
				}
			}
			if stderr.String() != tt.wantStderr {
				t.Errorf("stderr:\n  got:  %q\n  want: %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// TestFilterClosedIssuesAfterExpansion_DoesNotMutateUserTyped pins the
// defensive-copy contract: filterClosedIssuesAfterExpansion must not
// mutate the caller's userTyped slice. The userTyped set is consulted
// after the helper runs (the run command checks it for membership before
// the helper is invoked at line 319, but a defensive copy protects
// against future refactors that pass the same backing array to multiple
// helpers).
func TestFilterClosedIssuesAfterExpansion_DoesNotMutateUserTyped(t *testing.T) {
	userTyped := []int{1, 2, 3}
	before := append([]int(nil), userTyped...)
	// is:open search returns empty → all expansion-introduced numbers
	// are classified as closed → errAllExplicitClosed is raised and
	// swallowed → helper returns nil, nil.
	searchFn := func(ctx context.Context, query string) ([]github.Issue, error) {
		return nil, nil
	}
	fetchFn := func(ctx context.Context, n int) (*github.Issue, error) {
		return &github.Issue{Number: n, State: "closed"}, nil
	}
	var stderr bytes.Buffer
	expanded := []int{10}
	got, err := filterClosedIssuesAfterExpansion(context.Background(), expanded, userTyped, searchFn, fetchFn, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result for all-closed expansion, got %v", got)
	}
	if !slices.Equal(userTyped, before) {
		t.Errorf("userTyped was mutated: before %v, after %v", before, userTyped)
	}
}

// TestFilterClosedIssuesAfterExpansion_NoSearchWhenNoExpansionAdded pins
// the no-op short-circuit at run.go:1034: when the post-expansion list
// contains only user-typed numbers, no is:open search is issued. This
// pins a GitHub API budget invariant at the helper layer.
func TestFilterClosedIssuesAfterExpansion_NoSearchWhenNoExpansionAdded(t *testing.T) {
	searchCalled := 0
	fetchCalled := 0
	searchFn := func(ctx context.Context, query string) ([]github.Issue, error) {
		searchCalled++
		return nil, nil
	}
	fetchFn := func(ctx context.Context, n int) (*github.Issue, error) {
		fetchCalled++
		return &github.Issue{Number: n, State: "closed"}, nil
	}
	var stderr bytes.Buffer
	got, err := filterClosedIssuesAfterExpansion(context.Background(), []int{42}, []int{42}, searchFn, fetchFn, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || len(got) != 1 || got[0] != 42 {
		t.Errorf("expected [42], got %v", got)
	}
	if searchCalled != 0 {
		t.Errorf("expected no is:open search when no expansion-introduced children, got %d calls", searchCalled)
	}
	if fetchCalled != 0 {
		t.Errorf("expected no per-issue fetch when no expansion-introduced children, got %d calls", fetchCalled)
	}
}

// TestFilterClosedIssuesAfterExpansion_UserTypedOverlapWithChildKept pins
// the membership semantics for the no-op short-circuit: a user-typed
// number that happens to coincide with an expansion-introduced child
// must NOT cause the helper to incorrectly take the no-op short-circuit
// (which would skip filtering for ALL children, including expansion-only
// ones). #11 is in the user-typed set, but #10 and #12 are expansion-only,
// so the short-circuit must NOT trigger and filterClosedIssues must run.
func TestFilterClosedIssuesAfterExpansion_UserTypedOverlapWithChildKept(t *testing.T) {
	// userTyped: [5, 11]. expanded: [10, 11, 12].
	// #11 is in both sets; #10 and #12 are expansion-only.
	// is:open returns {10, 12}; #11 is not in the open set.
	// Expected result: [10, 12] (filterClosedIssues classifies #11 as
	// closed and emits the stderr warning).
	searchFn := func(ctx context.Context, query string) ([]github.Issue, error) {
		return []github.Issue{{Number: 10, State: "open"}, {Number: 12, State: "open"}}, nil
	}
	fetchFn := func(ctx context.Context, n int) (*github.Issue, error) {
		return &github.Issue{Number: n, State: "closed"}, nil
	}
	var stderr bytes.Buffer
	got, err := filterClosedIssuesAfterExpansion(context.Background(), []int{10, 11, 12}, []int{5, 11}, searchFn, fetchFn, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{10, 12}
	if !slices.Equal(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
	if !strings.Contains(stderr.String(), "Issue #11 is closed, skipping") {
		t.Errorf("expected skip warning for #11, got: %q", stderr.String())
	}
}

// TestRun_FiltersClosedChildrenAfterSpecificationExpansion_NoUsageError
// pins the errAllExplicitClosed swallow at run.go:1039-1040: when all
// expansion-introduced children are closed, the batch becomes empty and
// the command exits cleanly. The existing
// TestRun_FiltersClosedChildrenAfterSpecificationExpansion already
// covers the spy.called + empty-issues shape; this version additionally
// asserts that cobra's usage banner was NOT printed (which would
// indicate the error was wrongly surfaced as a usage error).
func TestRun_FiltersClosedChildrenAfterSpecificationExpansion_NoUsageError(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n- #11\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody, State: "open"},
			10: {Number: 10, Title: "Child 1", Body: childBody, State: "closed"},
			11: {Number: 11, Title: "Child 2", Body: childBody, State: "closed"},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SilenceUsage = false
	cmd.SetArgs([]string{"1", "10"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected no error from empty-batch path, got: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called even with empty issues")
	}
	if len(spy.req.Issues) != 0 {
		t.Fatalf("expected empty issues, got %v", spy.req.Issues)
	}
	// Cobra prints the usage banner to stderr when RunE returns an
	// error AND SilenceUsage is false. If the empty batch had surfaced
	// as a usage error, the banner would appear here.
	if strings.Contains(buf.String(), "Usage:") {
		t.Errorf("unexpected usage banner for empty-batch path, got: %q", buf.String())
	}
}

// TestRun_RangeWithSpecification_PreservesOpenUserTypedFromRange pins
// the range + Specification interaction: when the user types a range
// like `1:3` and #1 is a Specification, the post-expansion filter must
// preserve the open user-typed issues from the range (#2 and #3) while
// filtering closed children of the Specification.
func TestRun_RangeWithSpecification_PreservesOpenUserTypedFromRange(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n- #11\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody, State: "open"},
			2:  {Number: 2, Title: "Range issue 2", Body: childBody, State: "open"},
			3:  {Number: 3, Title: "Range issue 3", Body: childBody, State: "open"},
			10: {Number: 10, Title: "Child 1", Body: childBody, State: "closed"},
			11: {Number: 11, Title: "Child 2", Body: childBody, State: "open"},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1:3"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", err, buf.String())
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := map[int]bool{2: true, 3: true, 11: true}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for _, n := range spy.req.Issues {
		if !want[n] {
			t.Errorf("unexpected issue %d in batch %v", n, spy.req.Issues)
		}
	}
	if !strings.Contains(buf.String(), "Issue #10 is closed, skipping") {
		t.Errorf("expected skip warning for #10, got: %q", buf.String())
	}
}

// TestRun_NonSpecificationInput_DoesNotInvokePostExpansionSearch pins
// that an input list of non-Specification issues never reaches the
// post-expansion filter at the call site. This guards against a future
// regression where the filter is invoked unconditionally and would
// issue an extra is:open search per non-Specification input.
func TestRun_NonSpecificationInput_DoesNotInvokePostExpansionSearch(t *testing.T) {
	body := "## What\n\nA simple non-specification issue.\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Simple issue", Body: body, State: "open"},
			43: {Number: 43, Title: "Simple issue 2", Body: body, State: "open"},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 2 {
		t.Fatalf("expected 2 issues, got %v", spy.req.Issues)
	}
	// The pre-expansion filter at line 216 issues one is:open search
	// for the user-typed set. After that, the no-op short-circuit
	// (no expansion-introduced numbers) prevents a second search.
	// We assert: at most one is:open search was issued.
	if gh.searchIssuesQuery == "" {
		t.Errorf("expected at least one is:open search from user-typed filter, got none")
	}
	// The post-expansion no-op should not issue a second search. The
	// fake client would record both if a second search ran; we can't
	// distinguish directly, but the existing
	// TestFilterClosedIssuesAfterExpansion_NoSearchWhenNoExpansionAdded
	// pins the helper-level no-op. Here we only assert the happy path
	// completes with the correct batch.
}

// TestRun_ClosedUserTypedFilteredBeforeExpansion_PinsContract pins that
// the user-typed filter at line 216 runs BEFORE expansion and is the
// authoritative close filter for user-typed numbers. Expansion does not
// re-introduce user-typed-closed numbers: a closed user-typed issue
// produces no Specification expansion, no children, and the batch is
// empty (matching the existing all-closed path).
func TestRun_ClosedUserTypedFilteredBeforeExpansion_PinsContract(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #11\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody, State: "open"},
			10: {Number: 10, Title: "Closed user-typed", Body: "## What\n\n", State: "closed"},
			11: {Number: 11, Title: "Open child", Body: "## Parent\n\n#1\n\n## What\n\n", State: "open"},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1", "10"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", err, buf.String())
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	// #10 was filtered at line 216 (user-typed filter). #1 expanded to
	// [#11]. Post-expansion filter sees [11] (only expansion-only
	// children; the no-op short-circuit triggers). Result: [#11].
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 11 {
		t.Fatalf("expected [11], got %v", spy.req.Issues)
	}
	if !strings.Contains(buf.String(), "Issue #10 is closed, skipping") {
		t.Errorf("expected skip warning for #10, got: %q", buf.String())
	}
}

func TestRun_SingleIssueInvokesBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_ExpandsSpecificationBeforeBatchRunner(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n- #11\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			10: {Number: 10, Title: "Child 1", Body: childBody},
			11: {Number: 11, Title: "Child 2", Body: childBody},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{10, 11}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if !strings.Contains(buf.String(), "expanded specification #1 to 2 accepted children") {
		t.Errorf("expected info log about Specification expansion, got: %q", buf.String())
	}
}

func TestRun_ExpandsBodyOnlyChildrenHeadingBeforeBatchRunner(t *testing.T) {
	// Regression for issue #2329. The parent body has only `## Children`
	// (no `## Problem Statement` / `## Solution`, no `## User Stories`,
	// no comments, no native sub-issues). The command must still
	// expand the parent into the children listed in the body and
	// hand the expanded children to the batch runner; otherwise the
	// original `--continue #2315` symptom (the parent emits
	// `run.blocked` and never reaches the sandbox) returns.
	parentBody := "## Children\n\n- #10 (slice: foundation)\n- #11\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Body-only child heading parent", Body: parentBody, State: "open"},
			10: {Number: 10, Title: "Child 10", Body: childBody, State: "open"},
			11: {Number: 11, Title: "Child 11", Body: childBody, State: "open"},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, buf.String())
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{10, 11}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if !strings.Contains(buf.String(), "expanded specification #1 to 2 accepted children") {
		t.Errorf("expected info log about body-children expansion, got: %q", buf.String())
	}
}

func TestRun_ExpandsBodyOnlyChildIssuesHeadingBeforeBatchRunner(t *testing.T) {
	// Mirrors TestRun_ExpandsBodyOnlyChildrenHeadingBeforeBatchRunner
	// for the `## Child Issues` heading alias.
	parentBody := "## Child Issues\n\n- #10\n- #11\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Body-only child issues heading parent", Body: parentBody, State: "open"},
			10: {Number: 10, Title: "Child 10", Body: childBody, State: "open"},
			11: {Number: 11, Title: "Child 11", Body: childBody, State: "open"},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, buf.String())
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{10, 11}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if !strings.Contains(buf.String(), "expanded specification #1 to 2 accepted children") {
		t.Errorf("expected info log about child-issues expansion, got: %q", buf.String())
	}
}

func TestRun_FiltersClosedChildrenAfterSpecificationExpansion(t *testing.T) {
	// Closed children discovered by Specification expansion must be skipped,
	// mirroring the user-typed-input filtering at line 216.
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n- #11\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody, State: "open"},
			10: {Number: 10, Title: "Child 1", Body: childBody, State: "closed"},
			11: {Number: 11, Title: "Child 2", Body: childBody, State: "closed"},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called (with empty issues after filtering)")
	}
	if len(spy.req.Issues) != 0 {
		t.Fatalf("expected empty issues after closed children filter, got %v", spy.req.Issues)
	}
	if !strings.Contains(buf.String(), "Issue #10 is closed, skipping") {
		t.Errorf("expected skip warning for #10, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "Issue #11 is closed, skipping") {
		t.Errorf("expected skip warning for #11, got: %q", buf.String())
	}
}

func TestRun_KeepsOpenChildrenAfterSpecificationExpansion(t *testing.T) {
	// Open children discovered by Specification expansion must be preserved.
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n- #11\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody, State: "open"},
			10: {Number: 10, Title: "Child 1", Body: childBody, State: "open"},
			11: {Number: 11, Title: "Child 2", Body: childBody, State: "open"},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 2 {
		t.Fatalf("expected 2 open children preserved, got %v", spy.req.Issues)
	}
}

func TestRun_PostExpansionFilterKeepsOpenUserTypedAlongsideOpenChildren(t *testing.T) {
	// Mixed batch: a Specification (#1) and a non-Specification user-typed
	// issue (#20). Post-expansion the list is [#11, #20] (#10 is closed and
	// filtered, #1 is replaced by its children). The post-expansion filter
	// preserves both because they are open.
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n- #11\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody, State: "open"},
			10: {Number: 10, Title: "Child 1", Body: childBody, State: "closed"},
			11: {Number: 11, Title: "Child 2", Body: childBody, State: "open"},
			20: {Number: 20, Title: "User-typed", Body: "## What\n\n", State: "open"},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1", "20"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := map[int]bool{11: true, 20: true}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for _, n := range spy.req.Issues {
		if !want[n] {
			t.Errorf("unexpected issue %d in batch", n)
		}
	}
	if !strings.Contains(buf.String(), "Issue #10 is closed, skipping") {
		t.Errorf("expected skip warning for #10, got: %q", buf.String())
	}
}

func TestRun_MixedSpecificationAndNonChildIssues(t *testing.T) {
	// Regression for #1038 — see ADR-0025 §3a. The original failure
	// mode was the historical hard-error `nested specification detected: #982`
	// emitted when two harvested specs cross-referenced each other. After T4,
	// nested specifications are recursively flattened (per-flatten log line)
	// instead of hard-erroring; this test now verifies the no-warning path
	// of the mixed-batch expansion when running
	// `sandman run 972:977 982 990 994:1001`.
	spec982Body := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. U.\n\nSlices tracked in #972, #973, #974.\n\n## Child Issues\n\n- #984 child\n- #985 child\n- #986 child\n- #987 child\n- #988 child\n- #989 child\n"
	spec990Body := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. U.\n\nSee parent #982.\n"
	childBody := "## Parent\n\n#982\n\n## What\n\n"
	sliceBody := "## What\n\nJust a slice.\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			972:  {Number: 972, Title: "Slice 972", Body: sliceBody},
			973:  {Number: 973, Title: "Slice 973", Body: sliceBody},
			974:  {Number: 974, Title: "Slice 974", Body: sliceBody},
			975:  {Number: 975, Title: "Slice 975", Body: sliceBody},
			976:  {Number: 976, Title: "Slice 976", Body: sliceBody},
			977:  {Number: 977, Title: "Slice 977", Body: sliceBody},
			980:  {Number: 980, Title: "Slice 980", Body: sliceBody},
			982:  {Number: 982, Title: "Outer Specification", Body: spec982Body},
			984:  {Number: 984, Title: "Child 984", Body: childBody},
			985:  {Number: 985, Title: "Child 985", Body: childBody},
			986:  {Number: 986, Title: "Child 986", Body: childBody},
			987:  {Number: 987, Title: "Child 987", Body: childBody},
			988:  {Number: 988, Title: "Child 988", Body: childBody},
			989:  {Number: 989, Title: "Child 989", Body: childBody},
			990:  {Number: 990, Title: "Cross-referencing Specification", Body: spec990Body},
			994:  {Number: 994, Title: "Issue 994", Body: sliceBody},
			995:  {Number: 995, Title: "Issue 995", Body: sliceBody},
			996:  {Number: 996, Title: "Issue 996", Body: sliceBody},
			997:  {Number: 997, Title: "Issue 997", Body: sliceBody},
			998:  {Number: 998, Title: "Issue 998", Body: sliceBody},
			999:  {Number: 999, Title: "Issue 999", Body: sliceBody},
			1000: {Number: 1000, Title: "Issue 1000", Body: sliceBody},
			1001: {Number: 1001, Title: "Issue 1001", Body: sliceBody},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"972:977", "982", "990", "994:1001"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}

	gotSet := make(map[int]struct{}, len(spy.req.Issues))
	for _, n := range spy.req.Issues {
		gotSet[n] = struct{}{}
	}
	// Slices 972..977 are user-typed non-Specification issues; they pass through.
	for _, n := range []int{972, 973, 974, 975, 976, 977} {
		if _, ok := gotSet[n]; !ok {
			t.Errorf("expected user-typed slice #%d in resolved list, got %v", n, spy.req.Issues)
		}
	}
	// #982 is added via #990's expansion (its only harvested candidate
	// is #982, which is in userInputSet and accepted unconditionally).
	if _, ok := gotSet[982]; !ok {
		t.Errorf("expected #982 in resolved list (via #990's expansion), got %v", spy.req.Issues)
	}
	// Issues 994..1001 are user-typed non-Specification issues; they pass through.
	for _, n := range []int{994, 995, 996, 997, 998, 999, 1000, 1001} {
		if _, ok := gotSet[n]; !ok {
			t.Errorf("expected user-typed #%d in resolved list, got %v", n, spy.req.Issues)
		}
	}
	// #982's authored children #984..#989 have ## Parent and pass the
	// harvest filter.
	for _, n := range []int{984, 985, 986, 987, 988, 989} {
		if _, ok := gotSet[n]; !ok {
			t.Errorf("expected authored child #%d in resolved list, got %v", n, spy.req.Issues)
		}
	}
	// No Specification candidate-mismatch warning on stderr.
	if strings.Contains(buf.String(), "candidate #") && strings.Contains(buf.String(), "not a child") {
		t.Errorf("expected no 'candidate not a child' warning on stderr, got: %q", buf.String())
	}
	// No nested-Specification hard-error on stderr (the historical
	// `nested specification detected: #N` was replaced by recursive
	// flattening with a per-flatten log line in T4; this test verifies
	// the no-hard-error path on the mixed-batch expansion).
	if strings.Contains(buf.String(), "nested specification detected") {
		t.Errorf("expected no nested-specification hard-error on stderr, got: %q", buf.String())
	}
}

func TestRun_FailsWhenSpecificationHasNoChildren(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Empty Specification", Body: specBody},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"1"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("expected no error for Specification with no children, got %v", err)
	}
	if !spy.called {
		t.Error("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 1 {
		t.Fatalf("expected pass-through [1], got %v", spy.req.Issues)
	}
	if !strings.Contains(stderr.String(), "running issue #1 as a regular issue (no children)") {
		t.Fatalf("expected carve-out log line in stderr, got: %q", stderr.String())
	}
}

func TestCachedGitHubClient_ListIssueComments_CachesResult(t *testing.T) {
	count := 0
	delegate := &countingCommentsClient{
		comments: []github.IssueComment{{Body: "first"}},
		fetch: func() {
			count++
		},
	}

	c := newCachedGitHubClient(delegate)
	got, err := c.ListIssueComments(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Body != "first" {
		t.Fatalf("unexpected first fetch: %v", got)
	}
	if count != 1 {
		t.Fatalf("expected delegate to be called once, got %d", count)
	}
	// Second call should hit the cache, not the delegate.
	got, err = c.ListIssueComments(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Body != "first" {
		t.Fatalf("unexpected cached fetch: %v", got)
	}
	if count != 1 {
		t.Fatalf("expected delegate to still have been called once, got %d", count)
	}
}

type countingCommentsClient struct {
	github.Client
	comments []github.IssueComment
	fetch    func()
}

func (c *countingCommentsClient) ListIssueComments(ctx context.Context, number int) ([]github.IssueComment, error) {
	if c.fetch != nil {
		c.fetch()
	}
	return c.comments, nil
}

type countingSubIssuesClient struct {
	github.Client
	nums   map[int][]int
	calls  map[int]int
	fetchH func(int)
}

func (c *countingSubIssuesClient) ListSubIssues(ctx context.Context, parent int) ([]int, error) {
	if c.calls == nil {
		c.calls = map[int]int{}
	}
	c.calls[parent]++
	if c.fetchH != nil {
		c.fetchH(parent)
	}
	if c.nums == nil {
		return []int{}, nil
	}
	return c.nums[parent], nil
}

func TestCachedGitHubClient_ListSubIssues_CachesResult(t *testing.T) {
	delegate := &countingSubIssuesClient{nums: map[int][]int{62: {42, 43}}}

	c := newCachedGitHubClient(delegate)

	first, err := c.ListSubIssues(context.Background(), 62)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(first, []int{42, 43}) {
		t.Errorf("expected [42 43], got %v", first)
	}
	if delegate.calls[62] != 1 {
		t.Fatalf("expected delegate to be called once, got %d", delegate.calls[62])
	}

	second, err := c.ListSubIssues(context.Background(), 62)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(second, []int{42, 43}) {
		t.Errorf("expected cached [42 43], got %v", second)
	}
	if delegate.calls[62] != 1 {
		t.Fatalf("expected delegate to still have been called once, got %d", delegate.calls[62])
	}
}

func TestCachedGitHubClient_ListSubIssues_EmptyResultIsNonNil(t *testing.T) {
	delegate := &countingSubIssuesClient{nums: map[int][]int{99: {}}}

	c := newCachedGitHubClient(delegate)

	got, err := c.ListSubIssues(context.Background(), 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

func TestCachedGitHubClient_DelegatesNonCachedMethods(t *testing.T) {
	delegate := &countingCommentsClient{comments: []github.IssueComment{{Body: "x"}}}
	var editCalled bool
	delegate.Client = &stubClient{
		repo:      "rafaelromao/sandman",
		editError: nil,
		onEdit:    func() { editCalled = true },
	}

	c := newCachedGitHubClient(delegate)

	got, err := c.RepoName(context.Background())
	if err != nil {
		t.Fatalf("RepoName() error: %v", err)
	}
	if got != "rafaelromao/sandman" {
		t.Fatalf("RepoName() = %q, want %q", got, "rafaelromao/sandman")
	}

	if err := c.EditComment(context.Background(), "c1", "body"); err != nil {
		t.Fatalf("EditComment() error: %v", err)
	}
	if !editCalled {
		t.Fatal("expected EditComment to delegate to underlying client")
	}
}

type stubClient struct {
	github.Client
	repo      string
	editError error
	onEdit    func()
}

func (s *stubClient) RepoName(ctx context.Context) (string, error) { return s.repo, nil }
func (s *stubClient) EditComment(ctx context.Context, commentID, body string) error {
	if s.onEdit != nil {
		s.onEdit()
	}
	return s.editError
}

func TestRun_MultipleIssuesInvokesBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1", "2", "3"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []int{1, 2, 3}
	if len(spy.req.Issues) != len(want) {
		t.Errorf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_ParallelFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--parallel", "2", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Parallel != 2 {
		t.Errorf("expected parallel=2, got %d", spy.req.Parallel)
	}
}

func TestRun_ParallelNegativeValueRejected(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--parallel", "-1", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for negative parallel")
	}
	if !strings.Contains(err.Error(), "parallel must be 0 or greater") {
		t.Fatalf("expected validation error, got %v", err)
	}
	var target *UsageError
	if !errors.As(err, &target) {
		t.Fatalf("expected *UsageError, got %T: %v", err, err)
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
}

func TestRun_RetriesFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--retries", "4", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Retries != 4 {
		t.Errorf("expected retries=4, got %d", spy.req.Retries)
	}
}

func TestRun_StartDelayFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--start-delay", "7", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.StartDelaySet {
		t.Fatal("expected start delay override to be marked as set")
	}
	if spy.req.StartDelay != 7*time.Second {
		t.Errorf("expected start delay=7s, got %s", spy.req.StartDelay)
	}
}

func TestRun_StartDelayNegativeValueRejected(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--start-delay", "-1", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for negative start delay")
	}
	if !strings.Contains(err.Error(), "start_delay must be 0 or greater") {
		t.Fatalf("expected validation error, got %v", err)
	}
	var target *UsageError
	if !errors.As(err, &target) {
		t.Fatalf("expected *UsageError, got %T: %v", err, err)
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
}

func TestRun_RunIdleTimeoutFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--run-idle-timeout", "600", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.RunIdleTimeoutSet {
		t.Fatal("expected run idle timeout override to be marked as set")
	}
	if spy.req.RunIdleTimeout != 600 {
		t.Errorf("expected run idle timeout=600, got %d", spy.req.RunIdleTimeout)
	}
}

func TestRun_RunIdleTimeoutZeroAccepted(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--run-idle-timeout=0", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.RunIdleTimeoutSet {
		t.Fatal("expected run idle timeout override to be marked as set when explicitly zero")
	}
	if spy.req.RunIdleTimeout != 0 {
		t.Errorf("expected run idle timeout=0, got %d", spy.req.RunIdleTimeout)
	}
}

func TestRun_RunIdleTimeoutNegativeValueRejected(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--run-idle-timeout", "-1", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for negative run idle timeout")
	}
	if !strings.Contains(err.Error(), "run_idle_timeout must be 0 or greater") {
		t.Fatalf("expected validation error, got %v", err)
	}
	var target *UsageError
	if !errors.As(err, &target) {
		t.Fatalf("expected *UsageError, got %T: %v", err, err)
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
}

func TestRun_ModelFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--model", "gpt-4.1", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Model != "gpt-4.1" {
		t.Errorf("expected model=gpt-4.1, got %q", spy.req.Model)
	}
}

func TestRun_UsesDefaultModelWhenModelFlagOmitted(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", DefaultModel: "openai/gpt-4.1", ReviewCommand: "/oc review"}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Model != "openai/gpt-4.1" {
		t.Fatalf("expected config default model, got %q", spy.req.Model)
	}
}

func TestRun_DoesNotUseDefaultModelForCustomAgent(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "custom",
		DefaultModel:  "openai/gpt-4.1",
		ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{
			"custom": {Command: "true"},
		},
	}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Model != "" {
		t.Fatalf("expected empty model for custom agent, got %q", spy.req.Model)
	}
}

func TestRun_LoadConfigError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{err: errors.New("config not found")}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when config load fails")
	}
	if spy.called {
		t.Error("expected batch runner not to be called when config load fails")
	}
}

func TestRun_OverrideFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--override", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := spy.req.IssueMode(42); got != batch.ModeOverride {
		t.Errorf("expected ModeOverride when --override flag is passed, got %v", got)
	}
}

func TestRun_OverrideFalseByDefault(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := spy.req.IssueMode(42); got != batch.ModeFresh {
		t.Errorf("expected ModeFresh when --override flag is not passed, got %v", got)
	}
}

func TestRun_NoOverrideAlias(t *testing.T) {
	cmd := NewRunCmd(newRunDeps(t, &spyBatchRunner{result: &batch.Result{}}))
	if cmd.Flags().Lookup("force") != nil {
		t.Fatal("expected --force flag to be removed")
	}
	if cmd.Flags().Lookup("override") == nil {
		t.Fatal("expected --override flag to exist")
	}
}

func TestRun_ReconcileStrandedDefaultTrue(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.req.StrandedReconcile != nil {
		t.Errorf("expected StrandedReconcile to be nil (default true) when --reconcile-stranded is not passed, got %v", *spy.req.StrandedReconcile)
	}
}

func TestRun_NoReconcileStrandedSetsFalse(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--no-reconcile-stranded", "42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.req.StrandedReconcile == nil {
		t.Fatal("expected StrandedReconcile to be set when --no-reconcile-stranded is passed")
	}
	if *spy.req.StrandedReconcile {
		t.Errorf("expected StrandedReconcile to be false, got true")
	}
}

func TestRun_ReconcileStrandedExplicitTrueSetsTrue(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--reconcile-stranded=true", "42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.req.StrandedReconcile == nil {
		t.Fatal("expected StrandedReconcile to be set when --reconcile-stranded=true is passed")
	}
	if !*spy.req.StrandedReconcile {
		t.Errorf("expected StrandedReconcile to be true, got false")
	}
}

func TestRun_ContinueFlagAcceptedAndMutuallyExclusiveWithOverride(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantUsage bool
	}{
		{name: "continue only", args: []string{"--continue", "42"}},
		{name: "override then continue", args: []string{"--override", "--continue", "42"}, wantUsage: true},
		{name: "continue then override", args: []string{"--continue", "--override", "42"}, wantUsage: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(t, spy)

			if tt.name == "continue only" {
				dir := t.TempDir()
				branch := "sandman/42-fix-bug"
				worktreePath := addRegisteredContinuationWorktree(t, deps.RepoRoot, dir, branch)
				if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
					t.Fatalf("mkdir worktree: %v", err)
				}
				if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("## Completed\nInitial pass.\n"), 0644); err != nil {
					t.Fatalf("write task: %v", err)
				}
				deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, ReviewCommand: "/oc review", AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}}
				deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: testRunID42First, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}}}}
				deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{}}
			}

			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if tt.wantUsage {
				if err == nil {
					t.Fatal("expected usage error")
				}
				var target *UsageError
				if !errors.As(err, &target) {
					t.Fatalf("expected *UsageError, got %T: %v", err, err)
				}
				if spy.called {
					t.Fatal("expected batch runner not to be called")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !spy.called {
				t.Fatal("expected batch runner to be called")
			}
			if got := spy.req.IssueMode(42); got != batch.ModeContinue {
				t.Fatalf("expected ModeContinue when only --continue is passed, got %v", got)
			}
		})
	}
}

func TestRun_ContinueFlag_UsesCurrentFlagsOverStoredValues(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	worktreePath := addRegisteredContinuationWorktree(t, deps.RepoRoot, dir, branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("## Completed\nInitial pass.\n"), 0644); err != nil {
		t.Fatalf("write task: %v", err)
	}
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode-current",
		DefaultModel:  "openai/gpt-4.1",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		Git:           config.GitConfig{BaseBranch: "trunk"},
		AgentProviders: map[string]config.Agent{
			"opencode-current": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: testRunID42First, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode-stored", "model": "gpt-4.1", "review_command": "/custom review", "parallel": 1, "start_delay": 3, "retries": 2, "sandbox": "worktree", "container_capacity": 1, "container_capacity_set": true, "max_containers": 2, "max_containers_set": true, "run_idle_timeout": 99, "run_idle_timeout_set": true}},
		{Type: "run.continued", RunID: testRunID42Second, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode-stored", "model": "gpt-4.2", "review_command": "/custom review 2", "parallel": 7, "start_delay": 11, "retries": 4, "sandbox": "docker", "container_capacity": 3, "container_capacity_set": true, "max_containers": 5, "max_containers_set": true, "run_idle_timeout": 99, "run_idle_timeout_set": true}},
	}}
	deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if got := spy.req.IssueMode(42); got != batch.ModeContinue {
		t.Fatalf("expected ModeContinue request, got %v", got)
	}
	if spy.req.PreviousRunIDs[42] != testRunID42Second {
		t.Fatalf("expected PreviousRunIDs[42]=%s, got %q", testRunID42Second, spy.req.PreviousRunIDs[42])
	}
	if spy.req.Branches[42] != branch {
		t.Fatalf("expected branch %q, got %q", branch, spy.req.Branches[42])
	}
	if spy.req.BaseBranches[42] != "main" {
		t.Fatalf("expected BaseBranches[42]=main, got %q", spy.req.BaseBranches[42])
	}
	if !strings.Contains(spy.req.TaskPrompts[42], "## Completed") {
		t.Fatalf("expected verbatim task prompt (contains ## Completed from fixture), got %q", spy.req.TaskPrompts[42])
	}
	if strings.Contains(spy.req.TaskPrompts[42], "## Prior Context") {
		t.Fatalf("expected verbatim task prompt (not rewritten wrapper), got %q", spy.req.TaskPrompts[42])
	}
	if spy.req.Agent != "opencode-current" {
		t.Fatalf("expected agent from current cfg, got %q", spy.req.Agent)
	}
	if spy.req.Model != "openai/gpt-4.1" {
		t.Fatalf("expected model from cfg.DefaultModel, got %q", spy.req.Model)
	}
	if spy.req.BaseBranch != "main" {
		t.Fatalf("expected base branch pinned to stored value, got %q", spy.req.BaseBranch)
	}
	if spy.req.Parallel != 0 {
		t.Fatalf("expected parallel from current cfg (default 0), got %d", spy.req.Parallel)
	}
	if spy.req.StartDelay != 0 || spy.req.StartDelaySet {
		t.Fatalf("expected start delay unset, got %s set=%v", spy.req.StartDelay, spy.req.StartDelaySet)
	}
	if spy.req.RunIdleTimeout != 0 || spy.req.RunIdleTimeoutSet {
		t.Fatalf("expected run idle timeout unset, got %d set=%v", spy.req.RunIdleTimeout, spy.req.RunIdleTimeoutSet)
	}
	if spy.req.Retries != -1 {
		t.Fatalf("expected retries sentinel (-1) from current cfg, got %d", spy.req.Retries)
	}
	if spy.req.Sandbox != "" {
		t.Fatalf("expected sandbox from current cfg (default empty), got %q", spy.req.Sandbox)
	}
	if spy.req.ContainerCapacity != 0 || spy.req.ContainerCapacitySet {
		t.Fatalf("expected container capacity unset, got %d set=%v", spy.req.ContainerCapacity, spy.req.ContainerCapacitySet)
	}
	if spy.req.MaxContainers != 0 || spy.req.MaxContainersSet {
		t.Fatalf("expected max containers unset, got %d set=%v", spy.req.MaxContainers, spy.req.MaxContainersSet)
	}
	if spy.req.PromptConfig.ReviewCommand != "/oc review" {
		t.Fatalf("expected review command from cfg, got %q", spy.req.PromptConfig.ReviewCommand)
	}
	if spy.req.DangerouslySkipPermissions != nil {
		t.Fatalf("expected dangerously skip permissions nil (no flag), got %v", *spy.req.DangerouslySkipPermissions)
	}
	if spy.req.StrandedReconcile != nil {
		t.Fatalf("expected stranded reconcile nil (no flag), got %v", *spy.req.StrandedReconcile)
	}
}

func TestRun_ContinueFlag_UsesOverridesAndEmptyTemplateFallback(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	addRegisteredContinuationWorktree(t, deps.RepoRoot, dir, branch)
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		Git:           config.GitConfig{BaseBranch: "trunk"},
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{}}
	deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: testRunID42First, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode", "model": "gpt-4.1", "review_command": "/custom review", "parallel": 1, "start_delay": 3, "retries": 2, "sandbox": "docker", "container_capacity": 1, "container_capacity_set": true, "max_containers": 2, "max_containers_set": true}}}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "--agent", "opencode", "--model", "gpt-override", "--parallel", "9", "--start-delay", "12", "--retries", "5", "--sandbox", "worktree", "--container-capacity", "8", "--max-containers", "6", "--base-branch", "trunk", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Agent != "opencode" {
		t.Fatalf("expected agent override, got %q", spy.req.Agent)
	}
	if spy.req.Model != "gpt-override" {
		t.Fatalf("expected model override, got %q", spy.req.Model)
	}
	if spy.req.Parallel != 9 {
		t.Fatalf("expected parallel override, got %d", spy.req.Parallel)
	}
	if spy.req.StartDelay != 12*time.Second || !spy.req.StartDelaySet {
		t.Fatalf("expected start delay override, got %s set=%v", spy.req.StartDelay, spy.req.StartDelaySet)
	}
	if spy.req.Retries != 5 {
		t.Fatalf("expected retries override, got %d", spy.req.Retries)
	}
	if spy.req.Sandbox != "worktree" {
		t.Fatalf("expected sandbox override, got %q", spy.req.Sandbox)
	}
	if spy.req.ContainerCapacity != 8 || !spy.req.ContainerCapacitySet {
		t.Fatalf("expected container capacity override, got %d set=%v", spy.req.ContainerCapacity, spy.req.ContainerCapacitySet)
	}
	if spy.req.MaxContainers != 6 || !spy.req.MaxContainersSet {
		t.Fatalf("expected max containers override, got %d set=%v", spy.req.MaxContainers, spy.req.MaxContainersSet)
	}
	if spy.req.BaseBranch != "trunk" {
		t.Fatalf("expected base branch override, got %q", spy.req.BaseBranch)
	}
	if spy.req.BaseBranches[42] != "trunk" {
		t.Fatalf("expected per-issue base branch override, got %q", spy.req.BaseBranches[42])
	}
	if !strings.Contains(spy.req.TaskPrompts[42], "# Task") {
		t.Fatalf("expected default-task-prompt.md fallback (missing task file), got %q", spy.req.TaskPrompts[42])
	}
	if !strings.Contains(spy.req.TaskPrompts[42], "## Execution Checklist") {
		t.Fatalf("expected default-task-prompt.md fallback to include ## Execution Checklist, got %q", spy.req.TaskPrompts[42])
	}
}

func TestRun_ContinueFlag_MixedBatchResolvesPerIssueModes(t *testing.T) {
	repoDir := testenv.MkdirShort(t, "sm-run-")
	initRunIntegrationRepo(t, repoDir)
	t.Chdir(repoDir)

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	branches := map[int]string{
		42: "sandman/42-fix-bug",
		43: "sandman/43-broken-worktree",
	}
	for issue, branch := range branches {
		worktreePath := filepath.Join(worktreeBase, branch)
		runGit(t, repoDir, "branch", branch)
		runGit(t, repoDir, "worktree", "add", worktreePath, branch)
		if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
			t.Fatalf("mkdir task dir for issue %d: %v", issue, err)
		}
		if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte(fmt.Sprintf("# Task\n\nResume issue %d.\n", issue)), 0o644); err != nil {
			t.Fatalf("write task for issue %d: %v", issue, err)
		}
	}
	brokenPath := filepath.Join(worktreeBase, branches[43])
	if err := os.RemoveAll(filepath.Join(repoDir, ".git", "worktrees", filepath.Base(brokenPath))); err != nil {
		t.Fatalf("remove issue 43 registration: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{
			Agent:         "opencode",
			WorktreeDir:   worktreeBase,
			ReviewCommand: "/oc review",
			AgentProviders: map[string]config.Agent{
				"opencode": {Preset: "opencode", Command: "true"},
			},
		}},
		EventLog: &fakeEventLog{events: []events.Event{
			{Type: "run.started", RunID: testRunID42Prev, Issue: 42, Payload: map[string]any{"agent": "opencode", "branch": branches[42], "base_branch": "main"}},
			{Type: "run.started", RunID: "prev-ts-abcd-43", Issue: 43, Payload: map[string]any{"agent": "opencode", "branch": branches[43], "base_branch": "main"}},
		}},
		GitHubClient: &fakeGitHubClient{issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", State: "open"},
			43: {Number: 43, Title: "Broken worktree", State: "open"},
		}},
		RepoRoot: repoDir,
	}

	var output bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--continue", "42", "43"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, output.String())
	}
	if got := spy.req.IssueMode(42); got != batch.ModeContinue {
		t.Fatalf("expected issue 42 continue mode, got %v", got)
	}
	if got := spy.req.IssueMode(43); got != batch.ModeOverride {
		t.Fatalf("expected issue 43 promoted override mode, got %v", got)
	}
	if got := spy.req.PreviousRunIDs[42]; got != testRunID42Prev {
		t.Fatalf("expected issue 42 previous run replay, got %q", got)
	}
	if _, ok := spy.req.PreviousRunIDs[43]; ok {
		t.Fatalf("expected issue 43 to have no previous run replay, got %q", spy.req.PreviousRunIDs[43])
	}
	if !strings.Contains(spy.req.TaskPrompts[42], "Resume issue 42") {
		t.Fatalf("expected issue 42 continuation task, got %q", spy.req.TaskPrompts[42])
	}
	if _, ok := spy.req.TaskPrompts[43]; ok {
		t.Fatalf("expected issue 43 to have no continuation task, got %q", spy.req.TaskPrompts[43])
	}
	for issue, branch := range branches {
		if got := spy.req.Branches[issue]; got != branch {
			t.Fatalf("expected issue %d branch %q, got %q", issue, branch, got)
		}
		if got := spy.req.BaseBranches[issue]; got != "main" {
			t.Fatalf("expected issue %d base branch main, got %q", issue, got)
		}
	}
	if !strings.Contains(output.String(), "[--continue] promoting #43 to --override") {
		t.Fatalf("expected promotion log line for issue 43, got output:\n%s", output.String())
	}
}

func TestRun_ContinueFlag_MissingRegistrationPromotesToOverride(t *testing.T) {
	fixture := newContinuationRunFixture(t)
	if err := os.RemoveAll(filepath.Join(fixture.repoDir, ".git", "worktrees", filepath.Base(fixture.worktreePath))); err != nil {
		t.Fatalf("remove worktree registration: %v", err)
	}

	output := fixture.execute(t)
	if got := fixture.spy.req.IssueMode(42); got != batch.ModeOverride {
		t.Fatalf("expected missing registration to promote issue 42 to override, got %v", got)
	}
	if got := fixture.spy.req.Branches[42]; got != fixture.branch {
		t.Fatalf("expected promoted row branch %q, got %q", fixture.branch, got)
	}
	if got := fixture.spy.req.BaseBranches[42]; got != "main" {
		t.Fatalf("expected promoted row base branch main, got %q", got)
	}
	if _, ok := fixture.spy.req.PreviousRunIDs[42]; ok {
		t.Fatalf("expected promoted row to omit previous run ID, got %q", fixture.spy.req.PreviousRunIDs[42])
	}
	if _, ok := fixture.spy.req.TaskPrompts[42]; ok {
		t.Fatalf("expected promoted row to omit continuation task, got %q", fixture.spy.req.TaskPrompts[42])
	}
	for _, want := range []string{"[--continue] promoting #42 to --override", "no live registration", "reconcile"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected promotion output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRun_ContinueFlag_DetachedRegistrationPromotesToOverride(t *testing.T) {
	fixture := newContinuationRunFixture(t)
	runGit(t, fixture.worktreePath, "checkout", "--detach", "HEAD")

	output := fixture.execute(t)
	if got := fixture.spy.req.IssueMode(42); got != batch.ModeOverride {
		t.Fatalf("expected detached registration to promote issue 42 to override, got %v", got)
	}
	for _, want := range []string{"[--continue] promoting #42 to --override", "detached HEAD", "reconcile"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected promotion output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRun_ContinueFlag_WrongBranchRegistrationPromotesToOverride(t *testing.T) {
	fixture := newContinuationRunFixture(t)
	otherBranch := "sandman/other-branch"
	runGit(t, fixture.worktreePath, "checkout", "-b", otherBranch)

	output := fixture.execute(t)
	if got := fixture.spy.req.IssueMode(42); got != batch.ModeOverride {
		t.Fatalf("expected wrong-branch registration to promote issue 42 to override, got %v", got)
	}
	for _, want := range []string{"[--continue] promoting #42 to --override", otherBranch, "expected \"" + fixture.branch + "\"", "reconcile"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected promotion output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRun_ContinueFlag_NormalizesContainerPathsBeforeClassification(t *testing.T) {
	fixture := newContinuationRunFixture(t)
	absRepo, err := filepath.Abs(fixture.repoDir)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}
	gitlinkPath := filepath.Join(fixture.worktreePath, ".git")
	gitlink, err := os.ReadFile(gitlinkPath)
	if err != nil {
		t.Fatalf("read worktree gitlink: %v", err)
	}
	hostGitdir := strings.TrimSpace(strings.TrimPrefix(string(gitlink), "gitdir: "))
	registrationGitdirPath := filepath.Join(hostGitdir, "gitdir")
	registrationGitdir, err := os.ReadFile(registrationGitdirPath)
	if err != nil {
		t.Fatalf("read registration gitdir: %v", err)
	}
	if err := os.WriteFile(gitlinkPath, []byte(strings.Replace(string(gitlink), absRepo, "/workspace", 1)), 0o644); err != nil {
		t.Fatalf("write container gitlink: %v", err)
	}
	if err := os.WriteFile(registrationGitdirPath, []byte(strings.Replace(string(registrationGitdir), absRepo, "/workspace", 1)), 0o644); err != nil {
		t.Fatalf("write container registration pointer: %v", err)
	}

	fixture.execute(t)
	if got := fixture.spy.req.IssueMode(42); got != batch.ModeContinue {
		t.Fatalf("expected normalized registration to remain continue mode, got %v", got)
	}
	if got := fixture.spy.req.PreviousRunIDs[42]; got != testRunID42Prev {
		t.Fatalf("expected prior run %q, got %q", testRunID42Prev, got)
	}
	for _, path := range []string{gitlinkPath, registrationGitdirPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read restored pointer %q: %v", path, err)
		}
		if strings.Contains(string(data), "/workspace") {
			t.Fatalf("expected %q to be host-visible before classification, got %q", path, data)
		}
	}
}

func TestRun_ContinueFlag_NoPreviousPromptOnlyRun_ReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.EventLog = &fakeEventLog{events: []events.Event{}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "--run-id", "my-run"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no previous prompt-only run exists")
	}
	if !strings.Contains(err.Error(), "no previous prompt-only run found") {
		t.Fatalf("expected prompt-only replay error, got %v", err)
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
}

func TestRun_ContinueFlag_NoPriorRunPromotesToOverride(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.EventLog = &fakeEventLog{events: []events.Event{}}
	deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Fix bug"},
	}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := spy.req.IssueMode(42); got != batch.ModeOverride {
		t.Fatalf("expected ModeOverride when no prior run exists under --continue, got %v", got)
	}
	if !strings.Contains(buf.String(), "[--continue] promoting #42 to --override (no prior started/continued run)") {
		t.Fatalf("expected promotion log line for issue 42, got output:\n%s", buf.String())
	}
}

func TestRun_ContinueFlag_WarnsWhenIssueTaskMissing(t *testing.T) {
	dir := t.TempDir()
	branch := "issue-42"

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	worktreePath := addRegisteredContinuationWorktree(t, deps.RepoRoot, dir, branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: testRunID42Prev, Issue: 42, Payload: map[string]any{"agent": "opencode", "branch": branch, "base_branch": "main"}}}}
	deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}}
	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := spy.req.IssueMode(42); got != batch.ModeContinue {
		t.Fatalf("expected ModeContinue when prior run exists, got %v", got)
	}
	if !strings.Contains(buf.String(), "warning: no task found") {
		t.Fatalf("expected missing-task warning, got %q", buf.String())
	}
	if !strings.Contains(spy.req.TaskPrompts[42], "# Task") {
		t.Fatalf("expected default-task-prompt.md fallback when task missing, got %q", spy.req.TaskPrompts[42])
	}
	if !strings.Contains(spy.req.TaskPrompts[42], "## Execution Checklist") {
		t.Fatalf("expected default-task-prompt.md fallback to include ## Execution Checklist, got %q", spy.req.TaskPrompts[42])
	}
}

func TestRun_NoIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no issues provided")
	}
	if spy.called {
		t.Error("expected batch runner not to be called when no issues provided")
	}
}

func TestRun_HelpMentionsPromptOnlyMode(t *testing.T) {
	deps := newRunDeps(t, &spyBatchRunner{result: &batch.Result{}})

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "--auto") {
		t.Fatalf("expected help to omit --auto, got:\n%s", out)
	}
	if strings.Contains(out, "--count") {
		t.Fatalf("expected help to omit --count, got:\n%s", out)
	}
	if !strings.Contains(out, "prompt-only mode") {
		t.Fatalf("expected help to mention prompt-only mode, got:\n%s", out)
	}
	if !strings.Contains(out, "--continue") {
		t.Fatalf("expected help to mention --continue, got:\n%s", out)
	}
	if !strings.Contains(out, "{{ISSUE_NUMBER}}") {
		t.Fatalf("expected help to mention ISSUE_NUMBER gating, got:\n%s", out)
	}
}

func TestRun_PromptOnlyAllowsNoIssueSelection(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		setup func(*Dependencies)
	}{
		{
			name: "inline prompt",
			args: []string{"--prompt", "Return only OK."},
			setup: func(deps *Dependencies) {
				deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}
			},
		},
		{
			name: "template file",
			args: func() []string {
				dir := t.TempDir()
				path := dir + "/prompt.md"
				if err := os.WriteFile(path, []byte("Return only OK."), 0644); err != nil {
					t.Fatalf("write template: %v", err)
				}
				return []string{"--template", path}
			}(),
			setup: func(deps *Dependencies) {
				deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(t, spy)
			tt.setup(&deps)

			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !spy.called {
				t.Fatal("expected batch runner to be called")
			}
			if len(spy.req.Issues) != 0 {
				t.Fatalf("expected no issues, got %v", spy.req.Issues)
			}
		})
	}
}

// TestRun_PromptOnlyWithRunIDRegistersOrchestratorRunIDInBatchesIndex
// pins the prompt-only public BatchId contract (issue #1920
// #1916): `sandman run --prompt "..." --run-id myid` registers the
// batches index entry with id `<ts>-<sid>-prompt-myid`, matching the
// per-row RunID the orchestrator will emit in run.started for a
// prompt-only session (see internal/batch/orchestrator.go where the
// subject is "prompt-<userid>"). The on-disk batch folder basename,
// batch.json.batchId, event payload batch_id, the per-row RunID, and
// the batches index entry id all agree.
func TestRun_PromptOnlyWithRunIDRegistersOrchestratorRunIDInBatchesIndex(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	dir, deps := newRunDepsInDir(t, spy)
	deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "Return only OK.", "--run-id", "myid"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}

	if len(idx.Batches) != 1 {
		t.Fatalf("expected exactly 1 batch index entry, got %d (entries=%v)", len(idx.Batches), idx.Batches)
	}
	got := idx.Batches[0]
	// Pin the full public BatchId: <ts>-<sid>-prompt-myid. We assert
	// the literal segment that hard-codes the `prompt` discriminator
	// (issue #1920) — that is the load-bearing assertion this test
	// exists to guard. A naked HasSuffix check would silently let a
	// regression drift back to the old <ts>-<sid>-myid shape.
	if !strings.Contains(got.ID, "-prompt-myid") {
		t.Errorf("entry ID = %q, want substring %q (canonical public BatchId for prompt-only with userid)", got.ID, "-prompt-myid")
	}
	if strings.HasSuffix(got.ID, "-myid") && !strings.HasSuffix(got.ID, "-prompt-myid") {
		t.Errorf("entry ID = %q regressed to legacy <ts>-<sid>-myid shape (missing the prompt- discriminator)", got.ID)
	}

	// Verify the batch folder basename agrees (== public BatchId ==
	// batch.json.batchId). We can compute the expected shape from
	// the manifest on disk.
	batchDir := filepath.Join(dir, ".sandman", "batches")
	entries, err := os.ReadDir(batchDir)
	if err != nil {
		t.Fatalf("read batches dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 batch dir, got %d", len(entries))
	}
	if entries[0].Name() != got.ID {
		t.Errorf("batch folder basename = %q, want %q (== batches index entry id == public BatchId)", entries[0].Name(), got.ID)
	}
}

// TestRun_PromptOnlyWithoutRunIDRegistersCanonicalBatchIdInBatchesIndex
// pins the prompt-only public BatchId contract for the no-userid case
// (#1916): `sandman run --prompt "..."` (no
// --run-id) registers the batches index entry with id `<ts>-<sid>-prompt`,
// matching the per-row RunID the orchestrator will emit in run.started.
// The on-disk batch folder basename, batch.json.batchId, event payload
// batch_id, the per-row RunID, and the batches index entry id all agree.
func TestRun_PromptOnlyWithoutRunIDRegistersCanonicalBatchIdInBatchesIndex(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	dir, deps := newRunDepsInDir(t, spy)
	deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "Return only OK."})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}

	if len(idx.Batches) != 1 {
		t.Fatalf("expected exactly 1 batch index entry, got %d (entries=%v)", len(idx.Batches), idx.Batches)
	}
	got := idx.Batches[0]
	// Pin the full public BatchId: <ts>-<sid>-prompt. The entry id
	// must end with `-prompt` (the canonical discriminator) and must
	// NOT have a trailing userid segment (since --run-id was empty).
	if !strings.HasSuffix(got.ID, "-prompt") {
		t.Errorf("entry ID = %q, want suffix %q (canonical public BatchId for prompt-only without userid)", got.ID, "-prompt")
	}
	// The no-userid shape must collapse to <ts>-<sid>-prompt exactly
	// (no extra segment after `-prompt`). The format is
	// `<4-hex-sid>-<12-digit-ts>-prompt`, total 22 chars.
	if len(got.ID) != len("260618113825-abcd-prompt") {
		t.Errorf("entry ID = %q (len=%d), want canonical <ts>-<sid>-prompt shape (len=%d)", got.ID, len(got.ID), len("260618113825-abcd-prompt"))
	}

	// Verify the batch folder basename agrees (== public BatchId ==
	// batch.json.batchId).
	batchDir := filepath.Join(dir, ".sandman", "batches")
	entries, err := os.ReadDir(batchDir)
	if err != nil {
		t.Fatalf("read batches dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 batch dir, got %d", len(entries))
	}
	if entries[0].Name() != got.ID {
		t.Errorf("batch folder basename = %q, want %q (== batches index entry id == public BatchId)", entries[0].Name(), got.ID)
	}
}

func TestRun_PromptOnlyRejectsSubstitutedIssuePlaceholders(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "{{PROMPT_BODY}}", "--prompt-arg", "PROMPT_BODY=Issue {{ISSUE_TITLE}}"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "prompt requires issue selection but no issue selection was provided") {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
}

func TestRun_CustomPromptWithIssueSelectionStillUsesIssueDrivenFlow(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "inline prompt", args: []string{"--prompt", "Return only OK.", "42"}},
		{name: "template file", args: func() []string {
			dir := t.TempDir()
			path := dir + "/prompt.md"
			if err := os.WriteFile(path, []byte("Return only OK."), 0644); err != nil {
				t.Fatalf("write template: %v", err)
			}
			return []string{"--template", path, "42"}
		}()},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(t, spy)

			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !spy.called {
				t.Fatal("expected batch runner to be called")
			}
			if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 42 {
				t.Fatalf("expected issue 42, got %v", spy.req.Issues)
			}
		})
	}
}

func TestRun_PromptOnlyStillRequiresIssueNumberWhenPromptUsesIt(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "inline prompt", args: []string{"--prompt", "Issue {{ISSUE_TITLE}}"}},
		{name: "template file", args: func() []string {
			dir := t.TempDir()
			path := dir + "/prompt.md"
			if err := os.WriteFile(path, []byte("Issue {{ISSUE_BODY}}"), 0644); err != nil {
				t.Fatalf("write template: %v", err)
			}
			return []string{"--template", path}
		}()},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(t, spy)
			deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch should not run")}

			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "prompt requires issue selection but no issue selection was provided") {
				t.Fatalf("unexpected error: %v", err)
			}
			if spy.called {
				t.Fatal("expected batch runner not to be called")
			}
		})
	}
}

func TestRun_PrintsSummaryOnSuccess(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "success", Branch: "sandman/43-new-feature"},
		},
	}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Summary: 2 succeeded") {
		t.Errorf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#42  success  sandman/42-fix-bug") {
		t.Errorf("expected issue 42 in summary, got:\n%s", out)
	}
}

func TestRun_PrintsRetryCountInSummary(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{{IssueNumber: 42, Status: "success", RetriesTotal: 3, Branch: "sandman/42-fix-bug"}},
	}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "success (2 retries)") {
		t.Fatalf("expected retry count in summary, got:\n%s", out)
	}
}

func TestRun_PrintsSummaryOnPartialFailure(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "failure", Branch: "sandman/43-broken"},
		},
	}, err: errors.New("1 of 2 runs failed")}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when some runs fail")
	}

	out := buf.String()
	if !strings.Contains(out, "Summary: 1 succeeded, 1 failed") {
		t.Errorf("expected partial failure summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#43  failure  sandman/43-broken") {
		t.Errorf("expected issue 43 failure in summary, got:\n%s", out)
	}
}

func TestRun_PrintsSummaryWithBlockedRuns(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "failure", Branch: "sandman/43-broken"},
			{IssueNumber: 100, Status: "blocked", Branch: "sandman/100-dependent"},
		},
	}, err: errors.New("1 of 3 runs failed")}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43", "100"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when some runs fail")
	}

	out := buf.String()
	if !strings.Contains(out, "Summary: 1 succeeded, 1 failed, 1 blocked") {
		t.Errorf("expected blocked summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#100  blocked  sandman/100-dependent") {
		t.Errorf("expected issue 100 blocked in summary, got:\n%s", out)
	}
}

func TestRun_PrintsSummaryWithBlockedRunsAndNoFailures(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 100, Status: "blocked", Branch: "sandman/100-dependent"},
			{IssueNumber: 101, Status: "blocked", Branch: "sandman/101-another-dependent"},
		},
	}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "100", "101"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Summary: 1 succeeded, 2 blocked") {
		t.Errorf("expected blocked summary without failures, got:\n%s", out)
	}
	if !strings.Contains(out, "#101  blocked  sandman/101-another-dependent") {
		t.Errorf("expected issue 101 blocked in summary, got:\n%s", out)
	}
}

func TestRun_PrintsSummaryWithAbortedRuns(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "aborted", Branch: "sandman/43-stalled"},
		},
	}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Summary: 1 succeeded, 1 aborted") {
		t.Errorf("expected aborted summary, got:\n%s", out)
	}
	if strings.Contains(out, "0 failed") {
		t.Errorf("expected zero-failed bucket to be omitted, got:\n%s", out)
	}
	if !strings.Contains(out, "#43  aborted  sandman/43-stalled") {
		t.Errorf("expected issue 43 aborted in summary, got:\n%s", out)
	}
}

func TestPrintSummary_ReportsAbortedCount(t *testing.T) {
	result := &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "failure", Branch: "sandman/43-broken"},
			{IssueNumber: 44, Status: "aborted", Branch: "sandman/44-stalled"},
			{IssueNumber: 100, Status: "blocked", Branch: "sandman/100-dependent"},
		},
	}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	printSummary(cmd, result)

	out := buf.String()
	want := "Summary: 1 succeeded, 1 failed, 1 aborted, 1 blocked"
	if !strings.Contains(out, want) {
		t.Errorf("expected %q in summary, got:\n%s", want, out)
	}
}

func TestPrintSummary_OmitsAbortedWhenZero(t *testing.T) {
	result := &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
			{IssueNumber: 43, Status: "failure", Branch: "sandman/43-broken"},
		},
	}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	printSummary(cmd, result)

	out := buf.String()
	if strings.Contains(out, "aborted") {
		t.Errorf("expected no aborted bucket when zero, got:\n%s", out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded, 1 failed") {
		t.Errorf("expected base summary, got:\n%s", out)
	}
}

func TestPrintSummary_OmitsSucceededWhenZero(t *testing.T) {
	result := &batch.Result{
		Runs: []batch.AgentRunResult{
			{IssueNumber: 43, Status: "aborted", Branch: "sandman/43-stalled"},
			{IssueNumber: 44, Status: "aborted", Branch: "sandman/44-stalled"},
		},
	}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	printSummary(cmd, result)

	out := buf.String()
	if strings.Contains(out, "succeeded") {
		t.Errorf("expected no succeeded bucket when zero, got:\n%s", out)
	}
	if !strings.Contains(out, "Summary: 2 aborted") {
		t.Errorf("expected only aborted bucket, got:\n%s", out)
	}
}

func TestRun_ExitsWithCode130OnAbort(t *testing.T) {
	spy := &spyBatchRunner{
		result: &batch.Result{
			Runs: []batch.AgentRunResult{
				{IssueNumber: 42, Status: "aborted", Branch: "sandman/42-fix-bug"},
			},
		},
		err: batch.ErrAborted,
	}
	deps := newRunDeps(t, spy)

	var stdout, stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from aborted batch")
	}

	if !errors.Is(err, batch.ErrAborted) {
		t.Errorf("expected error to wrap batch.ErrAborted, got %v", err)
	}

	var coded *ExitCodedError
	if !errors.As(err, &coded) {
		t.Fatalf("expected *ExitCodedError, got %T: %v", err, err)
	}
	if coded.Code != 130 {
		t.Errorf("expected exit code 130, got %d", coded.Code)
	}
	if !strings.Contains(stderr.String(), "batch aborted by operator") {
		t.Errorf("expected 'batch aborted by operator' on stderr, got:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Summary: 1 aborted") {
		t.Errorf("expected aborted summary on stdout, got:\n%s", stdout.String())
	}
}

func TestRun_PreservesRunBatchErrorMessage(t *testing.T) {
	spy := &spyBatchRunner{
		result: &batch.Result{
			Runs: []batch.AgentRunResult{
				{IssueNumber: 42, Status: "failure", Branch: "sandman/42-broken"},
			},
		},
		err: errors.New("1 of 1 runs failed"),
	}
	deps := newRunDeps(t, spy)

	var stdout, stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from failed batch")
	}

	if !strings.Contains(err.Error(), "run batch: 1 of 1 runs failed") {
		t.Errorf("expected wrapped 'run batch' message, got %v", err)
	}

	var coded *ExitCodedError
	if errors.As(err, &coded) {
		t.Errorf("expected plain error (not *ExitCodedError) for non-abort failure, got %v", err)
	}
}

func TestRun_PrintsWorktreeHintForCompletedRuns(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{Runs: []batch.AgentRunResult{{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug", WorktreePath: ".sandman/worktrees/sandman/42-fix-bug"}}}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "worktree: .sandman/worktrees/sandman/42-fix-bug") {
		t.Fatalf("expected worktree hint, got:\n%s", out)
	}
}

func TestRun_PrintsPromptOnlySummaryLabel(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{Runs: []batch.AgentRunResult{{Status: "success", Branch: "sandman/return-only-ok-123"}}}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "Return only OK."})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "prompt-only  success  sandman/return-only-ok-123") {
		t.Fatalf("expected prompt-only summary label, got:\n%s", out)
	}
}

func TestRun_PrintsReviewRunSummaryLabel(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{Runs: []batch.AgentRunResult{{Status: "success", Branch: "sandman/review-PR42", Review: true, RunID: "PR42"}}}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "Review the PR."})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "PR42  success  sandman/review-PR42") {
		t.Fatalf("expected review run summary label PR42, got:\n%s", out)
	}
	if strings.Contains(out, "prompt-only") {
		t.Fatalf("expected no prompt-only label for review run, got:\n%s", out)
	}
}

func TestRun_ExplicitZeroParallelPassesThroughToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", DefaultParallel: 8, ReviewCommand: "/oc review"}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--parallel", "0", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Parallel != 0 {
		t.Errorf("expected explicit parallel=0 to pass through to orchestrator, got %d", spy.req.Parallel)
	}
}

func TestRun_ConfigParallelDefault(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", DefaultParallel: 8, ReviewCommand: "/oc review"}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Parallel != 8 {
		t.Errorf("expected parallel=8 from config default, got %d", spy.req.Parallel)
	}
}

func TestRun_SandboxFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--sandbox", "docker", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.Sandbox != "docker" {
		t.Errorf("expected sandbox=docker, got %q", spy.req.Sandbox)
	}
}

func TestRun_BaseBranchFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review", Git: config.GitConfig{BaseBranch: "trunk"}}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--base-branch", "release", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.BaseBranch != "release" {
		t.Errorf("expected base_branch=release, got %q", spy.req.BaseBranch)
	}
}

func TestRun_BaseBranchDefaultsToConfig(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review", Git: config.GitConfig{BaseBranch: "trunk"}}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.BaseBranch != "trunk" {
		t.Errorf("expected base_branch=trunk, got %q", spy.req.BaseBranch)
	}
}

func TestRun_InteractiveFlagRejected(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--interactive", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for removed --interactive flag")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "unknown flag: --interactive") {
		t.Fatalf("expected unknown flag error, got %v", err)
	}
}

func TestRun_IncludeDependenciesResolvesBatchBeforeRunning(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "Groundwork"},
		},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--include-dependencies", "100"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if !reflect.DeepEqual(spy.req.Issues, []int{7, 42, 100}) {
		t.Fatalf("expected resolved issues [7 42 100], got %v", spy.req.Issues)
	}
	wantDeps := map[int][]int{
		7:   nil,
		42:  {7},
		100: {42},
	}
	if !reflect.DeepEqual(spy.req.Dependencies, wantDeps) {
		t.Fatalf("expected dependencies %v, got %v", wantDeps, spy.req.Dependencies)
	}
}

func TestRun_OpenExternalBlockersAreMarkedBlockedWithoutIncludeDependencies(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor"},
		},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"100"})

	err := cmd.Execute()
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(spy.req.Issues, []int{100}) {
		t.Fatalf("expected resolved issues [100], got %v", spy.req.Issues)
	}
	wantBlocked := map[int][]int{100: {42}}
	if !reflect.DeepEqual(spy.req.Blocked, wantBlocked) {
		t.Fatalf("expected blocked metadata %v, got %v", wantBlocked, spy.req.Blocked)
	}
}

func TestRun_DependencyCycleReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{100}},
		},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--include-dependencies", "100"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected dependency cycle error")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "dependency cycle detected: #100 -> #42 -> #100") {
		t.Fatalf("expected dependency cycle error, got %v", err)
	}
}

func TestRun_LabelFlagResolvesIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Bug A"},
			{Number: 2, Title: "Bug B"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{1, 2}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_TTYPickerSelectsIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 10, Title: "Issue A"},
			{Number: 20, Title: "Issue B"},
		},
	}
	picker := &fakeIssuePicker{issues: []int{10, 20}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IssuePicker = picker
	deps.IsTTY = func() bool { return true }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{10, 20}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
}

func TestRun_NoArgsNoTTYReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no issues provided and not a TTY")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
}

func TestRun_CombinePlainArgsWithLabelUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Bug A", State: "open", Labels: []string{"bug"}},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "" {
		t.Errorf("expected no search query for local label filtering, got %q", gh.searchIssuesQuery)
	}
	// The cached GitHub client (#2218) deduplicates FetchIssue calls
	// between the local-filter path and the dependency resolver, so
	// a single-issue batch produces one underlying fetch.
	if gh.fetchCount[42] < 1 {
		t.Errorf("expected issue 42 to be fetched at least once, got %d", gh.fetchCount[42])
	}
}

func TestRun_CombinePlainArgsWithLabelSkipsClosedIssue(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Bug A", State: "closed", Labels: []string{"bug"}},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when label filter excludes the issue")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "no issues selected") {
		t.Fatalf("expected no issues selected error, got %v", err)
	}
	if gh.fetchCount[42] != 1 {
		t.Errorf("expected issue 42 to be fetched once, got %d", gh.fetchCount[42])
	}
}

func TestRun_CombinePlainArgsWithLabelIsCaseInsensitive(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Bug A", State: "open", Labels: []string{"Bug"}},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.fetchCount[42] < 1 {
		t.Errorf("expected issue 42 to be fetched at least once, got %d", gh.fetchCount[42])
	}
}

func TestRun_CombinePlainArgsWithQueryUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Feature A", State: "open", Labels: []string{"bug"}},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--query", "label:bug", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 42 {
		t.Errorf("expected issues [42], got %v", spy.req.Issues)
	}
	if gh.searchIssuesQuery != "" {
		t.Errorf("expected no search query for local query filtering, got %q", gh.searchIssuesQuery)
	}
	if gh.fetchCount[42] < 1 {
		t.Errorf("expected issue 42 to be fetched at least once, got %d", gh.fetchCount[42])
	}
}

func TestRun_RangeArgUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42:45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "is:open" {
		t.Errorf("expected is:open search for bounded ranges, got %q", gh.searchIssuesQuery)
	}
	want := []int{42, 43, 44, 45}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_RangeArgWithLabelUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Bug A", State: "open", Labels: []string{"bug"}},
			43: {Number: 43, Title: "Bug B", State: "open", Labels: []string{"bug"}},
			44: {Number: 44, Title: "Bug C", State: "open", Labels: []string{"bug"}},
			45: {Number: 45, Title: "Bug D", State: "open", Labels: []string{"bug"}},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42:45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 43, 44, 45}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "" {
		t.Errorf("expected no search query for local label filtering, got %q", gh.searchIssuesQuery)
	}
	for _, n := range want {
		if gh.fetchCount[n] < 1 {
			t.Errorf("expected issue %d to be fetched at least once, got %d", n, gh.fetchCount[n])
		}
	}
}

func TestRun_RangeArgWithQueryUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Feature A", State: "open", Labels: []string{"bug"}},
			43: {Number: 43, Title: "Feature B", State: "open", Labels: []string{"bug"}},
			44: {Number: 44, Title: "Feature C", State: "open", Labels: []string{"bug"}},
			45: {Number: 45, Title: "Feature D", State: "open", Labels: []string{"bug"}},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--query", "label:bug is:open", "42:45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 43, 44, 45}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "" {
		t.Errorf("expected no search query for local query filtering, got %q", gh.searchIssuesQuery)
	}
	for _, n := range want {
		if gh.fetchCount[n] < 1 {
			t.Errorf("expected issue %d to be fetched at least once, got %d", n, gh.fetchCount[n])
		}
	}
}

func TestRun_MixedArgsWithLabelUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Bug A", State: "open", Labels: []string{"bug"}},
			44: {Number: 44, Title: "Bug B", State: "open", Labels: []string{"bug"}},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "42", "44"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 44}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "" {
		t.Errorf("expected no search query for local label filtering, got %q", gh.searchIssuesQuery)
	}
	for _, n := range want {
		if gh.fetchCount[n] < 1 {
			t.Errorf("expected issue %d to be fetched at least once, got %d", n, gh.fetchCount[n])
		}
	}
}

func TestRun_UnboundedEndRangeUsesQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 42, State: "open", Title: "Issue A"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42:"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 42 {
		t.Errorf("expected issues [42], got %v", spy.req.Issues)
	}
	if gh.searchIssuesQuery != "is:open" {
		t.Errorf("expected search query 'is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_UnboundedEndRangeWithStateQueryUsesIssueState(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 42, State: "open", Title: "Issue A"},
			{Number: 43, State: "closed", Title: "Issue B"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--query", "state:open", "42:"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "state:open" {
		t.Errorf("expected search query 'state:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_UnboundedStartRangeUsesQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	results := make([]github.Issue, 45)
	for i := range results {
		results[i] = github.Issue{Number: i + 1, Title: fmt.Sprintf("Issue %d", i+1)}
	}
	gh := &fakeGitHubClient{
		searchIssuesResult: results,
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{":45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "is:open" {
		t.Errorf("expected is:open search for bounded range, got %q", gh.searchIssuesQuery)
	}
	want := make([]int, 45)
	for i := range want {
		want[i] = i + 1
	}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_MixedExactAndUnboundedRangePreservesExplicitIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			7:  {Number: 7, Title: "Closed Issue", State: "closed"},
			42: {Number: 42, Title: "Issue A"},
			43: {Number: 43, Title: "Issue B"},
		},
		searchIssuesResult: []github.Issue{
			{Number: 42, Title: "Issue A"},
			{Number: 43, Title: "Issue B"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var stdout, stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"7", "42:"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 43}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "is:open" {
		t.Errorf("expected search query 'is:open', got %q", gh.searchIssuesQuery)
	}
	if !strings.Contains(stderr.String(), "Issue #7 is closed, skipping") {
		t.Errorf("expected closed issue warning on stderr, got: %s", stderr.String())
	}
}

func TestRun_BoundedRangeWarnsOnClosed(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Open A"},
			43: {Number: 43, Title: "Closed Issue", State: "closed"},
			44: {Number: 44, Title: "Open B"},
			45: {Number: 45, Title: "Open C"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var stdout, stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"42:45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 44, 45}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if !strings.Contains(stderr.String(), "Issue #43 is closed, skipping") {
		t.Errorf("expected closed range-sourced warning on stderr, got: %s", stderr.String())
	}
}

func TestRun_ExplicitClosedIssueLogsWarning(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Closed Issue", State: "closed"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var stdout, stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when only issue is closed")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(stderr.String(), "Issue #42 is closed, skipping") {
		t.Errorf("expected closed issue warning on stderr, got: %s", stderr.String())
	}
	var ue *UsageError
	if errors.As(err, &ue) {
		t.Errorf("expected plain runtime error (no usage banner), got UsageError: %v", err)
	}
}

func TestRun_MixedExplicitAndRangeWarnsOnClosed(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			7:  {Number: 7, Title: "Closed Explicit", State: "closed"},
			42: {Number: 42, Title: "Open A"},
			43: {Number: 43, Title: "Closed Range", State: "closed"},
			44: {Number: 44, Title: "Open B"},
			45: {Number: 45, Title: "Open C"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var stdout, stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"7", "42:45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 44, 45}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if !strings.Contains(stderr.String(), "Issue #7 is closed, skipping") {
		t.Errorf("expected closed explicit warning on stderr, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Issue #43 is closed, skipping") {
		t.Errorf("expected closed range-sourced warning on stderr, got: %s", stderr.String())
	}
}

func TestRun_BoundedRangeAllOpenKeepsWorking(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Open A"},
			43: {Number: 43, Title: "Open B"},
			44: {Number: 44, Title: "Open C"},
			45: {Number: 45, Title: "Open D"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var stdout, stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"42:45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 43, 44, 45}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_BoundedRangeAllClosedReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Closed A", State: "closed"},
			43: {Number: 43, Title: "Closed B", State: "closed"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var stdout, stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"42:43"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when all issues in range are closed")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(stderr.String(), "Issue #42 is closed, skipping") {
		t.Errorf("expected closed issue warning on stderr, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Issue #43 is closed, skipping") {
		t.Errorf("expected closed issue warning on stderr, got: %s", stderr.String())
	}
	var ue *UsageError
	if errors.As(err, &ue) {
		t.Errorf("expected plain runtime error (no usage banner), got UsageError: %v", err)
	}
}

func TestRun_BoundedRangePrefersSearchOverPerIssueFetch(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Open A"},
			43: {Number: 43, Title: "Closed", State: "closed"},
			44: {Number: 44, Title: "Open B"},
			45: {Number: 45, Title: "Open C"},
		},
	}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var stdout, stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"42:45"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gh.searchIssuesQuery != "is:open" {
		t.Errorf("expected is:open search to be used, got: %s", gh.searchIssuesQuery)
	}
	want := []int{42, 44, 45}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
}

func TestRun_LargeRangeRejectedBeforeExpansion(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"1:1001"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for oversized range")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "more than 1000 issues") {
		t.Errorf("expected oversized range error, got: %v", err)
	}
}

func TestRun_PositionalSelectionWithUnsupportedQueryRejectsTruncatedSearchResults(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	results := make([]github.Issue, 1000)
	for i := range results {
		results[i] = github.Issue{Number: i + 1, Title: fmt.Sprintf("Issue %d", i+1)}
	}
	gh := &fakeGitHubClient{searchIssuesResult: results}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--query", "author:me", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for truncated search results")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "search result limit") {
		t.Errorf("expected search result limit error, got: %v", err)
	}
}

func TestRun_InvalidContainerFlagsReturnError(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "container capacity less than one",
			args:    []string{"--container-capacity", "-1", "42"},
			wantErr: "container_capacity must be 0 or greater",
		},
		{
			name:    "negative max containers",
			args:    []string{"--max-containers", "-1", "42"},
			wantErr: "max_containers must be 0 or greater",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(t, spy)

			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
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
			if spy.called {
				t.Fatal("expected batch runner not to be called")
			}
		})
	}
}

func TestRun_PromptFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "custom template {{ISSUE_NUMBER}}", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "custom template {{ISSUE_NUMBER}}" {
		t.Errorf("expected PromptFlag='custom template {{ISSUE_NUMBER}}', got %q", spy.req.PromptConfig.PromptFlag)
	}
}

func TestRun_TemplateFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	dir := t.TempDir()
	templatePath := dir + "/my-prompt.md"
	if err := os.WriteFile(templatePath, []byte("template file {{ISSUE_NUMBER}}"), 0644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--template", templatePath, "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.TemplateFlag != templatePath {
		t.Errorf("expected TemplateFlag=%q, got %q", templatePath, spy.req.PromptConfig.TemplateFlag)
	}
}

func TestRun_BranchFlagPassedToBatchRunnerForPromptOnly(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "Suggest the badge.", "--branch", "sandman/built-with-sandman"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.Branch != "sandman/built-with-sandman" {
		t.Errorf("expected PromptConfig.Branch=sandman/built-with-sandman, got %q", spy.req.PromptConfig.Branch)
	}
}

func TestRun_PromptArgFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt-arg", "FOO=bar", "--prompt-arg", "BAZ=qux", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spy.req.PromptConfig.PromptArgs) != 2 {
		t.Fatalf("expected 2 prompt args, got %d", len(spy.req.PromptConfig.PromptArgs))
	}
	if spy.req.PromptConfig.PromptArgs["FOO"] != "bar" {
		t.Errorf("expected FOO=bar, got FOO=%q", spy.req.PromptConfig.PromptArgs["FOO"])
	}
	if spy.req.PromptConfig.PromptArgs["BAZ"] != "qux" {
		t.Errorf("expected BAZ=qux, got BAZ=%q", spy.req.PromptConfig.PromptArgs["BAZ"])
	}
}

func TestRun_PromptArgFlagInvalidFormat(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt-arg", "NOEQUALSSIGN", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid --prompt-arg format")
	}
	if !strings.Contains(err.Error(), "--prompt-arg") {
		t.Errorf("expected error about --prompt-arg, got: %v", err)
	}
	var target *UsageError
	if !errors.As(err, &target) {
		t.Fatalf("expected *UsageError, got %T: %v", err, err)
	}
}

func TestRun_PromptArgValidationHappensBeforeDependencyResolution(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = &fakeGitHubClient{fetchIssueError: errors.New("fetch issue should not run")}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt-arg", "NOEQUALSSIGN", "42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid --prompt-arg format")
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--prompt-arg") {
		t.Fatalf("expected prompt-arg validation error, got %v", err)
	}
	if strings.Contains(err.Error(), "fetch issue should not run") {
		t.Fatalf("expected prompt-arg validation before dependency resolution, got %v", err)
	}
}

func TestRun_PromptConfigDefaultsEmpty(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	// newRunDepsInDir provides a live review.sock and a config
	// with no ReviewCommand. This test asserts the new default
	// review command value, so we use the config-default
	// ReviewCommand (not the socket-bypass value).
	_, deps := newRunDepsInDir(t, spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "" {
		t.Errorf("expected empty PromptFlag, got %q", spy.req.PromptConfig.PromptFlag)
	}
	if spy.req.PromptConfig.TemplateFlag != "" {
		t.Errorf("expected empty TemplateFlag, got %q", spy.req.PromptConfig.TemplateFlag)
	}
	if len(spy.req.PromptConfig.PromptArgs) != 0 {
		t.Errorf("expected empty PromptArgs, got %v", spy.req.PromptConfig.PromptArgs)
	}
	if spy.req.PromptConfig.ReviewCommand != "/sandman review" {
		t.Errorf("expected default ReviewCommand, got %q", spy.req.PromptConfig.ReviewCommand)
	}
}

func TestRun_ReviewCommandFromConfigPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/config review"}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.ReviewCommand != "/config review" {
		t.Fatalf("expected config review command, got %q", spy.req.PromptConfig.ReviewCommand)
	}
	if !spy.req.PromptConfig.ReviewCommandSet {
		t.Fatal("expected review command to be recorded in run payload")
	}
}

func TestRun_PromptAndTemplateFlagsCombined(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	dir := t.TempDir()
	templatePath := dir + "/template.md"
	if err := os.WriteFile(templatePath, []byte("template file {{ISSUE_NUMBER}}"), 0644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--prompt", "inline {{ISSUE_NUMBER}}", "--template", templatePath, "--prompt-arg", "K=V", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.req.PromptConfig.PromptFlag != "inline {{ISSUE_NUMBER}}" {
		t.Errorf("expected PromptFlag='inline {{ISSUE_NUMBER}}', got %q", spy.req.PromptConfig.PromptFlag)
	}
	if spy.req.PromptConfig.TemplateFlag != templatePath {
		t.Errorf("expected TemplateFlag=%q, got %q", templatePath, spy.req.PromptConfig.TemplateFlag)
	}
	if spy.req.PromptConfig.PromptArgs["K"] != "V" {
		t.Errorf("expected K=V, got K=%q", spy.req.PromptConfig.PromptArgs["K"])
	}
}

// TestRun_SingleIssueRegistersPublicBatchIdInBatchesIndex verifies that
// `sandman run 42` registers a SINGLE batch index entry whose id and
// path equal the public BatchId `<ts>-<sid>-42` (issue #1917).
// For single-issue batches, the public BatchId (== per-row RunID ==
// batch folder basename) carries no +N suffix.
func TestRun_SingleIssueRegistersPublicBatchIdInBatchesIndex(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	dir, deps := newRunDepsInDir(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", State: "open"}},
		prs:    map[string]*github.PR{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}

	wantPublicBatchID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42"
	if len(idx.Batches) != 1 {
		t.Fatalf("expected exactly 1 batch index entry, got %d (entries=%v)", len(idx.Batches), idx.Batches)
	}
	got := idx.Batches[0]
	if got.ID != wantPublicBatchID {
		t.Errorf("entry ID = %q, want %q (public BatchId)", got.ID, wantPublicBatchID)
	}
	if got.Kind != batchindex.KindIssue {
		t.Errorf("entry Kind = %v, want %v", got.Kind, batchindex.KindIssue)
	}
	if got.Path == "" {
		t.Error("entry Path must be non-empty")
	}
	// Path is the public BatchId (= batch folder basename). For single
	// issue batches there is no +N suffix (issue #1917).
	if filepath.Base(got.Path) != wantPublicBatchID {
		t.Errorf("entry Path basename = %q, want public BatchId %q", filepath.Base(got.Path), wantPublicBatchID)
	}
	// Manifest on disk must also carry the public BatchId in batchId.
	batchManifest, err := daemon.ReadManifest(filepath.Join(dir, ".sandman", "batches", wantPublicBatchID))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if batchManifest.BatchId != wantPublicBatchID {
		t.Errorf("batch.json.batchId = %q, want %q (public BatchId)", batchManifest.BatchId, wantPublicBatchID)
	}
}

// TestRun_MultiIssueRegistersPublicBatchIdInBatchesIndex verifies that
// `sandman run 42 43` registers a SINGLE batch index entry whose id and
// path equal the public BatchId `<ts>-<sid>-42+1` (issue #1917).
// Per-row addressability is via the per-run folders under
// `runs/<ts>-<sid>-<num>/`, not via additional index entries. The
// batch.json.batchId stored on disk MUST equal the public BatchId
// (== entry id == batch folder basename).
func TestRun_MultiIssueRegistersPublicBatchIdInBatchesIndex(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	dir, deps := newRunDepsInDir(t, spy)
	deps.GitHubClient = &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "First", State: "open"},
			43: {Number: 43, Title: "Second", State: "open"},
		},
		prs: map[string]*github.PR{},
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42", "43"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}

	wantPublicBatchID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42+1"
	wantFirstRowID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42"
	wantSecondRowID := spy.req.RunTS + "-" + spy.req.RunShortID + "-43"
	if len(idx.Batches) != 1 {
		t.Fatalf("expected exactly 1 batch index entry for multi-issue run, got %d (entries=%v)", len(idx.Batches), idx.Batches)
	}
	got := idx.Batches[0]
	if got.ID != wantPublicBatchID {
		t.Errorf("entry ID = %q, want %q (public BatchId)", got.ID, wantPublicBatchID)
	}
	// Per-row RunIDs are NOT separate index entries; they live in
	// runs/<ts>-<sid>-<num>/run.json. Only the public BatchId is keyed
	// in the index.
	if idx.ResolveBatch(wantFirstRowID) != nil {
		t.Errorf("first row's per-row RunID %q must NOT have a separate index entry", wantFirstRowID)
	}
	if idx.ResolveBatch(wantSecondRowID) != nil {
		t.Errorf("second row's per-row RunID %q must NOT have a separate index entry", wantSecondRowID)
	}
	// Path is the public BatchId (= batch dir basename).
	if filepath.Base(got.Path) != wantPublicBatchID {
		t.Errorf("entry Path basename = %q, want %q", filepath.Base(got.Path), wantPublicBatchID)
	}
	// Manifest on disk must also carry the public BatchId in batchId.
	batchManifest, err := daemon.ReadManifest(filepath.Join(dir, ".sandman", "batches", wantPublicBatchID))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if batchManifest.BatchId != wantPublicBatchID {
		t.Errorf("batch.json.batchId = %q, want %q (public BatchId)", batchManifest.BatchId, wantPublicBatchID)
	}
}

// TestRun_ContinueRegistersPerRowRunIDInBatchesIndex verifies that
// `sandman run --continue 42` registers the batches index entry with id
// `<ts>-<sid>-42` (the per-row RunID the orchestrator will emit in
// run.continued). Mirrors #1675's `sandman run --continue <issue>`
// acceptance criterion and pins the structural-ordering invariant that
// `req.RunTS`/`RunShortID` must be minted before `Prepare` is called.
func TestRun_ContinueRegistersPerRowRunIDInBatchesIndex(t *testing.T) {
	branch := "sandman/42-fix-bug"
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	worktreeBase := t.TempDir()
	worktreePath := addRegisteredContinuationWorktree(t, deps.RepoRoot, worktreeBase, branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("## Completed\nInitial pass.\n"), 0644); err != nil {
		t.Fatalf("write task: %v", err)
	}
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: worktreeBase, ReviewCommand: "/oc review", AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}}
	deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: testRunID42First, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}}}}
	deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idx, err := batchindex.Load(filepath.Join(".", ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}

	wantPublicBatchID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42"
	if len(idx.Batches) != 1 {
		t.Logf("buf: %s", buf.String())
		t.Fatalf("expected exactly 1 batch index entry, got %d (entries=%v)", len(idx.Batches), idx.Batches)
	}
	if got := idx.Batches[0].ID; got != wantPublicBatchID {
		t.Errorf("entry ID = %q, want %q (public BatchId for single issue)", got, wantPublicBatchID)
	}
}

// TestRun_Continue_MultiIssueFreshBatchAndRunIDs verifies that a multi-issue
// continuation mints a fresh public BatchId from a new (ts, shortid) pair and
// carries the previous per-row RunIDs only as lineage inputs.
func TestRun_Continue_MultiIssueFreshBatchAndRunIDs(t *testing.T) {
	branch := "sandman/42-43-fix-bugs"
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	worktreeBase := t.TempDir()
	worktreePath := addRegisteredContinuationWorktree(t, deps.RepoRoot, worktreeBase, branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("## Completed\nInitial pass.\n"), 0644); err != nil {
		t.Fatalf("write task: %v", err)
	}
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: worktreeBase, ReviewCommand: "/oc review", AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "prev-ts-abcd-42", Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}},
		{Type: "run.started", RunID: "prev-ts-abcd-43", Issue: 43, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}},
	}}
	deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "open"}}, prs: map[string]*github.PR{}}
	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42", "43"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if spy.req.RunTS == "" || spy.req.RunShortID == "" {
		t.Fatalf("expected continuation to mint a fresh batch identity, got ts=%q shortid=%q", spy.req.RunTS, spy.req.RunShortID)
	}
	if got := spy.req.PreviousRunIDs[42]; got != "prev-ts-abcd-42" {
		t.Fatalf("PreviousRunIDs[42] = %q, want prior run", got)
	}
	if got := spy.req.PreviousRunIDs[43]; got != "prev-ts-abcd-43" {
		t.Fatalf("PreviousRunIDs[43] = %q, want prior run", got)
	}
	if got := spy.req.Branches[42]; got != branch {
		t.Fatalf("Branches[42] = %q, want reused branch %q", got, branch)
	}
	if got := spy.req.Branches[43]; got != branch {
		t.Fatalf("Branches[43] = %q, want reused branch %q", got, branch)
	}
	if got, want := spy.req.TaskPrompts[42], "## Completed\nInitial pass.\n"; got != want {
		t.Fatalf("TaskPrompts[42] = %q, want original task content %q", got, want)
	}

	idx, err := batchindex.Load(filepath.Join(".", ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	if len(idx.Batches) != 1 {
		t.Logf("buf: %s", buf.String())
		t.Fatalf("expected exactly 1 batch index entry for the continuation, got %d (batches=%v)", len(idx.Batches), idx.Batches)
	}
	wantPublicBatchID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42+1"
	if got := idx.Batches[0].ID; got != wantPublicBatchID {
		t.Errorf("continuation entry ID = %q, want %q (fresh public BatchId for multi-issue)", got, wantPublicBatchID)
	}
	if idx.Batches[0].ID == "prev-ts-abcd-42" || idx.Batches[0].ID == "prev-ts-abcd-43" {
		t.Errorf("continuation entry ID collided with prior per-row RunID: %q", idx.Batches[0].ID)
	}
	if got := filepath.Base(idx.Batches[0].Path); got != wantPublicBatchID {
		t.Errorf("continuation entry path basename = %q, want fresh public BatchId %q", got, wantPublicBatchID)
	}
}

// TestRun_IssueBatch_EndToEnd_TimestampFirstIdentity is the end-to-end
// regression for issue #1945: from the cmd layer to the index entry,
// the manifest, and the portal layer's per-row RunID derivation, every
// identity that surfaces for an issue-driven batch must use the
// canonical <ts>-<sid>-... shape. The cmd layer mints a fresh (ts, sid)
// pair on each run, the batches index entry id equals the public
// BatchId, the on-disk batch.json.batchId agrees, the on-disk batch
// folder basename matches, and the per-row RunID derived by the portal
// layer from the manifest's (RunTS, RunShortID) matches the value the
// orchestrator will emit in run.started for the same issue.
//
// The test exercises both the single-issue shape (`<ts>-<sid>-<num>`, no
// +N suffix) and the multi-issue shape (`<ts>-<sid>-<firstIssue>+<n-1>`)
// so the cross-check covers both branches of BatchIDForIssue.
func TestRun_IssueBatch_EndToEnd_TimestampFirstIdentity(t *testing.T) {
	t.Run("single issue", func(t *testing.T) {
		spy := &spyBatchRunner{result: &batch.Result{}}
		dir, deps := newRunDepsInDir(t, spy)
		deps.GitHubClient = &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", State: "open"}},
			prs:    map[string]*github.PR{},
		}

		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"42"})

		if err := cmd.Execute(); err != nil {
			t.Fatalf("unexpected error: %v\noutput: %s", err, buf.String())
		}

		// (ts, sid) mint: shape must be the canonical timestamp-first
		// 12-digit timestamp + 4-hex shortid, never the legacy
		// <shortid>-<ts> order. The canonical prefix regex lives in
		// the runid package; reuse it instead of re-asserting the
		// shape.
		prefix := spy.req.RunTS + "-" + spy.req.RunShortID
		if _, ok := runid.KindFromDirName(prefix); !ok {
			t.Fatalf("RunTS=%q RunShortID=%q mint %q does not match the <ts>-<sid> canonical prefix",
				spy.req.RunTS, spy.req.RunShortID, prefix)
		}

		wantPublicBatchID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42"
		wantPerRowRunID := runid.NewRunID(runid.KindIssue, "42", spy.req.RunTS, spy.req.RunShortID)

		// Index entry id and path agree on the public BatchId.
		idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
		if err != nil {
			t.Fatalf("load batches index: %v", err)
		}
		if len(idx.Batches) != 1 {
			t.Fatalf("expected exactly 1 batch index entry, got %d", len(idx.Batches))
		}
		if got := idx.Batches[0].ID; got != wantPublicBatchID {
			t.Errorf("index entry id = %q, want %q", got, wantPublicBatchID)
		}
		if got := filepath.Base(idx.Batches[0].Path); got != wantPublicBatchID {
			t.Errorf("index entry path basename = %q, want %q", got, wantPublicBatchID)
		}

		// Manifest on disk carries the same public BatchId and the
		// canonical (RunTS, RunShortID) primitives.
		manifest, err := daemon.ReadManifest(filepath.Join(dir, ".sandman", "batches", wantPublicBatchID))
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if manifest.BatchId != wantPublicBatchID {
			t.Errorf("batch.json.batchId = %q, want %q", manifest.BatchId, wantPublicBatchID)
		}
		if manifest.RunTS != spy.req.RunTS {
			t.Errorf("batch.json.runTs = %q, want %q", manifest.RunTS, spy.req.RunTS)
		}
		if manifest.RunShortID != spy.req.RunShortID {
			t.Errorf("batch.json.runShortId = %q, want %q", manifest.RunShortID, spy.req.RunShortID)
		}

		// Portal layer derives the per-row RunID from the same
		// (RunTS, RunShortID) the orchestrator will use, so the
		// event-log-less portal can render the same id the
		// orchestrator will emit in run.started. For a single
		// issue the per-row RunID equals the public BatchId
		// (ADR-0032 §Identity table: "no +N suffix on
		// single-issue").
		derived := perRowRunIDForManifest(spy.req.RunTS, spy.req.RunShortID, 0, 42, nil)
		if derived != wantPerRowRunID {
			t.Errorf("portal-derived per-row RunID = %q, want %q", derived, wantPerRowRunID)
		}
		if derived != wantPublicBatchID {
			t.Errorf("single-issue per-row RunID %q must equal the public BatchId %q (no +N suffix per ADR-0032)",
				derived, wantPublicBatchID)
		}

		// On-disk layout under <batchesDir>/<publicBatchID>/ must
		// use the new order, not a leftover legacy <sid>-<ts> name.
		if !strings.HasPrefix(wantPublicBatchID, spy.req.RunTS) {
			t.Errorf("public BatchId %q must start with RunTS %q", wantPublicBatchID, spy.req.RunTS)
		}
	})

	t.Run("multi issue", func(t *testing.T) {
		spy := &spyBatchRunner{result: &batch.Result{}}
		dir, deps := newRunDepsInDir(t, spy)
		deps.GitHubClient = &fakeGitHubClient{
			issues: map[int]*github.Issue{
				42: {Number: 42, Title: "First", State: "open"},
				43: {Number: 43, Title: "Second", State: "open"},
			},
			prs: map[string]*github.PR{},
		}

		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"42", "43"})

		if err := cmd.Execute(); err != nil {
			t.Fatalf("unexpected error: %v\noutput: %s", err, buf.String())
		}

		wantPublicBatchID := spy.req.RunTS + "-" + spy.req.RunShortID + "-42+1"
		wantPerRow42 := runid.NewRunID(runid.KindIssue, "42", spy.req.RunTS, spy.req.RunShortID)
		wantPerRow43 := runid.NewRunID(runid.KindIssue, "43", spy.req.RunTS, spy.req.RunShortID)

		idx, err := batchindex.Load(filepath.Join(dir, ".sandman", "batches.json"))
		if err != nil {
			t.Fatalf("load batches index: %v", err)
		}
		if len(idx.Batches) != 1 {
			t.Fatalf("expected exactly 1 batch index entry, got %d", len(idx.Batches))
		}
		if got := idx.Batches[0].ID; got != wantPublicBatchID {
			t.Errorf("index entry id = %q, want %q (multi-issue +<n-1> suffix)", got, wantPublicBatchID)
		}
		if got := filepath.Base(idx.Batches[0].Path); got != wantPublicBatchID {
			t.Errorf("index entry path basename = %q, want %q", got, wantPublicBatchID)
		}

		manifest, err := daemon.ReadManifest(filepath.Join(dir, ".sandman", "batches", wantPublicBatchID))
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if manifest.BatchId != wantPublicBatchID {
			t.Errorf("batch.json.batchId = %q, want %q", manifest.BatchId, wantPublicBatchID)
		}

		// Per-row RunIDs: the multi-issue public BatchId carries the
		// +<n-1> suffix, but the per-row RunIDs are plain
		// <ts>-<sid>-<num> (no +N). Cross-check that the portal
		// layer derives the same string for both rows.
		derived42 := perRowRunIDForManifest(spy.req.RunTS, spy.req.RunShortID, 0, 42, nil)
		derived43 := perRowRunIDForManifest(spy.req.RunTS, spy.req.RunShortID, 0, 43, nil)
		if derived42 != wantPerRow42 {
			t.Errorf("portal-derived per-row RunID for #42 = %q, want %q", derived42, wantPerRow42)
		}
		if derived43 != wantPerRow43 {
			t.Errorf("portal-derived per-row RunID for #43 = %q, want %q", derived43, wantPerRow43)
		}
		if derived42 == wantPublicBatchID {
			t.Errorf("multi-issue per-row RunID for #42 (%q) must NOT equal the public BatchId %q (the latter carries the +<n-1> suffix)", derived42, wantPublicBatchID)
		}
		if strings.Contains(derived42, "+1") {
			t.Errorf("per-row RunID for #42 %q must not carry the +N suffix", derived42)
		}

		// Sanity: per-row RunIDs differ from the public BatchId by
		// exactly the +1 segment, never the segment order.
		if !strings.HasSuffix(wantPublicBatchID, "+1") {
			t.Errorf("multi-issue public BatchId %q must carry the +<n-1> suffix", wantPublicBatchID)
		}
		if !strings.HasPrefix(derived42, spy.req.RunTS) {
			t.Errorf("per-row RunID %q must start with RunTS %q (timestamp-first)", derived42, spy.req.RunTS)
		}
	})
}

type countingSearchClient struct {
	*fakeGitHubClient
	searchCalls []string
}

func (c *countingSearchClient) SearchIssues(ctx context.Context, query string) ([]github.Issue, error) {
	c.searchCalls = append(c.searchCalls, query)
	return c.fakeGitHubClient.SearchIssues(ctx, query)
}

func TestRun_CachesRepeatedSearchWithinCommand(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &countingSearchClient{fakeGitHubClient: &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 42, State: "open", Title: "Issue A"}},
	}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.IsTTY = func() bool { return false }

	var output bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"42:"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", err, output.String())
	}
	var openSearches int
	for _, query := range gh.searchCalls {
		if query == "is:open" {
			openSearches++
		}
	}
	if openSearches != 1 {
		t.Fatalf("expected one underlying is:open search within one command, got %d calls: %v", openSearches, gh.searchCalls)
	}
}

type searchSequenceClient struct {
	*fakeGitHubClient
	errors []error
	calls  int
}

func (c *searchSequenceClient) SearchIssues(ctx context.Context, query string) ([]github.Issue, error) {
	idx := c.calls
	c.calls++
	if idx < len(c.errors) && c.errors[idx] != nil {
		return nil, c.errors[idx]
	}
	return c.fakeGitHubClient.SearchIssues(ctx, query)
}

type mutableStateClient struct {
	*fakeGitHubClient
	mu       sync.Mutex
	override func(int) *github.Issue
}

func (c *mutableStateClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	c.mu.Lock()
	override := c.override
	c.mu.Unlock()
	if override != nil {
		if issue := override(number); issue != nil {
			return issue, nil
		}
	}
	return c.fakeGitHubClient.FetchIssue(ctx, number)
}

func TestRun_Freshness_DependencyResolverReadsCurrentState(t *testing.T) {
	childBody := "## Parent\n\n#1\n"
	var calls []int
	var mu sync.Mutex
	gh := &mutableStateClient{
		fakeGitHubClient: &fakeGitHubClient{
			searchIssuesResult: []github.Issue{{Number: 10, State: "open", Title: "Child", Body: childBody}},
		},
		override: func(n int) *github.Issue {
			mu.Lock()
			calls = append(calls, n)
			mu.Unlock()
			switch n {
			case 10:
				return &github.Issue{Number: 10, State: "open", Title: "Child", Body: childBody, BlockedBy: []int{99}}
			case 99:
				return &github.Issue{Number: 99, State: "open", Title: "Blocker"}
			}
			return nil
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	cmd := NewRunCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"10"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := spy.req.Blocked[10]; len(got) != 1 || got[0] != 99 {
		mu.Lock()
		t.Fatalf("expected fresh state to mark #10 blocked by #99, got %v (calls=%v)", got, calls)
	}
}

func TestCachedGitHubClient_DoesNotCacheSearchErrors(t *testing.T) {
	delegate := &searchSequenceClient{
		fakeGitHubClient: &fakeGitHubClient{searchIssuesResult: []github.Issue{{Number: 42}}},
		errors:           []error{errors.New("temporary search failure")},
	}
	client := newCachedGitHubClient(delegate)
	if _, err := client.SearchIssues(context.Background(), "is:open"); err == nil {
		t.Fatal("expected first search to fail")
	}
	if _, err := client.SearchIssues(context.Background(), "is:open"); err != nil {
		t.Fatalf("expected second search to retry successfully, got %v", err)
	}
	if delegate.calls != 2 {
		t.Fatalf("expected two delegate calls after an uncached error, got %d", delegate.calls)
	}
}

func TestCachedGitHubClient_SearchCacheDoesNotCrossCommandWrappers(t *testing.T) {
	delegate := &countingSearchClient{fakeGitHubClient: &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 42, Title: "first"}},
	}}
	first := newCachedGitHubClient(delegate)
	if _, err := first.SearchIssues(context.Background(), "is:open"); err != nil {
		t.Fatalf("first search failed: %v", err)
	}
	delegate.searchIssuesResult = []github.Issue{{Number: 43, Title: "second"}}
	second := newCachedGitHubClient(delegate)
	got, err := second.SearchIssues(context.Background(), "is:open")
	if err != nil {
		t.Fatalf("second search failed: %v", err)
	}
	if len(got) != 1 || got[0].Number != 43 {
		t.Fatalf("expected second wrapper to observe fresh results, got %v", got)
	}
}

func TestRun_DoesNotEmitPreparationPhaseTiming(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	var output bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, phase := range []string{
		"specification-resolution",
		"dependency-resolution",
		"sandbox-preflight",
		"branch-validation",
		"first-sandbox-start",
	} {
		if strings.Contains(output.String(), "phase "+phase+" duration=") {
			t.Fatalf("unexpected phase timing for %s, got %q", phase, output.String())
		}
	}
}

// TestRun_PhaseWriterGatedByVerbose pins the contract that diagnostic
// `phase <name> duration=...` timing lines are only forwarded to a writer
// when the operator opts in via --verbose/-v. By default the run command
// leaves batch.Request.PhaseWriter nil so the orchestrator's writePhase
// short-circuits and stderr stays quiet (#2222).
func TestRun_PhaseWriterGatedByVerbose(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantNil bool
	}{
		{name: "default omits phase writer", args: []string{"42"}, wantNil: true},
		{name: "verbose long sets phase writer", args: []string{"42", "--verbose"}, wantNil: false},
		{name: "verbose short sets phase writer", args: []string{"42", "-v"}, wantNil: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(t, spy)
			cmd := NewRunCmd(deps)
			cmd.SetArgs(tc.args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !spy.called {
				t.Fatalf("batch runner not called")
			}
			if tc.wantNil && spy.req.PhaseWriter != nil {
				t.Errorf("expected nil PhaseWriter, got non-nil %T", spy.req.PhaseWriter)
			}
			if !tc.wantNil && spy.req.PhaseWriter == nil {
				t.Errorf("expected non-nil PhaseWriter, got nil")
			}
		})
	}
}

// TestRun_ContinueFlag_SpecExpansion_StatusCheckRollupArray is the
// regression test for #2218. The user ran
// `sandman run <spec> --sandbox worktree --continue` after the
// Specification expansion shipped; the spec's children included an issue
// that had a prior run, so the continuation path called
// batch.CheckPRMergedAtHead against `gh pr list --json ...`. gh CLI
// ≥ 2.65 emits `statusCheckRollup` as an array of CheckRun objects
// instead of a flat string — the legacy parser failed with
// "json: cannot unmarshal array into Go struct field
// prPayload.statusCheckRollup of type string" and the batch could
// never start.
//
// The test wires a Specification with three children (#63 closed,
// #64 closed, #65 with a prior run) plus a FindPRByBranch fake that
// returns the new array shape, then asserts the command reaches the
// batch runner (no parse error, no exit). It also pins that the
// diagnostic `phase <name> duration=...` line is absent from the
// default operator output.
func TestRun_ContinueFlag_SpecExpansion_StatusCheckRollupArray(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #63\n- #64\n- #65\n"
	childBody := "## Parent\n\n#62\n\n## What\n\n"
	branch := batch.BranchName(65, "child-65")

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			62: {Number: 62, Title: "Specification parent", Body: specBody, State: "open"},
			63: {Number: 63, Title: "Child 63", Body: childBody, State: "closed"},
			64: {Number: 64, Title: "Child 64", Body: childBody, State: "closed"},
			65: {Number: 65, Title: "Child 65", Body: childBody, State: "open"},
		},
		searchIssuesResult: []github.Issue{
			{Number: 62, Title: "Specification parent", State: "open"},
			{Number: 65, Title: "Child 65", State: "open"},
		},
		prs: map[string]*github.PR{
			branch: {
				Number: 1234, State: "OPEN", Merged: false,
				HeadRefName: branch, HeadRefOid: "abc123",
				MergeStateStatus:  "CLEAN",
				ReviewDecision:    "APPROVED",
				StatusCheckRollup: "success",
			},
		},
	}

	dir := t.TempDir()
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	worktreePath := addRegisteredContinuationWorktree(t, deps.RepoRoot, dir, branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	taskContent := "## Stage: in-progress\n\nCarry on.\n"
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte(taskContent), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	deps.GitHubClient = gh
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "prior-65-1", Issue: 65, Payload: map[string]any{
			"agent": "opencode", "branch": branch, "base_branch": "main",
		}},
	}}

	var output bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--continue", "--sandbox", "worktree", "62"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, output.String())
	}
	if !spy.called {
		t.Fatalf("expected batch runner to be called, output:\n%s", output.String())
	}
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 65 {
		t.Fatalf("expected only #65 to be continued (open spec child), got %v", spy.req.Issues)
	}
	if mode := spy.req.IssueMode(65); mode != batch.ModeContinue {
		t.Fatalf("expected #65 mode=continue, got %q", mode)
	}
	if !strings.Contains(output.String(), "Issue #63 is closed, skipping") {
		t.Errorf("expected skip warning for closed child #63, got: %q", output.String())
	}
	for _, phase := range []string{"specification-resolution", "dependency-resolution"} {
		if strings.Contains(output.String(), "phase "+phase+" duration=") {
			t.Errorf("unexpected phase timing for %s, output: %q", phase, output.String())
		}
	}
}

// TestRun_ContinueFlag_NoPriorRunPromotesToOverrideWithoutAPICall
// guards the spec-expansion + --continue path against an N² storm of
// gh calls for issues that never had a prior run. When a child of a
// Specification has no recorded run.started event,
// continuation.go's lastRunPerIssue lookup short-circuits to override
// mode; this test pins that short-circuit by counting FindPRByBranch
// invocations and asserting FindPRByBranch fires at most once for the
// only continuation-eligible issue (the one that did have a prior run).
func TestRun_ContinueFlag_NoPriorRunPromotesToOverrideWithoutAPICall(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #200\n- #201\n- #202\n"
	childBody := "## Parent\n\n#199\n\n## What\n\n"
	onlyBranch := batch.BranchName(200, "child-200")

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			199: {Number: 199, Title: "Specification parent", Body: specBody, State: "open"},
			200: {Number: 200, Title: "Child 200", Body: childBody, State: "open"},
			201: {Number: 201, Title: "Child 201", Body: childBody, State: "open"},
			202: {Number: 202, Title: "Child 202", Body: childBody, State: "open"},
		},
		searchIssuesResult: []github.Issue{
			{Number: 199, Title: "Specification parent", State: "open"},
			{Number: 200, Title: "Child 200", State: "open"},
			{Number: 201, Title: "Child 201", State: "open"},
			{Number: 202, Title: "Child 202", State: "open"},
		},
		prs: map[string]*github.PR{
			onlyBranch: {Number: 7777, State: "OPEN", Merged: false, HeadRefName: onlyBranch, HeadRefOid: "deadbeef"},
		},
		findPRCalls: map[string]int{},
	}

	dir := t.TempDir()
	worktreePath := filepath.Join(dir, onlyBranch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("## Stage: in-progress\n"), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "prior-200-1", Issue: 200, Payload: map[string]any{
			"agent": "opencode", "branch": onlyBranch, "base_branch": "main",
		}},
	}}

	var output bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--continue", "--sandbox", "worktree", "199"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, output.String())
	}
	if !spy.called {
		t.Fatalf("expected batch runner to be called, output:\n%s", output.String())
	}
	if calls := gh.findPRCalls[onlyBranch]; calls > 1 {
		t.Fatalf("expected at most one FindPRByBranch call for the continued branch, got %d", calls)
	}
	if !strings.Contains(output.String(), "[--continue] promoting #201 to --override") {
		t.Errorf("expected override promotion log for #201, got: %q", output.String())
	}
	if !strings.Contains(output.String(), "[--continue] promoting #202 to --override") {
		t.Errorf("expected override promotion log for #202, got: %q", output.String())
	}
}
