package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultCallTimeout is the default per-call deadline applied to every
// gh invocation when the caller's context has no deadline. A hung gh
// cannot wedge the calling goroutine beyond this value. Configurable
// via WithTimeout when constructing via NewCLIClient.
const DefaultCallTimeout = 30 * time.Second

// blockedByPattern parses inline references; see docs/usage/issue-body-formats.md.
var blockedByPattern = regexp.MustCompile(`(?i)\b(?:blocked by|depends on|blocked-by)[:\s]+(?:\[#(\d+)\](?:\([^)]+\))?|#(\d+)\b)`)

// blockedByHeadingPattern parses blocker headings; see docs/usage/issue-body-formats.md.
var blockedByHeadingPattern = regexp.MustCompile(`(?im)^\s*##\s+(?:blocked by|depends on|blocked-by)\s*$`)

// bulletIssuePattern parses strict issue lines; see docs/usage/issue-body-formats.md.
var bulletIssuePattern = regexp.MustCompile(`(?m)^\s*(?:-\s*)?(?:\[#(\d+)\]|#(\d+)|\[(?:[^\]]*)\]\([^()]*?/issues/(\d+)\))\s*$`)

// bulletLinePattern parses annotated bullet lines; see docs/usage/issue-body-formats.md.
var bulletLinePattern = regexp.MustCompile(`(?m)^\s*-\s+(?:\[#(\d+)\]|#(\d+)|\[(?:[^\]]*)\]\([^()]*?/issues/(\d+)\))[^\n]*$`)

// nextHeadingPattern finds the next H2 section; see docs/usage/issue-body-formats.md.
var nextHeadingPattern = regexp.MustCompile(`(?m)^\s*##\s`)

// childrenPattern parses inline children references; see docs/usage/issue-body-formats.md.
var childrenPattern = regexp.MustCompile(`(?i)\b(?:children|child issues)[:\s]+(?:\[#(\d+)\](?:\([^)]+\))?|#(\d+)\b)`)

// childrenHeadingPattern parses children headings; see docs/usage/issue-body-formats.md.
var childrenHeadingPattern = regexp.MustCompile(`(?im)^\s*##\s+(?:children|child issues)\s*$`)

// execRunner abstracts os/exec for testability. The context is threaded
// through so fakes can honour cancellation when the caller cancels its
// context.
type execRunner interface {
	Run(ctx context.Context, name string, arg ...string) *exec.Cmd
}

// realRunner delegates to exec.CommandContext. The Cmd is configured
// with a process-group cancel so a context cancellation kills the
// entire process tree, not just the immediate child — without this, a
// `gh` invocation that spawned grandchild processes would leak the
// hung subprocess even after ctx was cancelled.
type realRunner struct{}

func (r *realRunner) Run(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, arg...)
	configureCancelProcessGroup(cmd)
	return cmd
}

// configureCancelProcessGroup wires the *exec.Cmd so context
// cancellation kills the entire process group, not just the immediate
// child. Called from realRunner.Run and reused by tests.
func configureCancelProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil {
			return syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		}
		return syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// CLIClient wraps the gh CLI for GitHub operations.
//
// Timeout, when non-zero, is applied as a per-call deadline to every
// gh invocation whose caller-supplied context has no deadline of its
// own. A hung gh cannot wedge the calling goroutine beyond Timeout.
// When the caller passes a context that already has a deadline, that
// deadline is honoured as-is and Timeout is ignored — a tighter
// caller-side deadline wins by virtue of context.WithTimeout returning
// the earlier deadline.
type CLIClient struct {
	runner       execRunner
	RepoOverride string
	Timeout      time.Duration
	mu           sync.Mutex
	repo         *repoRef
}

type repoRef struct {
	override string
	owner    string
	name     string
}

// CLIOption configures a CLIClient returned by NewCLIClient.
type CLIOption func(*CLIClient)

// WithTimeout returns a CLIOption that sets the per-call timeout on
// the constructed CLIClient. A non-positive value disables the
// per-call timeout (callers must then provide their own deadline).
func WithTimeout(d time.Duration) CLIOption {
	return func(c *CLIClient) {
		c.Timeout = d
	}
}

