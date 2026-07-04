//go:build e2e

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	"github.com/rafaelromao/sandman/internal/testenv"
)

const (
	prFlowIssueNumber = 1
	prFlowBranch      = "sandman/1-fix-failing-test"

	parallelIssue150  = 150
	parallelIssue151  = 151
	parallelIssue152  = 152
	parallelBranch150 = "sandman/150-fix-150"
	parallelBranch151 = "sandman/151-fix-151"
	parallelBranch152 = "sandman/152-fix-152"
)

type prFlowProviderCase struct {
	name              string
	hostCLI           string
	model             string
	requiredAuthPaths []string
	authPaths         []string
}

type prFlowAgentOptions struct {
	container bool
	echo      bool
}

var prFlowProviderCases = []prFlowProviderCase{
	{
		name:              "opencode",
		hostCLI:           "opencode",
		model:             "opencode/big-pickle",
		requiredAuthPaths: []string{"~/.local/share/opencode/auth.json"},
		authPaths:         []string{"~/.config/opencode", "~/.local/share/opencode"},
	},
}

// applyPRFlowModelOverrides lets operators steer the prflow e2e tests
// at a different model per agent via the SANDMAN_TEST_MODEL_<AGENT>
// env vars. When unset, the literal model in prFlowProviderCases is
// used. Runs in an init so every subtest sees the override before its
// testCase is copied.
func applyPRFlowModelOverrides() {
	for i := range prFlowProviderCases {
		tc := &prFlowProviderCases[i]
		tc.model = testenv.ResolveTestModel(tc.name, tc.model)
	}
}

func init() {
	applyPRFlowModelOverrides()
}

func parseE2EProviders() (map[string]bool, error) {
	return testenv.ResolveProviderAllowlist(prFlowProviderNames())
}

func runPRFlowProviderCases(t *testing.T, fn func(t *testing.T, tc prFlowProviderCase)) {
	t.Helper()

	allowed, err := parseE2EProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) == 0 {
		t.Skip("set SANDMAN_TEST_PROVIDERS=opencode and run `go test -tags e2e ./internal/cmd -run PRFlow`")
	}

	for _, tc := range prFlowProviderCases {
		tc := tc
		if !allowed[tc.name] {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			fn(t, tc)
		})
	}
}

func TestPRFlow_PodmanSandboxBinaryCommitsAndPushes(t *testing.T) {
	// CI: JUSTIFIED — calls requirePRFlowProvider (real provider auth) and requirePodmanE2E.
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	runPRFlowProviderCases(t, func(t *testing.T, tc prFlowProviderCase) {
		realHome := requirePRFlowProvider(t, tc)
		requirePodmanE2E(t)

		binPath := buildSandmanBinary(t)

		repoDir := t.TempDir()
		t.Chdir(repoDir)
		initRunIntegrationRepo(t, repoDir)

		remoteDir := filepath.Join(repoDir, "remote")
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

		setupIsolatedPRFlowHome(t, realHome, repoDir, "sandman-podman-e2e-binary-", tc.authPaths)

		out, err := runSandmanBinary(t, binPath, repoDir, "init", "--agent", tc.name)
		if err != nil {
			t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
		}
		for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
			if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
				t.Fatalf("expected scaffolded %s: %v", rel, err)
			}
		}
		if _, err := runSandmanBinary(t, binPath, repoDir, "config", "set", "review_command", "/oc review"); err != nil {
			t.Fatalf("sandman config set failed: %v", err)
		}
		baselineHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

		ghShimDir := t.TempDir()
		writeFakeGHShim(t, ghShimDir)
		prependPath(t, ghShimDir)

		containerGhShimDir := filepath.Join(repoDir, ".sandman", "bin")
		writeFakeGHShimForContainer(t, containerGhShimDir)

		buildCmd := exec.Command("podman", "build", "-t", "sandman-e2e-model-detect", "-f",
			filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("build image for model detection: %v: %s", err, out)
		}
		if out, err := exec.Command("podman", "run", "--rm", "sandman-e2e-model-detect", "sh", "-c", "command -v go >/dev/null").CombinedOutput(); err != nil {
			t.Fatalf("go toolchain missing in container image: %v\n%s", err, out)
		}
		t.Logf("using provider model: %s", tc.model)

		customizePRFlowAgent(t, repoDir, tc, prFlowAgentOptions{container: true})
		writePRFlowPrompt(t, repoDir)

		out, err = runSandmanBinary(t, binPath, repoDir, "run", "--agent", tc.name, "--sandbox", "podman", strconv.Itoa(prFlowIssueNumber))
		t.Logf("sandman run returned err=%v output=%s", err, out)

		logPath := filepath.Join(repoDir, ".sandman", "logs", fmt.Sprintf("%d.log", prFlowIssueNumber))
		logData, logErr := os.ReadFile(logPath)
		if logErr != nil {
			t.Fatalf("read log: %v", logErr)
		}
		log := string(logData)

		if !strings.Contains(log, "https://example.test/example/sandbox/pull/1") {
			t.Fatalf("expected fake PR URL in log, got:\n%s", log)
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
	})
}

