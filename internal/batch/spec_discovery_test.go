package batch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
)

// discoveryClient is a fake github.Client used by the open-issue scan
// tests. It implements the full Client interface plus the two
// optional interfaces (OpenIssueLister, IssueCommentPoster) that the
// new harvest step type-asserts against. Existing fakes in
// spec_test.go / orchestrator_test.go do not implement those optional
// interfaces, which is what keeps them unchanged: the new step
// silently no-ops on them because the type assertion fails.
//
// The fake's tracking fields capture every observation the resolver
// makes so tests can assert on call counts, arguments, and ordering.
type discoveryClient struct {
	mu sync.Mutex

	// Spec issue returned by FetchIssue for the parent number.
	specIssue *github.Issue
	// Sub-issues returned by ListSubIssues for the parent.
	subIssues []int
	// Comments returned by ListIssueComments for the parent.
	specComments []github.IssueComment
	// Search results returned by SearchIssues.
	searchResults []github.Issue

	// Open issues returned by ListOpenIssues.
	openIssues []github.Issue
	// Posted comment bodies, in order.
	postedComments []postedComment
	// Forced error returned by PostIssueComment on the next call.
	forcePostError error

	// Call counters / records.
	listOpenIssuesCalls    int
	postIssueCommentCalls  int
	listIssueCommentsCalls int
	listSubIssuesCalls     int
	searchIssuesCalls      int
	fetchIssueCalls        map[int]int
}

type postedComment struct {
	issueNumber int
	body        string
}

func newDiscoveryClient() *discoveryClient {
	return &discoveryClient{
		fetchIssueCalls: make(map[int]int),
	}
}

// --- Client interface methods ---

func (c *discoveryClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetchIssueCalls[number]++
	if c.specIssue != nil && c.specIssue.Number == number {
		return c.specIssue, nil
	}
	// Look for an open issue with the same number — used to simulate
	// FetchIssue on child candidates during the verifier pass.
	for i := range c.openIssues {
		if c.openIssues[i].Number == number {
			issue := c.openIssues[i]
			return &issue, nil
		}
	}
	return nil, fmt.Errorf("issue #%d not found", number)
}

func (c *discoveryClient) FetchIssueDependencies(ctx context.Context, number int) ([]int, error) {
	return nil, nil
}

func (c *discoveryClient) FetchPR(ctx context.Context, number int) (*github.PR, error) {
	return nil, fmt.Errorf("not a PR")
}

func (c *discoveryClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	return nil, fmt.Errorf("no PR")
}

func (c *discoveryClient) SearchIssues(ctx context.Context, query string) ([]github.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.searchIssuesCalls++
	out := make([]github.Issue, len(c.searchResults))
	copy(out, c.searchResults)
	return out, nil
}

func (c *discoveryClient) ListOpenPRs(ctx context.Context) ([]github.PR, error) {
	return nil, nil
}

func (c *discoveryClient) ListPRComments(ctx context.Context, number int) ([]github.PRComment, error) {
	return nil, nil
}

func (c *discoveryClient) AuthenticatedLogin(ctx context.Context) (string, error) {
	return "test-user", nil
}

func (c *discoveryClient) ListIssueComments(ctx context.Context, number int) ([]github.IssueComment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listIssueCommentsCalls++
	out := make([]github.IssueComment, len(c.specComments))
	copy(out, c.specComments)
	return out, nil
}

func (c *discoveryClient) ListSubIssues(ctx context.Context, parent int) ([]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listSubIssuesCalls++
	out := make([]int, len(c.subIssues))
	copy(out, c.subIssues)
	return out, nil
}

func (c *discoveryClient) RepoName(ctx context.Context) (string, error) {
	return "owner/repo", nil
}

func (c *discoveryClient) EditComment(ctx context.Context, commentID, body string) error {
	return nil
}

func (c *discoveryClient) EditPRBody(ctx context.Context, prNumber int, body string) error {
	return nil
}

func (c *discoveryClient) AddCommentReaction(ctx context.Context, commentID, content string) (string, error) {
	return "1", nil
}

