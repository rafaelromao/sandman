package github

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Issue holds metadata fetched from GitHub.
type Issue struct {
	Number int
	State  string
	Title  string
	Body   string
	Labels []string
	// BlockedBy is populated by FetchIssue. SearchIssues leaves it empty.
	BlockedBy []int
}

// PR holds pull request metadata fetched from GitHub.
type PR struct {
	Number            int
	State             string
	Title             string
	Body              string
	Merged            bool
	HeadRefName       string
	HeadRefOid        string
	linkedIssueNumber int
}

var prIssueLinkRe = regexp.MustCompile(`\b(?i)(?:fixes|closes|resolves|implements)\s+#(\d+)`)

// LinkedIssueNumber returns the linked issue number for the PR.
// It first checks the native closingIssuesReferences metadata from GitHub,
// then falls back to searching the PR body for Fixes/Closes/Resolves keywords.
func (pr *PR) LinkedIssueNumber() int {
	if pr.linkedIssueNumber > 0 {
		return pr.linkedIssueNumber
	}
	if pr.Body == "" {
		return 0
	}
	if m := prIssueLinkRe.FindStringSubmatch(pr.Body); len(m) > 1 {
		if n, err := strconv.Atoi(strings.TrimSpace(m[1])); err == nil {
			return n
		}
	}
	return 0
}

// PRComment holds a PR conversation comment fetched from the GitHub REST API.
type PRComment struct {
	ID        string
	Body      string
	CreatedAt time.Time
}

// IssueComment holds an issue conversation comment fetched from the GitHub
// REST API. These are the comments posted on an issue (not a PR), used by
// Specification expansion to discover child references that live in the conversation
// rather than the issue body.
type IssueComment struct {
	ID        string
	Body      string
	CreatedAt time.Time
}

// Client wraps gh CLI for GitHub operations. Every method takes a
// context.Context so callers can cancel a hung gh invocation; the
// underlying CLIClient applies a per-call timeout when the caller's
// context has no deadline. See internal/github/cli_client.go.
type Client interface {
	FetchIssue(ctx context.Context, number int) (*Issue, error)
	FetchIssueDependencies(ctx context.Context, number int) ([]int, error)
	FetchPR(ctx context.Context, number int) (*PR, error)
	FindPRByBranch(ctx context.Context, branch string) (*PR, error)
	SearchIssues(ctx context.Context, query string) ([]Issue, error)
	ListOpenPRs(ctx context.Context) ([]PR, error)
	ListPRComments(ctx context.Context, number int) ([]PRComment, error)
	ListIssueComments(ctx context.Context, number int) ([]IssueComment, error)
	RepoName(ctx context.Context) (string, error)
	EditComment(ctx context.Context, commentID, body string) error
	EditPRBody(ctx context.Context, prNumber int, body string) error
	AddCommentReaction(ctx context.Context, commentID, content string) (string, error)
	AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error)
	RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error
	RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error
	CloseIssue(ctx context.Context, issueNumber int, comment string) error
}
