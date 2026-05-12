package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
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
	runner execRunner
}

func (c *CLIClient) command(name string, arg ...string) *exec.Cmd {
	if c.runner != nil {
		return c.runner.Run(name, arg...)
	}
	return exec.Command(name, arg...)
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
