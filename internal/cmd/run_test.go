package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
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

	testRunIDIssue42First  = testRunShortID + "-" + testRunTS + "-issue-42-1"
	testRunIDIssue42Second = testRunShortID + "-" + testRunTS + "-issue-42-2"
	testRunIDIssue42Prev   = testRunShortID + "-" + testRunTS + "-issue-42-prev"
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

func (f *fakeGitHubClient) FetchIssue(number int) (*github.Issue, error) {
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
		<-release
	}
	f.mu.Lock()
	issue, ok := f.issues[number]
	f.mu.Unlock()
	if ok {
		return issue, nil
	}
	return &github.Issue{Number: number}, nil
}

func (f *fakeGitHubClient) FetchIssueDependencies(number int) ([]int, error) {
	f.mu.Lock()
	issue, ok := f.issues[number]
	f.mu.Unlock()
	if ok {
		return issue.BlockedBy, nil
	}
	return nil, nil
}

func (f *fakeGitHubClient) FetchPR(number int) (*github.PR, error) {
	return &github.PR{Number: number, State: "open"}, nil
}

func (f *fakeGitHubClient) SearchIssues(query string) ([]github.Issue, error) {
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

func (f *fakeGitHubClient) FindPRByBranch(branch string) (*github.PR, error) {
	if f.prs != nil {
		if pr, ok := f.prs[branch]; ok {
			return pr, nil
		}
		return nil, nil
	}
	return nil, nil
}

func (f *fakeGitHubClient) ListOpenPRs() ([]github.PR, error) {
	return nil, nil
}

func (f *fakeGitHubClient) ListPRComments(number int) ([]github.PRComment, error) {
	return nil, nil
}

func (f *fakeGitHubClient) ListIssueComments(number int) ([]github.IssueComment, error) {
	return nil, nil
}

func (f *fakeGitHubClient) RepoName() (string, error) {
	return "owner/repo", nil
}

func (f *fakeGitHubClient) EditComment(commentID, body string) error {
	return nil
}

func (f *fakeGitHubClient) EditPRBody(prNumber int, body string) error {
	return nil
}

func (f *fakeGitHubClient) AddCommentReaction(commentID, content string) (string, error) {
	return "", nil
}

func (f *fakeGitHubClient) AddIssueReaction(issueNumber int, content string) (string, error) {
	return "", nil
}

func (f *fakeGitHubClient) RemoveCommentReaction(commentID, reactionID string) error {
	return nil
}

func (f *fakeGitHubClient) RemoveIssueReaction(issueNumber int, reactionID string) error {
	return nil
}

// newRunDeps returns Dependencies for a run command test. The
// default review command is overridden to "/oc review" so the
// review daemon guard (issue #383) is bypassed by default. Tests
// that need to exercise the guard must build their own
// Dependencies and chdir into a temp dir without a live socket.
func newRunDeps(runner batch.Runner) Dependencies {
	return Dependencies{
		BatchRunner:  runner,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
	}
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
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
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
			searchFn := func(query string) ([]github.Issue, error) {
				results := make([]github.Issue, 0, len(tt.openSet))
				for n := range tt.openSet {
					results = append(results, github.Issue{Number: n, State: "open"})
				}
				return results, nil
			}
			fetchFn := func(n int) (*github.Issue, error) {
				return &github.Issue{Number: n, State: tt.states[n]}, nil
			}
			var stderr bytes.Buffer
			got, err := filterClosedIssues(tt.numbers, searchFn, fetchFn, &stderr)
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
	searchFn := func(query string) ([]github.Issue, error) {
		return nil, fmt.Errorf("transient gh error")
	}
	fetchFn := func(n int) (*github.Issue, error) {
		return &github.Issue{Number: n, State: "open"}, nil
	}
	var stderr bytes.Buffer
	got, err := filterClosedIssues([]int{42, 43}, searchFn, fetchFn, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected fallback to return all open, got %v", got)
	}
}

func TestFilterClosedIssues_FetchErrorIsSkipped(t *testing.T) {
	searchFn := func(query string) ([]github.Issue, error) {
		return nil, fmt.Errorf("transient gh error")
	}
	fetchFn := func(n int) (*github.Issue, error) {
		return nil, fmt.Errorf("network error")
	}
	var stderr bytes.Buffer
	got, err := filterClosedIssues([]int{42}, searchFn, fetchFn, &stderr)
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

func TestRun_SingleIssueInvokesBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

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

func TestRun_ExpandsPRDBeforeBatchRunner(t *testing.T) {
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n- #11\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD", Body: prdBody},
			10: {Number: 10, Title: "Child 1", Body: childBody},
			11: {Number: 11, Title: "Child 2", Body: childBody},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
	}

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
	if !strings.Contains(buf.String(), "expanded PRD #1 to 2 accepted children") {
		t.Errorf("expected info log about PRD expansion, got: %q", buf.String())
	}
}

func TestRun_MixedPRDAndNonChildIssues(t *testing.T) {
	// Regression for #1038 — see ADR-0025 §3a. The original failure
	// mode was `resolve PRDs: nested PRD detected: #982` when running
	// `sandman run 972:977 982 990 994:1001`.
	prd982Body := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. U.\n\nSlices tracked in #972, #973, #974.\n\n## Child Issues\n\n- #984 child\n- #985 child\n- #986 child\n- #987 child\n- #988 child\n- #989 child\n"
	prd990Body := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. U.\n\nSee parent #982.\n"
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
			982:  {Number: 982, Title: "Outer PRD", Body: prd982Body},
			984:  {Number: 984, Title: "Child 984", Body: childBody},
			985:  {Number: 985, Title: "Child 985", Body: childBody},
			986:  {Number: 986, Title: "Child 986", Body: childBody},
			987:  {Number: 987, Title: "Child 987", Body: childBody},
			988:  {Number: 988, Title: "Child 988", Body: childBody},
			989:  {Number: 989, Title: "Child 989", Body: childBody},
			990:  {Number: 990, Title: "Cross-referencing PRD", Body: prd990Body},
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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
	}

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
	// Slices 972..977 are user-typed non-PRD issues; they pass through.
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
	// Issues 994..1001 are user-typed non-PRD issues; they pass through.
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
	// No PRD candidate-mismatch warning on stderr.
	if strings.Contains(buf.String(), "candidate #") && strings.Contains(buf.String(), "not a child") {
		t.Errorf("expected no 'candidate not a child' warning on stderr, got: %q", buf.String())
	}
	// No nested-PRD error on stderr.
	if strings.Contains(buf.String(), "nested PRD") {
		t.Errorf("expected no 'nested PRD' warning on stderr, got: %q", buf.String())
	}
}