// WithRunner returns a CLIOption that swaps the execRunner used to
// build *exec.Cmd values. Production code uses the real runner;
// tests inject fakes.
func WithRunner(r execRunner) CLIOption {
	return func(c *CLIClient) {
		c.runner = r
	}
}

// NewCLIClient returns a CLIClient wired with the default 30 s
// per-call timeout. Zero-value `&CLIClient{runner: ...}` construction
// remains supported for unit tests that want to opt out of the
// production timeout; production wiring goes through NewCLIClient so
// the bug class from issue #1780 — a hung gh wedging the daemon — is
// closed by default.
func NewCLIClient(opts ...CLIOption) *CLIClient {
	c := &CLIClient{Timeout: DefaultCallTimeout}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// boundContext returns the caller's context, layered with the
// configured Timeout when the caller's context has no deadline.
// When the configured Timeout is zero (zero-value struct) the caller's
// context is returned unchanged so unit tests can opt out of the
// production deadline. The returned cancel func, if non-nil, must be
// invoked to release the timer.
func (c *CLIClient) boundContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {}
	}
	if c.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.Timeout)
}

type issuePayload struct {
	Number                   int                      `json:"number"`
	State                    string                   `json:"state"`
	Title                    string                   `json:"title"`
	Body                     string                   `json:"body"`
	BlockedBy                json.RawMessage          `json:"blocked_by"`
	IssueDependencies        issueDependenciesPayload `json:"issue_dependencies"`
	IssueDependenciesSummary dependencySummary        `json:"issue_dependencies_summary"`
	Labels                   []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type issueDependenciesPayload struct {
	BlockedBy json.RawMessage `json:"blocked_by"`
}

type prPayload struct {
	Number                  int             `json:"number"`
	State                   string          `json:"state"`
	Title                   string          `json:"title"`
	Body                    string          `json:"body"`
	MergedAt                string          `json:"mergedAt"`
	HeadRefName             string          `json:"headRefName"`
	HeadRefOid              string          `json:"headRefOid"`
	ReviewDecision          string          `json:"reviewDecision"`
	MergeStateStatus        string          `json:"mergeStateStatus"`
	StatusCheckRollup       json.RawMessage `json:"statusCheckRollup"`
	ClosingIssuesReferences []struct {
		Number int `json:"number"`
	} `json:"closingIssuesReferences"`
}

// statusCheckRun mirrors the CheckRun shape that gh CLI ≥ 2.65 emits
// for statusCheckRollup. Older versions emit a flat string like
// "success"; the helper below tolerates both.
type statusCheckRun struct {
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
}

// rollupStateFromJSON converts the raw `gh pr list --json
// statusCheckRollup` value into the canonical rollup string the rest of
// the codebase compares against in T4CheapGate.
//
// Older gh versions emit the value as a flat string ("success",
// "failure", "pending"); newer versions emit an array of CheckRun
// objects whose Conclusion and Status fields must be folded. The
// returned string is "success", "failure", "pending", or "" when the
// value is absent / empty / unrecognised.
func rollupStateFromJSON(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var legacy string
	if err := json.Unmarshal(raw, &legacy); err == nil {
		return legacy
	}
	var checks []statusCheckRun
	if err := json.Unmarshal(raw, &checks); err != nil {
		return ""
	}
	if len(checks) == 0 {
		return ""
	}
	for _, run := range checks {
		if !strings.EqualFold(run.Status, "COMPLETED") {
			return "pending"
		}
	}
	for _, run := range checks {
		conclusion := strings.ToUpper(strings.TrimSpace(run.Conclusion))
		if conclusion != "" && conclusion != "SUCCESS" && conclusion != "SKIPPED" && conclusion != "NEUTRAL" {
			return "failure"
		}
	}
	return "success"
}

type dependencySummary struct {
	BlockedBy      int `json:"blocked_by"`
	TotalBlockedBy int `json:"total_blocked_by"`
	Blocking       int `json:"blocking"`
	TotalBlocking  int `json:"total_blocking"`
}

type issueEventPayload struct {
	Event         string              `json:"event"`
	BlockingIssue *dependencyIssueRef `json:"blocking_issue"`
	Source        *eventSource        `json:"source"`
}

type dependencyIssueRef struct {
	Number int `json:"number"`
}

type eventSource struct {
	Issue *dependencyIssueRef `json:"issue"`
}

func (c *CLIClient) command(ctx context.Context, name string, arg ...string) *exec.Cmd {
	if name == "gh" && c.RepoOverride != "" {
		arg = append(arg, "--repo", c.RepoOverride)
	}
	if c.runner != nil {
		return c.runner.Run(ctx, name, arg...)
	}
	return exec.CommandContext(ctx, name, arg...)
}

// runCmd executes the cmd with the supplied ctx and reports a wrapped
// error. When ctx is done at exit time, the wrapped error preserves the
// ctx error (via errors.Is) instead of the underlying "signal: killed"
// so callers can detect cancellation. For both branches the captured
// stdout/stderr is appended to the error message when non-empty so the
// surfaced `gh` output (auth failure, rate-limit message, network error
// text) survives the wrap.
func runCmd(ctx context.Context, cmd *exec.Cmd, errMsg string) ([]byte, error) {
	out, err := cmd.CombinedOutput()
	if err == nil {
		return out, nil
	}
	suffix := ""
	if len(bytes.TrimSpace(out)) > 0 {
		suffix = "\n" + string(out)
	}
	if cerr := ctx.Err(); cerr != nil {
		return out, fmt.Errorf("%s (context: %w): %w%s", errMsg, cerr, err, suffix)
	}
	return out, fmt.Errorf("%s: %w%s", errMsg, err, suffix)
}

func (c *CLIClient) resolveRepo(ctx context.Context) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.repo != nil && c.repo.override == c.RepoOverride {
		return c.repo.owner, c.repo.name, nil
	}

	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "repo", "view", "--json", "owner,name")
	out, err := runCmd(callCtx, cmd, "gh repo view")
	if err != nil {
		return "", "", fmt.Errorf("gh repo view: %w", err)
	}

	var repo struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := json.Unmarshal(out, &repo); err != nil {
		return "", "", fmt.Errorf("parse repo: %w", err)
	}
	if repo.Owner.Login == "" || repo.Name == "" {
		return "", "", fmt.Errorf("parse repo: missing owner or name")
	}

	c.repo = &repoRef{override: c.RepoOverride, owner: repo.Owner.Login, name: repo.Name}
	return c.repo.owner, c.repo.name, nil
}

