package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func statusPageServer(t *testing.T, status int, body string) *StatusPageClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return &StatusPageClient{URL: server.URL, Client: server.Client()}
}

func TestStatusPageClient_ClassifiesGitHubHealth(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		outage     bool
	}{
		{"all operational", `{"status":{"indicator":"none","description":"All Systems Operational"},"components":[{"name":"API Requests","status":"operational"},{"name":"Git Operations","status":"operational"}]}`, false},
		{"unrelated component degraded", `{"status":{"indicator":"minor","description":"Minor Service Outage"},"components":[{"name":"Copilot","status":"partial_outage"},{"name":"API Requests","status":"operational"}]}`, false},
		{"API degraded", `{"status":{"indicator":"minor","description":"Minor Service Outage"},"components":[{"name":"API Requests","status":"degraded_performance"}]}`, true},
		{"pull requests outage", `{"status":{"indicator":"minor"},"components":[{"name":"Pull Requests","status":"partial_outage"}]}`, true},
		{"major incident", `{"status":{"indicator":"major","description":"Partial System Outage"},"components":[]}`, true},
		{"critical incident", `{"status":{"indicator":"critical"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := statusPageServer(t, http.StatusOK, tc.body).GitHubStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if report.Outage != tc.outage {
				t.Fatalf("outage = %v, want %v (%+v)", report.Outage, tc.outage, report)
			}
		})
	}
}

// A recovery judges health by the components that matter to it: a degraded
// component outside the requested set is not an outage, one inside it is, and
// a major or critical incident is an outage for every set.
func TestStatusPageClient_JudgesHealthByRequestedComponents(t *testing.T) {
	const (
		pullRequestsDown = `{"status":{"indicator":"minor","description":"Minor Service Outage"},"components":[{"name":"Actions","status":"operational"},{"name":"Pull Requests","status":"partial_outage"}]}`
		actionsDown      = `{"status":{"indicator":"minor","description":"Minor Service Outage"},"components":[{"name":"Actions","status":"degraded_performance"},{"name":"Pull Requests","status":"operational"}]}`
		majorIncident    = `{"status":{"indicator":"major","description":"Partial System Outage"},"components":[]}`
	)
	actionsOnly := []StatusComponent{ComponentActions}
	for _, tc := range []struct {
		name       string
		body       string
		components []StatusComponent
		outage     bool
		reported   []string
	}{
		{"actions set ignores a pull request outage", pullRequestsDown, actionsOnly, false, nil},
		{"actions set sees an actions outage", actionsDown, actionsOnly, true, []string{"Actions: degraded_performance"}},
		{"pull request set sees a pull request outage", pullRequestsDown, []StatusComponent{ComponentPullRequests}, true, []string{"Pull Requests: partial_outage"}},
		{"component names match case-insensitively", pullRequestsDown, []StatusComponent{"pull requests"}, true, []string{"Pull Requests: partial_outage"}},
		{"lookup set sees both in status page order", `{"status":{"indicator":"minor"},"components":[{"name":"Pull Requests","status":"partial_outage"},{"name":"Actions","status":"major_outage"}]}`,
			PullRequestLookupComponents, true, []string{"Pull Requests: partial_outage", "Actions: major_outage"}},
		{"a major incident is an outage for any set", majorIncident, actionsOnly, true, nil},
		{"an empty set judges by incidents only", pullRequestsDown, nil, false, nil},
		{"an empty set still sees a major incident", majorIncident, nil, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := statusPageServer(t, http.StatusOK, tc.body).GitHubComponentStatus(context.Background(), tc.components)
			if err != nil {
				t.Fatal(err)
			}
			if report.Outage != tc.outage || !slices.Equal(report.Components, tc.reported) {
				t.Fatalf("report = %+v, want outage=%v components=%v", report, tc.outage, tc.reported)
			}
		})
	}
}

// The plain call keeps the pull-request lookup set GitHub recovery has always
// used, and an unreadable status page is unknown health for a scoped check too.
func TestStatusPageClient_PlainCallJudgesByPullRequestLookupComponents(t *testing.T) {
	client := statusPageServer(t, http.StatusOK, `{"status":{"indicator":"minor"},"components":[{"name":"Git Operations","status":"degraded_performance"}]}`)
	report, err := client.GitHubStatus(context.Background())
	if err != nil || !report.Outage || !slices.Equal(report.Components, []string{"Git Operations: degraded_performance"}) {
		t.Fatalf("report = %+v, err = %v, want a Git Operations outage", report, err)
	}
	var nilClient *StatusPageClient
	if _, err := nilClient.GitHubComponentStatus(context.Background(), PullRequestLookupComponents); err == nil {
		t.Error("nil status client reported scoped health")
	}
	if _, err := statusPageServer(t, http.StatusServiceUnavailable, `{}`).GitHubComponentStatus(context.Background(), PullRequestLookupComponents); err == nil {
		t.Error("unreadable status page reported scoped health")
	}
}

func TestStatusPageClient_UnreadableStatusIsAnError(t *testing.T) {
	for name, client := range map[string]*StatusPageClient{
		"server error": statusPageServer(t, http.StatusServiceUnavailable, `{}`),
		"not json":     statusPageServer(t, http.StatusOK, `<html>`),
		"no indicator": statusPageServer(t, http.StatusOK, `{"status":{}}`),
	} {
		if _, err := client.GitHubStatus(context.Background()); err == nil {
			t.Errorf("%s: unreadable status page reported health", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := statusPageServer(t, http.StatusOK, `{"status":{"indicator":"none"}}`).GitHubStatus(ctx); err == nil {
		t.Error("cancelled status check reported health")
	}
}

type plainStatusChecker struct {
	report StatusReport
	err    error
	calls  int
}

func (c *plainStatusChecker) GitHubStatus(context.Context) (StatusReport, error) {
	c.calls++
	return c.report, c.err
}

type scopedStatusChecker struct {
	plainStatusChecker
	requested [][]StatusComponent
}

func (c *scopedStatusChecker) GitHubComponentStatus(_ context.Context, components []StatusComponent) (StatusReport, error) {
	c.requested = append(c.requested, components)
	return c.report, c.err
}

// A checker that can scope its judgement is asked for the requested
// components; one written against the plain StatusChecker answers with its own
// judgement, so existing injected checkers keep working.
func TestCheckStatus_ScopesCheckersThatCanAndFallsBackForThoseThatCannot(t *testing.T) {
	want := StatusReport{Indicator: "minor", Outage: true, Components: []string{"Actions: degraded_performance"}}

	scoped := &scopedStatusChecker{plainStatusChecker: plainStatusChecker{report: want}}
	got, err := CheckStatus(context.Background(), scoped, []StatusComponent{ComponentActions})
	if err != nil || got.Outage != want.Outage || !slices.Equal(got.Components, want.Components) {
		t.Fatalf("scoped report = %+v, err = %v, want %+v", got, err, want)
	}
	if scoped.calls != 0 || len(scoped.requested) != 1 || !slices.Equal(scoped.requested[0], []StatusComponent{ComponentActions}) {
		t.Fatalf("scoped checker: plain calls = %d, requested = %v, want one request for Actions", scoped.calls, scoped.requested)
	}

	plain := &plainStatusChecker{report: want}
	got, err = CheckStatus(context.Background(), plain, []StatusComponent{ComponentActions})
	if err != nil || got.Outage != want.Outage || plain.calls != 1 {
		t.Fatalf("plain report = %+v, err = %v, calls = %d, want its own judgement from one call", got, err, plain.calls)
	}

	failing := &plainStatusChecker{err: context.DeadlineExceeded}
	if _, err := CheckStatus(context.Background(), failing, nil); err == nil {
		t.Fatal("unreadable status was not reported as an error")
	}
}
