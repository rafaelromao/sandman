package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var blockedByPattern = regexp.MustCompile(`(?i)\b(?:blocked by|depends on|blocked-by)[:\s]+#(\d+)\b`)
var blockedByHeadingPattern = regexp.MustCompile(`(?im)^\s*##\s+(?:blocked by|depends on|blocked-by)\s*$`)
var bulletIssuePattern = regexp.MustCompile(`(?m)^\s*-\s*#(\d+)`)
var nextHeadingPattern = regexp.MustCompile(`(?m)^\s*##\s`)

// execRunner abstracts os/exec for testability.
type execRunner interface {
	Run(name string, arg ...string) *exec.Cmd
}

// realRunner delegates to exec.Command.
type realRunner struct{}

func (r *realRunner) Run(name string, arg ...string) *exec.Cmd {
	return exec.Command(name, arg...)
}

// CLIClient wraps the gh CLI for GitHub operations.
type CLIClient struct {
	runner       execRunner
	RepoOverride string
	mu           sync.Mutex
	repo         *repoRef
}

type repoRef struct {
	override string
	owner    string
	name     string
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
	Number      int    `json:"number"`
	State       string `json:"state"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	MergedAt    string `json:"mergedAt"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
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

func (c *CLIClient) command(name string, arg ...string) *exec.Cmd {
	if name == "gh" && c.RepoOverride != "" {
		arg = append(arg, "--repo", c.RepoOverride)
	}
	if c.runner != nil {
		return c.runner.Run(name, arg...)
	}
	return exec.Command(name, arg...)
}

func (c *CLIClient) resolveRepo() (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.repo != nil && c.repo.override == c.RepoOverride {
		return c.repo.owner, c.repo.name, nil
	}

	cmd := c.command("gh", "repo", "view", "--json", "owner,name")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("gh repo view: %w\n%s", err, out)
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

// FetchIssue fetches issue metadata via gh CLI.
func (c *CLIClient) FetchIssue(number int) (*Issue, error) {
	owner, repo, err := c.resolveRepo()
	if err != nil {
		return nil, err
	}

	issue, err := c.fetchIssuePayload(owner, repo, number)
	if err != nil {
		return nil, err
	}

	blockedBy := parseBlockedBy(issue.Body)
	if nativeBlockedBy, err := c.fetchIssueDependencies(owner, repo, number, issue); err == nil {
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
func (c *CLIClient) FetchPR(number int) (*PR, error) {
	_, _, err := c.resolveRepo()
	if err != nil {
		return nil, err
	}

	cmd := c.command("gh", "pr", "view", fmt.Sprintf("%d", number), "--json", "number,title,body,state,mergedAt,headRefName,headRefOid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr view: %w\n%s", err, out)
	}

	var payload prPayload
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("parse pr: %w", err)
	}

	return &PR{
		Number:      payload.Number,
		State:       payload.State,
		Title:       payload.Title,
		Body:        payload.Body,
		Merged:      strings.TrimSpace(payload.MergedAt) != "",
		HeadRefName: payload.HeadRefName,
		HeadRefOid:  payload.HeadRefOid,
	}, nil
}

// FetchIssueDependencies fetches native GitHub issue dependencies via gh CLI.
func (c *CLIClient) FetchIssueDependencies(number int) ([]int, error) {
	owner, repo, err := c.resolveRepo()
	if err != nil {
		return nil, err
	}

	issue, err := c.fetchIssuePayload(owner, repo, number)
	if err != nil {
		return nil, err
	}

	return c.fetchIssueDependencies(owner, repo, number, issue)
}

// FindPRByBranch finds the most recent pull request for a branch via gh CLI.
func (c *CLIClient) FindPRByBranch(branch string) (*PR, error) {
	cmd := c.command("gh", "pr", "list", "--head", branch, "--state", "all", "--json", "number,state,mergedAt,headRefName,headRefOid", "--limit", "1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w\n%s", err, out)
	}

	var payloads []prPayload
	if err := json.Unmarshal(out, &payloads); err != nil {
		return nil, fmt.Errorf("parse prs: %w", err)
	}
	if len(payloads) == 0 {
		return nil, nil
	}
	payload := payloads[0]
	return &PR{Number: payload.Number, State: payload.State, Merged: strings.TrimSpace(payload.MergedAt) != "", HeadRefName: payload.HeadRefName, HeadRefOid: payload.HeadRefOid}, nil
}

// ListOpenPRs lists all open pull requests in the current repo via gh CLI.
func (c *CLIClient) ListOpenPRs() ([]PR, error) {
	cmd := c.command("gh", "pr", "list", "--state", "open", "--json", "number,state,title,body,mergedAt,headRefName,headRefOid", "--limit", "1000")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w\n%s", err, out)
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
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ListPRComments fetches PR conversation (issue-style) comments for the given
// PR number via the GitHub REST API. These are the comments that appear in
// the PR's "Conversation" tab, where `/sandman review` is typically posted.
func (c *CLIClient) ListPRComments(number int) ([]PRComment, error) {
	owner, repo, err := c.resolveRepo()
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, number)
	cmd := c.command("gh", "api", path, "--paginate")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api pr comments: %w\n%s", err, out)
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}

	var payloads []prCommentPayload
	if err := json.Unmarshal([]byte(trimmed), &payloads); err != nil {
		return nil, fmt.Errorf("parse pr comments: %w", err)
	}

	comments := make([]PRComment, 0, len(payloads))
	for _, payload := range payloads {
		comments = append(comments, PRComment{
			ID:     strconv.FormatInt(payload.ID, 10),
			Author: payload.User.Login,
			Body:   payload.Body,
		})
	}
	return comments, nil
}

