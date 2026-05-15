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
)

const (
	prFlowIssueNumber = 1
	prFlowBranch      = "sandman/1-fix-failing-test"
)

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
When green, create one commit and push ` + "`{{SOURCE_BRANCH}}`" + ` to origin.
`
	if err := os.WriteFile(promptPath, []byte(prompt), 0644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
}

func writeFakeGHShim(t *testing.T, dir string) {
	t.Helper()

	script := `#!/bin/sh
set -eu

case "$1" in
  repo)
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"name":"sandbox","owner":{"login":"example"}}
JSON
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

printf 'unexpected gh command: %s\n' "$*" >&2
exit 1
`
	ghPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
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
