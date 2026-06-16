package github

import (
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
	Number      int
	State       string
	Title       string
	Body        string
	Merged      bool
	HeadRefName string
	HeadRefOid  string
}

var prIssueLinkRe = regexp.MustCompile(`(?i)(?:fixes|closes|resolves)\s+#(\d+)`)

// LinkedIssueNumber returns the first issue number referenced by a
// Fixes/Closes/Resolves keyword in the PR body, or 0 if none is found.
func (pr *PR) LinkedIssueNumber() int {
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
// PRD expansion to discover child references that live in the conversation
// rather than the issue body.
type IssueComment struct {
	ID        string
	Body      string
	CreatedAt time.Time
}

// Client wraps gh CLI for GitHub operations.
type Client interface {
	FetchIssue(number int) (*Issue, error)
	FetchIssueDependencies(number int) ([]int, error)
	FetchPR(number int) (*PR, error)
	FindPRByBranch(branch string) (*PR, error)
	SearchIssues(query string) ([]Issue, error)
	ListOpenPRs() ([]PR, error)
	ListPRComments(number int) ([]PRComment, error)
	ListIssueComments(number int) ([]IssueComment, error)
	RepoName() (string, error)
	EditComment(commentID, body string) error
	EditPRBody(prNumber int, body string) error
	AddCommentReaction(commentID, content string) (string, error)
	AddIssueReaction(issueNumber int, content string) (string, error)
	RemoveCommentReaction(commentID, reactionID string) error
	RemoveIssueReaction(issueNumber int, reactionID string) error
}
