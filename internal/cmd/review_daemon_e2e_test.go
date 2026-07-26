//go:build e2e

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/review"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// writeReviewDaemonRealAgentGHShim writes a gh shim that returns one open PR
// with a /sandman review trigger comment, records gh pr comment calls, and
// handles the subcommands the daemon invokes.
func writeReviewDaemonRealAgentGHShim(t *testing.T, dir, triggerCommentID, triggerBody string) {
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
      user) echo '{"login":"user1"}' ;;
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

// containerReviewRunner is a BatchRunner that runs opencode inside a podman
// container to produce a review decision. It bypasses the real batch
// orchestrator (sandbox creation, event logs, container lifecycle) so the
// test focuses on the daemon's trigger-to-post pipeline.
type containerReviewRunner struct {
	repoDir  string
	imageTag string
	model    string
}

func (r *containerReviewRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	branch := req.PromptConfig.Branch
	worktreePath := filepath.Join(r.repoDir, ".sandman", "worktrees", branch)
	decisionPath := filepath.Join(worktreePath, "decision.md")

	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		return nil, fmt.Errorf("create review worktree: %w", err)
	}

	promptContent := req.PromptConfig.PromptFlag
	promptFile := filepath.Join(worktreePath, "prompt.md")
	if err := os.WriteFile(promptFile, []byte(promptContent), 0644); err != nil {
		return nil, fmt.Errorf("write prompt file: %w", err)
	}

	agentCmd := fmt.Sprintf(
		`opencode run --pure --dangerously-skip-permissions -m %s "/workspace/.sandman/worktrees/%s/prompt.md"`,
		r.model, branch,
	)

	cmd := exec.CommandContext(ctx, "podman", "run", "--rm",
		"-v", r.repoDir+":/workspace:Z",
		"--workdir", "/workspace/.sandman/worktrees/"+branch,
		r.imageTag,
		"sh", "-c", agentCmd,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run opencode in container: %w\noutput: %s", err, output)
	}

	if err := os.WriteFile(decisionPath, output, 0644); err != nil {
		return nil, fmt.Errorf("write decision.md: %w", err)
	}

	return &batch.Result{}, nil
}

func TestReviewDaemonE2E_RealAgentInContainer(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioReviewDaemon) {
		t.Skip("set SANDMAN_E2E_GATES=review_daemon (or all) to run review_daemon e2e tests")
	}

	if os.Getenv("SANDMAN_RUN_AGENT_E2E") != "1" {
		t.Skip("skip review_daemon real-agent e2e: SANDMAN_RUN_AGENT_E2E=1 not set")
	}

	if _, err := exec.LookPath("podman"); err != nil {
		t.Skipf("skip review_daemon e2e: podman unavailable: %v", err)
	}

	opencodePath, err := exec.LookPath("opencode")
	if err != nil {
		t.Skipf("skip review_daemon e2e: opencode binary not found on PATH: %v", err)
	}

	repoDir := t.TempDir()
	initRunIntegrationRepo(t, repoDir)

	sandmanDir := filepath.Join(repoDir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatalf("create .sandman dir: %v", err)
	}

	model := testenv.ResolveTestModel("opencode", "opencode/big-pickle")
	imageTag := fmt.Sprintf("sandman-review-daemon-e2e-%d:latest", time.Now().UnixNano())

	binDir := filepath.Join(sandmanDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("create .sandman/bin dir: %v", err)
	}
	containerOpencodePath := filepath.Join(binDir, "opencode")
	if err := copyFile(opencodePath, containerOpencodePath); err != nil {
		t.Fatalf("copy opencode binary to build context: %v", err)
	}

	dockerfile := `FROM docker.io/ubuntu:latest
RUN apt-get update && apt-get install -y git ca-certificates && rm -rf /var/lib/apt/lists/*
COPY .sandman/bin/opencode /usr/local/bin/opencode
RUN chmod +x /usr/local/bin/opencode
WORKDIR /workspace
`
	if err := os.WriteFile(filepath.Join(sandmanDir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	buildCmd := exec.Command("podman", "build", "-t", imageTag,
		"-f", filepath.Join(sandmanDir, "Dockerfile"), repoDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("podman build: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("podman", "rmi", "-f", imageTag).Run()
	})

	worktreeDir := filepath.Join(".sandman", "worktrees")
	cfg := &config.Config{
		DefaultModel:       model,
		DefaultAgent:       "opencode",
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: model,
		WorktreeDir:        worktreeDir,
		Sandbox:            "podman",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Preset: "opencode", Command: "opencode"},
	}

	shimDir := t.TempDir()
	writeReviewDaemonRealAgentGHShim(t, shimDir, "100", "/sandman review check tests")

	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "fake")
	t.Setenv("GITHUB_TOKEN", "fake")

	ghClient := &github.CLIClient{}
	runner := &containerReviewRunner{
		repoDir:  repoDir,
		imageTag: imageTag,
		model:    model,
	}
	poster := github.NewGHCommentPoster(ghClient)

	broadcaster := daemon.NewBroadcaster()
	d := review.New(repoDir, ghClient, &prompt.Engine{}, runner, cfg, broadcaster, 0, false, poster)
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

	var posted bool
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		commentCountFile := filepath.Join(shimDir, "gh-comment.count")
		if data, err := os.ReadFile(commentCountFile); err == nil {
			count := strings.TrimSpace(string(data))
			if count != "" && count != "0" {
				posted = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after cancel")
	}

	if !posted {
		commentBodyFile := filepath.Join(shimDir, "gh-comment.body")
		if body, err := os.ReadFile(commentBodyFile); err == nil {
			t.Logf("gh-comment.body content: %s", body)
		}
	}

	if !posted {
		t.Fatal("expected gh pr comment to be posted, but it was not")
	}

	commentBodyFile := filepath.Join(shimDir, "gh-comment.body")
	body, err := os.ReadFile(commentBodyFile)
	if err != nil {
		t.Fatalf("read gh-comment.body: %v", err)
	}
	bodyStr := strings.TrimSpace(string(body))
	if len(bodyStr) == 0 {
		t.Fatal("expected non-empty review comment body")
	}
	t.Logf("review comment body (%d bytes)", len(bodyStr))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}
	return out.Sync()
}
