package github

import (
	"context"
	"net/http"
	"net/http/httptest"
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