func (c *CLIClient) fetchIssuePayload(owner, repo string, number int) (issuePayload, error) {
	cmd := c.command("gh", "api", "-H", "Accept: application/vnd.github+json", fmt.Sprintf("repos/%s/%s/issues/%d", owner, repo, number))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return issuePayload{}, fmt.Errorf("gh api issue: %w\n%s", err, out)
	}

	var issue issuePayload
	if err := json.Unmarshal(out, &issue); err != nil {
		return issuePayload{}, fmt.Errorf("parse issue: %w", err)
	}

	return issue, nil
}

func (c *CLIClient) fetchIssueDependencies(owner, repo string, number int, issue issuePayload) ([]int, error) {
	blockedBy := mergeIssueNumbers(
		parseDependencyIssueNumbers(issue.BlockedBy),
		parseDependencyIssueNumbers(issue.IssueDependencies.BlockedBy),
	)
	if len(blockedBy) > 0 {
		return blockedBy, nil
	}

	cmd := c.command("gh", "api", "-H", "Accept: application/vnd.github+json", fmt.Sprintf("repos/%s/%s/issues/%d/events", owner, repo, number))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api issue events: %w\n%s", err, out)
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
		number, err := strconv.Atoi(match[1])
		if err != nil {
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

	matches := bulletIssuePattern.FindAllStringSubmatch(section, -1)
	if len(matches) == 0 {
		return nil
	}

	blockedBy := make([]int, 0, len(matches))
	seen := make(map[int]struct{}, len(matches))
	for _, match := range matches {
		number, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if _, ok := seen[number]; ok {
			continue
		}
		seen[number] = struct{}{}
		blockedBy = append(blockedBy, number)
	}

	if len(blockedBy) == 0 {
		return nil
	}
	return blockedBy
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
func (c *CLIClient) SearchIssues(query string) ([]Issue, error) {
	cmd := c.command("gh", "issue", "list", "--search", query, "--json", "number,state,title,body,labels", "--limit", "1000")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w\n%s", err, out)
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

// Ensure CLIClient implements Client.
var _ Client = (*CLIClient)(nil)
