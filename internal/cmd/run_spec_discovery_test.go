package cmd

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/github"
)

// discoveredChildrenMarker is the hidden marker ADR-0044 writes on a
// Specification to persist the children its open-issue scan discovered.
const discoveredChildrenMarker = "<!-- sandman-discovered-children -->"

// coldSpecificationBody carries the canonical Specification shape but no
// child signal: no child section, no issue references outside a Parent
// section. Only the open-issue scan (ADR-0044) can discover its children.
const coldSpecificationBody = "## Problem Statement\n\nImported tickets keep their relationship on the child side only.\n\n## Solution\n\nDiscover the children from their Parent sections.\n"

// specDiscoveryGitHubClient is the GitHub boundary for Specification
// discovery through `sandman run`. It extends fakeGitHubClient with
// per-issue comments that reflect its own posts, native sub-issues,
// mention-search results kept apart from the repo-wide `is:open` search,
// and the optional ADR-0044 capabilities (open-issue listing and issue
// comment posting) that the production GitHub client provides.
type specDiscoveryGitHubClient struct {
	*fakeGitHubClient

	state     sync.Mutex
	comments  map[int][]github.IssueComment
	subIssues map[int][]int
	mentions  map[int][]github.Issue
	listings  int
	listErr   error
	posts     []postedIssueComment
	// postErr fails every post; with postErrStores the comment still
	// lands, like a gh timeout that fires after GitHub stored it.
	postErr       error
	postErrStores bool
}

type postedIssueComment struct {
	issue int
	body  string
}

func newSpecDiscoveryGitHub(issues map[int]*github.Issue) *specDiscoveryGitHubClient {
	return &specDiscoveryGitHubClient{
		fakeGitHubClient: &fakeGitHubClient{issues: issues},
		comments:         make(map[int][]github.IssueComment),
		subIssues:        make(map[int][]int),
		mentions:         make(map[int][]github.Issue),
	}
}

// newColdSpecificationGitHub seeds cold Specification #58, its open
// children #232 and #234 (whose Parent sections cite #58), and an open
// decoy whose Parent section cites another issue.
func newColdSpecificationGitHub() *specDiscoveryGitHubClient {
	return newSpecDiscoveryGitHub(map[int]*github.Issue{
		58:  {Number: 58, State: "open", Title: "Cold Specification", Body: coldSpecificationBody},
		232: {Number: 232, State: "open", Title: "Child 232", Body: "## Parent\n\n#58\n"},
		234: {Number: 234, State: "open", Title: "Child 234", Body: "## Parent\n\n#58\n"},
		999: {Number: 999, State: "open", Title: "Decoy", Body: "## Parent\n\n#900\n"},
	})
}

// SearchIssues answers the resolver's `issues/<n>` mention search from
// its own table, so the embedded fake's `is:open` answer (every open
// issue) cannot masquerade as mention results.
func (c *specDiscoveryGitHubClient) SearchIssues(ctx context.Context, query string) ([]github.Issue, error) {
	if rest, ok := strings.CutPrefix(query, "issues/"); ok {
		number, err := strconv.Atoi(rest)
		if err != nil {
			return nil, err
		}
		c.state.Lock()
		defer c.state.Unlock()
		return append([]github.Issue(nil), c.mentions[number]...), nil
	}
	return c.fakeGitHubClient.SearchIssues(ctx, query)
}

func (c *specDiscoveryGitHubClient) ListIssueComments(ctx context.Context, number int) ([]github.IssueComment, error) {
	c.state.Lock()
	defer c.state.Unlock()
	return append([]github.IssueComment(nil), c.comments[number]...), nil
}

func (c *specDiscoveryGitHubClient) ListSubIssues(ctx context.Context, parent int) ([]int, error) {
	c.state.Lock()
	defer c.state.Unlock()
	return append([]int(nil), c.subIssues[parent]...), nil
}

