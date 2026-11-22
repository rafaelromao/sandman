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

// StatusComponent names a githubstatus.com component. Names compare
// case-insensitively.
type StatusComponent string

// The githubstatus.com components Sandman's recoveries depend on.
const (
	ComponentAPIRequests   StatusComponent = "API Requests"
	ComponentGitOperations StatusComponent = "Git Operations"
	ComponentPullRequests  StatusComponent = "Pull Requests"
	ComponentActions       StatusComponent = "Actions"
)

// PullRequestLookupComponents returns the components whose degradation makes
// Sandman's pull-request lookups fail. GitHub recovery judges health by this
// set, and a plain GitHubStatus call does too. Each call returns a fresh slice,
// so no caller can change another's judgement.
func PullRequestLookupComponents() []StatusComponent {
	return []StatusComponent{
		ComponentAPIRequests,
		ComponentGitOperations,
		ComponentPullRequests,
		ComponentActions,
	}
}

// StatusReport is GitHub's reported health, judged by a set of components.
// Outage is true when GitHub reports a major or critical incident, or when a
// component in the requested set is degraded or unavailable. Components
// outside the set never make it an outage.
type StatusReport struct {
	Indicator   string
	Description string
	Outage      bool
	// Components names the requested components that are not operational, in
	// the status page's order.
	Components []string
}

// StatusChecker reports GitHub's health judged by the pull-request lookup
// components. An error means the status page itself could not be read; callers
// treat that as unknown health.
type StatusChecker interface {
	GitHubStatus(ctx context.Context) (StatusReport, error)
}

// ComponentStatusChecker is a StatusChecker that can also judge health by the
// components that matter to a given recovery.
type ComponentStatusChecker interface {
	StatusChecker
	GitHubComponentStatus(ctx context.Context, components []StatusComponent) (StatusReport, error)
}

// CheckStatus asks checker for GitHub's health judged by components. A checker
// that cannot scope its judgement answers with its plain GitHubStatus, which
// keeps checkers written against StatusChecker working unchanged.
func CheckStatus(ctx context.Context, checker StatusChecker, components []StatusComponent) (StatusReport, error) {
	if scoped, ok := checker.(ComponentStatusChecker); ok {
		return scoped.GitHubComponentStatus(ctx, components)
	}
	return checker.GitHubStatus(ctx)
}

// StatusPageClient reads GitHub's health from the public Statuspage API.
type StatusPageClient struct {
	URL    string
	Client *http.Client
}

// The production client judges health by requested components, which is what
// lets each recovery scope its status gate.
var _ ComponentStatusChecker = (*StatusPageClient)(nil)

// NewStatusPageClient returns a client for githubstatus.com.
func NewStatusPageClient() *StatusPageClient {
	return &StatusPageClient{URL: GitHubStatusSummaryURL, Client: &http.Client{Timeout: statusPageTimeout}}
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

// GitHubStatus fetches and classifies the Statuspage summary by the
// pull-request lookup components.
func (c *StatusPageClient) GitHubStatus(ctx context.Context) (StatusReport, error) {
	return c.GitHubComponentStatus(ctx, PullRequestLookupComponents())
}

// GitHubComponentStatus fetches and classifies the Statuspage summary by the
// requested components. An empty set judges by GitHub-wide incidents only.
func (c *StatusPageClient) GitHubComponentStatus(ctx context.Context, components []StatusComponent) (StatusReport, error) {
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
	requested := make(map[string]bool, len(components))
	for _, component := range components {
		requested[strings.ToLower(strings.TrimSpace(string(component)))] = true
	}
	for _, component := range summary.Components {
		status := strings.ToLower(strings.TrimSpace(component.Status))
		if !requested[strings.ToLower(strings.TrimSpace(component.Name))] || status == "" || status == "operational" {
			continue
		}
		report.Outage = true
		report.Components = append(report.Components, component.Name+": "+status)
	}
	return report, nil
}