func TestRun_FailsWhenPRDHasNoChildren(t *testing.T) {
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n"
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Empty PRD", Body: prdBody},
		},
	}
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
	}

	cmd := NewRunCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for PRD with no children, got nil")
	}
	if !strings.Contains(err.Error(), "no child issues for PRD #1") {
		t.Fatalf("expected 'no child issues for PRD #1' in error, got %q", err)
	}
	if spy.called {
		t.Error("expected batch runner NOT to be called when PRD resolution fails")
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
	got, err := c.ListIssueComments(42)
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
	got, err = c.ListIssueComments(42)
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

func (c *countingCommentsClient) ListIssueComments(number int) ([]github.IssueComment, error) {
	if c.fetch != nil {
		c.fetch()
	}
	return c.comments, nil
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

	got, err := c.RepoName()
	if err != nil {
		t.Fatalf("RepoName() error: %v", err)
	}
	if got != "rafaelromao/sandman" {
		t.Fatalf("RepoName() = %q, want %q", got, "rafaelromao/sandman")
	}

	if err := c.EditComment("c1", "body"); err != nil {
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

func (s *stubClient) RepoName() (string, error) { return s.repo, nil }
func (s *stubClient) EditComment(commentID, body string) error {
	if s.onEdit != nil {
		s.onEdit()
	}
	return s.editError
}

func TestRun_MultipleIssuesInvokesBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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

func TestRun_FreshRunErrorsWhenBranchAlreadyExists(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}
	dir := t.TempDir()
	t.Chdir(dir)
	initRunIntegrationRepo(t, dir)
	writeSandmanDockerfile(t, dir)

	branch := "sandman/42-fix-bug"
	runGit(t, dir, "checkout", "-b", branch)
	runGit(t, dir, "checkout", "main")

	gh := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}}
	store := &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}}
	deps := Dependencies{
		BatchRunner:  batch.NewOrchestrator(gh, &prompt.Engine{}, store, &fakeEventLog{}),
		ConfigStore:  store,
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when branch already exists")
	}
	if !strings.Contains(err.Error(), "#42: no prior run — use --override") {
		t.Fatalf("expected per-issue override guidance, got %v", err)
	}
	if !strings.Contains(err.Error(), "git branch -D <branch>") {
		t.Fatalf("expected branch deletion hint, got %v", err)
	}
}

