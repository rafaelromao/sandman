package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHubStatusSummaryURL is GitHub's public Statuspage summary endpoint.
const GitHubStatusSummaryURL = "https://www.githubstatus.com/api/v2/summary.json"

// statusPageTimeout bounds one health check so an unreachable status page
// can never delay a recovery attempt or an abort.
const statusPageTimeout = 15 * time.Second

// StatusReport is GitHub's reported health. Outage is true when GitHub
// reports a major or critical incident, or when a component Sandman depends
// on (API Requests, Git Operations, Pull Requests, Actions) is degraded or
// unavailable. Unrelated components never make it an outage.
type StatusReport struct {
	Indicator   string
	Description string
	Outage      bool
	// Components names the relevant components that are not operational.
	Components []string
}

// StatusChecker reports GitHub's health. An error means the status page
// itself could not be read; callers treat that as unknown health.
type StatusChecker interface {
	GitHubStatus(ctx context.Context) (StatusReport, error)
}

// StatusPageClient reads GitHub's health from the public Statuspage API.
type StatusPageClient struct {
	URL    string
	Client *http.Client
}

// NewStatusPageClient returns a client for githubstatus.com.
func NewStatusPageClient() *StatusPageClient {
	return &StatusPageClient{URL: GitHubStatusSummaryURL, Client: &http.Client{Timeout: statusPageTimeout}}
}

// relevantStatusComponents are the githubstatus.com components whose
// degradation makes Sandman's pull-request lookups fail.
var relevantStatusComponents = map[string]bool{
	"api requests":   true,
	"git operations": true,
	"pull requests":  true,
	"actions":        true,
}

type statusPageSummary struct {
	Status struct {
		Indicator   string `json:"indicator"`
		Description string `json:"description"`
	} `json:"status"`
	Components []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"components"`
}

// GitHubStatus fetches and classifies the Statuspage summary.
func (c *StatusPageClient) GitHubStatus(ctx context.Context) (StatusReport, error) {
	if c == nil {
		return StatusReport{}, fmt.Errorf("githubstatus.com: no status client")
	}
	url := c.URL
	if url == "" {
		url = GitHubStatusSummaryURL
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: statusPageTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, statusPageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return StatusReport{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return StatusReport{}, fmt.Errorf("githubstatus.com: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return StatusReport{}, fmt.Errorf("githubstatus.com: HTTP %d", resp.StatusCode)
	}
	var summary statusPageSummary
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&summary); err != nil {
		return StatusReport{}, fmt.Errorf("githubstatus.com: parse summary: %w", err)
	}
	indicator := strings.ToLower(strings.TrimSpace(summary.Status.Indicator))
	if indicator == "" {
		return StatusReport{}, fmt.Errorf("githubstatus.com: summary has no status indicator")
	}
	report := StatusReport{Indicator: indicator, Description: strings.TrimSpace(summary.Status.Description)}
	report.Outage = indicator == "major" || indicator == "critical"
	for _, component := range summary.Components {
		status := strings.ToLower(strings.TrimSpace(component.Status))
		if !relevantStatusComponents[strings.ToLower(strings.TrimSpace(component.Name))] || status == "" || status == "operational" {
			continue
		}
		report.Outage = true
		report.Components = append(report.Components, component.Name+": "+status)
	}
	return report, nil
}
