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
