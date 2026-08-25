package batch

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestDependencyResolverResolve_SortsIssuesTopologically(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42, 7}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "Groundwork"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{100}, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{7, 42, 100}) {
		t.Fatalf("expected topological order [7 42 100], got %v", resolved.Issues)
	}

	wantDeps := map[int][]int{
		7:   nil,
		42:  {7},
		100: {7, 42},
	}
	if !reflect.DeepEqual(resolved.Deps, wantDeps) {
		t.Fatalf("expected deps %v, got %v", wantDeps, resolved.Deps)
	}
}

func TestDependencyResolverResolve_StableTopologicalOrderForMixedDependencyLevels(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Independent A"},
			2: {Number: 2, Title: "Independent B"},
			3: {Number: 3, Title: "Dependent on 1", BlockedBy: []int{1}},
			4: {Number: 4, Title: "Dependent on 2", BlockedBy: []int{2}},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{3, 4, 1, 2}, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	depLevel := map[int]int{}
	for _, issue := range resolved.Issues {
		if issue == 1 || issue == 2 {
			depLevel[issue] = 0
		} else if issue == 3 || issue == 4 {
			depLevel[issue] = 1
		}
	}
	if depLevel[1] != 0 || depLevel[2] != 0 || depLevel[3] != 1 || depLevel[4] != 1 {
		t.Fatalf("expected dependency levels {1:0, 2:0, 3:1, 4:1}, got %v", depLevel)
	}

	idx1 := -1
	idx2 := -1
	idx3 := -1
	idx4 := -1
	for i, issue := range resolved.Issues {
		switch issue {
		case 1:
			idx1 = i
		case 2:
			idx2 = i
		case 3:
			idx3 = i
		case 4:
			idx4 = i
		}
	}
	if idx1 > idx3 || idx2 > idx4 {
		t.Fatalf("expected dependents after blockers, got %v", resolved.Issues)
	}
}

func TestDependencyResolverResolve_PreservesRequestedOrderForIndependentIssues(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			3: {Number: 3, Title: "Third"},
			1: {Number: 1, Title: "First"},
			2: {Number: 2, Title: "Second"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{3, 1, 2}, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{3, 1, 2}) {
		t.Fatalf("expected requested order [3 1 2], got %v", resolved.Issues)
	}
}

func TestDependencyResolverResolve_OpenExternalBlockerMarkedAsBlocked(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "External open blocker"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{100}, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{100}) {
		t.Fatalf("expected issues [100], got %v", resolved.Issues)
	}

	if !reflect.DeepEqual(resolved.Blocked[100], []int{7}) {
		t.Fatalf("expected 100 blocked by [7], got %v", resolved.Blocked[100])
	}
}

func TestDependencyResolverResolve_ClosedBlockerNotInDeps(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42, 7}},
			42:  {Number: 42, Title: "Implemented blocker", State: "closed"},
			7:   {Number: 7, Title: "Open blocker"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{100}, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{100}) {
		t.Fatalf("expected issues [100], got %v", resolved.Issues)
	}

	if !reflect.DeepEqual(resolved.Blocked[100], []int{7}) {
		t.Fatalf("expected 100 blocked by [7] only (42 is closed), got %v", resolved.Blocked[100])
	}
}

func TestDependencyResolverResolve_MarksOpenExternalBlockersWithoutFallingOutOfBatch(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Runnable"},
			100: {Number: 100, Title: "Feature", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "External blocker"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{42, 100}, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{42, 100}) {
		t.Fatalf("expected mixed batch order [42 100], got %v", resolved.Issues)
	}

	wantBlocked := map[int][]int{
		100: {7},
	}
	if !reflect.DeepEqual(resolved.Blocked, wantBlocked) {
		t.Fatalf("expected blocked metadata %v, got %v", wantBlocked, resolved.Blocked)
	}
}