func TestRun_NoOverrideAlias(t *testing.T) {
	cmd := NewRunCmd(newRunDeps(&spyBatchRunner{result: &batch.Result{}}))
	if cmd.Flags().Lookup("force") != nil {
		t.Fatal("expected --force flag to be removed")
	}
	if cmd.Flags().Lookup("override") == nil {
		t.Fatal("expected --override flag to exist")
	}
}

func TestRun_ReconcileStrandedDefaultTrue(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
			deps := newRunDeps(spy)

			if tt.name == "continue only" {
				dir := t.TempDir()
				branch := "sandman/42-fix-bug"
				if err := os.MkdirAll(filepath.Join(dir, branch, ".sandman"), 0755); err != nil {
					t.Fatalf("mkdir worktree: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, branch, ".sandman", "task.md"), []byte("## Completed\nInitial pass.\n"), 0644); err != nil {
					t.Fatalf("write task: %v", err)
				}
				deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, ReviewCommand: "/oc review", AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}}
				deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: testRunIDIssue42First, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}}}}
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

func TestRun_ContinueFlag_ReplaysStoredContinuationState(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, branch, ".sandman", "task.md"), []byte("## Completed\nInitial pass.\n"), 0644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode",
		DefaultModel:  "openai/gpt-4.1",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		Git:           config.GitConfig{BaseBranch: "trunk"},
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: testRunIDIssue42First, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode", "model": "gpt-4.1", "review_command": "/custom review", "parallel": 1, "start_delay": 3, "retries": 2, "sandbox": "worktree", "container_capacity": 1, "container_capacity_set": true, "max_containers": 2, "max_containers_set": true}},
		{Type: "run.continued", RunID: testRunIDIssue42Second, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode", "model": "gpt-4.2", "review_command": "/custom review 2", "parallel": 7, "start_delay": 11, "retries": 4, "sandbox": "docker", "container_capacity": 3, "container_capacity_set": true, "max_containers": 5, "max_containers_set": true}},
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
	if spy.req.PreviousRunIDs[42] != testRunIDIssue42Second {
		t.Fatalf("expected PreviousRunIDs[42]=%s, got %q", testRunIDIssue42Second, spy.req.PreviousRunIDs[42])
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
	if spy.req.Agent != "opencode" {
		t.Fatalf("expected agent replay, got %q", spy.req.Agent)
	}
	if spy.req.Model != "gpt-4.2" {
		t.Fatalf("expected model replay from prior run, got %q", spy.req.Model)
	}
	if spy.req.BaseBranch != "main" {
		t.Fatalf("expected base branch replay, got %q", spy.req.BaseBranch)
	}
	if spy.req.Parallel != 7 {
		t.Fatalf("expected parallel replay, got %d", spy.req.Parallel)
	}
	if spy.req.StartDelay != 11*time.Second || !spy.req.StartDelaySet {
		t.Fatalf("expected start delay replay, got %s set=%v", spy.req.StartDelay, spy.req.StartDelaySet)
	}
	if spy.req.Retries != 4 {
		t.Fatalf("expected retries replay, got %d", spy.req.Retries)
	}
	if spy.req.Sandbox != "docker" {
		t.Fatalf("expected sandbox replay, got %q", spy.req.Sandbox)
	}
	if spy.req.ContainerCapacity != 3 || !spy.req.ContainerCapacitySet {
		t.Fatalf("expected container capacity replay, got %d set=%v", spy.req.ContainerCapacity, spy.req.ContainerCapacitySet)
	}
	if spy.req.MaxContainers != 5 || !spy.req.MaxContainersSet {
		t.Fatalf("expected max containers replay, got %d set=%v", spy.req.MaxContainers, spy.req.MaxContainersSet)
	}
	if spy.req.PromptConfig.ReviewCommand != "/custom review 2" || !spy.req.PromptConfig.ReviewCommandSet {
		t.Fatalf("expected review command replay, got %q set=%v", spy.req.PromptConfig.ReviewCommand, spy.req.PromptConfig.ReviewCommandSet)
	}
}

func TestRun_ContinueFlag_UsesOverridesAndEmptyTemplateFallback(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
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
	deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: testRunIDIssue42First, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode", "model": "gpt-4.1", "review_command": "/custom review", "parallel": 1, "start_delay": 3, "retries": 2, "sandbox": "docker", "container_capacity": 1, "container_capacity_set": true, "max_containers": 2, "max_containers_set": true}}}}

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
	dir := t.TempDir()
	branch := "sandman/42-fix-bug"
	if err := os.MkdirAll(filepath.Join(dir, branch, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, branch, ".sandman", "task.md"), []byte("## Completed\nInitial pass.\n"), 0644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: testRunIDIssue42Prev, Issue: 42, Payload: map[string]any{"agent": "opencode", "branch": branch, "base_branch": "main"}}}}
	deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Fix bug"},
		43: {Number: 43, Title: "Fresh bug"},
	}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "42", "43"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := spy.req.IssueMode(42); got != batch.ModeContinue {
		t.Fatalf("expected issue 42 continue mode, got %v", got)
	}
	if got := spy.req.IssueMode(43); got != batch.ModeOverride {
		t.Fatalf("expected issue 43 override mode (promoted from --continue), got %v", got)
	}
	if spy.req.PreviousRunIDs[42] != testRunIDIssue42Prev {
		t.Fatalf("expected issue 42 previous run replay, got %q", spy.req.PreviousRunIDs[42])
	}
	if _, ok := spy.req.PreviousRunIDs[43]; ok {
		t.Fatalf("expected issue 43 to have no previous run replay, got %q", spy.req.PreviousRunIDs[43])
	}
	if spy.req.Branches[42] != branch {
		t.Fatalf("expected issue 42 branch replay, got %q", spy.req.Branches[42])
	}
	if _, ok := spy.req.Branches[43]; ok {
		t.Fatalf("expected issue 43 to have no branch replay, got %q", spy.req.Branches[43])
	}
	if !strings.Contains(buf.String(), "[--continue] promoting #43 to override (no prior started/continued run)") {
		t.Fatalf("expected promotion log line for issue 43, got output:\n%s", buf.String())
	}
}

