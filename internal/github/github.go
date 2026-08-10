package github

import (
	"context"
	"errors"
	"fmt"
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
	UpdatedAt   time.Time
	// ReviewDecision, MergeStateStatus, and StatusCheckRollup are populated
	// by FindPRByBranch when available; they are empty when the underlying
	// `gh` invocation omits the columns (e.g. on PRs that never had a
	// review submitted or where the merge state is still being computed).
	// The T4 cheap-gate oracle reads these to decide whether to defer to T1
	// (Approved + CLEAN + green checks) or abstain (any other state).
	ReviewDecision     string
	MergeStateStatus   string
	StatusCheckRollup  string
	linkedIssueNumber  int
	linkedIssueNumbers []int
}

var prClosingIssueRe = regexp.MustCompile(`\b(?i)(?:close(?:s|d)?|fix(?:es|ed)?|resolve(?:s|d)?)\s*:?\s*((?:#\d+)(?:(?:\s*,\s*(?:and\s+)?|\s+and\s+)#\d+)*)`)
var prImplementsIssueRe = regexp.MustCompile(`\b(?i)implements\s+#(\d+)`)
var prIssueNumberRe = regexp.MustCompile(`#(\d+)`)

// LinkedIssueNumber returns the linked issue number for the PR.
// It first checks the native closingIssuesReferences metadata from GitHub,
// then falls back to searching the PR body for any GitHub closing keyword.
func (pr *PR) LinkedIssueNumber() int {
	if pr.linkedIssueNumber > 0 {
		return pr.linkedIssueNumber
	}
	if pr.Body == "" {
		return 0
	}
	if match := prImplementsIssueRe.FindStringSubmatch(pr.Body); len(match) > 1 {
		if n, err := strconv.Atoi(strings.TrimSpace(match[1])); err == nil {
			return n
		}
	}
	for _, match := range prClosingIssueRe.FindAllStringSubmatch(pr.Body, -1) {
		if len(match) < 2 {
			continue
		}
		if references := prIssueNumberRe.FindStringSubmatch(match[1]); len(references) > 1 {
			if n, err := strconv.Atoi(strings.TrimSpace(references[1])); err == nil {
				return n
			}
		}
	}
	return 0
}

// ClosesIssue reports whether the PR has GitHub closing intent for issueNumber.
// Native closing metadata wins for manually linked PRs; the body fallback
// recognizes every closing-keyword form documented by GitHub.
func (pr *PR) ClosesIssue(issueNumber int) bool {
	if pr == nil || issueNumber <= 0 {
		return false
	}
	if pr.linkedIssueNumber == issueNumber {
		return true
	}
	for _, number := range pr.linkedIssueNumbers {
		if number == issueNumber {
			return true
		}
	}
	for _, match := range prClosingIssueRe.FindAllStringSubmatch(pr.Body, -1) {
		if len(match) < 2 {
			continue
		}
		for _, reference := range prIssueNumberRe.FindAllStringSubmatch(match[1], -1) {
			if len(reference) < 2 {
				continue
			}
			if number, err := strconv.Atoi(reference[1]); err == nil && number == issueNumber {
				return true
			}
		}
	}
	return false
}

// EnsureClosingReference returns a PR body with closing intent for issueNumber.
// A standalone non-closing reference is repaired in place; otherwise the
// canonical closing line is prepended without disturbing the existing body.
func EnsureClosingReference(body string, issueNumber int) (string, bool) {
	if issueNumber <= 0 || (&PR{Body: body}).ClosesIssue(issueNumber) {
		return body, false
	}

	canonical := fmt.Sprintf("Closes #%d", issueNumber)
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		for _, reference := range prIssueNumberRe.FindAllStringSubmatch(line, -1) {
			if len(reference) < 2 {
				continue
			}
			number, err := strconv.Atoi(reference[1])
			if err == nil && number == issueNumber {
				lines[i] = canonical
				return strings.Join(lines, "\n"), true
			}
		}
	}

	if strings.TrimSpace(body) == "" {
		return canonical, true
	}
	return canonical + "\n\n" + body, true
}

// PRComment holds a PR conversation comment fetched from the GitHub REST API.
type PRComment struct {
	ID          string
	Body        string
	AuthorLogin string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// PRReview holds a formal pull-request review supplied to the review daemon.
type PRReview struct {
	ID          string
	Body        string
	State       string
	AuthorLogin string
	CreatedAt   time.Time
}

// PRReviewComment holds an inline pull-request review comment supplied to the
// review daemon. The path and line identify the reviewed location when GitHub
// provides them.
type PRReviewComment struct {
	ID          string
	Body        string
	Path        string
	Line        int
	AuthorLogin string
	CreatedAt   time.Time
}

// PRReviewLister is an optional capability for clients that can supply formal
// review history without widening the core Client interface.
type PRReviewLister interface {
	ListPRReviews(ctx context.Context, number int) ([]PRReview, error)
}

// PRReviewCommentLister is an optional capability for clients that can supply
// inline review history without widening the core Client interface.
type PRReviewCommentLister interface {
	ListPRReviewComments(ctx context.Context, number int) ([]PRReviewComment, error)
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
	AuthenticatedLogin(ctx context.Context) (string, error)
	ListIssueComments(ctx context.Context, number int) ([]IssueComment, error)
	ListSubIssues(ctx context.Context, parent int) ([]int, error)
	RepoName(ctx context.Context) (string, error)
	EditComment(ctx context.Context, commentID, body string) error
	EditPRBody(ctx context.Context, prNumber int, body string) error
	AddCommentReaction(ctx context.Context, commentID, content string) (string, error)
	AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error)
	RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error
	RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error
	CloseIssue(ctx context.Context, issueNumber int, comment string) error
}

// IssueContentFetcher optionally provides a fresh issue payload without
// resolving BlockedBy. Callers that only render an AgentRun prompt should use
// it to avoid loading dependency metadata they do not consume.
type IssueContentFetcher interface {
	FetchIssueContent(ctx context.Context, number int) (*Issue, error)
}

// IssueStateFetcher optionally provides fresh state for a blocker recheck
// without resolving its dependency graph.
type IssueStateFetcher interface {
	FetchIssueState(ctx context.Context, number int) (string, error)
}

// RateLimitError marks a GitHub CLI failure that explicitly reports a rate
// limit. It retains the underlying command error for errors.Is/As callers.
type RateLimitError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return e.Err.Error() }
func (e *RateLimitError) Unwrap() error { return e.Err }

// IsRateLimited reports whether GitHub explicitly rejected a request for a
// primary or secondary rate limit.
func IsRateLimited(err error) bool {
	var target *RateLimitError
	return errors.As(err, &target)
}

// OpenIssueLister is the optional capability the Specification
// resolver type-asserts against to run the last-resort open-issue
// scan (see ADR-0044). Production `CLIClient` satisfies it via
// `gh api repos/<owner>/<repo>/issues?state=open --paginate` with
// a client-side pull-request filter; existing test fakes do not,
// which preserves their behaviour without modifying them. See
// `CLIClient.ListOpenIssues` for the full invocation and PR filter.
type OpenIssueLister interface {
	ListOpenIssues(ctx context.Context) ([]Issue, error)
}

// IssueCommentPoster is the optional capability the Specification
// resolver type-asserts against to persist auto-discovered children
// as a marker comment on the spec (see ADR-0044). Production
// `CLIClient` satisfies it via `gh issue comment`; existing test
// fakes do not, so the new harvest step silently no-ops on them.
type IssueCommentPoster interface {
	PostIssueComment(ctx context.Context, issueNumber int, body string) error
}
