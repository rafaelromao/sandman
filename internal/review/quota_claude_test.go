package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

const claudeReviewUsageLimitResult = `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your session limit · resets 3pm"}`

func TestReviewQuotaGateFollowsTheReviewAgentStrategy(t *testing.T) {
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
		Agents:             map[string]config.Agent{"other": {Preset: "custom", Command: "other"}},
	}
	tests := []struct {
		agent      string
		err        error
		wantGate   bool
		wantQuotaE bool
	}{
		{agent: "opencode", err: errors.New("Error: The usage limit has been reached"), wantGate: true, wantQuotaE: true},
		{agent: "opencode", err: errors.New(claudeReviewUsageLimitResult), wantGate: true, wantQuotaE: false},
		{agent: "claude", err: errors.New(claudeReviewUsageLimitResult), wantGate: true, wantQuotaE: true},
		{agent: "claude", err: errors.New("Error: The usage limit has been reached"), wantGate: true, wantQuotaE: false},
		{agent: "other", err: errors.New("Error: The usage limit has been reached"), wantGate: false, wantQuotaE: false},
	}
	for _, tt := range tests {
		d := &Daemon{Config: cfg, Agent: tt.agent}
		if got := d.reviewAwaitsUsageLimit(); got != tt.wantGate {
			t.Errorf("agent %q: reviewAwaitsUsageLimit = %t, want %t", tt.agent, got, tt.wantGate)
		}
		if got := d.isQuotaError(tt.err); got != tt.wantQuotaE {
			t.Errorf("agent %q: isQuotaError(%q) = %t, want %t", tt.agent, tt.err, got, tt.wantQuotaE)
		}
	}
}

func TestReviewEffectiveModelDoesNotCrossPresets(t *testing.T) {
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/big-pickle",
		Agents:             map[string]config.Agent{"fast-review": {Preset: "opencode", Model: "opencode/fast"}},
	}
	tests := []struct {
		name  string
		agent string
		model string
		want  string
	}{
		{name: "configured review agent", want: "opencode/big-pickle"},
		{name: "override on the same preset", agent: "fast-review", want: "opencode/big-pickle"},
		{name: "override on another preset", agent: "claude", want: ""},
		{name: "override with explicit model", agent: "claude", model: "opus", want: "opus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Daemon{Config: cfg, Agent: tt.agent, Model: tt.model}
			if got := d.effectiveModel(); got != tt.want {
				t.Fatalf("effectiveModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReviewClaudeQuotaPauseClearsAfterCleanProbe(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "c1", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "T", Body: "B"}},
	}
	runner := &quotaProbeRunner{
		reviewResult: &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
		probeResult:  &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: false, Status: "success"}}},
	}
	cfg := &config.Config{DefaultReviewAgent: "claude", DefaultReviewModel: "sonnet"}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Clock = func() time.Time { return now }
	d.quotaProbeInterval = 10 * time.Minute
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	if !d.IsQuotaPaused() {
		t.Fatal("expected quota paused after a claude usage-limit review")
	}
	if paused := readQuotaPauseFlag(t, d); !paused {
		t.Fatal("quota-pause.json does not record the pause")
	}
	if got := runner.requests[0]; got.Agent != "claude" || got.Model != "sonnet" {
		t.Fatalf("review request agent/model = %q/%q, want claude/sonnet", got.Agent, got.Model)
	}

	runner.reviewResult = &batch.Result{Runs: []batch.AgentRunResult{{Status: "success"}}}
	now = now.Add(10 * time.Minute)
	d.Clock = func() time.Time { return now }
	tickAndWait(t, d, context.Background())
	if runner.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", runner.probeCalls)
	}
	if d.IsQuotaPaused() {
		t.Fatal("clean probe did not clear the claude quota pause")
	}
	if paused := readQuotaPauseFlag(t, d); paused {
		t.Fatal("quota-pause.json still records the pause after a clean probe")
	}
}

func readQuotaPauseFlag(t *testing.T, d *Daemon) bool {
	t.Helper()
	data, err := os.ReadFile(d.quotaPauseStatePath())
	if err != nil {
		t.Fatalf("read quota-pause.json: %v", err)
	}
	var state quotaPauseState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode quota-pause.json %q: %v", strings.TrimSpace(string(data)), err)
	}
	return state.Paused
}
