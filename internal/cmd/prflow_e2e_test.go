//go:build e2e

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/scaffold"
)

const (
	prFlowIssueNumber = 1
	prFlowBranch      = "sandman/1-fix-failing-test"
)

func TestPRFlow_PodmanSandboxOpencodeCommitsAndPushes(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	allowed, err := parseE2EProviders()
	if err != nil {
		t.Fatal(err)
	}
	if !allowed["opencode"] {
		t.Skip("set SANDMAN_E2E_PROVIDERS=opencode and run `go test -tags e2e ./internal/cmd -run PRFlow`")
	}

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	if !hasOpenCodeAuth(realHome) {
		t.Skipf("skip opencode e2e: missing auth under %s", realHome)
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("skip opencode e2e: host CLI unavailable: %v", err)
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skipf("skip podman e2e: podman unavailable: %v", err)
	}

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	runRootCommand(t, prFlowDeps(repoDir), "init")
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	// Create the bare remote inside the repo so it's accessible inside the container.
	remoteDir := filepath.Join(repoDir, ".sandman", "remote")
	if err := os.MkdirAll(remoteDir, 0755); err != nil {
		t.Fatalf("create remote dir: %v", err)
	}
	bareInit := exec.Command("git", "init", "--bare")
	bareInit.Dir = remoteDir
	if out, err := bareInit.CombinedOutput(); err != nil {
		t.Fatalf("init bare remote: %v: %s", err, out)
	}
	runGit(t, repoDir, "remote", "add", "origin", remoteDir)
	runGit(t, repoDir, "push", "-u", "origin", "main")

	seedPRFlowRepo(t, repoDir)
	runGit(t, repoDir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")
	baselineHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

	homeDir, err := os.MkdirTemp("", "sandman-podman-e2e-")
	if err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0755); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	// Use the host absolute path in insteadOf; rewriteGitPaths will rewrite
	// it to the container path inside the container.
	absRepo, _ := filepath.Abs(repoDir)
	gitConfigContent := fmt.Sprintf("[user]\n\tname = Test\n\temail = test@test.com\n[url %q]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n",
		"file://"+filepath.Join(absRepo, ".sandman", "remote"))
	if err := os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitConfigContent), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	// Link real opencode config into the isolated home so the container mounts it
	for _, dir := range []string{".config/opencode", ".local/share/opencode"} {
		src := filepath.Join(realHome, dir)
		dst := filepath.Join(homeDir, dir)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatalf("create parent for %s: %v", dst, err)
		}
		if err := os.Symlink(src, dst); err != nil {
			t.Fatalf("link %s: %v", dir, err)
		}
	}
	t.Setenv("HOME", homeDir)
	// Warm podman with isolated HOME so image cache lives outside repo tree
	if out, err := exec.Command("podman", "run", "--rm", "alpine", "echo", "ok").CombinedOutput(); err != nil {
		t.Fatalf("warm podman image for test home: %v: %s", err, out)
	}

	ghShimDir := t.TempDir()
	writeFakeGHShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	deps := prFlowDeps(repoDir)

	containerGhShimDir := filepath.Join(repoDir, ".sandman", "bin")
	writeFakeGHShimForContainer(t, containerGhShimDir)

	patchDockerfileToHostOpenCodeVersion(t, repoDir)

	// Build the image and detect the model from inside the container (models
	// available may differ from the host's cached model list).
	buildCmd := exec.Command("podman", "build", "-t", "sandman-e2e-model-detect", "-f",
		filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build image for model detection: %v: %s", err, out)
	}
	modelOut, err := exec.Command("podman", "run", "--rm", "sandman-e2e-model-detect",
		"opencode", "models").Output()
	if err != nil {
		t.Fatalf("detect model in container: %v", err)
	}
	containerModel := pickModel(strings.TrimSpace(string(modelOut)))
	if containerModel == "" {
		t.Fatal("no model available in container")
	}
	t.Logf("using container model: %s", containerModel)

	customizeOpenCodeAgentForContainer(t, repoDir, containerModel)
	writePRFlowPrompt(t, repoDir)

	_, err = runRootCommand(t, deps, "run", "--sandbox", "podman", "--preserve", strconv.Itoa(prFlowIssueNumber))
	t.Logf("sandman run returned err=%v", err)

	logPath := filepath.Join(repoDir, ".sandman", "logs", fmt.Sprintf("%d.log", prFlowIssueNumber))
	logData, logErr := os.ReadFile(logPath)
	if logErr != nil {
		t.Fatalf("read log: %v", logErr)
	}
	log := string(logData)

	if !strings.Contains(log, "https://example.test/example/sandbox/pull/1") {
		t.Fatalf("expected fake PR URL in log, got:\n%s", log)
	}
	if !strings.Contains(log, "To file:///workspace/.sandman/remote") {
		t.Fatalf("expected git push output in log, got:\n%s", log)
	}

	argsData, err := os.ReadFile(filepath.Join(containerGhShimDir, "pr-create.args"))
	if err != nil {
		t.Fatalf("read pr create args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if got := prFlowFlagValue(args, "--base"); got != "main" {
		t.Fatalf("pr create --base: got %q, want %q", got, "main")
	}
	if got := prFlowFlagValue(args, "--head"); got != prFlowBranch {
		t.Fatalf("pr create --head: got %q, want %q", got, prFlowBranch)
	}
	if got := prFlowFlagValue(args, "--title"); got != "Fix failing test" {
		t.Fatalf("pr create --title: got %q, want %q", got, "Fix failing test")
	}

	bodyData, err := os.ReadFile(filepath.Join(containerGhShimDir, "pr-create.body"))
	if err != nil {
		t.Fatalf("read pr create body: %v", err)
	}
	if got := strings.TrimSpace(string(bodyData)); got != "Fixes #1" {
		t.Fatalf("pr create body: got %q, want %q", got, "Fixes #1")
	}

	countData, err := os.ReadFile(filepath.Join(containerGhShimDir, "pr-create.count"))
	if err != nil {
		t.Fatalf("read pr create count: %v", err)
	}
	if got := strings.TrimSpace(string(countData)); got != "1" {
		t.Fatalf("expected exactly one pr create invocation, got %q", got)
	}

	branchHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", prFlowBranch))
	if branchHash == baselineHash {
		t.Fatalf("expected branch commit beyond baseline, got %s", branchHash)
	}
	if out, err := exec.Command("git", "merge-base", "--is-ancestor", baselineHash, branchHash).CombinedOutput(); err != nil {
		t.Fatalf("expected branch commit to descend from baseline: %v\n%s", err, out)
	}

	remoteHash := strings.TrimSpace(runGit(t, repoDir, "ls-remote", "origin", "refs/heads/"+prFlowBranch))
	if remoteHash == "" {
		t.Fatal("expected pushed remote branch")
	}
	fields := strings.Fields(remoteHash)
	if len(fields) == 0 || fields[0] != branchHash {
		t.Fatalf("remote branch hash mismatch: got %q, want %q", remoteHash, branchHash)
	}
}