func TestDependencyResolverResolve_StillErrorsOnUnfetchableBlockers(t *testing.T) {
	client := &fetchIssueErrorClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{999}},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	_, err := resolver.Resolve(context.Background(), []int{100}, false, nil)
	if err == nil {
		t.Fatal("expected error for unfetchable blocker")
	}
	if err.Error() != "missing blockers: #999" {
		t.Fatalf("expected missing blocker error for #999, got %q", err)
	}
}

type fetchIssueErrorClient struct {
	issues map[int]*github.Issue
}

func (c *fetchIssueErrorClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	if number == 999 {
		return nil, errors.New("boom")
	}
	return c.issues[number], nil
}

func (c *fetchIssueErrorClient) FetchIssueDependencies(ctx context.Context, number int) ([]int, error) {
	if issue := c.issues[number]; issue != nil {
		return issue.BlockedBy, nil
	}
	return nil, nil
}

func (c *fetchIssueErrorClient) FetchPR(ctx context.Context, number int) (*github.PR, error) {
	return nil, nil
}

func (c *fetchIssueErrorClient) SearchIssues(ctx context.Context, query string) ([]github.Issue, error) {
	return nil, nil
}

func (c *fetchIssueErrorClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	return nil, nil
}

func (c *fetchIssueErrorClient) ListOpenPRs(ctx context.Context) ([]github.PR, error) {
	return nil, nil
}

func (c *fetchIssueErrorClient) ListPRComments(ctx context.Context, number int) ([]github.PRComment, error) {
	return nil, nil
}

func (c *fetchIssueErrorClient) AuthenticatedLogin(ctx context.Context) (string, error) {
	return "sandman", nil
}

func (c *fetchIssueErrorClient) ListIssueComments(ctx context.Context, number int) ([]github.IssueComment, error) {
	return nil, nil
}

func (c *fetchIssueErrorClient) ListSubIssues(ctx context.Context, parent int) ([]int, error) {
	return nil, nil
}

func (c *fetchIssueErrorClient) RepoName(ctx context.Context) (string, error) {
	return "owner/repo", nil
}

func (c *fetchIssueErrorClient) EditComment(ctx context.Context, commentID, body string) error {
	return nil
}

func (c *fetchIssueErrorClient) EditPRBody(ctx context.Context, prNumber int, body string) error {
	return nil
}

func (c *fetchIssueErrorClient) AddCommentReaction(ctx context.Context, commentID, content string) (string, error) {
	return "", nil
}

func (c *fetchIssueErrorClient) AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error) {
	return "", nil
}

func (c *fetchIssueErrorClient) RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error {
	return nil
}

func (c *fetchIssueErrorClient) RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error {
	return nil
}

func (c *fetchIssueErrorClient) CloseIssue(ctx context.Context, issueNumber int, comment string) error {
	return nil
}

func (c *fetchIssueErrorClient) ClosePR(ctx context.Context, prNumber int) error {
	return nil
}

func TestDependencyResolverResolve_IgnoresClosedBlockers(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42, 7}},
			42:  {Number: 42, Title: "Done blocker", State: "closed"},
			7:   {Number: 7, Title: "Open blocker"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{100}, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{7, 100}) {
		t.Fatalf("expected closed blocker to be ignored, got %v", resolved.Issues)
	}

	wantDeps := map[int][]int{
		7:   nil,
		100: {7},
	}
	if !reflect.DeepEqual(resolved.Deps, wantDeps) {
		t.Fatalf("expected deps %v, got %v", wantDeps, resolved.Deps)
	}
}

func TestDependencyResolverResolve_ExpandsTransitiveBlockers(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "Groundwork"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{100}, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{7, 42, 100}) {
		t.Fatalf("expected expanded topological order [7 42 100], got %v", resolved.Issues)
	}
}

func TestDependencyResolverResolve_DetectsCycles(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{100}},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	_, err := resolver.Resolve(context.Background(), []int{100}, true, nil)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if err.Error() != "dependency cycle detected: #100 -> #42 -> #100" {
		t.Fatalf("expected cycle path in error, got %q", err)
	}
}