func TestRun_ContinueFlag_NoPreviousPromptOnlyRun_ReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	if !strings.Contains(buf.String(), "[--continue] promoting #42 to override (no prior started/continued run)") {
		t.Fatalf("expected promotion log line for issue 42, got output:\n%s", buf.String())
	}
}

func TestRun_ContinueFlag_WarnsWhenIssueTaskMissing(t *testing.T) {
	dir := t.TempDir()
	branch := "issue-42"
	if err := os.MkdirAll(filepath.Join(dir, branch, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: testRunIDIssue42Prev, Issue: 42, Payload: map[string]any{"agent": "opencode", "branch": branch, "base_branch": "main"}}}}
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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(&spyBatchRunner{result: &batch.Result{}})

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
			deps := newRunDeps(spy)
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

func TestRun_PromptOnlyRejectsSubstitutedIssuePlaceholders(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
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
			deps := newRunDeps(spy)

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
			deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IssuePicker:  picker,
		IsTTY:        func() bool { return true },
	}

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
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	if gh.fetchCount[42] != 1 {
		t.Errorf("expected issue 42 to be fetched once, got %d", gh.fetchCount[42])
	}
}

func TestRun_CombinePlainArgsWithLabelSkipsClosedIssue(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Bug A", State: "closed", Labels: []string{"bug"}},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	if gh.fetchCount[42] != 1 {
		t.Errorf("expected issue 42 to be fetched once, got %d", gh.fetchCount[42])
	}
}

func TestRun_CombinePlainArgsWithQueryUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Feature A", State: "open", Labels: []string{"bug"}},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	if gh.fetchCount[42] != 1 {
		t.Errorf("expected issue 42 to be fetched once, got %d", gh.fetchCount[42])
	}
}

func TestRun_RangeArgUsesCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
		if gh.fetchCount[n] != 1 {
			t.Errorf("expected issue %d to be fetched once, got %d", n, gh.fetchCount[n])
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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
		if gh.fetchCount[n] != 1 {
			t.Errorf("expected issue %d to be fetched once, got %d", n, gh.fetchCount[n])
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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
		if gh.fetchCount[n] != 1 {
			t.Errorf("expected issue %d to be fetched once, got %d", n, gh.fetchCount[n])
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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: &fakeGitHubClient{},
		IsTTY:        func() bool { return false },
	}

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
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

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

func TestRun_AutoFlagDelegatesLowestIssue(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 3, Title: "Feature C"},
			{Number: 1, Title: "Feature A"},
			{Number: 2, Title: "Feature B"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "1"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{1}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	if spy.req.Issues[0] != 1 {
		t.Errorf("expected issue 1, got %d", spy.req.Issues[0])
	}
	if gh.searchIssuesQuery != "label:ready-for-agent is:open" {
		t.Errorf("expected search query 'label:ready-for-agent is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_AutoFlagWithCountDelegatesN(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 5, Title: "Feature E"},
			{Number: 2, Title: "Feature B"},
			{Number: 3, Title: "Feature C"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "2"})

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
}

func TestRun_AutoFlagWithFewerAvailableDelegatesAll(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 2, Title: "Feature B"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "5"})

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
}

func TestRun_AutoFlagNoIssuesReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no ready-for-agent issues")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "no issues ready for agent") {
		t.Errorf("expected 'no issues ready for agent' error, got: %v", err)
	}
}

func TestRun_AutoFlagZeroCountIsUnlimited(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "0"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called (--count 0 is unlimited)")
	}
	if len(spy.req.Issues) != 1 {
		t.Fatalf("expected 1 issue (unlimited), got %d", len(spy.req.Issues))
	}
}

func TestRun_AutoFlagAcceptsExplicitArgs(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, State: "open", Title: "Issue 42"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called (--auto accepts args)")
	}
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 42 {
		t.Fatalf("expected [42], got %v", spy.req.Issues)
	}
}

func TestRun_AutoFlagWithLabelUsesLabelSearch(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Bug A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--label", "bug"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_AutoFlagWithQueryUsesRawQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 3, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--query", "label:bug is:open"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_AutoFlagWithLabelAndQueryReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--label", "bug", "--query", "is:open"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when combining --label with --query")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "cannot combine") {
		t.Errorf("expected mutual exclusivity error, got: %v", err)
	}
}

func TestResolveAutoIssues_LegacyPriorityPromptFileIgnored(t *testing.T) {
	sandmanDir := t.TempDir()
	promptPath := filepath.Join(sandmanDir, "priority-selection-prompt.md")
	if err := os.WriteFile(promptPath, []byte("test"), 0644); err != nil {
		t.Fatalf("create prompt: %v", err)
	}

	gh := &fakeGitHubClient{}

	issues, _, _, err := resolveAutoIssues(context.Background(), gh, 1, []int{1, 3}, sandmanDir, "", "", &config.Config{}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0] != 1 {
		t.Errorf("expected numeric-sort fallback [1], got %v", issues)
	}
}