// RepoName returns the current repo in owner/name format.
func (c *CLIClient) RepoName(ctx context.Context) (string, error) {
	owner, name, err := c.resolveRepo(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s", owner, name), nil
}

// FetchIssue fetches issue metadata via gh CLI.
func (c *CLIClient) FetchIssue(ctx context.Context, number int) (*Issue, error) {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return nil, err
	}

	issue, err := c.fetchIssuePayload(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}

	blockedBy := parseBlockedBy(issue.Body)
	if nativeBlockedBy, err := c.fetchIssueDependencies(ctx, owner, repo, number, issue); err == nil {
		blockedBy = mergeIssueNumbers(blockedBy, nativeBlockedBy)
	}

	return &Issue{
		Number:    issue.Number,
		State:     issue.State,
		Title:     issue.Title,
		Body:      issue.Body,
		Labels:    labelNames(issue.Labels),
		BlockedBy: blockedBy,
	}, nil
}

// FetchPR fetches pull request metadata via gh CLI.
func (c *CLIClient) FetchPR(ctx context.Context, number int) (*PR, error) {
	_, _, err := c.resolveRepo(ctx)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "pr", "view", fmt.Sprintf("%d", number), "--json", "number,title,body,state,mergedAt,headRefName,headRefOid,closingIssuesReferences")
	out, err := runCmd(callCtx, cmd, "gh pr view")
	if err != nil {
		return nil, fmt.Errorf("gh pr view: %w", err)
	}

	var payload prPayload
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("parse pr: %w", err)
	}

	var linkedIssueNumber int
	if len(payload.ClosingIssuesReferences) > 0 && payload.ClosingIssuesReferences[0].Number > 0 {
		linkedIssueNumber = payload.ClosingIssuesReferences[0].Number
	}

	return &PR{
		Number:            payload.Number,
		State:             payload.State,
		Title:             payload.Title,
		Body:              payload.Body,
		Merged:            strings.TrimSpace(payload.MergedAt) != "",
		HeadRefName:       payload.HeadRefName,
		HeadRefOid:        payload.HeadRefOid,
		linkedIssueNumber: linkedIssueNumber,
	}, nil
}