func TestDependencyResolverResolve_DetectsCyclesWithClosedBlockersIgnored(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42, 7}},
			42:  {Number: 42, Title: "Closed blocker", State: "closed"},
			7:   {Number: 7, Title: "Refactor", BlockedBy: []int{100}},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	_, err := resolver.Resolve(context.Background(), []int{100}, true, nil)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if err.Error() != "dependency cycle detected: #100 -> #7 -> #100" {
		t.Fatalf("expected cycle path to ignore closed blocker, got %q", err)
	}
}

func TestDependencyResolverResolve_WarnsWhenExpansionGetsLarge(t *testing.T) {
	issues := make(map[int]*github.Issue, 51)
	for issue := 1; issue <= 51; issue++ {
		issues[issue] = &github.Issue{Number: issue, Title: "Issue"}
	}
	for issue := 2; issue <= 51; issue++ {
		issues[issue].BlockedBy = []int{issue - 1}
	}

	client := &fakeGitHubClient{issues: issues}
	var warnings bytes.Buffer

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &warnings

	resolved, err := resolver.Resolve(context.Background(), []int{51}, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resolved.Issues) != 51 {
		t.Fatalf("expected 51 resolved issues, got %d", len(resolved.Issues))
	}
	if !strings.Contains(warnings.String(), "warning: resolved batch expanded to 51 issues") {
		t.Fatalf("expected expansion warning, got %q", warnings.String())
	}
}

func TestDependencyResolverResolve_DoesNotWarnForLargeExplicitBatch(t *testing.T) {
	issues := make(map[int]*github.Issue, 51)
	requested := make([]int, 0, 51)
	for issue := 1; issue <= 51; issue++ {
		issues[issue] = &github.Issue{Number: issue, Title: "Issue"}
		requested = append(requested, issue)
	}

	client := &fakeGitHubClient{issues: issues}
	var warnings bytes.Buffer

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &warnings

	resolved, err := resolver.Resolve(context.Background(), requested, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resolved.Issues) != 51 {
		t.Fatalf("expected 51 resolved issues, got %d", len(resolved.Issues))
	}
	if warnings.Len() != 0 {
		t.Fatalf("expected no expansion warning, got %q", warnings.String())
	}
}

func TestDependencyResolverResolve_IgnoresSelfReferentialBlocker(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1222: {Number: 1222, Title: "Self-referential issue", BlockedBy: []int{1222}},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{1222}, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{1222}) {
		t.Fatalf("expected issues [1222], got %v", resolved.Issues)
	}

	if !reflect.DeepEqual(resolved.Deps[1222], []int(nil)) {
		t.Fatalf("expected no blockers for 1222, got %v", resolved.Deps[1222])
	}
}

// dependencyConcurrencyClient mirrors specificationConcurrencyClient
// and tracks the highest number of concurrent FetchIssue calls observed
// during a single Resolve invocation. The dep resolver fans out
// blocker fetches across a bounded worker pool (maxConcurrentFetches).
type dependencyConcurrencyClient struct {
	*fakeGitHubClient
	mu      sync.Mutex
	active  int
	max     int
	overlap int
	calls   map[int]int
	delay   time.Duration
}

func (c *dependencyConcurrencyClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	c.mu.Lock()
	c.calls[number]++
	c.active++
	if c.active > 1 && c.active > c.overlap {
		c.overlap = c.active
	}
	if c.active > c.max {
		c.max = c.active
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
	}()
	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.fakeGitHubClient.FetchIssue(ctx, number)
}

