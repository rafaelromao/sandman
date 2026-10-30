package review

import (
	"context"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

func TestDaemon_ReviewVariantPrecedenceReachesBatchRequest(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		configured string
		cli        string
		cliSet     bool
		want       string
	}{
		{name: "configured", configured: "configured/provider", want: "configured/provider"},
		{name: "cli override", configured: "configured/provider", cli: " cli/provider ", cliSet: true, want: "cli/provider"},
		{name: "explicit empty", configured: "configured/provider", cli: "   ", cliSet: true, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			gh := &fakeGH{
				prs:      []github.PR{{Number: 42, State: "open"}},
				comments: map[int][]github.PRComment{42: {{ID: "100", Body: "/sandman review", CreatedAt: now}}},
				prFetch:  map[int]*github.PR{42: {Number: 42, Title: "PR 42", Body: "body"}},
			}
			runner := &capturedRequest{}
			d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/foo",
				ReviewVariant:      test.configured,
			})
			d.Variant = test.cli
			d.VariantSet = test.cliSet
			d.Clock = func() time.Time { return now }

			tickAndWait(t, d, context.Background())
			if runner.calls != 1 {
				t.Fatalf("expected one batch request, got %d", runner.calls)
			}
			if runner.last.Variant != test.want {
				t.Fatalf("request variant = %q, want %q", runner.last.Variant, test.want)
			}
			if runner.last.VariantSet != test.cliSet {
				t.Fatalf("request variant set = %v, want %v", runner.last.VariantSet, test.cliSet)
			}
		})
	}
}

func TestDaemon_QuotaProbePreservesResolvedReviewVariant(t *testing.T) {
	for agent := range config.BuiltInAgentPresets {
		for _, tc := range []struct {
			name, configured, cli, want string
			cliSet                      bool
		}{
			{name: "empty-review-variant"},
			{name: "configured-review-variant", configured: "review-high", want: "review-high"},
			{name: "cli-override", configured: "review-high", cli: " high ", cliSet: true, want: "high"},
			{name: "explicit-empty", configured: "review-high", cli: "   ", cliSet: true},
		} {
			t.Run(agent+"/"+tc.name, func(t *testing.T) {
				runner := &quotaProbeRunner{}
				d := &Daemon{
					Config: &config.Config{DefaultReviewAgent: agent, DefaultReviewModel: "provider/reviewer", Variant: "implementation-only", ReviewVariant: tc.configured},
					Runner: runner, Variant: tc.cli, VariantSet: tc.cliSet,
				}
				if !d.quotaProbeRecovered(context.Background()) {
					t.Fatal("successful probe did not recover")
				}
				if len(runner.requests) != 1 {
					t.Fatalf("requests=%d", len(runner.requests))
				}
				req := runner.requests[0]
				if req.Variant != tc.want || !req.VariantSet {
					t.Fatalf("probe must preserve resolved review variant, including empty: %q set=%t, want %q set=true", req.Variant, req.VariantSet, tc.want)
				}
				if req.Agent != agent || req.Model != "provider/reviewer" {
					t.Fatalf("probe changed reviewer selection: %+v", req)
				}
				if req.Review || req.PRNumber != 0 || !req.PortalHidden {
					t.Fatalf("probe must stay ephemeral prompt-only work: %+v", req)
				}
			})
		}
	}
}