func TestResolveAutoIssues_PriorityPromptFileAbsentUsesNumericSort(t *testing.T) {
	sandmanDir := t.TempDir()
	gh := &fakeGitHubClient{}

	issues, _, _, err := resolveAutoIssues(context.Background(), gh, 1, []int{1, 3}, sandmanDir, "", "", &config.Config{}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0] != 1 {
		t.Errorf("expected [1], got %v", issues)
	}
}

func TestReadSelectedIssues_ValidJSONReturnsNumbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte("[1, 2, 3]"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := readSelectedIssues(dir, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("expected %d at index %d, got %d", v, i, got[i])
		}
	}
}

func TestReadSelectedIssues_CapsAtMaxCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte("[1, 2, 3, 4, 5]"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := readSelectedIssues(dir, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestReadSelectedIssues_MissingFileReturnsError(t *testing.T) {
	dir := t.TempDir()

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "produced no output") {
		t.Errorf("expected 'produced no output' error, got: %v", err)
	}
}

func TestReadSelectedIssues_InvalidJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte("not-json"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid selection format") {
		t.Errorf("expected 'invalid selection format' error, got: %v", err)
	}
}

func TestReadSelectedIssues_NonArrayJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte(`{"key": "value"}`), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for non-array JSON")
	}
	if !strings.Contains(err.Error(), "invalid selection format") {
		t.Errorf("expected 'invalid selection format' error, got: %v", err)
	}
}

func TestReadSelectedIssues_EmptyArrayReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte("[]"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for empty array")
	}
	if !strings.Contains(err.Error(), "selected no issues") {
		t.Errorf("expected 'selected no issues' error, got: %v", err)
	}
}

func TestReadSelectedIssues_NonIntArrayReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected-issues.json")
	if err := os.WriteFile(path, []byte(`["a", "b"]`), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := readSelectedIssues(dir, 5)
	if err == nil {
		t.Fatal("expected error for non-int array")
	}
	if !strings.Contains(err.Error(), "invalid selection format") {
		t.Errorf("expected 'invalid selection format' error, got: %v", err)
	}
}

func TestRunSelectionPhase_AgentWritesSelectedIssuesAndReturnsNumbers(t *testing.T) {
	sandmanDir := t.TempDir()

	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A", Body: "Description A", Labels: []string{"bug"}},
			{Number: 2, Title: "Feature B", Body: "Description B", Labels: []string{"enhancement"}},
			{Number: 3, Title: "Feature C", Body: "Description C", Labels: []string{"bug"}},
		},
	}

	cfg := &config.Config{
		Agent:         "test-agent",
		ReviewCommand: "/oc review",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {
			Command: fmt.Sprintf("echo '[2, 1]' > %s/selected-issues.json", sandmanDir),
		},
	}

	got, err := runSelectionPhase(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1, 2, 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{2, 1}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("expected %d at index %d, got %d", v, i, got[i])
		}
	}
}

func TestRunSelectionPhase_AgentFailureReturnsError(t *testing.T) {
	sandmanDir := t.TempDir()

	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A"},
		},
	}

	cfg := &config.Config{
		Agent:         "test-agent",
		ReviewCommand: "/oc review",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {
			Command: "exit 1",
		},
	}

	_, err := runSelectionPhase(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1})
	if err == nil {
		t.Fatal("expected error from agent failure")
	}
	if !strings.Contains(err.Error(), "selection agent failed") {
		t.Errorf("expected agent failure error, got: %v", err)
	}
}