func TestPRFlow_WorktreeSandboxOpencodeCommitsAndPushes(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	allowed, err := parseE2EProviders()
	if err != nil {
		t.Fatal(err)
	}
	if !allowed["opencode"] {
		t.Skip("set SANDMAN_E2E_PROVIDERS=opencode and run `go test -tags e2e ./internal/cmd -run PRFlow`")
	}

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	if !hasOpenCodeAuth(realHome) {
		t.Skipf("skip opencode e2e: missing auth under %s", realHome)
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("skip opencode e2e: host CLI unavailable: %v", err)
	}
	model, err := detectOpenCodeModel(realHome)
	if err != nil {
		t.Skipf("skip opencode e2e: %v", err)
	}

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	_ = initRunIntegrationRepoWithRemote(t, repoDir)
	seedPRFlowRepo(t, repoDir)
	baselineHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

	ghShimDir := t.TempDir()
	writeFakeGHShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	deps := prFlowDeps(repoDir)

	runRootCommand(t, deps, "init")

	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	customizeOpenCodeAgent(t, repoDir, model)
	writePRFlowPrompt(t, repoDir)

	out, err := runRootCommand(t, deps, "run", "--sandbox", "worktree", strconv.Itoa(prFlowIssueNumber))
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, prFlowBranch) {
		t.Fatalf("expected branch %q in output, got:\n%s", prFlowBranch, out)
	}

	logPath := filepath.Join(repoDir, ".sandman", "logs", fmt.Sprintf("%d.log", prFlowIssueNumber))
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "go test ./...") {
		t.Fatalf("expected go test command in log, got:\n%s", log)
	}
	if !strings.Contains(log, "ok") && !strings.Contains(log, "PASS") {
		t.Fatalf("expected passing go test output in log, got:\n%s", log)
	}
	if !strings.Contains(log, "https://example.test/example/sandbox/pull/1") {
		t.Fatalf("expected fake PR URL in log, got:\n%s", log)
	}

	argsData, err := os.ReadFile(filepath.Join(ghShimDir, "pr-create.args"))
	if err != nil {
		t.Fatalf("read pr create args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if got := prFlowFlagValue(args, "--base"); got != "main" {
		t.Fatalf("pr create --base: got %q, want %q", got, "main")
	}
	if got := prFlowFlagValue(args, "--head"); got != prFlowBranch {
		t.Fatalf("pr create --head: got %q, want %q", got, prFlowBranch)
	}
	if got := prFlowFlagValue(args, "--title"); got != "Fix failing test" {
		t.Fatalf("pr create --title: got %q, want %q", got, "Fix failing test")
	}

	bodyData, err := os.ReadFile(filepath.Join(ghShimDir, "pr-create.body"))
	if err != nil {
		t.Fatalf("read pr create body: %v", err)
	}
	if got := strings.TrimSpace(string(bodyData)); got != "Fixes #1" {
		t.Fatalf("pr create body: got %q, want %q", got, "Fixes #1")
	}

	countData, err := os.ReadFile(filepath.Join(ghShimDir, "pr-create.count"))
	if err != nil {
		t.Fatalf("read pr create count: %v", err)
	}
	if got := strings.TrimSpace(string(countData)); got != "1" {
		t.Fatalf("expected exactly one pr create invocation, got %q", got)
	}

	branchHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", prFlowBranch))
	if branchHash == baselineHash {
		t.Fatalf("expected branch commit beyond baseline, got %s", branchHash)
	}
	if out, err := exec.Command("git", "merge-base", "--is-ancestor", baselineHash, branchHash).CombinedOutput(); err != nil {
		t.Fatalf("expected branch commit to descend from baseline: %v\n%s", err, out)
	}

	bareRemote := filepath.Join(repoDir, ".sandman", "remote")
	remoteHash := strings.TrimSpace(runGit(t, bareRemote, "rev-parse", "refs/heads/"+prFlowBranch))
	if remoteHash != branchHash {
		t.Fatalf("remote branch hash mismatch: got %q, want %q", remoteHash, branchHash)
	}

	worktreePath := filepath.Join(repoDir, ".sandman", "worktrees", prFlowBranch)
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree cleanup, got: %v", err)
	}
}