func (c *discoveryClient) AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error) {
	return "1", nil
}

func (c *discoveryClient) RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error {
	return nil
}

func (c *discoveryClient) RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error {
	return nil
}

func (c *discoveryClient) CloseIssue(ctx context.Context, issueNumber int, comment string) error {
	return nil
}

func (c *discoveryClient) ClosePR(ctx context.Context, prNumber int) error {
	return nil
}

// --- Optional interfaces ---

func (c *discoveryClient) ListOpenIssues(ctx context.Context) ([]github.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listOpenIssuesCalls++
	out := make([]github.Issue, len(c.openIssues))
	copy(out, c.openIssues)
	return out, nil
}

func (c *discoveryClient) PostIssueComment(ctx context.Context, issueNumber int, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.postIssueCommentCalls++
	if c.forcePostError != nil {
		err := c.forcePostError
		c.forcePostError = nil
		return err
	}
	c.postedComments = append(c.postedComments, postedComment{issueNumber: issueNumber, body: body})
	return nil
}

// --- Tests ---

// TestBuildDiscoveredChildrenComment pins the comment-body format:
// hidden HTML marker followed by a `## Discovered children` H2
// section with `- #N` bullets sorted ascending. The format is what
// the existing comment harvest (github.ParseChildrenFromBody) recognises on
// subsequent runs, so the expensive open-issue scan is short-circuited
// for any spec whose marker comment is intact.
func TestBuildDiscoveredChildrenComment(t *testing.T) {
	t.Parallel()
	got := buildDiscoveredChildrenComment([]int{103, 101, 102})
	want := "<!-- sandman-discovered-children -->\n\n## Discovered children\n\n- #101\n- #102\n- #103\n"
	if got != want {
		t.Errorf("unexpected comment body:\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestOpenIssueScan_FiresWhenCheaperSourcesEmpty reproduces the
// threeterm-#58-style gap: spec body is silent, no comments, no
// native sub-issues, search returns nothing, but two open issues in
// the repo carry `## Parent` citations of the spec. The scan must
// find them, accept them via the verifier, and post the marker
// comment exactly once.
func TestOpenIssueScan_FiresWhenCheaperSourcesEmpty(t *testing.T) {
	t.Parallel()
	spec := &github.Issue{
		Number: 58,
		Title:  "umbrella spec",
		Body:   "## Problem Statement\n\nUmbrella for v2.\n",
	}
	client := newDiscoveryClient()
	client.specIssue = spec
	client.openIssues = []github.Issue{
		{
			Number: 232,
			Body:   "## Parent\n\n#58\n",
		},
		{
			Number: 234,
			Body:   "## Parent\n\n#58\n",
		},
		// A non-matching open issue — must be ignored.
		{
			Number: 999,
			Body:   "## Parent\n\n#999\n",
		},
	}

	resolver := &SpecificationResolver{
		client:        client,
		warningWriter: io.Discard,
	}
	got := resolver.collectCandidates(context.Background(), 58, spec.Body, nil)
	wantSet := map[int]bool{232: true, 234: true}
	if len(got) != len(wantSet) {
		t.Fatalf("expected %d candidates, got %d: %v", len(wantSet), len(got), got)
	}
	for _, n := range got {
		if !wantSet[n] {
			t.Errorf("unexpected candidate #%d in %v", n, got)
		}
	}
	if client.listOpenIssuesCalls != 1 {
		t.Errorf("expected exactly 1 ListOpenIssues call, got %d", client.listOpenIssuesCalls)
	}
	if client.postIssueCommentCalls != 1 {
		t.Fatalf("expected exactly 1 PostIssueComment call, got %d", client.postIssueCommentCalls)
	}
	posted := client.postedComments[0]
	if posted.issueNumber != 58 {
		t.Errorf("expected comment on issue 58, got %d", posted.issueNumber)
	}
	if !strings.Contains(posted.body, discoveredChildrenMarker) {
		t.Errorf("posted comment missing marker %q; got:\n%s", discoveredChildrenMarker, posted.body)
	}
	if !strings.Contains(posted.body, "## Discovered children") {
		t.Errorf("posted comment missing heading; got:\n%s", posted.body)
	}
	for _, n := range []string{"- #232", "- #234"} {
		if !strings.Contains(posted.body, n) {
			t.Errorf("posted comment missing bullet %s; got:\n%s", n, posted.body)
		}
	}
	if strings.Contains(posted.body, "- #999") {
		t.Errorf("posted comment should not contain non-matching issue #999; got:\n%s", posted.body)
	}
}

// TestOpenIssueScan_SkippedWhenCheaperSourcesReturnCandidates
// verifies the gating: when the spec body has a child reference (or
// any cheaper source fires), the expensive open-issue scan is not
// invoked. This is the same gate the existing mention-search fallback
// uses (`len(order) == 0` after the cheaper sources).
func TestOpenIssueScan_SkippedWhenCheaperSourcesReturnCandidates(t *testing.T) {
	t.Parallel()
	spec := &github.Issue{
		Number: 100,
		Body:   "## Children\n\n- #101\n",
	}
	client := newDiscoveryClient()
	client.specIssue = spec
	// Even with open issues ready, the scan should not fire because
	// the body already declared #101 as a child.
	client.openIssues = []github.Issue{
		{Number: 200, Body: "## Parent\n\n#100\n"},
	}

	resolver := &SpecificationResolver{
		client:        client,
		warningWriter: io.Discard,
	}
	got := resolver.collectCandidates(context.Background(), 100, spec.Body, nil)
	if len(got) != 1 || got[0] != 101 {
		t.Fatalf("expected [101] from body, got %v", got)
	}
	if client.listOpenIssuesCalls != 0 {
		t.Errorf("expected 0 ListOpenIssues calls when cheaper sources fired, got %d", client.listOpenIssuesCalls)
	}
	if client.postIssueCommentCalls != 0 {
		t.Errorf("expected 0 PostIssueComment calls when cheaper sources fired, got %d", client.postIssueCommentCalls)
	}
}

// TestOpenIssueScan_FilterUsesBroadenedParentMatcher verifies the
// scan reuses HasParentSectionBacklinkTo from ADR-0042: a candidate
// whose body uses `## Parent area` (substring match, not the strict
// `^## Parent$` literal) is harvested. Without the broadened matcher
// the candidate would be invisible to the scan.
func TestOpenIssueScan_FilterUsesBroadenedParentMatcher(t *testing.T) {
	t.Parallel()
	spec := &github.Issue{
		Number: 500,
		Body:   "## Problem Statement\n\nUmbrella for the v2 effort.\n",
	}
	client := newDiscoveryClient()
	client.specIssue = spec
	client.openIssues = []github.Issue{
		{Number: 501, Body: "## Parent area\n\n#500\n"},
		{Number: 502, Body: "## parent\n\n#500\n"},  // case-insensitive
		{Number: 503, Body: "### Parent\n\n#500\n"}, // H3 — must NOT match (broadened matcher is H2 only)
	}

	resolver := &SpecificationResolver{
		client:        client,
		warningWriter: io.Discard,
	}
	got := resolver.collectCandidates(context.Background(), 500, spec.Body, nil)
	gotSet := map[int]bool{}
	for _, n := range got {
		gotSet[n] = true
	}
	if !gotSet[501] {
		t.Errorf("expected `## Parent area` candidate #501 to be accepted; got %v", got)
	}
	if !gotSet[502] {
		t.Errorf("expected lowercase `## parent` candidate #502 to be accepted; got %v", got)
	}
	if gotSet[503] {
		t.Errorf("expected H3 `### Parent` candidate #503 to be REJECTED; got %v", got)
	}
}

// TestOpenIssueScan_FilterAcceptsMultiRefParentSection reproduces
// the threeterm umbrella pattern: a candidate's `## Parent area`
// section cites both its intermediate parent area AND the umbrella
// spec. The scan must accept it when the spec number is in the
// reference set.
func TestOpenIssueScan_FilterAcceptsMultiRefParentSection(t *testing.T) {
	t.Parallel()
	spec := &github.Issue{
		Number: 58,
		Body:   "## Problem Statement\n\nUmbrella.\n",
	}
	client := newDiscoveryClient()
	client.specIssue = spec
	client.openIssues = []github.Issue{
		{Number: 232, Body: "## Parent area\n\n#59\n#58\n"},
	}

	resolver := &SpecificationResolver{
		client:        client,
		warningWriter: io.Discard,
	}
	got := resolver.collectCandidates(context.Background(), 58, spec.Body, nil)
	if len(got) != 1 || got[0] != 232 {
		t.Fatalf("expected multi-ref candidate #232 accepted; got %v", got)
	}
}

// TestOpenIssueScan_ExcludesSpecItself guards against the scan
// harvesting the spec issue as its own child. The resolver's `add`
// closure already filters by `n == parent`, but the scan helper
// short-circuits earlier for clarity.
func TestOpenIssueScan_ExcludesSpecItself(t *testing.T) {
	t.Parallel()
	spec := &github.Issue{
		Number: 100,
		Body:   "## Problem Statement\n\nSelf-referential.\n",
	}
	client := newDiscoveryClient()
	client.specIssue = spec
	// The spec itself appears in the open-issue list (the gh CLI
	// returns it because it is open). It must be excluded.
	client.openIssues = []github.Issue{
		{Number: 100, Body: "## Parent\n\n#100\n"},
		{Number: 101, Body: "## Parent\n\n#100\n"},
	}

	resolver := &SpecificationResolver{
		client:        client,
		warningWriter: io.Discard,
	}
	got := resolver.collectCandidates(context.Background(), 100, spec.Body, nil)
	for _, n := range got {
		if n == 100 {
			t.Errorf("spec #100 must not be harvested as its own child; got %v", got)
		}
	}
	if len(got) != 1 || got[0] != 101 {
		t.Fatalf("expected exactly [#101]; got %v", got)
	}
}

// TestOpenIssueScan_IdempotentMarkerComment pins the idempotency
// contract: when the marker comment is already on the spec, the
// second run does not post a duplicate comment. Operators force a
// re-scan by deleting the marker.
func TestOpenIssueScan_IdempotentMarkerComment(t *testing.T) {
	t.Parallel()
	spec := &github.Issue{
		Number: 58,
		Body:   "## Problem Statement\n\nUmbrella.\n",
	}
	client := newDiscoveryClient()
	client.specIssue = spec
	client.openIssues = []github.Issue{
		{Number: 232, Body: "## Parent\n\n#58\n"},
	}
	// Pre-existing marker comment — second run sees this and skips.
	client.specComments = []github.IssueComment{
		{
			Body: "<!-- sandman-discovered-children -->\n\n## Discovered children\n\n- #232\n",
		},
	}

	resolver := &SpecificationResolver{
		client:        client,
		warningWriter: io.Discard,
	}
	got := resolver.collectCandidates(context.Background(), 58, spec.Body, nil)
	if len(got) != 1 || got[0] != 232 {
		t.Fatalf("expected [#232] from scan; got %v", got)
	}
	if client.postIssueCommentCalls != 0 {
		t.Errorf("expected 0 PostIssueComment calls when marker exists, got %d", client.postIssueCommentCalls)
	}
}

// TestOpenIssueScan_PostFailureDoesNotAbortResolver verifies that a
// failed PostIssueComment is logged but does not abort the resolver:
// the candidates harvested in memory are still added to the accept
// set and the operator sees the warning.
func TestOpenIssueScan_PostFailureDoesNotAbortResolver(t *testing.T) {
	t.Parallel()
	spec := &github.Issue{
		Number: 58,
		Body:   "## Problem Statement\n\nUmbrella.\n",
	}
	client := newDiscoveryClient()
	client.specIssue = spec
	client.openIssues = []github.Issue{
		{Number: 232, Body: "## Parent\n\n#58\n"},
	}
	client.forcePostError = errors.New("simulated post failure")

	var warning bytes.Buffer
	resolver := &SpecificationResolver{
		client:        client,
		warningWriter: &warning,
	}
	got := resolver.collectCandidates(context.Background(), 58, spec.Body, nil)
	if len(got) != 1 || got[0] != 232 {
		t.Fatalf("expected [#232] despite post failure; got %v", got)
	}
	if client.postIssueCommentCalls != 1 {
		t.Errorf("expected exactly 1 PostIssueComment call (the failed one), got %d", client.postIssueCommentCalls)
	}
	if !strings.Contains(warning.String(), "simulated post failure") {
		t.Errorf("expected failure logged to warningWriter; got %q", warning.String())
	}
}

// TestOpenIssueScan_NoOpWhenClientLacksOptionalInterfaces mirrors
// the existing test fakes that do not implement OpenIssueLister /
// IssueCommentPoster. The resolver must silently skip the new step
// so those tests keep passing without modification.
func TestOpenIssueScan_NoOpWhenClientLacksOptionalInterfaces(t *testing.T) {
	t.Parallel()
	spec := &github.Issue{
		Number: 58,
		Body:   "## Problem Statement\n\nUmbrella.\n",
	}
	// bareClient satisfies only the Client interface; no
	// ListOpenIssues / PostIssueComment methods.
	bare := &bareClient{specIssue: spec}

	resolver := &SpecificationResolver{
		client:        bare,
		warningWriter: io.Discard,
	}
	got := resolver.collectCandidates(context.Background(), 58, spec.Body, nil)
	if len(got) != 0 {
		t.Errorf("expected no candidates from bare client; got %v", got)
	}
}

// bareClient is the minimal Client implementation needed to prove
// the new harvest step is a no-op when the optional interfaces are
// absent. Only the methods collectCandidates actually invokes are
// non-trivial; the rest return zero values so the fake satisfies
// the interface without exercising those code paths.
type bareClient struct {
	specIssue *github.Issue
}

func (b *bareClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	if b.specIssue != nil && b.specIssue.Number == number {
		return b.specIssue, nil
	}
	return nil, fmt.Errorf("issue #%d not found", number)
}

func (b *bareClient) FetchIssueDependencies(ctx context.Context, number int) ([]int, error) {
	return nil, nil
}

func (b *bareClient) FetchPR(ctx context.Context, number int) (*github.PR, error) {
	return nil, nil
}

func (b *bareClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	return nil, nil
}

func (b *bareClient) SearchIssues(ctx context.Context, query string) ([]github.Issue, error) {
	return nil, nil
}

func (b *bareClient) ListOpenPRs(ctx context.Context) ([]github.PR, error) {
	return nil, nil
}

func (b *bareClient) ListPRComments(ctx context.Context, number int) ([]github.PRComment, error) {
	return nil, nil
}

func (b *bareClient) AuthenticatedLogin(ctx context.Context) (string, error) {
	return "", nil
}

func (b *bareClient) ListIssueComments(ctx context.Context, number int) ([]github.IssueComment, error) {
	return nil, nil
}

func (b *bareClient) ListSubIssues(ctx context.Context, parent int) ([]int, error) {
	return nil, nil
}

func (b *bareClient) RepoName(ctx context.Context) (string, error) {
	return "owner/repo", nil
}

func (b *bareClient) EditComment(ctx context.Context, commentID, body string) error {
	return nil
}

func (b *bareClient) EditPRBody(ctx context.Context, prNumber int, body string) error {
	return nil
}

func (b *bareClient) AddCommentReaction(ctx context.Context, commentID, content string) (string, error) {
	return "", nil
}

func (b *bareClient) AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error) {
	return "", nil
}

func (b *bareClient) RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error {
	return nil
}

func (b *bareClient) RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error {
	return nil
}

func (b *bareClient) CloseIssue(ctx context.Context, issueNumber int, comment string) error {
	return nil
}

func (b *bareClient) ClosePR(ctx context.Context, prNumber int) error {
	return nil
}
