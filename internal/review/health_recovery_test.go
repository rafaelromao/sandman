package review

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/reviewlaunch"
	"github.com/rafaelromao/sandman/internal/testenv"
)

type healthRunner struct {
	mu      sync.Mutex
	repairs []batch.Request
	repair  func(context.Context, batch.Request) (*batch.Result, error)
	review  func(context.Context, batch.Request) (*batch.Result, error)
}

type healthConfigStore struct{ cfg *config.Config }

func (s healthConfigStore) Load() (*config.Config, error) { return s.cfg, nil }
func (s healthConfigStore) Save(*config.Config) error     { return nil }

type productionHealthRunner struct{ *batch.Orchestrator }

func (r productionHealthRunner) RunBatch(context.Context, batch.Request) (*batch.Result, error) {
	return &batch.Result{Runs: []batch.AgentRunResult{{Status: "failure", OperationalError: errors.New("start sandbox: delete branch from stranded worktree at /repo/.sandman/worktrees/review-17-stranded: exit status 128\nfatal: not a git repository: (null)")}}}, errors.New("prompt-only run failed")
}

func TestDaemonHealthRepairStrandedFailureLaunchesRealIndependentAdapter(t *testing.T) {
	dir := testenv.MkdirShort(t, "health-host-")
	t.Chdir(dir) // No git repository: repair must not require ordinary preparation.
	if err := os.WriteFile("opencode", []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > repaired-launch.txt\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	base := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/repair-configured", Sandbox: "podman", WorktreeDir: ".sandman/worktrees"}
	log := &events.JSONLLogger{Path: filepath.Join(base, "events.jsonl")}
	orchestrator := batch.NewOrchestrator(nil, &prompt.Engine{}, healthConfigStore{cfg}, log, batch.WithErrorLog(io.Discard))
	gh := &fakeGH{prs: []github.PR{{Number: 17}}, comments: map[int][]github.PRComment{17: {{ID: "stranded", Body: "/sandman review", AuthorLogin: "sandman", CreatedAt: time.Now()}}}, prFetch: map[int]*github.PR{17: {Number: 17}}}
	logs := &lockedBuffer{}
	d := New(base, gh, &prompt.Engine{}, productionHealthRunner{orchestrator}, cfg, logs, 0, false, nil)
	d.launchBackoff = func(int) time.Duration { return 0 }
	if err := d.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.inFlight.Wait()
	launch, err := os.ReadFile("repaired-launch.txt")
	if err != nil {
		t.Fatalf("independent repair not executed: %v; %s", err, logs.String())
	}
	for _, evidence := range []string{"openai/repair-configured", "fatal: not a git repository: (null)", "stranded worktree"} {
		if !strings.Contains(string(launch), evidence) {
			t.Fatalf("launch missing %q: %s", evidence, launch)
		}
	}
	state := readHealthState(t, d)
	states, err := events.ReadRunStates(log)
	if err != nil {
		t.Fatal(err)
	}
	if states[state.RunID].Status() != "success" || d.IsTerminalSeen(17, "stranded") {
		t.Fatal("repair lifecycle or original request authority violated")
	}
}

func TestDaemonHealthRepairExcludesNormalWaitsAndCancellation(t *testing.T) {
	for _, err := range []error{context.Canceled, batch.ErrAborted, errReviewDeferred, reviewlaunch.ErrLaunchOwned,
		&github.RateLimitError{Err: errors.New("API rate limit exceeded")}, errors.New("Error: The usage limit has been reached")} {
		t.Run(err.Error(), func(t *testing.T) {
			runner := &healthRunner{}
			d, _, _ := newDaemonForTest(t, &fakeGH{}, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
			d.observeHealthFailure(context.Background(), "normal", "normal wait", 0, err)
			d.inFlight.Wait()
			if len(runner.requests()) != 0 {
				t.Fatalf("normal wait/cancel launched repair: %v", err)
			}
		})
	}
}

func TestDaemonHealthRepairExecutionFailureDoesNotCompleteOrRecurse(t *testing.T) {
	gh := &fakeGH{listErr: errors.New("remote broken")}
	runner := &healthRunner{repair: func(context.Context, batch.Request) (*batch.Result, error) {
		return &batch.Result{Runs: []batch.AgentRunResult{{Status: "failure"}}}, nil
	}}
	d, logs, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
	_ = d.tick(context.Background())
	d.inFlight.Wait()
	_ = d.tick(context.Background())
	d.inFlight.Wait()
	if len(runner.requests()) != 1 || len(readHealthState(t, d).Failures) != 1 {
		t.Fatal("failed repair must retain original failure and cooldown")
	}
	if !strings.Contains(logs.String(), "unresolved") {
		t.Fatal("unresolved execution outcome lost")
	}
}

func TestDaemonHealthRepairAuthenticationReobservesBeforeProcessing(t *testing.T) {
	gh := &fakeGH{authenticatedLoginErr: errors.New("GitHub credential helper is broken")}
	runner := &healthRunner{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
	if err := d.tick(context.Background()); err == nil {
		t.Fatal("authentication failure must stay observable")
	}
	d.inFlight.Wait()
	if len(runner.requests()) != 1 || gh.listCalls != 0 {
		t.Fatal("broken authentication must be repaired before scanning")
	}
	if !strings.Contains(runner.requests()[0].PromptConfig.TaskPrompt, gh.authenticatedLoginErr.Error()) {
		t.Fatal("authentication diagnostic missing")
	}
	gh.authenticatedLoginErr = nil
	if err := d.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.authenticatedLogin != "sandman" || gh.listCalls != 1 || len(readHealthState(t, d).Failures) != 0 {
		t.Fatal("original authentication must be reobserved before progress")
	}
}

func TestDaemonHealthRepairOperationTimeoutIsNotOperatorCancellation(t *testing.T) {
	gh := &fakeGH{listErr: context.DeadlineExceeded}
	runner := &healthRunner{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
	if err := d.tick(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("operation timeout lost: %v", err)
	}
	d.inFlight.Wait()
	if len(runner.requests()) != 1 {
		t.Fatal("live daemon must repair an operation-local timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = d.tick(ctx)
	d.inFlight.Wait()
	if len(runner.requests()) != 1 {
		t.Fatal("operator cancellation must not launch another repair")
	}
}

func TestDaemonHealthRepairOwnerlessGraceIsNotRenewed(t *testing.T) {
	gh := &fakeGH{listErr: errors.New("remote broken")}
	runner := &healthRunner{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
	now := time.Now()
	firstObservation := now
	d.Clock = func() time.Time { return now }
	id := "261009123456-abcd-prompt-health-repair"
	if err := d.withHealthState(func(state *healthRepairState) error {
		*state = healthRepairState{Version: 1, Failures: map[string]healthFailure{"scan-list": {Operation: "list open PRs", Evidence: gh.listErr.Error()}}, Attempts: 1, RunID: id, AttemptPending: true, Deadline: now.Add(30 * time.Minute), NextAttempt: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	log := &events.JSONLLogger{Path: filepath.Join(d.BaseDir, "events.jsonl")}
	if err := log.Log(events.Event{Type: "run.started", RunID: id, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	for _, elapsed := range []time.Duration{0, 4 * time.Minute} {
		now = firstObservation.Add(elapsed)
		_ = d.tick(context.Background())
		d.inFlight.Wait()
		if len(runner.requests()) != 0 || !readHealthState(t, d).OwnerlessSince.Equal(firstObservation) {
			t.Fatal("restart observation must preserve original ownerless grace")
		}
	}
	now = firstObservation.Add(5 * time.Minute)
	_ = d.tick(context.Background())
	d.inFlight.Wait()
	states, err := events.ReadRunStates(log)
	if err != nil || states[id].Status() != "aborted" || len(runner.requests()) != 1 {
		t.Fatalf("expired grace did not settle and resume bounded repair: %+v %v", states[id], err)
	}
}

func TestDaemonHealthRepairCorruptAndSaturatedEvidenceFailClosed(t *testing.T) {
	for _, mode := range []string{"corrupt", "saturated"} {
		t.Run(mode, func(t *testing.T) {
			runner := &healthRunner{}
			d, logs, _ := newDaemonForTest(t, &fakeGH{listErr: errors.New("remote broken")}, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
			if err := os.MkdirAll(d.reviewsDir(), 0755); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt" {
				if err := os.WriteFile(d.healthStatePath(), []byte("{bad"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := d.withHealthState(func(state *healthRepairState) error {
					state.Attempts = healthRepairAttempts
					for i := 0; i < healthFailureLimit; i++ {
						state.Failures[string(rune('a'+i))] = healthFailure{Operation: "still broken"}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(d.healthStatePath())
			if err != nil {
				t.Fatal(err)
			}
			_ = d.tick(context.Background())
			d.inFlight.Wait()
			after, err := os.ReadFile(d.healthStatePath())
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) || len(runner.requests()) != 0 {
				t.Fatal("invalid/saturated evidence must not discard unresolved budget or authorize launch")
			}
			if !strings.Contains(logs.String(), "blocked") {
				t.Fatal("admission blocker must be logged")
			}
		})
	}
}

func TestDaemonHealthRepairOrdinaryFindingsAreHealthy(t *testing.T) {
	gh := &fakeGH{prs: []github.PR{{Number: 17}}, comments: map[int][]github.PRComment{17: {{ID: "finding", Body: "/sandman review", AuthorLogin: "sandman", CreatedAt: time.Now()}}}, prFetch: map[int]*github.PR{17: {Number: 17}}}
	decision := newDecisionRunner()
	decision.body = "## Decision\nCHANGES_REQUESTED\nFix the implementation."
	runner := &healthRunner{review: decision.RunBatch}
	d, logs, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured", WorktreeDir: ".sandman/worktrees"})
	if err := d.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.inFlight.Wait()
	if len(runner.requests()) != 0 || !d.IsTerminalSeen(17, "finding") {
		t.Fatalf("ordinary findings should complete reviewer without health repair; logs=%s", logs.String())
	}
}

func readHealthState(t *testing.T, d *Daemon) healthRepairState {
	t.Helper()
	data, err := os.ReadFile(d.healthStatePath())
	if err != nil {
		t.Fatal(err)
	}
	var state healthRepairState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestDaemonHealthRepairBoundedUnresolvedAcrossRestart(t *testing.T) {
	gh := &fakeGH{listErr: errors.New("remote broken")}
	runner := &healthRunner{repair: func(context.Context, batch.Request) (*batch.Result, error) {
		return nil, errors.New("repair binary unavailable")
	}}
	d, logs, dir := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
	now := time.Now()
	d.Clock = func() time.Time { return now }
	for attempt := 1; attempt <= healthRepairAttempts; attempt++ {
		_ = d.tick(context.Background())
		d.inFlight.Wait()
		if got := len(runner.requests()); got != attempt {
			t.Fatalf("attempt %d launches=%d", attempt, got)
		}
		state := readHealthState(t, d)
		if state.Attempts != attempt || state.AttemptPending || !strings.Contains(state.Outcome, "repair binary unavailable") {
			t.Fatalf("outcome not durable: %+v", state)
		}
		_ = d.tick(context.Background())
		d.inFlight.Wait()
		if len(runner.requests()) != attempt {
			t.Fatal("cooldown must suppress recursive/tight launches")
		}
		// Unrelated healthy operations do not replenish this episode's budget.
		d.resolveHealthOperation("request:999:unrelated")
		now = state.NextAttempt.Add(time.Second)
		d = New(dir, gh, &prompt.Engine{}, runner, d.Config, logs, 0, false, nil)
		d.Clock = func() time.Time { return now }
	}
	_ = d.tick(context.Background())
	d.inFlight.Wait()
	if len(runner.requests()) != healthRepairAttempts {
		t.Fatal("restart must retain exhausted budget")
	}
	if !strings.Contains(logs.String(), "repair binary unavailable") {
		t.Fatal("repair launch failure must remain observable")
	}
	// Actual recovery of the failed operation permits a later new episode.
	gh.listErr = nil
	if err := d.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	gh.listErr = errors.New("new failure")
	_ = d.tick(context.Background())
	d.inFlight.Wait()
	if len(runner.requests()) != healthRepairAttempts+1 {
		t.Fatal("verified original recovery should allow a fresh episode")
	}
}

func TestDaemonHealthRepairConcurrentOwnerAndCancellation(t *testing.T) {
	gh := &fakeGH{listErr: errors.New("remote broken")}
	entered, cancelled := make(chan struct{}), make(chan struct{})
	runner := &healthRunner{repair: func(ctx context.Context, _ batch.Request) (*batch.Result, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}}
	d, logs, dir := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = d.tick(ctx)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("repair not started")
	}
	restarted := New(dir, gh, &prompt.Engine{}, runner, d.Config, logs, 0, false, nil)
	var observations sync.WaitGroup
	for i := 0; i < 8; i++ {
		observations.Add(1)
		go func() {
			defer observations.Done()
			restarted.observeHealthFailure(ctx, "scan-list", "list open PRs", 0, gh.listErr)
		}()
	}
	observations.Wait()
	if len(runner.requests()) != 1 {
		t.Fatal("duplicate repair launched while owner active")
	}
	cancel()
	d.inFlight.Wait()
	select {
	case <-cancelled:
	default:
		t.Fatal("owner released before execution cancellation finished")
	}
	_ = d.tick(ctx)
	d.inFlight.Wait()
	if len(runner.requests()) != 1 {
		t.Fatal("cancelled daemon launched follow-on repair")
	}
	if !strings.Contains(readHealthState(t, d).Outcome, "cancelled") {
		t.Fatal("cancelled outcome not observable")
	}
}

func TestDaemonHealthRepairRestartReservationUsesEventLifecycle(t *testing.T) {
	for _, mode := range []string{"reserved", "running", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			gh := &fakeGH{listErr: errors.New("remote broken")}
			runner := &healthRunner{}
			d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
			now := time.Now()
			d.Clock = func() time.Time { return now }
			deadline := now.Add(time.Minute)
			id := "261009123456-abcd-prompt-health-repair"
			if err := d.withHealthState(func(state *healthRepairState) error {
				*state = healthRepairState{Version: 1, Failures: map[string]healthFailure{"scan-list": {Operation: "list open PRs", Evidence: gh.listErr.Error()}}, Attempts: 1, RunID: id, AttemptPending: true, Deadline: deadline, NextAttempt: deadline}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if mode != "reserved" {
				log := &events.JSONLLogger{Path: filepath.Join(d.BaseDir, "events.jsonl")}
				if err := log.Log(events.Event{Type: "run.started", RunID: id, Timestamp: now}); err != nil {
					t.Fatal(err)
				}
				if mode == "terminal" {
					if err := log.Log(events.Event{Type: "run.finished", RunID: id, Timestamp: now, Payload: map[string]any{"status": "failure"}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			_ = d.tick(context.Background())
			d.inFlight.Wait()
			if len(runner.requests()) != 0 {
				t.Fatal("restart renewed reserved deadline or bypassed cooldown")
			}
			now = deadline.Add(time.Second)
			_ = d.tick(context.Background())
			d.inFlight.Wait()
			want := 1
			if len(runner.requests()) != want {
				t.Fatalf("%s restart launches=%d want=%d", mode, len(runner.requests()), want)
			}
			if mode == "running" {
				states, err := events.ReadRunStates(d.healthEvents)
				if err != nil || states[id].Status() != "aborted" {
					t.Fatalf("orphan lifecycle not safely settled: %+v %v", states[id], err)
				}
			}
		})
	}
}

func TestDaemonHealthRepairStrandedStartupResumesOriginalRequest(t *testing.T) {
	gh := &fakeGH{prs: []github.PR{{Number: 17, State: "open", UpdatedAt: time.Now()}},
		comments: map[int][]github.PRComment{17: {{ID: "stranded", Body: "/sandman review", AuthorLogin: "sandman", CreatedAt: time.Now()}}},
		prFetch:  map[int]*github.PR{17: {Number: 17, Title: "repair"}}}
	decision := newDecisionRunner()
	broken := true
	runner := &healthRunner{}
	runner.review = func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		if broken {
			return nil, errors.New("start sandbox for prompt-only run: delete branch from stranded worktree at /repo/.sandman/worktrees/review-17-stranded: exit status 128\nfatal: not a git repository: (null)")
		}
		return decision.RunBatch(ctx, req)
	}
	runner.repair = func(_ context.Context, req batch.Request) (*batch.Result, error) {
		return &batch.Result{Runs: []batch.AgentRunResult{{RunID: req.RunID, Status: "success"}}}, nil
	}
	d, logs, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured", WorktreeDir: ".sandman/worktrees"})
	if err := d.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.inFlight.Wait()
	if len(runner.requests()) != 1 {
		t.Fatalf("expected stranded startup repair; logs=%s", logs.String())
	}
	if !strings.Contains(runner.requests()[0].PromptConfig.TaskPrompt, "fatal: not a git repository: (null)") {
		t.Fatal("startup diagnostic lost")
	}
	if d.IsTerminalSeen(17, "stranded") {
		t.Fatal("repair exit must not complete request")
	}
	broken = false
	if !d.shouldReadComments(gh.prs[0]) {
		t.Fatal("failed original request must be observed despite unchanged updatedAt")
	}
	if err := d.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.inFlight.Wait()
	if !d.IsTerminalSeen(17, "stranded") {
		t.Fatalf("original review did not resume; logs=%s", logs.String())
	}
	if d.hasHealthFailure(17) {
		t.Fatal("verified original success should retire request failure")
	}
}

func (r *healthRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	if r.review != nil {
		return r.review(ctx, req)
	}
	return &batch.Result{}, nil
}

func (r *healthRunner) RunRepair(ctx context.Context, req batch.Request, _ *config.Config) (*batch.Result, error) {
	r.mu.Lock()
	r.repairs = append(r.repairs, req)
	r.mu.Unlock()
	if r.repair != nil {
		return r.repair(ctx, req)
	}
	return &batch.Result{Runs: []batch.AgentRunResult{{Status: "success", RunID: req.RunID}}}, nil
}

func (r *healthRunner) requests() []batch.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]batch.Request(nil), r.repairs...)
}

func TestDaemonHealthRepairListFailure(t *testing.T) {
	gh := &fakeGH{listErr: errors.New("git remote configuration unavailable")}
	runner := &healthRunner{}
	d, logs, dir := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
	d.Model = "openai/override"
	if err := d.tick(context.Background()); err == nil {
		t.Fatal("operational error must remain observable")
	}
	d.inFlight.Wait()
	reqs := runner.requests()
	if len(reqs) != 1 {
		t.Fatalf("repair launches = %d, want 1; logs=%s", len(reqs), logs.String())
	}
	req := reqs[0]
	if req.Agent != "opencode" || req.Model != "openai/override" || req.Review || len(req.Issues) != 0 {
		t.Fatalf("repair configuration = %+v", req)
	}
	for _, evidence := range []string{"list open PRs", gh.listErr.Error(), dir, "events.jsonl", "diagnose", "verify", "review progress"} {
		if !strings.Contains(strings.ToLower(req.PromptConfig.TaskPrompt), strings.ToLower(evidence)) {
			t.Errorf("repair prompt missing %q: %s", evidence, req.PromptConfig.TaskPrompt)
		}
	}
}

func TestDaemonHealthRepairPromptKeepsTemplateBracesLiteral(t *testing.T) {
	malformed := errors.New("missing substitution keys: {{UNKNOWN_KEY}}")
	gh := &fakeGH{listErr: malformed}
	runner := &healthRunner{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
	if err := d.tick(context.Background()); err == nil {
		t.Fatal("malformed template failure must stay observable")
	}
	d.inFlight.Wait()
	reqs := runner.requests()
	if len(reqs) != 1 {
		t.Fatalf("repair launches = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.PromptConfig.PromptFlag != "" {
		t.Fatalf("diagnostic must not use template PromptFlag: %q", req.PromptConfig.PromptFlag)
	}
	if !strings.Contains(req.PromptConfig.TaskPrompt, "{{UNKNOWN_KEY}}") {
		t.Fatalf("literal evidence lost: %q", req.PromptConfig.TaskPrompt)
	}
}

func TestDaemonHealthRepairStartupResolvesAuthentication(t *testing.T) {
	gh := &fakeGH{}
	runner := &healthRunner{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "openai/configured"})
	exhausted := healthRepairState{Version: 1, Attempts: healthRepairAttempts,
		Failures: map[string]healthFailure{"authentication": {Operation: "authenticated GitHub login", Evidence: "old credential failure", ObservedAt: time.Now()}}}
	data, err := json.Marshal(exhausted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(d.healthStatePath()), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.healthStatePath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	d.Trigger = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	d.inFlight.Wait()
	state := readHealthState(t, d)
	if _, ok := state.Failures["authentication"]; ok {
		t.Fatalf("startup login did not retire authentication: %+v", state.Failures)
	}
	gh.listErr = errors.New("new remote failure")
	if err := d.tick(context.Background()); err == nil {
		t.Fatal("new failure must stay observable")
	}
	d.inFlight.Wait()
	if len(runner.requests()) != 1 {
		t.Fatalf("verified startup recovery must allow fresh episode, launches=%d", len(runner.requests()))
	}
}
