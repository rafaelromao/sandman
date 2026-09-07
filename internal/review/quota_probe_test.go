package review

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

type quotaProbeRunner struct {
	mu           sync.Mutex
	reviewResult *batch.Result
	probeResult  *batch.Result
	reviewErr    error
	probeErr     error
	reviewCalls  int
	probeCalls   int
	requests     []batch.Request
}

func (r *quotaProbeRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	if req.PromptConfig.PromptFlag == "quota-probe" {
		r.probeCalls++
		if r.probeResult != nil || r.probeErr != nil {
			return r.probeResult, r.probeErr
		}
		return &batch.Result{Runs: []batch.AgentRunResult{{Status: "success"}}}, nil
	}
	r.reviewCalls++
	if r.reviewResult != nil || r.reviewErr != nil {
		return r.reviewResult, r.reviewErr
	}
	return &batch.Result{Runs: []batch.AgentRunResult{{Status: "success"}}}, nil
}

func TestReviewQuotaPauseSetsTenMinuteGate(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "c1", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "T", Body: "B"}},
	}
	runner := &quotaProbeRunner{
		reviewResult: &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
	}
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "m"}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Clock = func() time.Time { return now }
	d.quotaProbeInterval = 10 * time.Minute
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	if runner.reviewCalls != 1 {
		t.Fatalf("review calls = %d, want 1", runner.reviewCalls)
	}
	if !d.IsQuotaPaused() {
		t.Fatal("expected quota paused after OpenCode usage-limit")
	}
	wantUntil := now.Add(10 * time.Minute)
	if got := d.QuotaPausedUntil(); !got.Equal(wantUntil) {
		t.Fatalf("paused until = %v, want %v", got, wantUntil)
	}
	key := reviewTriggerKey(github.PRComment{ID: "c1", CreatedAt: now})
	if got := d.NextAttemptAt(42, key); !got.Equal(wantUntil) {
		t.Fatalf("next attempt = %v, want %v", got, wantUntil)
	}
	if d.slotHeldCount() != 0 {
		t.Fatalf("slot held count = %d, want 0", d.slotHeldCount())
	}
	// Second tick before probe due should not launch review nor probe
	now2 := now.Add(5 * time.Minute)
	d.Clock = func() time.Time { return now2 }
	tickAndWait(t, d, context.Background())
	if runner.reviewCalls != 1 {
		t.Fatalf("second tick before probe due should not launch review, got %d", runner.reviewCalls)
	}
	if runner.probeCalls != 0 {
		t.Fatalf("probe calls = %d, want 0 before interval", runner.probeCalls)
	}
}

func TestReviewQuotaProbeWhilePaused(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "c1", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "T", Body: "B"}},
	}
	runner := &quotaProbeRunner{
		reviewResult: &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
		probeResult:  &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
	}
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "m"}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Clock = func() time.Time { return now }
	d.quotaProbeInterval = 10 * time.Minute
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	if runner.reviewCalls != 1 || runner.probeCalls != 0 {
		t.Fatalf("initial: review %d probe %d", runner.reviewCalls, runner.probeCalls)
	}
	// Advance to probe due, still quota exhausted
	now = now.Add(10 * time.Minute)
	d.Clock = func() time.Time { return now }
	tickAndWait(t, d, context.Background())
	if runner.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1 at interval", runner.probeCalls)
	}
	if runner.reviewCalls != 1 {
		t.Fatalf("review should stay paused, got %d", runner.reviewCalls)
	}
	if !d.IsQuotaPaused() {
		t.Fatal("should remain paused when probe still reports quota")
	}
	// Probe request should be promptonly ephemeral
	foundProbe := false
	for _, req := range runner.requests {
		if req.PromptConfig.PromptFlag == "quota-probe" {
			foundProbe = true
			if req.Review {
				t.Error("probe should not be Review")
			}
			if req.PRNumber != 0 {
				t.Errorf("probe PRNumber = %d, want 0", req.PRNumber)
			}
		}
	}
	if !foundProbe {
		t.Fatal("probe request not found")
	}
	// Next probe after another 10m should also run, still paused
	now = now.Add(10 * time.Minute)
	d.Clock = func() time.Time { return now }
	tickAndWait(t, d, context.Background())
	if runner.probeCalls != 2 {
		t.Fatalf("second probe calls = %d, want 2", runner.probeCalls)
	}
	if !d.IsQuotaPaused() {
		t.Fatal("should remain paused after second probe failure")
	}
}

func TestReviewQuotaProbeSuccessResumes(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
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
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "m"}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Clock = func() time.Time { return now }
	d.quotaProbeInterval = 10 * time.Minute
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	// Make the next review succeed so the probe-success tick does not immediately re-enter quota pause
	runner.reviewResult = &batch.Result{Runs: []batch.AgentRunResult{{Status: "success"}}}
	now = now.Add(10 * time.Minute)
	d.Clock = func() time.Time { return now }
	tickAndWait(t, d, context.Background())
	if runner.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", runner.probeCalls)
	}
	if d.IsQuotaPaused() {
		t.Fatal("should clear pause after probe success")
	}
}

