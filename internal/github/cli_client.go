package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
)

var blockedByPattern = regexp.MustCompile(`(?i)\b(?:blocked by|depends on|blocked-by)\s+#(\d+)\b`)

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
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
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

	cmd := c.command("gh", "api", "-H", "Accept: application/vnd.github+json", fmt.Sprintf("repos/%s/%s/issues/%d", owner, repo, number))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api issue: %w\n%s", err, out)
	}

	var issue issuePayload
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parse issue: %w", err)
	}

	return &Issue{
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      issue.Body,
		Labels:    labelNames(issue.Labels),
		BlockedBy: parseBlockedBy(issue.Body),
	}, nil
}

func parseBlockedBy(body string) []int {
	matches := blockedByPattern.FindAllStringSubmatch(body, -1)
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

// SearchIssues searches for issues via gh CLI.
func (c *CLIClient) SearchIssues(query string) ([]Issue, error) {
	cmd := c.command("gh", "issue", "list", "--search", query, "--json", "number,title,body,labels", "--limit", "100")
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