func prFlowDeps(repoDir string) Dependencies {
	ghClient := &github.CLIClient{}
	cfgStore := &config.FileStore{Path: filepath.Join(repoDir, ".sandman", "config.yaml")}
	renderer := &prompt.Engine{}
	eventLog := &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")}

	return Dependencies{
		BatchRunner:    batch.NewOrchestrator(ghClient, renderer, cfgStore, eventLog),
		ConfigStore:    cfgStore,
		EventLog:       eventLog,
		GitHubClient:   ghClient,
		PromptRenderer: renderer,
		IssuePicker:    &SimpleIssuePicker{},
		IsTTY:          isStdoutTTY,
	}
}

func seedPRFlowRepo(t *testing.T, dir string) {
	t.Helper()

	files := map[string]string{
		"go.mod": `module example.com/prflow

go 1.24
`,
		"double.go": `package prflow

func Double(n int) int {
	return 0
}
`,
		"double_test.go": `package prflow

import "testing"

func TestDouble(t *testing.T) {
	if got := Double(2); got != 4 {
		t.Fatalf("Double(2) = %d, want 4", got)
	}
}
`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "feat: seed failing test")
	runGit(t, dir, "push", "origin", "main")
}

func customizeOpenCodeAgent(t *testing.T, repoDir, model string) {
	t.Helper()

	cfgPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	agent, err := cfg.ResolveAgentProvider("opencode")
	if err != nil {
		t.Fatalf("resolve opencode agent: %v", err)
	}
	agent.Command = fmt.Sprintf(`opencode run --pure -m %s "$(cat {{.PromptFile}})"`, model)
	if cfg.AgentProviders == nil {
		cfg.AgentProviders = map[string]config.Agent{}
	}
	cfg.AgentProviders["opencode"] = agent
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

func writePRFlowPrompt(t *testing.T, repoDir string) {
	t.Helper()

	promptPath := filepath.Join(repoDir, ".sandman", "prompt.md")
	prompt := `# Task

Issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

{{ISSUE_BODY}}

Run ` + "`go test ./...`" + `.
Fix only what is needed.
When green, create one commit, push ` + "`{{SOURCE_BRANCH}}`" + ` to origin, run ` + "`gh pr create --base {{TARGET_BRANCH}} --head {{SOURCE_BRANCH}} --title \"{{ISSUE_TITLE}}\" --body \"Fixes #{{ISSUE_NUMBER}}\"`" + `, and print the PR URL.
`
	if err := os.WriteFile(promptPath, []byte(prompt), 0644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
}

func pickModel(modelsOutput string) string {
	models := strings.Fields(modelsOutput)
	preferred := []string{
		"openai/gpt-5.3-codex",
		"openai/gpt-5.3-codex-spark",
		"openai/gpt-5.2-codex",
		"openai/gpt-5.1-codex",
		"openai/gpt-5-codex",
		"openai/gpt-5.1-codex-mini",
		"openai/gpt-5.1-codex-max",
	}
	for _, want := range preferred {
		for _, model := range models {
			if model == want {
				return model
			}
		}
	}
	for _, model := range models {
		if strings.Contains(model, "codex") {
			return model
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}

func writeFakeGHShimForContainer(t *testing.T, hostDir string) {
	t.Helper()

	containerShimDir := "/workspace/.sandman/bin"
	script := strings.ReplaceAll(`#!/bin/sh
set -eu

shim_dir="__SHIM_DIR__"

case "$1" in
  repo)
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"name":"sandbox","owner":{"login":"example"}}
JSON
      exit 0
    fi
    ;;
  pr)
    if [ "${2:-}" = "create" ]; then
      shift 2
      count_file="$shim_dir/pr-create.count"
      args_file="$shim_dir/pr-create.args"
      body_file="$shim_dir/pr-create.body"

      count=0
      if [ -f "$count_file" ]; then
        count=$(cat "$count_file")
      fi
      count=$((count + 1))
      printf '%s\n' "$count" > "$count_file"
      if [ "$count" -ne 1 ]; then
        printf 'unexpected gh pr create invocation #%s\n' "$count" >&2
        exit 1
      fi

      printf '%s\n' "$@" > "$args_file"

      body=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --body)
            shift
            body="${1:-}"
            ;;
          --body-file)
            shift
            body="$(cat "$1")"
            ;;
        esac
        shift
      done

      printf '%s' "$body" > "$body_file"
      printf 'https://example.test/example/sandbox/pull/1\n'
      exit 0
    fi
    ;;
  api)
    path=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -H)
          shift 2
          ;;
        --repo)
          shift 2
          ;;
        repos/*)
          path="$1"
          shift
          ;;
        *)
          shift
          ;;
      esac
    done
    case "$path" in
      repos/example/sandbox/issues/1)
        cat <<'JSON'
{"number":1,"title":"Fix failing test","body":"The repo has a tiny failing Go test. Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/1/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %s\n' "$path" >&2
    exit 1
    ;;
esac

  auth)
    if [ "${2:-}" = "status" ]; then
      cat <<'JSON'
github.com
  ✓ Logged in to github.com as test-user (keyring)
  ✓ Git operations for github.com configured to use https protocol.
  ✓ Token: ghp_xxxxxxxxxxxxxxxxxxxx
JSON
      exit 0
    fi
    if [ "${2:-}" = "setup-git" ]; then
      exit 0
    fi
    ;;
esac

printf 'unexpected gh command: %s\n' "$*" >&2
exit 1
`, "__SHIM_DIR__", containerShimDir)
	if err := os.MkdirAll(hostDir, 0755); err != nil {
		t.Fatalf("create gh shim dir: %v", err)
	}
	ghPath := filepath.Join(hostDir, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
}

func patchDockerfileToHostOpenCodeVersion(t *testing.T, repoDir string) {
	t.Helper()

	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	cmd := exec.Command("opencode", "--version")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("detect opencode version: %v", err)
	}
	hostVersion := strings.TrimSpace(string(out))

	content := string(data)
	oldPin := "opencode-ai@" + scaffold.DefaultBuiltInAgentVersion("opencode")
	newPin := "opencode-ai@" + hostVersion
	content = strings.ReplaceAll(content, oldPin, newPin)

	if err := os.WriteFile(dockerfilePath, []byte(content), 0644); err != nil {
		t.Fatalf("write patched Dockerfile: %v", err)
	}
}

func customizeOpenCodeAgentForContainer(t *testing.T, repoDir, model string) {
	t.Helper()

	cfgPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	agent, err := cfg.ResolveAgentProvider("opencode")
	if err != nil {
		t.Fatalf("resolve opencode agent: %v", err)
	}
	agent.Command = fmt.Sprintf(`PATH=/workspace/.sandman/bin:${PATH} opencode run --pure -m %s "$(cat {{.PromptFile}})"`, model)
	if cfg.AgentProviders == nil {
		cfg.AgentProviders = map[string]config.Agent{}
	}
	cfg.AgentProviders["opencode"] = agent
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

func writeFakeGHShim(t *testing.T, dir string) {
	t.Helper()

	script := strings.ReplaceAll(`#!/bin/sh
set -eu

shim_dir="__SHIM_DIR__"

case "$1" in
  repo)
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"name":"sandbox","owner":{"login":"example"}}
JSON
      exit 0
    fi
    ;;
  pr)
    if [ "${2:-}" = "create" ]; then
      shift 2
      count_file="$shim_dir/pr-create.count"
      args_file="$shim_dir/pr-create.args"
      body_file="$shim_dir/pr-create.body"

      count=0
      if [ -f "$count_file" ]; then
        count=$(cat "$count_file")
      fi
      count=$((count + 1))
      printf '%s\n' "$count" > "$count_file"
      if [ "$count" -ne 1 ]; then
        printf 'unexpected gh pr create invocation #%s\n' "$count" >&2
        exit 1
      fi

      printf '%s\n' "$@" > "$args_file"

      body=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --body)
            shift
            body="${1:-}"
            ;;
          --body-file)
            shift
            body="$(cat "$1")"
            ;;
        esac
        shift
      done

      printf '%s' "$body" > "$body_file"
      printf 'https://example.test/example/sandbox/pull/1\n'
      exit 0
    fi
    ;;
  api)
    path=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -H)
          shift 2
          ;;
        --repo)
          shift 2
          ;;
        repos/*)
          path="$1"
          shift
          ;;
        *)
          shift
          ;;
      esac
    done
    case "$path" in
      repos/example/sandbox/issues/1)
        cat <<'JSON'
{"number":1,"title":"Fix failing test","body":"The repo has a tiny failing Go test. Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/1/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %s\n' "$path" >&2
    exit 1
    ;;
  auth)
    if [ "${2:-}" = "status" ]; then
      cat <<'JSON'
github.com
  ✓ Logged in to github.com as test-user (keyring)
  ✓ Git operations for github.com configured to use https protocol.
  ✓ Token: ghp_xxxxxxxxxxxxxxxxxxxx
JSON
      exit 0
    fi
    if [ "${2:-}" = "setup-git" ]; then
      exit 0
    fi
    ;;
esac

printf 'unexpected gh command: %s\n' "$*" >&2
exit 1
`, "__SHIM_DIR__", dir)
	ghPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
}

func prFlowFlagValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func prependPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func parseE2EProviders() (map[string]bool, error) {
	raw := strings.TrimSpace(os.Getenv("SANDMAN_E2E_PROVIDERS"))
	if raw == "" {
		return nil, nil
	}
	if raw == "all" || raw == "*" {
		return map[string]bool{"opencode": true}, nil
	}

	allowed := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		switch name {
		case "opencode":
			allowed[name] = true
		default:
			return nil, fmt.Errorf("unknown e2e provider %q", name)
		}
	}
	return allowed, nil
}

func hasOpenCodeAuth(home string) bool {
	_, err := os.Stat(homePath(home, "~/.local/share/opencode/auth.json"))
	return err == nil
}

func detectOpenCodeModel(home string) (string, error) {
	cmd := exec.Command("opencode", "models")
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("list supported opencode models: %w", err)
	}

	var models []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			models = append(models, line)
		}
	}
	if len(models) == 0 {
		return "", fmt.Errorf("no supported opencode models found")
	}

	preferred := []string{
		"openai/gpt-5.3-codex",
		"openai/gpt-5.3-codex-spark",
		"openai/gpt-5.2-codex",
		"openai/gpt-5.1-codex",
		"openai/gpt-5-codex",
		"openai/gpt-5.1-codex-mini",
		"openai/gpt-5.1-codex-max",
	}
	for _, want := range preferred {
		for _, model := range models {
			if model == want {
				return model, nil
			}
		}
	}
	for _, model := range models {
		if strings.Contains(model, "codex") {
			return model, nil
		}
	}
	return models[0], nil
}

func homePath(home, rel string) string {
	if strings.HasPrefix(rel, "~") {
		rel = strings.TrimPrefix(rel, "~")
	}
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	return filepath.Join(home, rel)
}

func runRootCommand(t *testing.T, deps Dependencies, args ...string) (string, error) {
	t.Helper()

	var buf bytes.Buffer
	root := NewRootCmd(deps)
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetIn(strings.NewReader(""))
	if cmd, _, err := root.Find(args); err == nil {
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
	}
	root.SetArgs(args)
	root.SetContext(e2eContext(t))
	err := root.Execute()
	return buf.String(), err
}

func e2eContext(t *testing.T) context.Context {
	t.Helper()

	if deadline, ok := t.Deadline(); ok {
		timeout := time.Until(deadline) - 30*time.Second
		if timeout > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			t.Cleanup(cancel)
			return ctx
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func isStdoutTTY() bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(os.Stdout.Fd()), &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}
