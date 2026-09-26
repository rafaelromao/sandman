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
// Specification to persist the children its open-Issue scan discovered.
const discoveredChildrenMarker = "<!-- sandman-discovered-children -->"

// coldSpecificationBody carries the canonical Specification shape but no
// child signal: no child section, no issue references outside a Parent
// section. Only the open-Issue scan (ADR-0044) can discover its children.
const coldSpecificationBody = "## Problem Statement\n\nImported tickets keep their relationship on the child side only.\n\n## Solution\n\nDiscover the children from their Parent sections.\n"

// specDiscoveryGitHubClient is the GitHub boundary for Specification
// discovery through `sandman run`. It extends fakeGitHubClient with
// per-Issue comments that reflect its own posts, mention-search results
// kept apart from the repo-wide `is:open` search, and the optional
// ADR-0044 capabilities (open-Issue listing and Issue comment posting)
// that the production GitHub client provides.
type specDiscoveryGitHubClient struct {
	*fakeGitHubClient

	state    sync.Mutex
	comments map[int][]github.IssueComment
	mentions map[int][]github.Issue
	listings int
	posts    []postedIssueComment
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
// Issue) cannot masquerade as mention results.
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

// ListOpenIssues lists every open Issue in ascending number order, like
// the production client.
func (c *specDiscoveryGitHubClient) ListOpenIssues(ctx context.Context) ([]github.Issue, error) {
	c.state.Lock()
	c.listings++
	c.state.Unlock()

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
		t.Errorf("expected the second run to reuse the marker instead of listing open Issues; listings = %d", gh.listings)
	}
	if len(gh.posts) != 1 {
		t.Errorf("expected the persisted marker to prevent a second post; posts = %+v", gh.posts)
	}
}

func TestRun_CuratedMarkerStillScansWithoutReposting(t *testing.T) {
	gh := newColdSpecificationGitHub()
	gh.comments[58] = []github.IssueComment{{Body: discoveredChildrenMarker + "\n\n## Discovered children\n\n"}}

	spy, stderr := executeRunWithGitHub(t, gh, "58")

	if want := []int{232, 234, 58}; !slices.Equal(spy.req.Issues, want) {
		t.Fatalf("Batch issues = %v, want %v (stderr: %q)", spy.req.Issues, want, stderr)
	}
	if gh.listings != 1 {
		t.Errorf("expected the open-Issue scan to run once, got %d listings", gh.listings)
	}
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