// FetchIssueDependencies fetches native GitHub issue dependencies via gh CLI.
func (c *CLIClient) FetchIssueDependencies(ctx context.Context, number int) ([]int, error) {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return nil, err
	}

	issue, err := c.fetchIssuePayload(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}

	return c.fetchIssueDependencies(ctx, owner, repo, number, issue)
}

// FindPRByBranch finds the most recent pull request for a branch via gh CLI.
func (c *CLIClient) FindPRByBranch(ctx context.Context, branch string) (*PR, error) {
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "pr", "list", "--head", branch, "--state", "all", "--json", "number,state,mergedAt,headRefName,headRefOid,reviewDecision,mergeStateStatus,statusCheckRollup", "--limit", "1")
	out, err := runCmd(callCtx, cmd, "gh pr list")
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}

	var payloads []prPayload
	if err := json.Unmarshal(out, &payloads); err != nil {
		return nil, fmt.Errorf("parse prs: %w", err)
	}
	if len(payloads) == 0 {
		return nil, nil
	}
	payload := payloads[0]
	return &PR{
		Number:            payload.Number,
		State:             payload.State,
		Merged:            strings.TrimSpace(payload.MergedAt) != "",
		HeadRefName:       payload.HeadRefName,
		HeadRefOid:        payload.HeadRefOid,
		ReviewDecision:    payload.ReviewDecision,
		MergeStateStatus:  payload.MergeStateStatus,
		StatusCheckRollup: rollupStateFromJSON(payload.StatusCheckRollup),
	}, nil
}

// prListPageLimit caps the number of PRs fetched in a single `gh pr list`
// invocation. The review daemon scans all open PRs but is not a real-time
// system; a high cap is appropriate.
const prListPageLimit = "1000"

// ListOpenPRs lists all open pull requests in the current repo via gh CLI.
func (c *CLIClient) ListOpenPRs(ctx context.Context) ([]PR, error) {
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "pr", "list", "--state", "open", "--json", "number,state,title,body,mergedAt,headRefName,headRefOid", "--limit", prListPageLimit)
	out, err := runCmd(callCtx, cmd, "gh pr list")
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}

	var payloads []prPayload
	if err := json.Unmarshal(out, &payloads); err != nil {
		return nil, fmt.Errorf("parse prs: %w", err)
	}

	prs := make([]PR, 0, len(payloads))
	for _, payload := range payloads {
		prs = append(prs, PR{
			Number:      payload.Number,
			State:       payload.State,
			Title:       payload.Title,
			Body:        payload.Body,
			Merged:      strings.TrimSpace(payload.MergedAt) != "",
			HeadRefName: payload.HeadRefName,
			HeadRefOid:  payload.HeadRefOid,
		})
	}
	return prs, nil
}

type prCommentPayload struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

type authenticatedUserPayload struct {
	Login string `json:"login"`
}

// AuthenticatedLogin returns the login for the GitHub user authenticated to
// the gh CLI. A blank login is rejected so callers can fail closed.
func (c *CLIClient) AuthenticatedLogin(ctx context.Context) (string, error) {
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", "user")
	out, err := runCmd(callCtx, cmd, "gh api authenticated user")
	if err != nil {
		return "", fmt.Errorf("gh api authenticated user: %w", err)
	}

	var user authenticatedUserPayload
	if err := json.Unmarshal(out, &user); err != nil {
		return "", fmt.Errorf("parse authenticated user: %w", err)
	}
	login := strings.TrimSpace(user.Login)
	if login == "" {
		return "", fmt.Errorf("parse authenticated user: empty login")
	}
	return login, nil
}

// prCommentPageSize is the per-page count for `gh api` paginated calls.
// 100 is the largest value GitHub accepts.
const prCommentPageSize = "100"