func TestRunSelectionPhase_GuardFiresWhenReviewCommandContainsSandmanAndNoSocket(t *testing.T) {
	sandmanDir := t.TempDir()
	gh := &fakeGitHubClient{searchIssuesResult: []github.Issue{{Number: 1}}}

	cfg := &config.Config{
		Agent:         "test-agent",
		ReviewCommand: "/sandman review",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {Command: "true"},
	}

	_, err := runSelectionPhase(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1})
	if err == nil {
		t.Fatal("expected error from review guard, got nil")
	}
	if err.Error() != reviewGuardMessage {
		t.Errorf("unexpected error message\nwant:\n%s\ngot:\n%s", reviewGuardMessage, err.Error())
	}
}

func TestSelectionPhase_FormatCandidateIssues(t *testing.T) {
	issues := []github.Issue{
		{Number: 1, Title: "Bug", Body: "Fix this bug", Labels: []string{"bug"}},
		{Number: 2, Title: "Feature", Body: "Add new feature", Labels: []string{"enhancement"}},
	}

	result := formatCandidateIssues(issues)
	if !strings.Contains(result, "#1") {
		t.Error("expected #1 in formatted output")
	}
	if !strings.Contains(result, "Bug") {
		t.Error("expected Bug in formatted output")
	}
	if !strings.Contains(result, "[bug]") {
		t.Error("expected [bug] in formatted output")
	}
	if !strings.Contains(result, "Fix this bug") {
		t.Error("expected body in formatted output")
	}
}

func TestRun_AutoFlagNegativeCountReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := Dependencies{
		BatchRunner: spy,
		ConfigStore: &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:    &fakeEventLog{},
		IsTTY:       func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "-1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --count -1")
	}
	if spy.called {
		t.Error("expected batch runner not to be called")
	}
	if !strings.Contains(err.Error(), "--count must be 0 or greater") {
		t.Errorf("expected --count validation error, got: %v", err)
	}
}

func TestRun_AutoFlagSetsConservativeDefaults(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 5, Title: "Feature E"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "1"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if spy.req.Parallel != 1 {
		t.Errorf("expected parallel=1, got %d", spy.req.Parallel)
	}
	if spy.req.ContainerCapacity != 1 {
		t.Errorf("expected container-capacity=1, got %d", spy.req.ContainerCapacity)
	}
	if spy.req.MaxContainers != 1 {
		t.Errorf("expected max-containers=1, got %d", spy.req.MaxContainers)
	}
	if spy.req.Retries != 3 {
		t.Errorf("expected retries=3, got %d", spy.req.Retries)
	}
}

func TestRun_AutoFlagParallelOverride(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 5, Title: "Feature E"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "1", "--parallel", "4"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if spy.req.Parallel != 4 {
		t.Errorf("expected parallel=4, got %d", spy.req.Parallel)
	}
	if spy.req.ContainerCapacity != 1 {
		t.Errorf("expected container-capacity=1 (auto default), got %d", spy.req.ContainerCapacity)
	}
	if spy.req.MaxContainers != 1 {
		t.Errorf("expected max-containers=1 (auto default), got %d", spy.req.MaxContainers)
	}
	if !spy.req.ContainerCapacitySet {
		t.Error("expected ContainerCapacitySet=true when auto defaults apply")
	}
	if !spy.req.MaxContainersSet {
		t.Error("expected MaxContainersSet=true when auto defaults apply")
	}
	if spy.req.Retries != 3 {
		t.Errorf("expected retries=3 (auto default), got %d", spy.req.Retries)
	}
}

func TestRun_AutoFlagRetriesZeroOverride(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 5, Title: "Feature E"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "1", "--retries", "0"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if spy.req.Parallel != 1 {
		t.Errorf("expected parallel=1 (auto default), got %d", spy.req.Parallel)
	}
	if spy.req.Retries != 0 {
		t.Errorf("expected retries=0 (CLI override), got %d", spy.req.Retries)
	}
}

