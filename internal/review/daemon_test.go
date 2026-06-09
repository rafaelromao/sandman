package review

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// fakeGH is a test double for the GitHubClient surface area used by the
// review daemon.
type fakeGH struct {
	mu         sync.Mutex
	prs        []github.PR
	comments   map[int][]github.PRComment
	prFetch    map[int]*github.PR
	listErr    error
	commentErr map[int]error
	fetchErr   map[int]error
	listCalls  int
}

func (f *fakeGH) ListOpenPRs() ([]github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.prs, f.listErr
}

func (f *fakeGH) ListPRComments(number int) ([]github.PRComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentErr != nil {
		if err, ok := f.commentErr[number]; ok {
			return nil, err
		}
	}
	return f.comments[number], nil
}

func (f *fakeGH) FetchPR(number int) (*github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil {
		if err, ok := f.fetchErr[number]; ok {
			return nil, err
		}
	}
	if pr, ok := f.prFetch[number]; ok {
		return pr, nil
	}
	return &github.PR{Number: number, Title: "T", Body: "B"}, nil
}

type capturedRequest struct {
	calls int
	mu    sync.Mutex
	last  batch.Request
}

func (c *capturedRequest) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.last = req
	return &batch.Result{}, nil
}

func newDaemonForTest(t *testing.T, gh GitHubClient, runner BatchRunner, cfg *config.Config) (*Daemon, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	var buf bytes.Buffer
	d := New(dir, gh, &prompt.Engine{}, runner, cfg, &buf)
	d.PollInterval = 0
	return d, &buf, dir
}

func TestDaemon_TickLaunchesReviewForTriggerComment(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review focus on tests"},
				{ID: "101", Body: "unrelated comment"},
			},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "PR 42", Body: "Body of 42"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}
	if runner.last.Agent != "opencode" {
		t.Errorf("expected review agent 'opencode', got %q", runner.last.Agent)
	}
	if runner.last.Sandbox != "worktree" {
		t.Errorf("expected sandbox 'worktree', got %q", runner.last.Sandbox)
	}
	if !strings.Contains(runner.last.PromptConfig.PromptFlag, "PR 42") {
		t.Errorf("rendered prompt should mention PR title, got: %q", runner.last.PromptConfig.PromptFlag)
	}
	if !strings.Contains(runner.last.PromptConfig.PromptFlag, "focus on tests") {
		t.Errorf("rendered prompt should contain focus, got: %q", runner.last.PromptConfig.PromptFlag)
	}
	if !runner.last.Review {
		t.Errorf("expected Review=true on daemon review batch request, got false")
	}
	if runner.last.PRNumber != 42 {
		t.Errorf("expected PRNumber=42 on daemon review batch request, got %d", runner.last.PRNumber)
	}
	if runner.last.ReviewFocus != "focus on tests" {
		t.Errorf("expected ReviewFocus='focus on tests', got %q", runner.last.ReviewFocus)
	}

	dir := d.PRDir(42)
	seenPath := filepath.Join(dir, "seen-comments.jsonl")
	data, err := os.ReadFile(seenPath)
	if err != nil {
		t.Fatalf("read seen file: %v", err)
	}
	if !strings.Contains(string(data), "100") {
		t.Errorf("seen file should contain 100, got %q", string(data))
	}
}

func TestDaemon_TickSkipsSeenComment(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {{ID: "100", Body: "/sandman review again"}},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{})

	dir := d.PRDir(42)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seen-comments.jsonl"), []byte("100\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runner.calls != 0 {
		t.Errorf("expected no batch runs, got %d", runner.calls)
	}
}

func TestDaemon_TickCaseInsensitive(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 7, State: "open"}},
		comments: map[int][]github.PRComment{
			7: {{ID: "1", Body: "/SANDMAN Review please"}},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{})

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("expected 1 batch run, got %d", runner.calls)
	}
}

func TestDaemon_StartSocketCreatesReviewSock(t *testing.T) {
	gh := &fakeGH{}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{})

	if err := d.StartSocket(); err != nil {
		t.Fatalf("StartSocket: %v", err)
	}
	defer d.Stop()

	conn, err := net.Dial("unix", d.SocketPath())
	if err != nil {
		t.Fatalf("connect to review.sock: %v", err)
	}
	conn.Close()
}

func TestDaemon_RunRespondsToTrigger(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{})

	trigger := make(chan struct{}, 4)
	d.Trigger = trigger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()

	trigger <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gh.mu.Lock()
		calls := gh.listCalls
		gh.mu.Unlock()
		if calls >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	gh.mu.Lock()
	calls := gh.listCalls
	gh.mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected exactly 1 list call, got %d", calls)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestDaemon_StopCancelsInflightBatch(t *testing.T) {
	gh := &fakeGH{
		prs: []github.PR{{Number: 1, State: "open"}},
		comments: map[int][]github.PRComment{
			1: {{ID: "x", Body: "/sandman review"}},
		},
	}

	released := make(chan struct{})
	started := make(chan struct{})
	blockingRunner := func(ctx context.Context, req batch.Request) (*batch.Result, error) {
		close(started)
		<-ctx.Done()
		<-released
		return nil, ctx.Err()
	}

	gh.prFetch = map[int]*github.PR{1: {Number: 1, Title: "T", Body: "B"}}
	d, _, _ := newDaemonForTest(t, gh, batchFunc(blockingRunner), &config.Config{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	<-started
	close(released)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

type batchFunc func(ctx context.Context, req batch.Request) (*batch.Result, error)

func (f batchFunc) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	return f(ctx, req)
}

func TestDaemon_ListOpenPRsErrorIsLogged(t *testing.T) {
	gh := &fakeGH{listErr: errList}
	runner := &capturedRequest{}
	d, buf, _ := newDaemonForTest(t, gh, runner, &config.Config{})

	trigger := make(chan struct{}, 1)
	d.Trigger = trigger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()

	trigger <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "list open PRs") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(buf.String(), "list open PRs") {
		t.Errorf("expected log to mention list open PRs, got %q", buf.String())
	}
}

var errList = jsonError("list failed")

type jsonError string

func (e jsonError) Error() string { return string(e) }

func TestDaemon_RunFailsFastOnInvalidReviewAgent(t *testing.T) {
	gh := &fakeGH{}
	runner := &capturedRequest{}
	d, buf, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "nonexistent-agent",
	})

	trigger := make(chan struct{}, 1)
	d.Trigger = trigger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Run to return an error for invalid review agent")
		}
		if !strings.Contains(err.Error(), "nonexistent-agent") {
			t.Errorf("expected error to mention agent name, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Run did not return for invalid review agent")
	}

	cancel()
	if !strings.Contains(buf.String(), "review agent validation failed") {
		t.Errorf("expected validation log, got %q", buf.String())
	}
}