// ListPRComments fetches PR conversation (issue-style) comments for the given
// PR number via the GitHub REST API. These are the comments that appear in
// the PR's "Conversation" tab, where `/sandman review` is typically posted.
func (c *CLIClient) ListPRComments(ctx context.Context, number int) ([]PRComment, error) {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=%s&sort=created&direction=asc", owner, repo, number, prCommentPageSize)
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", path, "--paginate")
	out, err := runCmd(callCtx, cmd, "gh api pr comments")
	if err != nil {
		return nil, fmt.Errorf("gh api pr comments: %w", err)
	}

	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}

	var payloads []prCommentPayload
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var page []prCommentPayload
		if err := dec.Decode(&page); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parse pr comments: %w", err)
		}
		payloads = append(payloads, page...)
	}

	comments := make([]PRComment, 0, len(payloads))
	for _, payload := range payloads {
		var createdAt time.Time
		if payload.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, payload.CreatedAt); err == nil {
				createdAt = t
			}
		}
		comments = append(comments, PRComment{
			ID:          strconv.FormatInt(payload.ID, 10),
			Body:        payload.Body,
			AuthorLogin: payload.User.Login,
			CreatedAt:   createdAt,
		})
	}
	return comments, nil
}

// ListIssueComments fetches issue conversation comments for the given
// issue number via the GitHub REST API. These are the comments posted on
// the issue (not on a PR), used by Specification expansion to discover child issue
// references that live in the conversation rather than the issue body.
func (c *CLIClient) ListIssueComments(ctx context.Context, number int) ([]IssueComment, error) {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=%s&sort=created&direction=asc", owner, repo, number, prCommentPageSize)
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", path, "--paginate")
	out, err := runCmd(callCtx, cmd, "gh api issue comments")
	if err != nil {
		return nil, fmt.Errorf("gh api issue comments: %w", err)
	}

	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}

	var payloads []prCommentPayload
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var page []prCommentPayload
		if err := dec.Decode(&page); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parse issue comments: %w", err)
		}
		payloads = append(payloads, page...)
	}

	comments := make([]IssueComment, 0, len(payloads))
	for _, payload := range payloads {
		var createdAt time.Time
		if payload.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, payload.CreatedAt); err == nil {
				createdAt = t
			}
		}
		comments = append(comments, IssueComment{
			ID:        strconv.FormatInt(payload.ID, 10),
			Body:      payload.Body,
			CreatedAt: createdAt,
		})
	}
	return comments, nil
}

// ListSubIssues fetches the issue numbers of the given parent issue's native
// GitHub sub-issues via the REST API. It returns a non-nil empty slice when
// the parent has no sub-issues. The endpoint is
// GET /repos/{owner}/{repo}/issues/{number}/sub_issues.
func (c *CLIClient) ListSubIssues(ctx context.Context, parent int) ([]int, error) {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("repos/%s/%s/issues/%d/sub_issues?per_page=%s", owner, repo, parent, prCommentPageSize)
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", path, "--paginate")
	out, err := runCmd(callCtx, cmd, "gh api sub issues")
	if err != nil {
		return nil, fmt.Errorf("gh api sub issues: %w", err)
	}

	if len(bytes.TrimSpace(out)) == 0 {
		return []int{}, nil
	}

	var payloads []struct {
		Number int `json:"number"`
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var page []struct {
			Number int `json:"number"`
		}
		if err := dec.Decode(&page); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parse sub issues: %w", err)
		}
		payloads = append(payloads, page...)
	}

	nums := make([]int, 0, len(payloads))
	for _, payload := range payloads {
		nums = append(nums, payload.Number)
	}
	return nums, nil
}

func (c *CLIClient) fetchIssuePayload(ctx context.Context, owner, repo string, number int) (issuePayload, error) {
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", "-H", "Accept: application/vnd.github+json", fmt.Sprintf("repos/%s/%s/issues/%d", owner, repo, number))
	out, err := runCmd(callCtx, cmd, "gh api issue")
	if err != nil {
		return issuePayload{}, fmt.Errorf("gh api issue: %w", err)
	}

	var issue issuePayload
	if err := json.Unmarshal(out, &issue); err != nil {
		return issuePayload{}, fmt.Errorf("parse issue: %w", err)
	}

	return issue, nil
}

