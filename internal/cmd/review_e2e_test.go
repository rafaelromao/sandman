//go:build e2e

package cmd

import (
	"context"
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
	d := review.New(repoDir, ghClient, &prompt.Engine{}, runner, cfg, broadcaster, 0, false)
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
