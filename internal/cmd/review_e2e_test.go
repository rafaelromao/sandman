//go:build e2e

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/review"
)

// writeReviewDaemonGHShim writes a gh shim that returns one open PR with a
// trigger comment, responds to repo view / pr view, and records gh pr comment
// calls so the test can verify the review was submitted.
func writeReviewDaemonGHShim(t *testing.T, dir, triggerCommentID, triggerBody string) {
	t.Helper()

	script := `#!/bin/sh
set -eu
shim_dir="` + dir + `"
case "${1:-}" in
  repo)
    echo '{"name":"sandbox","owner":{"login":"example"}}'
    exit 0 ;;
  pr)
    case "${2:-}" in
      list)
        echo '[{"number":1,"state":"open","mergedAt":null,"headRefName":"feature-x","headRefOid":"0000000000000000000000000000000000000000"}]' ;;
      view)
        echo '{"number":1,"title":"Test PR","body":"A test pull request","state":"open","mergedAt":null,"headRefName":"feature-x","headRefOid":"0000000000000000000000000000000000000000"}' ;;
      comment)
        c=$(cat "$shim_dir/gh-comment.count" 2>/dev/null || echo 0)
        echo $((c+1)) > "$shim_dir/gh-comment.count"
        while [ $# -gt 0 ]; do case "$1" in --body) shift; printf '%s\n' "${1:-}" > "$shim_dir/gh-comment.body";; esac; shift; done
        echo "commented" ;;
    esac
    exit 0 ;;
  api)
    shift
    path=""
    for a do [ -z "$path" ] && case "$a" in --*) ;; *) path="$a" ;; esac; done
    case "$path" in
      *repos*issues*comments*)
        cat <<JSON
[{"id":` + triggerCommentID + `,"body":"` + triggerBody + `","user":{"login":"user1"}}]
JSON
        ;;
      *) echo "[]" ;;
    esac
    exit 0 ;;
  auth) exit 0 ;;
esac
echo "unhandled gh: $*" >&2
exit 1
`

	binPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
}

func writeParallelReviewDaemonGHShim(t *testing.T, dir string, prs []int) {
	t.Helper()

	var prList strings.Builder
	prList.WriteString("[")
	for i, pr := range prs {
		if i > 0 {
			prList.WriteString(",")
		}
		fmt.Fprintf(&prList, `{"number":%d,"state":"open","title":"PR %d","body":"Body %d","mergedAt":null,"headRefName":"feature-%d","headRefOid":"0000000000000000000000000000000000000000"}`, pr, pr, pr, pr)
	}
	prList.WriteString("]")

	var viewCases strings.Builder
	var commentCases strings.Builder
	for _, pr := range prs {
		fmt.Fprintf(&viewCases, "      %d) echo '{\"number\":%d,\"title\":\"PR %d\",\"body\":\"Body %d\",\"state\":\"open\",\"mergedAt\":null,\"headRefName\":\"feature-%d\",\"headRefOid\":\"0000000000000000000000000000000000000000\"}' ; exit 0 ;;\n", pr, pr, pr, pr, pr)
		fmt.Fprintf(&commentCases, "      *issues/%d/comments*) echo '[{\"id\":%d00,\"body\":\"/sandman review parallel %d\",\"created_at\":\"2026-07-08T12:00:00Z\",\"user\":{\"login\":\"user%d\"}}]' ; exit 0 ;;\n", pr, pr, pr, pr)
	}

	script := `#!/bin/sh
set -eu
case "${1:-}" in
  repo)
    echo '{"name":"sandbox","owner":{"login":"example"}}'
    exit 0 ;;
  pr)
    case "${2:-}" in
      list)
        echo '` + prList.String() + `'
        exit 0 ;;
      view)
        number=""
        for a do case "$a" in ''|*[!0-9]*) ;; *) number="$a" ;; esac; done
        case "$number" in
` + viewCases.String() + `        esac
        ;;
    esac
    ;;
  api)
    shift
    method="GET"
    path=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -X) shift; method="${1:-}" ;;
        -*) ;;
        *) [ -z "$path" ] && path="$1" ;;
      esac
      shift
    done
    case "$method:$path" in
      POST:*reactions) echo '1'; exit 0 ;;
      DELETE:*reactions*) exit 0 ;;
      GET:*)
        case "$path" in
` + commentCases.String() + `        esac
        echo '[]'
        exit 0 ;;
    esac
    ;;
  auth) exit 0 ;;
esac
echo "unhandled gh: $*" >&2
exit 1
`

	binPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
}