func (c *CLIClient) fetchIssueDependencies(ctx context.Context, owner, repo string, number int, issue issuePayload) ([]int, error) {
	blockedBy := mergeIssueNumbers(
		parseDependencyIssueNumbers(issue.BlockedBy),
		parseDependencyIssueNumbers(issue.IssueDependencies.BlockedBy),
	)
	if len(blockedBy) > 0 {
		return blockedBy, nil
	}

	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", "-H", "Accept: application/vnd.github+json", fmt.Sprintf("repos/%s/%s/issues/%d/events", owner, repo, number))
	out, err := runCmd(callCtx, cmd, "gh api issue events")
	if err != nil {
		return nil, fmt.Errorf("gh api issue events: %w", err)
	}

	var events []issueEventPayload
	if err := json.Unmarshal(out, &events); err != nil {
		return nil, fmt.Errorf("parse issue events: %w", err)
	}

	return parseDependencyEvents(events), nil
}

func parseBlockedBy(body string) []int {
	inline := blockedByPattern.FindAllStringSubmatch(body, -1)

	var blockedBy []int
	seen := make(map[int]struct{})

	for _, match := range inline {
		number, ok := issueNumberFromMatch(match[1], match[2])
		if !ok {
			continue
		}
		if _, ok := seen[number]; ok {
			continue
		}
		seen[number] = struct{}{}
		blockedBy = append(blockedBy, number)
	}

	blockedBy = mergeIssueNumbers(blockedBy, parseBlockedByHeading(body))

	if len(blockedBy) == 0 {
		return nil
	}
	return blockedBy
}

func parseBlockedByHeading(body string) []int {
	headingIdx := blockedByHeadingPattern.FindStringIndex(body)
	if headingIdx == nil {
		return nil
	}

	afterHeading := body[headingIdx[1]:]
	nextHeadingIdx := nextHeadingPattern.FindStringIndex(afterHeading)

	var section string
	if nextHeadingIdx != nil {
		section = afterHeading[:nextHeadingIdx[0]]
	} else {
		section = afterHeading
	}

	return parseBulletsInSection(section, bulletIssuePattern, bulletLinePattern)
}

func parseChildrenInline(body string) []int {
	inline := childrenPattern.FindAllStringSubmatch(body, -1)

	var children []int
	seen := make(map[int]struct{})

	for _, match := range inline {
		number, ok := issueNumberFromMatch(match[1], match[2])
		if !ok {
			continue
		}
		if _, ok := seen[number]; ok {
			continue
		}
		seen[number] = struct{}{}
		children = append(children, number)
	}

	return children
}

func parseChildrenFromHeading(body string) []int {
	headingIdx := childrenHeadingPattern.FindStringIndex(body)
	if headingIdx == nil {
		return nil
	}

	afterHeading := body[headingIdx[1]:]
	nextHeadingIdx := nextHeadingPattern.FindStringIndex(afterHeading)

	var section string
	if nextHeadingIdx != nil {
		section = afterHeading[:nextHeadingIdx[0]]
	} else {
		section = afterHeading
	}

	return parseBulletsInSection(section, bulletIssuePattern, bulletLinePattern)
}

func ParseChildrenFromBody(body string) []int {
	children := parseChildrenInline(body)
	children = mergeIssueNumbers(children, parseChildrenFromHeading(body))

	if len(children) == 0 {
		return nil
	}
	return children
}

func parseBulletsInSection(section string, patterns ...*regexp.Regexp) []int {
	type bulletMatch struct {
		start  int
		number int
	}

	var matches []bulletMatch
	for _, pattern := range patterns {
		for _, indexes := range pattern.FindAllStringSubmatchIndex(section, -1) {
			groups := make([]string, 0, len(indexes)/2-1)
			for i := 2; i < len(indexes); i += 2 {
				if indexes[i] == -1 {
					groups = append(groups, "")
					continue
				}
				groups = append(groups, section[indexes[i]:indexes[i+1]])
			}
			number, ok := issueNumberFromMatch(groups...)
			if !ok {
				continue
			}
			matches = append(matches, bulletMatch{start: indexes[0], number: number})
		}
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].start < matches[j].start
	})

	blockedBy := make([]int, 0, len(matches))
	seen := make(map[int]struct{}, len(matches))
	for _, match := range matches {
		if _, ok := seen[match.number]; ok {
			continue
		}
		seen[match.number] = struct{}{}
		blockedBy = append(blockedBy, match.number)
	}
	if len(blockedBy) == 0 {
		return nil
	}
	return blockedBy
}