func TestPRFlow_PodmanSandboxCommitsAndPushes(t *testing.T) {
	// CI: JUSTIFIED — calls requirePRFlowProvider (real provider auth) and requirePodmanE2E.
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	runPRFlowProviderCases(t, func(t *testing.T, tc prFlowProviderCase) {
		realHome := requirePRFlowProvider(t, tc)
		requirePodmanE2E(t)

		repoDir := t.TempDir()
		t.Chdir(repoDir)
		initRunIntegrationRepo(t, repoDir)

		remoteDir := filepath.Join(repoDir, "remote")
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

		setupIsolatedPRFlowHome(t, realHome, repoDir, "sandman-podman-e2e-", tc.authPaths)

		runRootCommand(t, prFlowDeps(repoDir), "init", "--agent", tc.name)
		for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
			if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
				t.Fatalf("expected scaffolded %s: %v", rel, err)
			}
		}
		runRootCommand(t, prFlowDeps(repoDir), "config", "set", "review_command", "/oc review")
		baselineHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

		ghShimDir := t.TempDir()
		writeFakeGHShim(t, ghShimDir)
		prependPath(t, ghShimDir)

		deps := prFlowDeps(repoDir)

		containerGhShimDir := filepath.Join(repoDir, ".sandman", "bin")
		writeFakeGHShimForContainer(t, containerGhShimDir)

		buildCmd := exec.Command("podman", "build", "-t", "sandman-e2e-model-detect", "-f",
			filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("build image for model detection: %v: %s", err, out)
		}
		if out, err := exec.Command("podman", "run", "--rm", "sandman-e2e-model-detect", "sh", "-c", "command -v go >/dev/null").CombinedOutput(); err != nil {
			t.Fatalf("go toolchain missing in container image: %v\n%s", err, out)
		}
		t.Logf("using provider model: %s", tc.model)

		customizePRFlowAgent(t, repoDir, tc, prFlowAgentOptions{container: true})
		writePRFlowPrompt(t, repoDir)

		_, err := runRootCommand(t, deps, "run", "--agent", tc.name, "--sandbox", "podman", strconv.Itoa(prFlowIssueNumber))
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
	})
}

func TestPRFlow_WorktreeSandboxCommitsAndPushes(t *testing.T) {
	// CI: JUSTIFIED — calls requirePRFlowProvider (real provider auth); worktree sandbox, no runtime.
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	runPRFlowProviderCases(t, func(t *testing.T, tc prFlowProviderCase) {
		requirePRFlowProvider(t, tc)

		repoDir := t.TempDir()
		t.Chdir(repoDir)
		bareRemote := initRunIntegrationRepoWithRemote(t, repoDir)
		seedPRFlowRepo(t, repoDir)
		baselineHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

		ghShimDir := t.TempDir()
		writeFakeGHShim(t, ghShimDir)
		prependPath(t, ghShimDir)

		deps := prFlowDeps(repoDir)

		runRootCommand(t, deps, "init", "--agent", tc.name)

		for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
			if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
				t.Fatalf("expected scaffolded %s: %v", rel, err)
			}
		}

		runRootCommand(t, deps, "config", "set", "review_command", "/oc review")
		customizePRFlowAgent(t, repoDir, tc, prFlowAgentOptions{})
		writePRFlowPrompt(t, repoDir)

		out, err := runRootCommand(t, deps, "run", "--agent", tc.name, "--sandbox", "worktree", strconv.Itoa(prFlowIssueNumber))
		if err != nil {
			t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
		}

		if !strings.Contains(out, "Summary: 1 succeeded") {
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

		remoteHash := strings.TrimSpace(runGit(t, bareRemote, "rev-parse", "refs/heads/"+prFlowBranch))
		if remoteHash != branchHash {
			t.Fatalf("remote branch hash mismatch: got %q, want %q", remoteHash, branchHash)
		}

		worktreePath := filepath.Join(repoDir, ".sandman", "worktrees", prFlowBranch)
		if _, err := os.Stat(worktreePath); err != nil {
			t.Fatalf("expected worktree to remain, got: %v", err)
		}
	})
}

func prFlowDeps(repoDir string) Dependencies {
	ghClient := &github.CLIClient{}
	cfgStore := &config.FileStore{Path: filepath.Join(repoDir, ".sandman", "config.yaml")}
	renderer := &prompt.Engine{}
	eventLog := &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")}

	return Dependencies{
		BatchRunner:  batch.NewOrchestrator(ghClient, renderer, cfgStore, eventLog),
		ConfigStore:  cfgStore,
		EventLog:     eventLog,
		GitHubClient: ghClient,
		Renderer:     renderer,
		IssuePicker:  &SimpleIssuePicker{},
		IsTTY:        isStdoutTTY,
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

func requirePRFlowProvider(t *testing.T, tc prFlowProviderCase) string {
	t.Helper()

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	if !hasProviderAuth(realHome, tc.requiredAuthPaths) {
		t.Skipf("skip %s e2e: missing auth under %s", tc.name, realHome)
	}
	if _, err := exec.LookPath(tc.hostCLI); err != nil {
		t.Skipf("skip %s e2e: host CLI unavailable: %v", tc.name, err)
	}
	return realHome
}

func requirePodmanE2E(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("podman"); err != nil {
		t.Skipf("skip podman e2e: podman unavailable: %v", err)
	}
}

func setupIsolatedPRFlowHome(t *testing.T, realHome, repoDir, prefix string, authPaths []string) string {
	t.Helper()

	homeDir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0755); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	absRepo, _ := filepath.Abs(repoDir)
	gitConfigContent := fmt.Sprintf("[user]\n\tname = Test\n\temail = test@test.com\n[url %q]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n",
		"file://"+filepath.Join(absRepo, "remote"))
	if err := os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitConfigContent), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	linkPRFlowAuthPaths(t, realHome, homeDir, authPaths)
	t.Setenv("HOME", homeDir)
	if out, err := exec.Command("podman", "run", "--rm", "alpine", "echo", "ok").CombinedOutput(); err != nil {
		t.Fatalf("warm podman image for test home: %v: %s", err, out)
	}
	return homeDir
}

func linkPRFlowAuthPaths(t *testing.T, realHome, tempHome string, paths []string) {
	t.Helper()

	for _, rel := range paths {
		src := homePath(realHome, rel)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		} else if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		dst := homePath(tempHome, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatalf("create parent for %s: %v", dst, err)
		}
		if err := os.Symlink(src, dst); err != nil {
			t.Fatalf("link %s: %v", rel, err)
		}
	}
}

