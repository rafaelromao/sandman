package github

import "fmt"

// CLIClient wraps the gh CLI for GitHub operations.
type CLIClient struct{}

// FetchIssue fetches issue metadata via gh CLI.
func (c *CLIClient) FetchIssue(number int) (*Issue, error) {
	return nil, fmt.Errorf("GitHub issue fetching not yet implemented")
}

// CreatePR opens a pull request via gh CLI.
func (c *CLIClient) CreatePR(branch string, title string, body string) (string, error) {
	return "", fmt.Errorf("GitHub PR creation not yet implemented")
}

// Ensure CLIClient implements Client.
var _ Client = (*CLIClient)(nil)