func issueNumberFromMatch(groups ...string) (int, bool) {
	for _, group := range groups {
		if group == "" {
			continue
		}
		number, err := strconv.Atoi(group)
		if err != nil {
			return 0, false
		}
		return number, true
	}
	return 0, false
}

func parseDependencyIssueNumbers(raw json.RawMessage) []int {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}

	var numbers []int
	collectDependencyIssueNumbers(value, &numbers)
	return mergeIssueNumbers(numbers)
}

func collectDependencyIssueNumbers(value any, numbers *[]int) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if number, ok := issueNumberFromAny(item); ok {
				*numbers = append(*numbers, number)
				continue
			}
			collectDependencyIssueNumbers(item, numbers)
		}
	case map[string]any:
		if number, ok := issueNumberFromAny(typed["number"]); ok {
			*numbers = append(*numbers, number)
			return
		}
		for _, nested := range typed {
			switch nested.(type) {
			case []any, map[string]any:
				collectDependencyIssueNumbers(nested, numbers)
			}
		}
	}
}

func issueNumberFromAny(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok {
		return 0, false
	}
	if number <= 0 || float64(int(number)) != number {
		return 0, false
	}
	return int(number), true
}

func parseDependencyEvents(events []issueEventPayload) []int {
	var blockedBy []int
	for _, event := range events {
		switch event.Event {
		case "blocked_by_added":
			if event.BlockingIssue == nil || event.BlockingIssue.Number == 0 {
				continue
			}
			blockedBy = mergeIssueNumbers(blockedBy, []int{event.BlockingIssue.Number})
		case "blocked_by_removed":
			if event.BlockingIssue == nil || event.BlockingIssue.Number == 0 {
				continue
			}
			blockedBy = removeIssueNumber(blockedBy, event.BlockingIssue.Number)
		case "cross-referenced":
			if event.Source == nil || event.Source.Issue == nil || event.Source.Issue.Number == 0 {
				continue
			}
			blockedBy = mergeIssueNumbers(blockedBy, []int{event.Source.Issue.Number})
		}
	}
	if len(blockedBy) == 0 {
		return nil
	}
	return blockedBy
}