// TestDependencyResolver_ParallelizesBlockerFetches asserts that the
// dep resolver fans out blocker fetches instead of blocking on each in
// turn. Each requested issue carries [blocker_a, blocker_b] so the
// per-issue blocker fetch must run FetchIssue concurrently for those
// two siblings; the worker cap (4) bounds the observed overlap.
func TestDependencyResolver_ParallelizesBlockerFetches(t *testing.T) {
	const (
		requested        = 8
		blockersPerIssue = 2
	)
	issues := make(map[int]*github.Issue, requested)
	requestedList := make([]int, 0, requested)
	for n := 1; n <= requested; n++ {
		blockers := []int{1000 + 2*n, 1001 + 2*n}
		for _, b := range blockers {
			issues[b] = &github.Issue{Number: b, Title: "Blocker"}
		}
		issues[n] = &github.Issue{Number: n, Title: "Independent", BlockedBy: blockers}
		requestedList = append(requestedList, n)
	}
	client := &dependencyConcurrencyClient{
		fakeGitHubClient: &fakeGitHubClient{issues: issues},
		calls:            make(map[int]int),
		delay:            5 * time.Millisecond,
	}
	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}
	resolver.maxConcurrentFetches = 4

	resolved, err := resolver.Resolve(context.Background(), requestedList, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resolved.Issues) != requested {
		t.Fatalf("expected %d resolved issues, got %d", requested, len(resolved.Issues))
	}
	if client.max > 4 {
		t.Fatalf("expected at most 4 concurrent fetches, got %d", client.max)
	}
	if client.overlap == 0 {
		t.Fatalf("expected overlapping fetches, got 0 (resolver ran sequentially)")
	}
}

// TestDependencyResolver_SingleFlightRepeatedBlockers asserts that two
// issues sharing the same blocker result in exactly one FetchIssue for
// the shared blocker, even when the resolver walks the request set in
// parallel batches.
func TestDependencyResolver_SingleFlightRepeatedBlockers(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			200: {Number: 200, Title: "A", BlockedBy: []int{42}},
			201: {Number: 201, Title: "B", BlockedBy: []int{42}},
			202: {Number: 202, Title: "C", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Shared blocker"},
		},
	}
	tracker := &dependencyConcurrencyClient{
		fakeGitHubClient: client,
		calls:            make(map[int]int),
	}
	resolver := NewDependencyResolver(tracker)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{200, 201, 202}, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resolved.Issues) != 4 {
		t.Fatalf("expected 4 resolved issues (200,201,202 + shared 42), got %v", resolved.Issues)
	}
	if calls := tracker.calls[42]; calls != 1 {
		t.Fatalf("expected shared blocker #42 to be fetched once, got %d", calls)
	}
}