// ListOpenIssues lists every open issue in ascending number order, like
// the production client.
func (c *specDiscoveryGitHubClient) ListOpenIssues(ctx context.Context) ([]github.Issue, error) {
	c.state.Lock()
	c.listings++
	listErr := c.listErr
	c.state.Unlock()
	if listErr != nil {
		return nil, listErr
	}

	c.fakeGitHubClient.mu.Lock()
	defer c.fakeGitHubClient.mu.Unlock()
	var open []github.Issue
	for _, issue := range c.issues {
		if !github.IsIssueClosed(issue) {
			open = append(open, *issue)
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].Number < open[j].Number })
	return open, nil
}

// PostIssueComment records every post attempt. The comment lands on the
// Issue unless the post fails before GitHub stored it.
func (c *specDiscoveryGitHubClient) PostIssueComment(ctx context.Context, issueNumber int, body string) error {
	c.state.Lock()
	defer c.state.Unlock()
	c.posts = append(c.posts, postedIssueComment{issue: issueNumber, body: body})
	if c.postErr == nil || c.postErrStores {
		c.comments[issueNumber] = append(c.comments[issueNumber], github.IssueComment{Body: body})
	}
	return c.postErr
}

func executeRunWithGitHub(t *testing.T, gh github.Client, args ...string) (*spyBatchRunner, string) {
	t.Helper()
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh

	var stderr bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sandman run %v: %v (stderr: %q)", args, err, stderr.String())
	}
	if !spy.called {
		t.Fatalf("sandman run %v: expected batch runner to be called", args)
	}
	return spy, stderr.String()
}

func TestRun_ColdSpecificationKeepsDiscoveredChildrenInBatch(t *testing.T) {
	gh := newColdSpecificationGitHub()

	spy, stderr := executeRunWithGitHub(t, gh, "58")

	if want := []int{232, 234, 58}; !slices.Equal(spy.req.Issues, want) {
		t.Fatalf("Batch issues = %v, want %v (stderr: %q)", spy.req.Issues, want, stderr)
	}
	if want := []int{232, 234}; !slices.Equal(spy.req.Dependencies[58], want) {
		t.Errorf("Specification #58 gated on %v, want %v", spy.req.Dependencies[58], want)
	}
	if !strings.Contains(stderr, "expanded specification #58 to 2 accepted children") {
		t.Errorf("expected expansion log line, got: %q", stderr)
	}
}

func TestRun_ColdSpecificationPersistsDiscoveredChildrenMarker(t *testing.T) {
	gh := newColdSpecificationGitHub()

	executeRunWithGitHub(t, gh, "58")

	if len(gh.posts) != 1 {
		t.Fatalf("expected exactly one discovered-children comment, got %d: %+v", len(gh.posts), gh.posts)
	}
	posted := gh.posts[0]
	if posted.issue != 58 {
		t.Errorf("marker posted on #%d, want #58", posted.issue)
	}
	if !strings.Contains(posted.body, discoveredChildrenMarker) {
		t.Errorf("posted comment lacks the discovered-children marker:\n%s", posted.body)
	}
	if got, want := github.ParseChildrenFromBody(posted.body), []int{232, 234}; !slices.Equal(got, want) {
		t.Errorf("posted comment lists children %v, want %v:\n%s", got, want, posted.body)
	}
}

func TestRun_PersistedMarkerSkipsScanOnNextRun(t *testing.T) {
	gh := newColdSpecificationGitHub()
	executeRunWithGitHub(t, gh, "58")

	spy, stderr := executeRunWithGitHub(t, gh, "58")

	if want := []int{232, 234, 58}; !slices.Equal(spy.req.Issues, want) {
		t.Fatalf("second run Batch issues = %v, want %v (stderr: %q)", spy.req.Issues, want, stderr)
	}
	if gh.listings != 1 {
		t.Errorf("expected the second run to reuse the marker instead of listing open issues; listings = %d", gh.listings)
	}
	if len(gh.posts) != 1 {
		t.Errorf("expected the persisted marker to prevent a second post; posts = %+v", gh.posts)
	}
}