func mergeIssueNumbers(groups ...[]int) []int {
	var merged []int
	seen := make(map[int]struct{})
	for _, group := range groups {
		for _, number := range group {
			if _, ok := seen[number]; ok {
				continue
			}
			seen[number] = struct{}{}
			merged = append(merged, number)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func removeIssueNumber(numbers []int, target int) []int {
	filtered := numbers[:0]
	for _, number := range numbers {
		if number == target {
			continue
		}
		filtered = append(filtered, number)
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// SearchIssues searches for issues via gh CLI.
//
// Results are sorted by issue number ascending because gh issue list
// returns issues in GitHub's default sort order (last updated) and many
// call sites (unbounded ranges, label/query selection) depend on a
// deterministic ascending order to produce stable batches.
func (c *CLIClient) SearchIssues(ctx context.Context, query string) ([]Issue, error) {
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "issue", "list", "--search", query, "--json", "number,state,title,body,labels", "--limit", "1000")
	out, err := runCmd(callCtx, cmd, "gh issue list")
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w", err)
	}
	var payloads []issuePayload
	if err := json.Unmarshal(out, &payloads); err != nil {
		return nil, fmt.Errorf("parse issues: %w", err)
	}

	issues := make([]Issue, 0, len(payloads))
	for _, payload := range payloads {
		issues = append(issues, Issue{
			Number: payload.Number,
			State:  payload.State,
			Title:  payload.Title,
			Body:   payload.Body,
			Labels: labelNames(payload.Labels),
		})
	}
	sort.SliceStable(issues, func(i, j int) bool {
		return issues[i].Number < issues[j].Number
	})
	return issues, nil
}

func labelNames(labels []struct {
	Name string `json:"name"`
}) []string {
	if len(labels) == 0 {
		return nil
	}

	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	return names
}

// EditComment overwrites a PR conversation comment body via the GitHub REST API.
func (c *CLIClient) EditComment(ctx context.Context, commentID, body string) error {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", "-X", "PATCH", fmt.Sprintf("repos/%s/%s/issues/comments/%s", owner, repo, commentID), "-f", fmt.Sprintf("body=%s", body))
	_, err = runCmd(callCtx, cmd, "gh api edit comment")
	if err != nil {
		return fmt.Errorf("gh api edit comment: %w", err)
	}
	return nil
}

// EditPRBody overwrites the PR description via the GitHub REST API.
func (c *CLIClient) EditPRBody(ctx context.Context, prNumber int, body string) error {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", "-X", "PATCH", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, prNumber), "-f", fmt.Sprintf("body=%s", body))
	_, err = runCmd(callCtx, cmd, "gh api edit pr")
	if err != nil {
		return fmt.Errorf("gh api edit pr: %w", err)
	}
	return nil
}

// AddCommentReaction adds a reaction to a PR conversation comment and returns the reaction ID.
func (c *CLIClient) AddCommentReaction(ctx context.Context, commentID, content string) (string, error) {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return "", err
	}
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", "-X", "POST", fmt.Sprintf("repos/%s/%s/issues/comments/%s/reactions", owner, repo, commentID), "-f", fmt.Sprintf("content=%s", content), "--jq", ".id")
	out, err := runCmd(callCtx, cmd, "gh api add comment reaction")
	if err != nil {
		return "", fmt.Errorf("gh api add comment reaction: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("gh api add comment reaction: empty reaction ID")
	}
	return id, nil
}

// AddIssueReaction adds a reaction to an issue or PR and returns the reaction ID.
func (c *CLIClient) AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error) {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return "", err
	}
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", "-X", "POST", fmt.Sprintf("repos/%s/%s/issues/%d/reactions", owner, repo, issueNumber), "-f", fmt.Sprintf("content=%s", content), "--jq", ".id")
	out, err := runCmd(callCtx, cmd, "gh api add issue reaction")
	if err != nil {
		return "", fmt.Errorf("gh api add issue reaction: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("gh api add issue reaction: empty reaction ID")
	}
	return id, nil
}

// RemoveCommentReaction removes a reaction from a PR conversation comment.
func (c *CLIClient) RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", "-X", "DELETE", fmt.Sprintf("repos/%s/%s/issues/comments/%s/reactions/%s", owner, repo, commentID, reactionID))
	_, err = runCmd(callCtx, cmd, "gh api remove comment reaction")
	if err != nil {
		return fmt.Errorf("gh api remove comment reaction: %w", err)
	}
	return nil
}

// RemoveIssueReaction removes a reaction from an issue or PR.
func (c *CLIClient) RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", "api", "-X", "DELETE", fmt.Sprintf("repos/%s/%s/issues/%d/reactions/%s", owner, repo, issueNumber, reactionID))
	_, err = runCmd(callCtx, cmd, "gh api remove issue reaction")
	if err != nil {
		return fmt.Errorf("gh api remove issue reaction: %w", err)
	}
	return nil
}

// CloseIssue closes a GitHub issue with an optional comment.
func (c *CLIClient) CloseIssue(ctx context.Context, issueNumber int, comment string) error {
	owner, repo, err := c.resolveRepo(ctx)
	if err != nil {
		return err
	}
	args := []string{"issue", "close", fmt.Sprintf("%d", issueNumber), "--repo", fmt.Sprintf("%s/%s", owner, repo)}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	callCtx, cancel := c.boundContext(ctx)
	defer cancel()
	cmd := c.command(callCtx, "gh", args...)
	_, err = runCmd(callCtx, cmd, "gh issue close")
	if err != nil {
		return fmt.Errorf("gh issue close: %w", err)
	}
	return nil
}

// Ensure CLIClient implements Client.
var _ Client = (*CLIClient)(nil)