func TestReviewQuotaProbeErrorKeepsPause(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "c1", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "T", Body: "B"}},
	}
	runner := &quotaProbeRunner{
		reviewResult: &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
		probeResult:  &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
		probeErr:     errors.New("prompt-only run failed"),
	}
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "m"}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Clock = func() time.Time { return now }
	d.quotaProbeInterval = 10 * time.Minute
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	now = now.Add(10 * time.Minute)
	d.Clock = func() time.Time { return now }
	tickAndWait(t, d, context.Background())
	if !d.IsQuotaPaused() {
		t.Fatal("quota result returned with prompt-only error must keep the pause")
	}
	if runner.reviewCalls != 1 {
		t.Fatalf("review calls = %d, want 1 while quota probe fails", runner.reviewCalls)
	}

	// A generic probe failure also cannot prove quota recovery.
	runner.probeResult = nil
	now = now.Add(10 * time.Minute)
	d.Clock = func() time.Time { return now }
	tickAndWait(t, d, context.Background())
	if !d.IsQuotaPaused() {
		t.Fatal("indeterminate prompt-only error must keep the pause")
	}
}

func TestReviewQuotaPauseSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "c1", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "T", Body: "B"}},
	}
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "m"}
	initialRunner := &quotaProbeRunner{
		reviewResult: &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
	}
	d, _, baseDir := newDaemonForTest(t, gh, initialRunner, cfg)
	d.Clock = func() time.Time { return now }
	d.quotaProbeInterval = 10 * time.Minute
	d.authenticatedLogin = "sandman"
	tickAndWait(t, d, context.Background())

	probeRunner := &quotaProbeRunner{
		probeResult: &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
	}
	restarted := New(baseDir, gh, &prompt.Engine{}, probeRunner, cfg, &lockedBuffer{}, 0, false, nil)
	restarted.Clock = func() time.Time { return now.Add(15 * time.Minute) }
	restarted.quotaProbeInterval = 10 * time.Minute
	restarted.authenticatedLogin = "sandman"
	tickAndWait(t, restarted, context.Background())
	if !restarted.IsQuotaPaused() {
		t.Fatal("restart should retain the global quota pause until a probe succeeds")
	}
	if probeRunner.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1 after expired gate", probeRunner.probeCalls)
	}
	if probeRunner.reviewCalls != 0 {
		t.Fatalf("review calls = %d, want 0 before quota recovery", probeRunner.reviewCalls)
	}
}

func TestReviewNonOpenCodeDoesNotTriggerQuotaPause(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "c1", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "T", Body: "B"}},
	}
	// Use error path with quota literal to trigger the launch-failure branch
	runner := &quotaProbeRunner{
		reviewErr: fmt.Errorf("Error: The usage limit has been reached"),
	}
	cfg := &config.Config{DefaultReviewAgent: "other", DefaultReviewModel: "m", AgentProviders: map[string]config.Agent{"other": {Preset: "custom", Command: "other"}}}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Clock = func() time.Time { return now }
	d.quotaProbeInterval = 10 * time.Minute
	d.Agent = "other"
	d.Model = "other/model"
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	if d.IsQuotaPaused() {
		t.Fatal("non-opencode should not trigger quota pause")
	}
	// Should have used normal exponential backoff, not quota 10m
	key := reviewTriggerKey(github.PRComment{ID: "c1", CreatedAt: now})
	got := d.NextAttemptAt(42, key)
	if got.IsZero() {
		t.Fatal("expected normal backoff gate, got zero")
	}
	if got.Equal(now.Add(10 * time.Minute)) {
		t.Error("non-opencode should not use 10m quota gate")
	}
}

func TestReviewQuotaPauseGatesParallelPRs(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}, {Number: 2, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "c1", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}},
			2: {{ID: "c2", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}},
		},
		prFetch: map[int]*github.PR{
			1: {Number: 1, Title: "T1", Body: "B1"},
			2: {Number: 2, Title: "T2", Body: "B2"},
		},
	}
	runner := &quotaProbeRunner{
		reviewResult: &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
		probeResult:  &batch.Result{Runs: []batch.AgentRunResult{{UsageLimitReached: true, Status: "failure"}}},
	}
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "m", DefaultReviewParallel: 2}
	d, _, _ := newDaemonForTestWithParallel(t, gh, runner, cfg, 2, true)
	d.Clock = func() time.Time { return now }
	d.quotaProbeInterval = 10 * time.Minute
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	// Both PRs launch in parallel before quota pause is observed; both will report quota
	if !d.IsQuotaPaused() {
		t.Fatal("expected quota paused after first review quota failure")
	}
	initialReviews := runner.reviewCalls
	if initialReviews != 2 {
		t.Fatalf("initial reviews = %d, want 2 (parallel 2, both hit quota)", initialReviews)
	}
	// Next tick before probe due should not launch more reviews
	now2 := now.Add(5 * time.Minute)
	d.Clock = func() time.Time { return now2 }
	tickAndWait(t, d, context.Background())
	if runner.reviewCalls != initialReviews {
		t.Fatalf("parallel PR should be gated while quota paused, got %d extra reviews", runner.reviewCalls-initialReviews)
	}
	if runner.probeCalls != 0 {
		t.Fatalf("probe should not run before interval, got %d", runner.probeCalls)
	}
}

func TestReviewOrdinaryFailureDoesNotTriggerQuotaPause(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "c1", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "T", Body: "B"}},
	}
	runner := &quotaProbeRunner{
		reviewResult: &batch.Result{Runs: []batch.AgentRunResult{{Status: "failure"}}},
	}
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "m"}
	d, _, _ := newDaemonForTest(t, gh, runner, cfg)
	d.Clock = func() time.Time { return now }
	d.quotaProbeInterval = 10 * time.Minute
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())
	if d.IsQuotaPaused() {
		t.Fatal("ordinary failure should not trigger quota pause")
	}
	if runner.probeCalls != 0 {
		t.Fatalf("probe calls = %d, want 0 for ordinary failure", runner.probeCalls)
	}
}