func TestRun_ExistingMarkerPreventsSecondPost(t *testing.T) {
	// The operator curated every bullet out of the marker comment, so the
	// comment harvest finds no children. Whatever the resolver harvests
	// next, the intact marker must stop a second post (ADR-0044 §3).
	gh := newColdSpecificationGitHub()
	gh.comments[58] = []github.IssueComment{{Body: discoveredChildrenMarker + "\n\n## Discovered children\n\n"}}

	executeRunWithGitHub(t, gh, "58")

	if len(gh.posts) != 0 {
		t.Errorf("expected the existing marker to prevent a post, got %+v", gh.posts)
	}
}

// newNestedColdSpecificationGitHub seeds Specification #58, which lists
// cold Specification #60 as its child, and open #61 whose Parent section
// cites #60. Running `sandman run 58 60` expands #60 twice in one
// command: nested inside #58, then as a typed input.
func newNestedColdSpecificationGitHub() *specDiscoveryGitHubClient {
	return newSpecDiscoveryGitHub(map[int]*github.Issue{
		58: {Number: 58, State: "open", Title: "Outer Specification", Body: "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Child Issues\n\n- #60\n"},
		60: {Number: 60, State: "open", Title: "Cold nested Specification", Body: "## Parent\n\n#58\n\n" + coldSpecificationBody},
		61: {Number: 61, State: "open", Title: "Child 61", Body: "## Parent\n\n#60\n"},
	})
}

func TestRun_ReexpandedColdSpecificationPostsOneMarkerPerCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		postErr error
	}{
		{name: "post succeeds"},
		{name: "post fails after GitHub stored the comment", postErr: errors.New("gh issue comment: context deadline exceeded")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newNestedColdSpecificationGitHub()
			gh.postErr = tc.postErr
			gh.postErrStores = true

			spy, stderr := executeRunWithGitHub(t, gh, "58", "60")

			for _, n := range []int{61, 60, 58} {
				if !slices.Contains(spy.req.Issues, n) {
					t.Fatalf("expected #%d in Batch issues %v (stderr: %q)", n, spy.req.Issues, stderr)
				}
			}
			if want := []int{61}; !slices.Equal(spy.req.Dependencies[60], want) {
				t.Errorf("Specification #60 gated on %v, want %v", spy.req.Dependencies[60], want)
			}
			var attempts int
			for _, post := range gh.posts {
				if post.issue == 60 {
					attempts++
				}
			}
			if attempts != 1 {
				t.Errorf("expected one marker post attempt on #60 within one command, got %d: %+v", attempts, gh.posts)
			}
		})
	}
}

func TestRun_OpenIssueListingFailureIsReported(t *testing.T) {
	gh := newColdSpecificationGitHub()
	gh.listErr = errors.New("gh api issues: HTTP 502")

	spy, stderr := executeRunWithGitHub(t, gh, "58")

	if want := []int{58}; !slices.Equal(spy.req.Issues, want) {
		t.Fatalf("Batch issues = %v, want %v (stderr: %q)", spy.req.Issues, want, stderr)
	}
	if !strings.Contains(stderr, "open-issue scan for specification #58 failed: gh api issues: HTTP 502") {
		t.Errorf("expected the listing failure to be reported, got: %q", stderr)
	}
	if len(gh.posts) != 0 {
		t.Errorf("expected no marker after a failed scan, got %+v", gh.posts)
	}
}

func TestRun_MarkerPostFailureIsReportedAndChildrenStayInBatch(t *testing.T) {
	gh := newColdSpecificationGitHub()
	gh.postErr = errors.New("gh issue comment: HTTP 403")

	spy, stderr := executeRunWithGitHub(t, gh, "58")

	if want := []int{232, 234, 58}; !slices.Equal(spy.req.Issues, want) {
		t.Fatalf("Batch issues = %v, want %v (stderr: %q)", spy.req.Issues, want, stderr)
	}
	if !strings.Contains(stderr, "could not post discovered-children comment for specification #58: gh issue comment: HTTP 403") {
		t.Errorf("expected the post failure to be reported, got: %q", stderr)
	}
	if len(gh.posts) != 1 {
		t.Errorf("expected exactly one post attempt, got %+v", gh.posts)
	}
}