// TestReviewDaemonE2E_TriggerCommentLaunchesReview verifies that the review
// daemon polls open PRs, detects a /sandman review comment, and launches a
// batch review with the correct agent, model, PR number, and focus. It mocks
// gh via a shell shim and uses a fake batch runner to capture the request.
func TestReviewDaemonE2E_TriggerCommentLaunchesReview(t *testing.T) {
	repoDir := t.TempDir()
	initRunIntegrationRepo(t, repoDir)

	shimDir := t.TempDir()
	writeReviewDaemonGHShim(t, shimDir, "100", "/sandman review check tests")

	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "fake")
	t.Setenv("GITHUB_TOKEN", "fake")
	t.Setenv("SANDMAN_TEST_MODEL_OPENCODE", "opencode-go/deepseek-v4-flash")

	ghClient := &github.CLIClient{}
	runner := &reviewE2ECapturedRequest{}
	cfg := &config.Config{
		DefaultModel:       "opencode-go/deepseek-v4-flash",
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode-go/deepseek-v4-flash",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	broadcaster := daemon.NewBroadcaster()
	d := review.New(repoDir, ghClient, &prompt.Engine{}, runner, cfg, broadcaster, 0, false, nil)
	d.PollInterval = 0

	if err := d.StartSocket(); err != nil {
		t.Fatalf("StartSocket: %v", err)
	}
	defer d.Stop()

	trigger := make(chan struct{}, 1)
	d.Trigger = trigger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	trigger <- struct{}{}

	var captured batch.Request
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		c := runner.calls
		req := runner.last
		runner.mu.Unlock()
		if c > 0 {
			captured = req
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop after cancel")
	}

	if runner.calls == 0 {
		t.Fatal("expected at least 1 batch run, got 0")
	}
	if captured.Agent != "opencode" {
		t.Errorf("expected agent 'opencode', got %q", captured.Agent)
	}
	if !strings.Contains(captured.Model, "opencode-go") {
		t.Errorf("expected model to contain 'opencode-go', got %q", captured.Model)
	}
	if !captured.Review {
		t.Errorf("expected Review=true, got false")
	}
	if captured.PRNumber != 1 {
		t.Errorf("expected PRNumber=1, got %d", captured.PRNumber)
	}
	if captured.ReviewFocus != "check tests" {
		t.Errorf("expected ReviewFocus='check tests', got %q", captured.ReviewFocus)
	}
	if !strings.Contains(captured.PromptConfig.PromptFlag, "Test PR") {
		t.Errorf("prompt should mention 'Test PR', got: %q", captured.PromptConfig.PromptFlag)
	}

	commentCountFile := filepath.Join(shimDir, "gh-comment.count")
	if _, err := os.Stat(commentCountFile); err == nil {
		data, _ := os.ReadFile(commentCountFile)
		t.Logf("gh pr comment was called %s time(s)", strings.TrimSpace(string(data)))
	}
}

func TestReviewDaemonE2E_ProcessesFourRequestsInParallel(t *testing.T) {
	repoDir := t.TempDir()
	initRunIntegrationRepo(t, repoDir)

	shimDir := t.TempDir()
	writeParallelReviewDaemonGHShim(t, shimDir, []int{1, 2, 3, 4})

	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "fake")
	t.Setenv("GITHUB_TOKEN", "fake")
	t.Setenv("SANDMAN_TEST_MODEL_OPENCODE", "opencode-go/deepseek-v4-flash")

	runner := newParallelReviewE2ERunner(4)
	cfg := &config.Config{
		DefaultModel:          "opencode-go/deepseek-v4-flash",
		DefaultAgent:          "opencode",
		DefaultReviewAgent:    "opencode",
		DefaultReviewModel:    "opencode-go/deepseek-v4-flash",
		DefaultReviewParallel: 4,
		WorktreeDir:           filepath.Join(repoDir, ".sandman", "worktrees"),
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	deps := Dependencies{
		BatchRunner:  runner,
		ConfigStore:  &fakeStore{config: cfg},
		EventLog:     &fakeEventLog{},
		GitHubClient: &github.CLIClient{},
		Renderer:     &prompt.Engine{},
		IssuePicker:  &fakeIssuePicker{},
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}
	cmd := NewReviewCmd(deps)
	cmd.SetContext(ctx)
	cmd.SetArgs(nil)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	defer func() {
		cancel()
		runner.Release()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("daemon run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not stop after cancel")
		}
	}()

	runner.WaitForConcurrent(t, 4)

	if got := runner.MaxConcurrent(); got != 4 {
		t.Fatalf("expected 4 concurrent review runs, got %d", got)
	}
}

type reviewE2ECapturedRequest struct {
	mu    sync.Mutex
	calls int
	last  batch.Request
}

func (c *reviewE2ECapturedRequest) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.last = req
	return &batch.Result{}, nil
}

type parallelReviewE2ERunner struct {
	mu            sync.Mutex
	current       int
	maxConcurrent int
	want          int
	reached       chan struct{}
	reachedOnce   sync.Once
	release       chan struct{}
}

func newParallelReviewE2ERunner(want int) *parallelReviewE2ERunner {
	return &parallelReviewE2ERunner{
		want:    want,
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *parallelReviewE2ERunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	r.mu.Lock()
	r.current++
	if r.current > r.maxConcurrent {
		r.maxConcurrent = r.current
	}
	if r.maxConcurrent >= r.want {
		r.reachedOnce.Do(func() { close(r.reached) })
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.current--
		r.mu.Unlock()
	}()

	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return &batch.Result{}, nil
}

func (r *parallelReviewE2ERunner) WaitForConcurrent(t *testing.T, want int) {
	t.Helper()
	select {
	case <-r.reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %d concurrent review runs; max was %d", want, r.MaxConcurrent())
	}
}

func (r *parallelReviewE2ERunner) MaxConcurrent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxConcurrent
}

func (r *parallelReviewE2ERunner) Release() {
	select {
	case <-r.release:
	default:
		close(r.release)
	}
}