// TestDependencyResolver_ParentGateEdgesHoldBackRetainedSpec is the
// tracer bullet for the in-memory parent-gate edge: a parent (the
// retained Specification row) is held back by every accepted open
// child passed in parentChildren. The union with declared BlockedBy
// is empty here, so deps[1] is exactly the two open children and
// the topological order is [child1, child2, parent].
func TestDependencyResolver_ParentGateEdgesHoldBackRetainedSpec(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Retained parent"},
			10: {Number: 10, Title: "Child 1"},
			11: {Number: 11, Title: "Child 2"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{10, 11, 1}, true, map[int][]int{
		1: {10, 11},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{10, 11, 1}) {
		t.Fatalf("expected children before retained parent [10 11 1], got %v", resolved.Issues)
	}
	if !reflect.DeepEqual(resolved.Deps[1], []int{10, 11}) {
		t.Fatalf("expected parent #1 blocked by [10 11] in memory, got %v", resolved.Deps[1])
	}
	if len(resolved.Blocked) != 0 {
		t.Fatalf("expected no external blockers, got %v", resolved.Blocked)
	}
}

// TestDependencyResolver_ParentGateDropsClosedChildren pins the
// contract that a closed accepted child is filtered out of the
// parent's blocker set: the parent's gate only includes children
// that actually made it into the requested batch.
func TestDependencyResolver_ParentGateDropsClosedChildren(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Retained parent"},
			10: {Number: 10, Title: "Open child"},
			11: {Number: 11, Title: "Closed child", State: "closed"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{10, 1}, true, map[int][]int{
		1: {10, 11},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{10, 1}) {
		t.Fatalf("expected [10 1], got %v", resolved.Issues)
	}
	if !reflect.DeepEqual(resolved.Deps[1], []int{10}) {
		t.Fatalf("expected parent #1 blocked by open child [10] only, got %v", resolved.Deps[1])
	}
}

// TestDependencyResolver_ParentGateUnionsDeclaredBlockedBy pins
// that an existing declared BlockedBy on the parent is unioned
// with the synthetic child edges, deduplicated and sorted.
func TestDependencyResolver_ParentGateUnionsDeclaredBlockedBy(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Retained parent", BlockedBy: []int{99}},
			10: {Number: 10, Title: "Child"},
			11: {Number: 11, Title: "Sibling"},
			99: {Number: 99, Title: "External blocker"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{99, 10, 11, 1}, true, map[int][]int{
		1: {10, 11},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Deps[1], []int{10, 11, 99}) {
		t.Fatalf("expected parent #1 blocked by [10 11 99] (synthetic + declared, sorted), got %v", resolved.Deps[1])
	}
}

// TestDependencyResolver_ParentGateDeduplicatesTwoSpecsSharingChild
// pins the contract for two parents sharing a child: each parent
// retains a synthetic edge to the shared child, and the shared
// child is fetched and placed once in the topological order.
func TestDependencyResolver_ParentGateDeduplicatesTwoSpecsSharingChild(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Parent 1"},
			2:  {Number: 2, Title: "Parent 2"},
			10: {Number: 10, Title: "Shared child"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{10, 1, 2}, true, map[int][]int{
		1: {10},
		2: {10},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{10, 1, 2}) {
		t.Fatalf("expected shared child first, then both parents [10 1 2], got %v", resolved.Issues)
	}
	if !reflect.DeepEqual(resolved.Deps[1], []int{10}) {
		t.Fatalf("expected parent #1 blocked by [10], got %v", resolved.Deps[1])
	}
	if !reflect.DeepEqual(resolved.Deps[2], []int{10}) {
		t.Fatalf("expected parent #2 blocked by [10], got %v", resolved.Deps[2])
	}
}

// TestDependencyResolver_ParentGateNestedOrdering pins the
// nested-spec ordering: grandchild, middle, root, with each level
// gated by its own children.
func TestDependencyResolver_ParentGateNestedOrdering(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:   {Number: 1, Title: "Root"},
			10:  {Number: 10, Title: "Middle"},
			100: {Number: 100, Title: "Leaf"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{100, 10, 1}, true, map[int][]int{
		1:  {10},
		10: {100},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{100, 10, 1}) {
		t.Fatalf("expected nested ordering [100 10 1], got %v", resolved.Issues)
	}
	if !reflect.DeepEqual(resolved.Deps[10], []int{100}) {
		t.Fatalf("expected middle #10 blocked by [100], got %v", resolved.Deps[10])
	}
	if !reflect.DeepEqual(resolved.Deps[1], []int{10}) {
		t.Fatalf("expected root #1 blocked by [10], got %v", resolved.Deps[1])
	}
}

// TestDependencyResolver_ParentGateIgnoresSyntheticChildNotInBatch
// pins the contract that a synthetic child listed in
// parentChildren but absent from the requested batch is silently
// skipped (no missing-blocker error). This is the safety valve for
// closed-issue filtering dropping a child before the dep resolver
// runs.
func TestDependencyResolver_ParentGateIgnoresSyntheticChildNotInBatch(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Parent"},
			10: {Number: 10, Title: "Child"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{10, 1}, true, map[int][]int{
		1: {10, 999},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Deps[1], []int{10}) {
		t.Fatalf("expected parent #1 blocked by [10] only (999 is not in batch), got %v", resolved.Deps[1])
	}
}

// TestDependencyResolver_NoParentGateKeepsLegacyBehaviour pins
// that callers passing nil for parentChildren get the same deps
// graph as before — the change is purely additive.
func TestDependencyResolver_NoParentGateKeepsLegacyBehaviour(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Issue", BlockedBy: []int{42}},
			42: {Number: 42, Title: "Blocker"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{1}, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Deps[1], []int{42}) {
		t.Fatalf("expected declared BlockedBy only [42], got %v", resolved.Deps[1])
	}
}