func customizePRFlowAgent(t *testing.T, repoDir string, tc prFlowProviderCase, opts prFlowAgentOptions) {
	t.Helper()

	cfgPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	agent, err := cfg.ResolveAgentProvider(tc.name)
	if err != nil {
		t.Fatalf("resolve %s agent: %v", tc.name, err)
	}
	agent.Model = tc.model
	var prefix strings.Builder
	if opts.echo {
		prefix.WriteString(`printf 'containerhostname=%s\ncontainerworkdir=%s\n' "$(hostname)" "$(pwd)" && `)
	}
	if opts.container {
		prefix.WriteString(`PATH=/workspace/.sandman/bin:${PATH} `)
	}
	switch tc.name {
	case "opencode":
		agent.Command = prefix.String() + fmt.Sprintf(`opencode run --pure --dangerously-skip-permissions -m %s "$(cat {{.PromptFile}})"`, tc.model)
	default:
		t.Fatalf("unsupported provider %q", tc.name)
	}
	if cfg.AgentProviders == nil {
		cfg.AgentProviders = map[string]config.Agent{}
	}
	cfg.AgentProviders[tc.name] = agent
	if cfg.Agents == nil {
		cfg.Agents = map[string]config.Agent{}
	}
	cfg.Agents[tc.name] = agent
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

Run ` + "`gh issue view {{ISSUE_NUMBER}}`" + `.
Run ` + "`go test ./...`" + `.
Run ` + "`go vet ./...`" + `.
Run ` + "`gofmt -w .`" + `.
Fix only what is needed.
When green, create one commit, push ` + "`{{SOURCE_BRANCH}}`" + ` to origin, run ` + "`gh pr create --base {{BASE_BRANCH}} --head {{SOURCE_BRANCH}} --title \"{{ISSUE_TITLE}}\" --body \"Fixes #{{ISSUE_NUMBER}}\"`" + `, then run ` + "`gh pr checks`" + `, ` + "`gh pr comment --body \"ready\"`" + `, ` + "`gh pr view`" + `, and print the PR URL.
`
	if err := os.WriteFile(promptPath, []byte(prompt), 0644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
}

func writeFakeGHShimForContainer(t *testing.T, hostDir string) {
	t.Helper()

	containerShimDir := "/workspace/.sandman/bin"
	script := strings.ReplaceAll(`#!/bin/sh
set -eu

shim_dir="__SHIM_DIR__"

case "$1" in
  issue)
    if [ "${2:-}" = "view" ]; then
      case "${3:-}" in
        1)
          issue_number="${3:-}"
          body="The repo has a tiny failing Go test. Make Double(2) return 4."
          cat <<JSON
{"number":$issue_number,"title":"Fix failing test","body":"$body"}
JSON
          exit 0
          ;;
        2)
          issue_number="${3:-}"
          body="The repo has a tiny failing Go test. Make Double(3) return 6."
          cat <<JSON
{"number":$issue_number,"title":"Fix failing test","body":"$body"}
JSON
          exit 0
          ;;
      esac
    fi
    ;;
  repo)
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"name":"sandbox","owner":{"login":"example"}}
JSON
      exit 0
    fi
    ;;
  pr)
    if [ "${2:-}" = "list" ]; then
      head=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --head)
            shift
            head="${1:-}"
            ;;
        esac
        shift
      done
      state_file="$shim_dir/pr-state"
      state="open"
      if [ -f "$state_file" ]; then
        state=$(cat "$state_file")
      fi
      case "$state" in
        merged)
          next_state="open"
          merged_at="2026-06-05T00:00:00Z"
          pr_state="merged"
          ;;
        *)
          next_state="merged"
          merged_at=""
          pr_state="open"
          ;;
      esac
      printf '%s\n' "$next_state" > "$state_file"
      head_ref_oid="$(git rev-parse HEAD)"
      cat <<JSON
[{"number":1,"state":"$pr_state","mergedAt":"$merged_at","headRefName":"$head","headRefOid":"$head_ref_oid"}]
JSON
      exit 0
    fi
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
    if [ "${2:-}" = "checks" ]; then
      printf 'all checks passed\n'
      exit 0
    fi
    if [ "${2:-}" = "comment" ]; then
      printf 'commented\n'
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
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
      repos/example/sandbox/issues/2)
        cat <<'JSON'
{"number":2,"title":"Fix failing test","body":"The repo has a tiny failing Go test. Make Double(3) return 6.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/1/events)
        printf '[]\n'
        exit 0
        ;;
      repos/example/sandbox/issues/2/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %s\n' "$path" >&2
    exit 1
    ;;
  auth)
    if [ "${2:-}" = "token" ]; then
      printf 'ghp_xxxxxxxxxxxxxxxxxxxx\n'
      exit 0
    fi
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

	repoDir := filepath.Dir(filepath.Dir(hostDir))
	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile = append(dockerfile, []byte("\nCOPY .sandman/bin/gh /usr/local/bin/gh\nRUN chmod +x /usr/local/bin/gh\n")...)
	if err := os.WriteFile(dockerfilePath, dockerfile, 0644); err != nil {
		t.Fatalf("append gh shim to Dockerfile: %v", err)
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
	agent.Command = fmt.Sprintf(`PATH=/workspace/.sandman/bin:${PATH} opencode run --pure --dangerously-skip-permissions -m %s "$(cat {{.PromptFile}})"`, model)
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
  issue)
    if [ "${2:-}" = "view" ]; then
      case "${3:-}" in
        1)
          issue_number="${3:-}"
          body="The repo has a tiny failing Go test. Make Double(2) return 4."
          cat <<JSON
{"number":$issue_number,"title":"Fix failing test","body":"$body"}
JSON
          exit 0
          ;;
        2)
          issue_number="${3:-}"
          body="The repo has a tiny failing Go test. Make Double(3) return 6."
          cat <<JSON
{"number":$issue_number,"title":"Fix failing test","body":"$body"}
JSON
          exit 0
          ;;
      esac
    fi
    ;;
  repo)
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"name":"sandbox","owner":{"login":"example"}}
JSON
      exit 0
    fi
    ;;
  pr)
    if [ "${2:-}" = "list" ]; then
      head=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --head)
            shift
            head="${1:-}"
            ;;
        esac
        shift
      done
      state_file="$shim_dir/pr-state"
      state="open"
      if [ -f "$state_file" ]; then
        state=$(cat "$state_file")
      fi
      case "$state" in
        merged)
          next_state="open"
          merged_at="2026-06-05T00:00:00Z"
          pr_state="merged"
          ;;
        *)
          next_state="merged"
          merged_at=""
          pr_state="open"
          ;;
      esac
      printf '%s\n' "$next_state" > "$state_file"
      head_ref_oid="$(git rev-parse HEAD)"
      cat <<JSON
[{"number":1,"state":"$pr_state","mergedAt":"$merged_at","headRefName":"$head","headRefOid":"$head_ref_oid"}]
JSON
      exit 0
    fi
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
    if [ "${2:-}" = "checks" ]; then
      printf 'all checks passed\n'
      exit 0
    fi
    if [ "${2:-}" = "comment" ]; then
      printf 'commented\n'
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
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
      repos/example/sandbox/issues/2)
        cat <<'JSON'
{"number":2,"title":"Fix failing test","body":"The repo has a tiny failing Go test. Make Double(3) return 6.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/1/events)
        printf '[]\n'
        exit 0
        ;;
      repos/example/sandbox/issues/2/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %s\n' "$path" >&2
    exit 1
    ;;
  auth)
    if [ "${2:-}" = "token" ]; then
      printf 'ghp_xxxxxxxxxxxxxxxxxxxx\n'
      exit 0
    fi
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

func TestPRFlow_PodmanSandboxBinaryParallelAgentRuns(t *testing.T) {
	// CI: JUSTIFIED — calls requirePRFlowProvider (real provider auth) and requirePodmanE2E.
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	runPRFlowProviderCases(t, func(t *testing.T, tc prFlowProviderCase) {
		realHome := requirePRFlowProvider(t, tc)
		requirePodmanE2E(t)

		binPath := buildSandmanBinary(t)

		repoDir := t.TempDir()
		t.Chdir(repoDir)
		initRunIntegrationRepo(t, repoDir)

		remoteDir := filepath.Join(repoDir, "remote")
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

		seedParallelPRFlowRepo(t, repoDir)
		runGit(t, repoDir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

		setupIsolatedPRFlowHome(t, realHome, repoDir, "sandman-podman-e2e-parallel-", tc.authPaths)

		out, err := runSandmanBinary(t, binPath, repoDir, "init", "--agent", tc.name)
		if err != nil {
			t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
		}
		for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
			if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
				t.Fatalf("expected scaffolded %s: %v", rel, err)
			}
		}
		if _, err := runSandmanBinary(t, binPath, repoDir, "config", "set", "review_command", "/oc review"); err != nil {
			t.Fatalf("sandman config set failed: %v", err)
		}
		baselineHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

		ghShimDir := t.TempDir()
		writeFakeGHShimParallel(t, ghShimDir)
		prependPath(t, ghShimDir)

		containerGhShimDir := filepath.Join(repoDir, ".sandman", "bin")
		writeFakeGHShimForContainerParallel(t, containerGhShimDir)

		buildCmd := exec.Command("podman", "build", "-t", "sandman-e2e-model-detect-parallel", "-f",
			filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("build image for model detection: %v: %s", err, out)
		}
		if out, err := exec.Command("podman", "run", "--rm", "sandman-e2e-model-detect-parallel", "sh", "-c", "command -v go >/dev/null").CombinedOutput(); err != nil {
			t.Fatalf("go toolchain missing in container image: %v\n%s", err, out)
		}
		t.Logf("using provider model: %s", tc.model)

		customizePRFlowAgent(t, repoDir, tc, prFlowAgentOptions{container: true, echo: true})
		writeParallelPRFlowPrompt(t, repoDir, tc)

		out, err = runSandmanBinary(t, binPath, repoDir, "run",
			"--agent", tc.name,
			"--sandbox", "podman",
			"--parallel", "2",
			"--container-capacity", "2",
			"--max-containers", "1",
			strconv.Itoa(parallelIssue150), strconv.Itoa(parallelIssue151))
		t.Logf("sandman run returned err=%v output=%s", err, out)

		eventsPath := filepath.Join(repoDir, ".sandman", "events.jsonl")
		eventsData, err := os.ReadFile(eventsPath)
		if err != nil {
			t.Fatalf("read events: %v", err)
		}
		var started, finished []time.Time
		for _, line := range strings.Split(strings.TrimSpace(string(eventsData)), "\n") {
			if line == "" {
				continue
			}
			var evt events.Event
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				t.Fatalf("parse event: %v: %s", err, line)
			}
			switch evt.Type {
			case "run.started":
				started = append(started, evt.Timestamp)
			case "run.finished":
				finished = append(finished, evt.Timestamp)
			}
		}
		if len(started) != 2 {
			t.Fatalf("expected 2 run.started events, got %d", len(started))
		}
		if len(finished) != 2 {
			t.Fatalf("expected 2 run.finished events, got %d", len(finished))
		}
		lastStarted := started[0]
		if started[1].After(started[0]) {
			lastStarted = started[1]
		}
		firstFinished := finished[0]
		if finished[1].Before(finished[0]) {
			firstFinished = finished[1]
		}
		if !lastStarted.Before(firstFinished) {
			t.Fatal("expected both run.started events before first run.finished — runs did not overlap")
		}

		for _, branch := range []string{parallelBranch150, parallelBranch151} {
			branchHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", branch))
			if branchHash == baselineHash {
				t.Fatalf("expected branch %s commit beyond baseline, got %s", branch, branchHash)
			}
			if out, err := exec.Command("git", "merge-base", "--is-ancestor", baselineHash, branchHash).CombinedOutput(); err != nil {
				t.Fatalf("expected branch %s to descend from baseline: %v\n%s", branch, err, out)
			}
			remoteHash := strings.TrimSpace(runGit(t, repoDir, "ls-remote", "origin", "refs/heads/"+branch))
			if remoteHash == "" {
				t.Fatalf("expected pushed remote branch %s", branch)
			}
			fields := strings.Fields(remoteHash)
			if len(fields) == 0 || fields[0] != branchHash {
				t.Fatalf("remote branch %s hash mismatch: got %q, want %q", branch, remoteHash, branchHash)
			}
		}

		for _, tc := range []struct {
			issue    int
			branch   string
			wantTest string
			failTest string
		}{
			{parallelIssue150, parallelBranch150, "TestDoubleFor150", "TestDoubleFor151"},
			{parallelIssue151, parallelBranch151, "TestDoubleFor151", "TestDoubleFor150"},
		} {
			checkoutDir := t.TempDir()
			clone := exec.Command("git", "clone", "--branch", tc.branch, "--single-branch", remoteDir, checkoutDir)
			if out, err := clone.CombinedOutput(); err != nil {
				t.Fatalf("clone branch %s: %v: %s", tc.branch, err, out)
			}

			testCmd := exec.Command("go", "test", "-run", tc.wantTest, "./...")
			testCmd.Dir = checkoutDir
			if out, err := testCmd.CombinedOutput(); err != nil {
				t.Fatalf("branch %s test %s failed: %v: %s", tc.branch, tc.wantTest, err, out)
			}

			testFailCmd := exec.Command("go", "test", "-run", tc.failTest, "./...")
			testFailCmd.Dir = checkoutDir
			if err := testFailCmd.Run(); err == nil {
				t.Fatalf("branch %s test %s should have failed but passed", tc.branch, tc.failTest)
			}
		}

		for i := 1; i <= 2; i++ {
			argsFile := filepath.Join(containerGhShimDir, fmt.Sprintf("pr-create.args.%d", i))
			if _, err := os.Stat(argsFile); err != nil {
				t.Fatalf("missing pr-create.args.%d: %v", i, err)
			}
		}
		countData, err := os.ReadFile(filepath.Join(containerGhShimDir, "pr-create.count"))
		if err != nil {
			t.Fatalf("read pr create count: %v", err)
		}
		if got := strings.TrimSpace(string(countData)); got != "2" {
			t.Fatalf("expected exactly two pr create invocations, got %q", got)
		}

		type prCreateCall struct {
			branch string
			title  string
			body   string
		}
		expected := map[string]prCreateCall{
			parallelBranch150: {parallelBranch150, "Fix 150", "Fixes #150"},
			parallelBranch151: {parallelBranch151, "Fix 151", "Fixes #151"},
		}
		seen := make(map[string]bool)
		for i := 1; i <= 2; i++ {
			argsData, err := os.ReadFile(filepath.Join(containerGhShimDir, fmt.Sprintf("pr-create.args.%d", i)))
			if err != nil {
				t.Fatalf("read pr create args.%d: %v", i, err)
			}
			args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
			head := prFlowFlagValue(args, "--head")
			if head == "" {
				t.Fatalf("pr create %d: missing --head flag in args:\n%s", i, argsData)
			}
			if seen[head] {
				t.Fatalf("pr create %d: duplicate branch %q", i, head)
			}
			seen[head] = true

			call, ok := expected[head]
			if !ok {
				t.Fatalf("pr create %d: unexpected --head %q, expected one of %q or %q", i, head, parallelBranch150, parallelBranch151)
			}
			if got := prFlowFlagValue(args, "--base"); got != "main" {
				t.Fatalf("pr create %d (%s): --base got %q, want %q", i, head, got, "main")
			}
			if got := prFlowFlagValue(args, "--title"); got != call.title {
				t.Fatalf("pr create %d (%s): --title got %q, want %q", i, head, got, call.title)
			}

			bodyData, err := os.ReadFile(filepath.Join(containerGhShimDir, fmt.Sprintf("pr-create.body.%d", i)))
			if err != nil {
				t.Fatalf("read pr create body.%d: %v", i, err)
			}
			if got := strings.TrimSpace(string(bodyData)); got != call.body {
				t.Fatalf("pr create %d (%s): body got %q, want %q", i, head, got, call.body)
			}
		}
		if len(seen) != 2 {
			t.Fatalf("expected pr creates for both branches, got %v", seen)
		}
	})
}

func TestPRFlow_PodmanSandboxBinaryParallelAgentRunsAutoCapacity(t *testing.T) {
	// CI: JUSTIFIED — calls requirePRFlowProvider (real provider auth) and requirePodmanE2E.
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	runPRFlowProviderCases(t, func(t *testing.T, tc prFlowProviderCase) {
		realHome := requirePRFlowProvider(t, tc)
		requirePodmanE2E(t)

		binPath := buildSandmanBinary(t)

		repoDir := t.TempDir()
		t.Chdir(repoDir)
		initRunIntegrationRepo(t, repoDir)

		remoteDir := filepath.Join(repoDir, "remote")
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

		seedParallelPRFlowRepo(t, repoDir)
		runGit(t, repoDir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

		setupIsolatedPRFlowHome(t, realHome, repoDir, "sandman-podman-e2e-parallel-auto-", tc.authPaths)

		out, err := runSandmanBinary(t, binPath, repoDir, "init", "--agent", tc.name)
		if err != nil {
			t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
		}
		for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
			if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
				t.Fatalf("expected scaffolded %s: %v", rel, err)
			}
		}
		if _, err := runSandmanBinary(t, binPath, repoDir, "config", "set", "review_command", "/oc review"); err != nil {
			t.Fatalf("sandman config set failed: %v", err)
		}
		baselineHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

		ghShimDir := t.TempDir()
		writeFakeGHShimParallel(t, ghShimDir)
		prependPath(t, ghShimDir)

		containerGhShimDir := filepath.Join(repoDir, ".sandman", "bin")
		writeFakeGHShimForContainerParallel(t, containerGhShimDir)

		buildCmd := exec.Command("podman", "build", "-t", "sandman-e2e-model-detect-parallel-auto", "-f",
			filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("build image for model detection: %v: %s", err, out)
		}
		if out, err := exec.Command("podman", "run", "--rm", "sandman-e2e-model-detect-parallel-auto", "sh", "-c", "command -v go >/dev/null").CombinedOutput(); err != nil {
			t.Fatalf("go toolchain missing in container image: %v\n%s", err, out)
		}
		t.Logf("using provider model: %s", tc.model)

		customizePRFlowAgent(t, repoDir, tc, prFlowAgentOptions{container: true, echo: true})
		writeParallelPRFlowPrompt(t, repoDir, tc)

		out, err = runSandmanBinary(t, binPath, repoDir, "run",
			"--agent", tc.name,
			"--sandbox", "podman",
			"--parallel", "2",
			"--container-capacity", "0",
			"--max-containers", "1",
			strconv.Itoa(parallelIssue150), strconv.Itoa(parallelIssue151))
		t.Logf("sandman run returned err=%v output=%s", err, out)

		eventsPath := filepath.Join(repoDir, ".sandman", "events.jsonl")
		eventsData, err := os.ReadFile(eventsPath)
		if err != nil {
			t.Fatalf("read events: %v", err)
		}
		var started, finished []time.Time
		for _, line := range strings.Split(strings.TrimSpace(string(eventsData)), "\n") {
			if line == "" {
				continue
			}
			var evt events.Event
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				t.Fatalf("parse event: %v: %s", err, line)
			}
			switch evt.Type {
			case "run.started":
				started = append(started, evt.Timestamp)
			case "run.finished":
				finished = append(finished, evt.Timestamp)
			}
		}
		if len(started) != 2 {
			t.Fatalf("expected 2 run.started events, got %d", len(started))
		}
		if len(finished) != 2 {
			t.Fatalf("expected 2 run.finished events, got %d", len(finished))
		}
		lastStarted := started[0]
		if started[1].After(started[0]) {
			lastStarted = started[1]
		}
		firstFinished := finished[0]
		if finished[1].Before(finished[0]) {
			firstFinished = finished[1]
		}
		if !lastStarted.Before(firstFinished) {
			t.Fatal("expected both run.started events before first run.finished — runs did not overlap")
		}

		for _, branch := range []string{parallelBranch150, parallelBranch151} {
			branchHash := strings.TrimSpace(runGit(t, repoDir, "rev-parse", branch))
			if branchHash == baselineHash {
				t.Fatalf("expected branch %s commit beyond baseline, got %s", branch, branchHash)
			}
			if out, err := exec.Command("git", "merge-base", "--is-ancestor", baselineHash, branchHash).CombinedOutput(); err != nil {
				t.Fatalf("expected branch %s to descend from baseline: %v\n%s", branch, err, out)
			}
			remoteHash := strings.TrimSpace(runGit(t, repoDir, "ls-remote", "origin", "refs/heads/"+branch))
			if remoteHash == "" {
				t.Fatalf("expected pushed remote branch %s", branch)
			}
			fields := strings.Fields(remoteHash)
			if len(fields) == 0 || fields[0] != branchHash {
				t.Fatalf("remote branch %s hash mismatch: got %q, want %q", branch, remoteHash, branchHash)
			}
		}

		for _, tc := range []struct {
			issue      int
			branch     string
			wantReturn string
			wantTest   string
			failTest   string
		}{
			{parallelIssue150, parallelBranch150, "5", "TestDoubleFor150", "TestDoubleFor151"},
			{parallelIssue151, parallelBranch151, "7", "TestDoubleFor151", "TestDoubleFor150"},
		} {
			checkoutDir := t.TempDir()
			clone := exec.Command("git", "clone", "--branch", tc.branch, "--single-branch", remoteDir, checkoutDir)
			if out, err := clone.CombinedOutput(); err != nil {
				t.Fatalf("clone branch %s: %v: %s", tc.branch, err, out)
			}

			doubleSrc, err := os.ReadFile(filepath.Join(checkoutDir, "double.go"))
			if err != nil {
				t.Fatalf("read double.go on branch %s: %v", tc.branch, err)
			}
			if !strings.Contains(string(doubleSrc), "return "+tc.wantReturn) {
				t.Fatalf("branch %s double.go: expected return %s, got:\n%s", tc.branch, tc.wantReturn, doubleSrc)
			}

			testCmd := exec.Command("go", "test", "-run", tc.wantTest, "./...")
			testCmd.Dir = checkoutDir
			if out, err := testCmd.CombinedOutput(); err != nil {
				t.Fatalf("branch %s test %s failed: %v: %s", tc.branch, tc.wantTest, err, out)
			}

			testFailCmd := exec.Command("go", "test", "-run", tc.failTest, "./...")
			testFailCmd.Dir = checkoutDir
			if err := testFailCmd.Run(); err == nil {
				t.Fatalf("branch %s test %s should have failed but passed", tc.branch, tc.failTest)
			}
		}

		for i := 1; i <= 2; i++ {
			argsFile := filepath.Join(containerGhShimDir, fmt.Sprintf("pr-create.args.%d", i))
			if _, err := os.Stat(argsFile); err != nil {
				t.Fatalf("missing pr-create.args.%d: %v", i, err)
			}
		}
		countData, err := os.ReadFile(filepath.Join(containerGhShimDir, "pr-create.count"))
		if err != nil {
			t.Fatalf("read pr create count: %v", err)
		}
		if got := strings.TrimSpace(string(countData)); got != "2" {
			t.Fatalf("expected exactly two pr create invocations, got %q", got)
		}

		type prCreateCall struct {
			branch string
			title  string
			body   string
		}
		expected := map[string]prCreateCall{
			parallelBranch150: {parallelBranch150, "Fix 150", "Fixes #150"},
			parallelBranch151: {parallelBranch151, "Fix 151", "Fixes #151"},
		}
		seen := make(map[string]bool)
		for i := 1; i <= 2; i++ {
			argsData, err := os.ReadFile(filepath.Join(containerGhShimDir, fmt.Sprintf("pr-create.args.%d", i)))
			if err != nil {
				t.Fatalf("read pr create args.%d: %v", i, err)
			}
			args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
			head := prFlowFlagValue(args, "--head")
			if head == "" {
				t.Fatalf("pr create %d: missing --head flag in args:\n%s", i, argsData)
			}
			if seen[head] {
				t.Fatalf("pr create %d: duplicate branch %q", i, head)
			}
			seen[head] = true

			call, ok := expected[head]
			if !ok {
				t.Fatalf("pr create %d: unexpected --head %q, expected one of %q or %q", i, head, parallelBranch150, parallelBranch151)
			}
			if got := prFlowFlagValue(args, "--base"); got != "main" {
				t.Fatalf("pr create %d (%s): --base got %q, want %q", i, head, got, "main")
			}
			if got := prFlowFlagValue(args, "--title"); got != call.title {
				t.Fatalf("pr create %d (%s): --title got %q, want %q", i, head, got, call.title)
			}

			bodyData, err := os.ReadFile(filepath.Join(containerGhShimDir, fmt.Sprintf("pr-create.body.%d", i)))
			if err != nil {
				t.Fatalf("read pr create body.%d: %v", i, err)
			}
			if got := strings.TrimSpace(string(bodyData)); got != call.body {
				t.Fatalf("pr create %d (%s): body got %q, want %q", i, head, got, call.body)
			}
		}
		if len(seen) != 2 {
			t.Fatalf("expected pr creates for both branches, got %v", seen)
		}
	})
}

func seedParallelPRFlowRepo(t *testing.T, dir string) {
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

func TestDoubleFor150(t *testing.T) {
	if got := Double(2); got != 5 {
		t.Fatalf("Double(2) = %d, want 5", got)
	}
}

func TestDoubleFor151(t *testing.T) {
	if got := Double(2); got != 7 {
		t.Fatalf("Double(2) = %d, want 7", got)
	}
}

func TestDoubleFor152(t *testing.T) {
	if got := Double(2); got != 9 {
		t.Fatalf("Double(2) = %d, want 9", got)
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
	runGit(t, dir, "commit", "-m", "feat: seed parallel failing tests")
	runGit(t, dir, "push", "origin", "main")
}

func TestE2E_QueuedIssuesPersistAfterBatchCompletes(t *testing.T) {
	// CI: JUSTIFIED — calls requirePRFlowProvider (real provider auth) and requirePodmanE2E.
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	runPRFlowProviderCases(t, func(t *testing.T, tc prFlowProviderCase) {
		realHome := requirePRFlowProvider(t, tc)
		requirePodmanE2E(t)

		binPath := buildSandmanBinary(t)

		repoDir := t.TempDir()
		t.Chdir(repoDir)
		initRunIntegrationRepo(t, repoDir)

		remoteDir := filepath.Join(repoDir, "remote")
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

		seedParallelPRFlowRepo(t, repoDir)
		runGit(t, repoDir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

		setupIsolatedPRFlowHome(t, realHome, repoDir, "sandman-podman-e2e-queued-", tc.authPaths)

		out, err := runSandmanBinary(t, binPath, repoDir, "init", "--agent", tc.name)
		if err != nil {
			t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
		}

		if _, err := runSandmanBinary(t, binPath, repoDir, "config", "set", "review_command", "/oc review"); err != nil {
			t.Fatalf("sandman config set failed: %v", err)
		}

		ghShimDir := t.TempDir()
		writeFakeGHShimParallel(t, ghShimDir)
		prependPath(t, ghShimDir)

		containerGhShimDir := filepath.Join(repoDir, ".sandman", "bin")
		writeFakeGHShimForContainerParallel(t, containerGhShimDir)

		buildCmd := exec.Command("podman", "build", "-t", "sandman-e2e-queued", "-f",
			filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("build image: %v: %s", err, out)
		}

		customizePRFlowAgent(t, repoDir, tc, prFlowAgentOptions{container: true})
		writeParallelPRFlowPrompt(t, repoDir, tc)

		out, err = runSandmanBinary(t, binPath, repoDir, "run",
			"--agent", tc.name,
			"--sandbox", "podman",
			"--parallel", "1",
			"--container-capacity", "1",
			strconv.Itoa(parallelIssue150), strconv.Itoa(parallelIssue151), strconv.Itoa(parallelIssue152))
		t.Logf("sandman run returned err=%v output=%s", err, out)

		eventsPath := filepath.Join(repoDir, ".sandman", "events.jsonl")
		eventsData, err := os.ReadFile(eventsPath)
		if err != nil {
			t.Fatalf("read events: %v", err)
		}

		var completed, queued, blocked, started int
		var queuedIssues []int
		for _, line := range strings.Split(strings.TrimSpace(string(eventsData)), "\n") {
			if line == "" {
				continue
			}
			var evt events.Event
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				t.Fatalf("parse event: %v: %s", err, line)
			}
			switch evt.Type {
			case "run.finished":
				completed++
			case "run.queued":
				queued++
				queuedIssues = append(queuedIssues, evt.Issue)
			case "run.blocked":
				blocked++
			case "run.started":
				started++
			}
		}

		if started != 2 {
			t.Fatalf("expected 2 run.started events, got %d", started)
		}
		if completed != 2 {
			t.Fatalf("expected 2 run.finished events, got %d", completed)
		}
		if queued != 1 {
			t.Fatalf("expected 1 run.queued event for blocked issue 152, got %d", queued)
		}
		if blocked != 0 {
			t.Fatalf("expected 0 run.blocked events (issue 152 should queue, not block), got %d", blocked)
		}

		foundQueued152 := false
		for _, q := range queuedIssues {
			if q == parallelIssue152 {
				foundQueued152 = true
				break
			}
		}
		if !foundQueued152 {
			t.Fatalf("expected run.queued for issue %d, got queued issues: %v", parallelIssue152, queuedIssues)
		}

		deps := prFlowDeps(repoDir)
		root := NewRootCmd(deps)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)

		var buf bytes.Buffer
		root.SetArgs([]string{"portal", "--no-open"})
		root.SetOut(&buf)
		root.SetErr(&buf)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		t.Cleanup(cancel)

		go func() {
			_ = root.ExecuteContext(ctx)
		}()

		portalReadyDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(portalReadyDeadline) {
			if conn, ok := pollTCPAddrOnce("127.0.0.1:5000", 100*time.Millisecond); ok {
				_ = conn.Close()
				break
			}
		}
		cancel()

		runs, err := (&portalRunsView{}).compute(repoDir, &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")})
		if err != nil {
			t.Fatalf("load portal runs after batch: %v", err)
		}

		byIssue := map[int]portalRun{}
		for _, run := range runs {
			byIssue[run.IssueNumber] = run
		}

		if run, ok := byIssue[parallelIssue150]; !ok {
			t.Fatalf("issue %d not found in portal runs", parallelIssue150)
		} else if run.Status != "success" {
			t.Fatalf("expected issue %d status=success, got %q", parallelIssue150, run.Status)
		}

		if run, ok := byIssue[parallelIssue151]; !ok {
			t.Fatalf("issue %d not found in portal runs", parallelIssue151)
		} else if run.Status != "success" {
			t.Fatalf("expected issue %d status=success, got %q", parallelIssue151, run.Status)
		}

		if run, ok := byIssue[parallelIssue152]; !ok {
			t.Fatalf("issue %d (queued) not found in portal runs after batch ends", parallelIssue152)
		} else if run.Kind != "completed" {
			t.Fatalf("expected issue %d kind=completed, got %q", parallelIssue152, run.Kind)
		} else if run.Status != "queued" {
			t.Fatalf("expected issue %d status=queued, got %q", parallelIssue152, run.Status)
		}
	})
}

func writeParallelPRFlowPrompt(t *testing.T, repoDir string, tc prFlowProviderCase) {
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
	agent.Command = fmt.Sprintf(`printf 'containerhostname=%%s\ncontainerworkdir=%%s\n' "$(hostname)" "$(pwd)" && PATH=/workspace/.sandman/bin:${PATH} opencode run --pure --dangerously-skip-permissions -m %s "$(cat {{.PromptFile}})"`, tc.model)
	if cfg.AgentProviders == nil {
		cfg.AgentProviders = map[string]config.Agent{}
	}
	cfg.AgentProviders["opencode"] = agent
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

func writeFakeGHShimParallel(t *testing.T, dir string) {
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
      if [ "$count" -gt 3 ]; then
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
      printf 'https://example.test/example/sandbox/pull/%s\n' "$count"
      exit 0
    fi
    if [ "${2:-}" = "checks" ]; then
      printf 'all checks passed\n'
      exit 0
    fi
    if [ "${2:-}" = "comment" ]; then
      printf 'commented\n'
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
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
      repos/example/sandbox/issues/150)
        cat <<'JSON'
{"number":150,"title":"Fix 150","body":"Run go test -run TestDoubleFor150 ./... Make Double(2) return 5. Do not make TestDoubleFor151 pass in this branch.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/150/events)
        printf '[]\n'
        exit 0
        ;;
      repos/example/sandbox/issues/151)
        cat <<'JSON'
{"number":151,"title":"Fix 151","body":"Run go test -run TestDoubleFor151 ./... Make Double(2) return 7. Do not make TestDoubleFor150 pass in this branch.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/151/events)
        printf '[]\n'
        exit 0
        ;;
      repos/example/sandbox/issues/152)
        cat <<'JSON'
{"number":152,"title":"Fix 152","body":"Run go test -run TestDoubleFor152 ./... Make Double(2) return 9. This issue is blocked by issue 150.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/152/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %s\n' "$path" >&2
    exit 1
    ;;
  auth)
    if [ "${2:-}" = "token" ]; then
      printf 'ghp_xxxxxxxxxxxxxxxxxxxx\n'
      exit 0
    fi
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

func writeFakeGHShimForContainerParallel(t *testing.T, hostDir string) {
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
      current=$(cat "$count_file" 2>/dev/null || echo 0)
      current=$((current + 1))
      printf '%s\n' "$current" > "$count_file"
      if [ "$current" -gt 3 ]; then
        printf 'unexpected gh pr create invocation #%s\n' "$current" >&2
        exit 1
      fi

      args_file="$shim_dir/pr-create.args.$current"
      body_file="$shim_dir/pr-create.body.$current"

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
      printf 'https://example.test/example/sandbox/pull/%s\n' "$current"
      exit 0
    fi
    if [ "${2:-}" = "checks" ]; then
      printf 'all checks passed\n'
      exit 0
    fi
    if [ "${2:-}" = "comment" ]; then
      printf 'commented\n'
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
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
      repos/example/sandbox/issues/150)
        cat <<'JSON'
{"number":150,"title":"Fix 150","body":"Run go test -run TestDoubleFor150 ./... Make Double(2) return 5. Do not make TestDoubleFor151 pass in this branch.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/150/events)
        printf '[]\n'
        exit 0
        ;;
      repos/example/sandbox/issues/151)
        cat <<'JSON'
{"number":151,"title":"Fix 151","body":"Run go test -run TestDoubleFor151 ./... Make Double(2) return 7. Do not make TestDoubleFor150 pass in this branch.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/151/events)
        printf '[]\n'
        exit 0
        ;;
      repos/example/sandbox/issues/152)
        cat <<'JSON'
{"number":152,"title":"Fix 152","body":"Run go test -run TestDoubleFor152 ./... Make Double(2) return 9. This issue is blocked by issue 150.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/152/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %s\n' "$path" >&2
    exit 1
    ;;
  auth)
    if [ "${2:-}" = "token" ]; then
      printf 'ghp_xxxxxxxxxxxxxxxxxxxx\n'
      exit 0
    fi
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

	repoDir := filepath.Dir(filepath.Dir(hostDir))
	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile = append(dockerfile, []byte("\nCOPY .sandman/bin/gh /usr/local/bin/gh\nRUN chmod +x /usr/local/bin/gh\n")...)
	if err := os.WriteFile(dockerfilePath, dockerfile, 0644); err != nil {
		t.Fatalf("append gh shim to Dockerfile: %v", err)
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

func prFlowProviderNames() []string {
	names := make([]string, len(prFlowProviderCases))
	for i, tc := range prFlowProviderCases {
		names[i] = tc.name
	}
	return names
}

func hasProviderAuth(home string, paths []string) bool {
	for _, rel := range paths {
		if _, err := os.Stat(homePath(home, rel)); err == nil {
			return true
		}
	}
	return false
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

func buildSandmanBinary(t *testing.T) string {
	t.Helper()

	binPath := filepath.Join(t.TempDir(), "sandman")
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/sandman")
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve source file path")
	}
	cmd.Dir = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build sandman binary: %v: %s", err, out)
	}
	return binPath
}

func runSandmanBinary(t *testing.T, binPath, workDir string, args ...string) (string, error) {
	t.Helper()

	ghBin := filepath.Join(workDir, ".sandman", "bin")
	cmd := exec.Command(binPath, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"PATH="+ghBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_TOKEN=fake",
		"GITHUB_TOKEN=fake",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
