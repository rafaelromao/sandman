package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
)

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
	return nil, fmt.Errorf("GitHub issue fetching not yet implemented")
}

// SearchIssues searches for issues via gh CLI.
func (c *CLIClient) SearchIssues(query string) ([]Issue, error) {
	cmd := c.command("gh", "issue", "list", "--search", query, "--json", "number,title,body,labels", "--limit", "100")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w\n%s", err, out)
	}
	var issues []Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parse issues: %w", err)
	}
	return issues, nil
}

// Ensure CLIClient implements Client.
var _ Client = (*CLIClient)(nil)