func TestRun_AutoFlagMaxContainersOverride(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 5, Title: "Feature E"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auto", "--count", "1", "--max-containers", "3"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if spy.req.Parallel != 1 {
		t.Errorf("expected parallel=1 (ralph default), got %d", spy.req.Parallel)
	}
	if spy.req.MaxContainers != 3 {
		t.Errorf("expected max-containers=3 (CLI override), got %d", spy.req.MaxContainers)
	}
	if spy.req.ContainerCapacity != 1 {
		t.Errorf("expected container-capacity=1 (ralph default), got %d", spy.req.ContainerCapacity)
	}
	if spy.req.Retries != 3 {
		t.Errorf("expected retries=3 (ralph default), got %d", spy.req.Retries)
	}
}

func TestRun_QueryFlagResolvesIssues(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 3, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--query", "author:me"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 1 || spy.req.Issues[0] != 3 {
		t.Errorf("expected issues [3], got %v", spy.req.Issues)
	}
	if gh.searchIssuesQuery != "author:me is:open" {
		t.Errorf("expected search query 'author:me is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_LabelAndQueryFlagsUseCombinedQuery(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 3, Title: "Feature A"}},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--label", "bug", "--query", "author:me"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "label:bug author:me is:open" {
		t.Errorf("expected search query 'label:bug author:me is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_QueryCommaSeparatedLabelUsesSearch(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 42, State: "open", Title: "Bug A"}},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"}},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
	}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--query", "label:bug,enhancement", "42"})

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
	if gh.searchIssuesQuery != "label:bug,enhancement is:open" {
		t.Errorf("expected search query 'label:bug,enhancement is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_ContainerFlagsPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--sandbox", "docker", "--container-capacity", "1", "--max-containers", "2", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.ContainerCapacitySet {
		t.Fatal("expected ContainerCapacitySet=true")
	}
	if spy.req.ContainerCapacity != 1 {
		t.Errorf("expected container_capacity=1, got %d", spy.req.ContainerCapacity)
	}
	if !spy.req.MaxContainersSet {
		t.Fatal("expected MaxContainersSet=true")
	}
	if spy.req.MaxContainers != 2 {
		t.Errorf("expected max_containers=2, got %d", spy.req.MaxContainers)
	}
	if spy.req.Sandbox != "docker" {
		t.Errorf("expected sandbox=docker, got %q", spy.req.Sandbox)
	}
}

func TestRun_MaxContainersAutoFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--max-containers", "0", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.MaxContainersSet {
		t.Fatal("expected MaxContainersSet=true")
	}
	if spy.req.MaxContainers != 0 {
		t.Errorf("expected max_containers=0, got %d", spy.req.MaxContainers)
	}
}

func TestRun_ContainerCapacityAutoFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--container-capacity", "0", "42"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.req.ContainerCapacitySet {
		t.Fatal("expected ContainerCapacitySet=true")
	}
	if spy.req.ContainerCapacity != 0 {
		t.Errorf("expected container_capacity=0, got %d", spy.req.ContainerCapacity)
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
			deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)
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

func TestRun_PromptArgFlagPassedToBatchRunner(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)

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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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
	deps := newRunDeps(spy)
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

// TestRun_IssueDrivenBatchUsesNewIDScheme verifies that `sandman run 42`
// builds a directory id matching the new <shortid>-<ts>-<N>+<N>
// shape (acceptance criterion #1) and that the (ts, shortid) pair is
// propagated into batch.Request.RunTS / RunShortID so the orchestrator
// can build per-row RunIDs from it.
func TestRun_IssueDrivenBatchUsesNewIDScheme(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
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
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if spy.req.RunTS == "" {
		t.Errorf("expected req.RunTS to be populated for issue-driven batch")
	}
	if spy.req.RunShortID == "" {
		t.Errorf("expected req.RunShortID to be populated for issue-driven batch")
	}
	// RunDir is captured on the session; verify the dir id matches the
	// new <shortid>-<ts> format used by both daemon and orchestrator.
	dir := spy.req.RunDir
	want := filepath.Join(".sandman", "batches", spy.req.RunShortID+"-"+spy.req.RunTS)
	if dir != want {
		t.Fatalf("expected run dir %q, got %q", want, dir)
	}
}