func TestRun_CheaperSpecificationSourcesKeepScanSkipped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		specBody string
		seed     func(gh *specDiscoveryGitHubClient)
	}{
		{
			name:     "body child section",
			specBody: coldSpecificationBody + "\n## Child Issues\n\n- #10\n",
		},
		{
			name:     "structured comment children",
			specBody: coldSpecificationBody,
			seed: func(gh *specDiscoveryGitHubClient) {
				gh.comments[58] = []github.IssueComment{{Body: "## Children\n\n- #10\n"}}
			},
		},
		{
			name:     "native sub-issues",
			specBody: coldSpecificationBody,
			seed: func(gh *specDiscoveryGitHubClient) {
				gh.subIssues[58] = []int{10}
			},
		},
		{
			name:     "mention search",
			specBody: coldSpecificationBody,
			seed: func(gh *specDiscoveryGitHubClient) {
				gh.mentions[58] = []github.Issue{{Number: 10, State: "open", Title: "Child 10", Body: "## Parent\n\n#58\n"}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// #11 also backlinks #58 but only the open-issue scan could
			// find it, so it must stay out of the Batch.
			gh := newSpecDiscoveryGitHub(map[int]*github.Issue{
				58: {Number: 58, State: "open", Title: "Specification", Body: tc.specBody},
				10: {Number: 10, State: "open", Title: "Child 10", Body: "## Parent\n\n#58\n"},
				11: {Number: 11, State: "open", Title: "Scan-only child", Body: "## Parent\n\n#58\n"},
			})
			if tc.seed != nil {
				tc.seed(gh)
			}

			spy, stderr := executeRunWithGitHub(t, gh, "58")

			if want := []int{10, 58}; !slices.Equal(spy.req.Issues, want) {
				t.Fatalf("Batch issues = %v, want %v (stderr: %q)", spy.req.Issues, want, stderr)
			}
			if want := []int{10}; !slices.Equal(spy.req.Dependencies[58], want) {
				t.Errorf("Specification #58 gated on %v, want %v", spy.req.Dependencies[58], want)
			}
			if gh.listings != 0 {
				t.Errorf("expected the open-issue scan to stay skipped, got %d listings", gh.listings)
			}
			if len(gh.posts) != 0 {
				t.Errorf("expected no discovered-children comment, got %+v", gh.posts)
			}
		})
	}
}

func TestRun_ColdSpecificationWithoutDiscoveryCapabilitiesRunsAlone(t *testing.T) {
	// The plain fake lacks the optional capabilities. Seeding only #58
	// keeps its query-blind search from feeding the mention fallback, so
	// the resolver reaches the open-issue scan.
	gh := &fakeGitHubClient{issues: map[int]*github.Issue{
		58: {Number: 58, State: "open", Title: "Cold Specification", Body: coldSpecificationBody},
	}}

	spy, stderr := executeRunWithGitHub(t, gh, "58")

	if want := []int{58}; !slices.Equal(spy.req.Issues, want) {
		t.Fatalf("Batch issues = %v, want %v (stderr: %q)", spy.req.Issues, want, stderr)
	}
	if !strings.Contains(stderr, "running issue #58 as a regular issue (no children)") {
		t.Errorf("expected the regular-issue log line, got: %q", stderr)
	}
	for _, unexpected := range []string{"warning:", "open-issue scan for specification #58", "discovered-children comment"} {
		if strings.Contains(stderr, unexpected) {
			t.Errorf("expected no scan or post warning without the optional capabilities, found %q in: %q", unexpected, stderr)
		}
	}
}
