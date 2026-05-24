//go:build smoke

package cmd

import (
	"bytes"
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
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/scaffold"
)

const smokePrompt = `# Smoke test

Issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

{{ISSUE_BODY}}

Return exactly SMOKE_OK and do not modify files.
`

const (
	smokeGitName  = "Smoke User"
	smokeGitEmail = "smoke@example.com"
)

type smokeProviderCase struct {
	name         string
	hostCLI      string
	buildTools   string
	model        string
	issue        github.Issue
	requiredAuth []string
	authPaths    []string
	wantBranch   string
}

type smokePrompter struct{}

func (smokePrompter) Confirm(string) (bool, error) {
	return false, nil
}

func (smokePrompter) Select(string, []string) (string, error) {
	return "", nil
}

var smokeProviderCases = []smokeProviderCase{
	{
		name:       "opencode",
		hostCLI:    "opencode",
		buildTools: "generic",
		model:      "opencode/big-pickle",
		issue: github.Issue{
			Number: 421,
			Title:  "Smoke opencode",
			Body:   "Reply with exactly SMOKE_OK.",
		},
		wantBranch: "sandman/421-smoke-opencode",
		requiredAuth: []string{
			"~/.local/share/opencode/auth.json",
		},
		authPaths: []string{
			"~/.config/opencode",
			"~/.local/share/opencode",
		},
	},
	{
		name:       "pi",
		hostCLI:    "pi",
		buildTools: "generic",
		model:      "kilo/kilo-auto/free",
		issue: github.Issue{
			Number: 424,
			Title:  "Smoke pi",
			Body:   "Reply with exactly SMOKE_OK.",
		},
		wantBranch: "sandman/424-smoke-pi",
		authPaths: []string{
			"~/.pi",
		},
	},
}

func TestSmoke_RealAgentCLIs(t *testing.T) {
	runSmokeProviderCases(t, smokeProviderCases)
}

func TestSmoke_RealAgentCLIs_GoPreset(t *testing.T) {
	cases := make([]smokeProviderCase, len(smokeProviderCases))
	for i, tc := range smokeProviderCases {
		tc.buildTools = "go"
		cases[i] = tc
	}
	runSmokeProviderCases(t, cases)
}

func TestSmoke_RealAgentCLIs_PythonPreset(t *testing.T) {
	cases := make([]smokeProviderCase, len(smokeProviderCases))
	for i, tc := range smokeProviderCases {
		tc.buildTools = "python"
		cases[i] = tc
	}
	runSmokeProviderCases(t, cases)
}

func runSmokeProviderCases(t *testing.T, cases []smokeProviderCase) {
	allowed, err := parseSmokeProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) == 0 {
		t.Skip("set SANDMAN_SMOKE_PROVIDERS=opencode,pi and run `go test -tags smoke ./internal/cmd -run Smoke`")
	}

	for _, tc := range cases {
		tc := tc
		if !allowed[tc.name] {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			runSmokeProvider(t, tc)
		})
	}
}

func parseSmokeProviders() (map[string]bool, error) {
	raw := strings.TrimSpace(os.Getenv("SANDMAN_SMOKE_PROVIDERS"))
	if raw == "" {
		return nil, nil
	}
	if raw == "all" || raw == "*" {
		allowed := make(map[string]bool, len(smokeProviderCases))
		for _, tc := range smokeProviderCases {
			allowed[tc.name] = true
		}
		return allowed, nil
	}

	allowed := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		switch name {
		case "opencode", "pi":
			allowed[name] = true
		default:
			return nil, fmt.Errorf("unknown smoke provider %q", name)
		}
	}
	return allowed, nil
}

func runSmokeProvider(t *testing.T, tc smokeProviderCase) {
	t.Helper()
	if tc.name == "pi" {
		setupPiTestShim(t)
	}
	ensureSmokeHostCLI(t, tc)

	runtime, err := sandbox.ResolveRuntime("podman")
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	remoteDir := initRunIntegrationRepoWithRemote(t, repoDir)
	runGit(t, repoDir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	homeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0755); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	if !hasSmokeAuth(realHome, tc.requiredAuth, tc.authPaths) {
		probePaths := tc.requiredAuth
		if len(probePaths) == 0 {
			probePaths = tc.authPaths
		}
		t.Skipf("missing file-backed auth for %s; expected one of %s under %s", tc.name, strings.Join(probePaths, ", "), realHome)
	}
	if !copySmokeAuthLayout(t, realHome, homeDir, tc.authPaths) {
		t.Skipf("missing file-backed auth for %s; expected one of %s under %s", tc.name, strings.Join(tc.authPaths, ", "), realHome)
	}
	ensureSmokeWritableDirs(t, homeDir, tc.name)
	if err := writeSmokeGitConfig(homeDir, remoteDir); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
	if tc.name == "pi" {
		ensurePiFreePackage(t, homeDir)
	}

	warmSmokeRuntime(t, runtime)

	s := &scaffold.Scaffolder{}
	if err := s.Scaffold(repoDir, scaffold.Options{BuildTools: tc.buildTools, Agent: tc.name}, smokePrompter{}); err != nil {
		t.Fatalf("scaffold repo: %v", err)
	}
	if tc.name == "pi" {
		writePiTestShim(t, filepath.Join(repoDir, ".sandman", "bin"))
		appendPiTestShimToDockerfile(t, repoDir)
	}
	if err := addSmokeDockerDeps(repoDir, tc.name); err != nil {
		t.Fatalf("update Dockerfile: %v", err)
	}
	smokeCfg, err := customizeSmokeConfig(repoDir, tc.name, tc.model)
	if err != nil {
		t.Fatalf("update config: %v", err)
	}
	imageTag := preflightSmokeImage(t, runtime, repoDir, tc.name)
	preflightSmokeContainer(t, runtime, imageTag, repoDir, homeDir, tc.name, tc.buildTools, tc.authPaths)
	preflightSmokeWorktree(t, repoDir, tc.wantBranch)

	issue := tc.issue
	gh := &fakeGitHubClient{issues: map[int]*github.Issue{issue.Number: &issue}}
	store := &fakeStore{config: smokeCfg}
	deps := Dependencies{
		BatchRunner:    batch.NewOrchestrator(gh, &prompt.Engine{}, store, nil),
		ConfigStore:    store,
		GitHubClient:   gh,
		PromptRenderer: &prompt.Engine{},
		IsTTY:          func() bool { return false },
	}

	out, err := executeSmokeRun(t, deps, runtime, issue.Number)
	if err != nil {
		logPath := filepath.Join(repoDir, ".sandman", "logs", fmt.Sprintf("%d.log", issue.Number))
		worktreePath := filepath.Join(repoDir, ".sandman", "worktrees", tc.wantBranch)
		promptPath := filepath.Join(worktreePath, ".sandman", "rendered-prompt.md")
		if _, statErr := os.Stat(worktreePath); statErr == nil {
			if promptData, promptErr := os.ReadFile(promptPath); promptErr == nil {
				t.Fatalf("unexpected error: %v\noutput:\n%s\nworktree exists: %s\nprompt:\n%s", err, out, worktreePath, promptData)
			}
			t.Fatalf("unexpected error: %v\noutput:\n%s\nworktree exists: %s\nprompt read error: %v", err, out, worktreePath, statErr)
		}
		logData, readErr := os.ReadFile(logPath)
		if readErr == nil {
			t.Fatalf("unexpected error: %v\noutput:\n%s\nlog:\n%s", err, out, logData)
		} else if entries, dirErr := os.ReadDir(filepath.Dir(logPath)); dirErr == nil {
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			t.Fatalf("unexpected error: %v\noutput:\n%s\nlog read error: %v\nlogs dir entries: %s", err, out, readErr, strings.Join(names, ", "))
		} else {
			t.Fatalf("unexpected error: %v\noutput:\n%s\nlog read error: %v", err, out, readErr)
		}
	}

	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, tc.wantBranch) {
		t.Fatalf("expected branch %q in output, got:\n%s", tc.wantBranch, out)
	}

	logPath := filepath.Join(repoDir, ".sandman", "logs", fmt.Sprintf("%d.log", issue.Number))
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "SMOKE_OK") {
		t.Fatalf("expected log to include SMOKE_OK, got:\n%s", logData)
	}

	worktreePath := filepath.Join(repoDir, ".sandman", "worktrees", tc.wantBranch)
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected worktree to be preserved, got: %v", err)
	}
}

func executeSmokeRun(t *testing.T, deps Dependencies, runtime string, issueNumber int) (string, error) {
	t.Helper()

	var buf bytes.Buffer
	cmd := NewRootCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"run",
		"--sandbox", runtime,
		"--parallel", "1",
		"--prompt", smokePrompt,
		fmt.Sprintf("%d", issueNumber),
	})

	ctx, cancel := smokeContext(t)
	defer cancel()
	cmd.SetContext(ctx)

	err := cmd.Execute()
	return buf.String(), err
}

func smokeContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()

	if deadline, ok := t.Deadline(); ok {
		timeout := time.Until(deadline) - 30*time.Second
		if timeout > 0 {
			return context.WithTimeout(context.Background(), timeout)
		}
	}

	return context.WithTimeout(context.Background(), 15*time.Minute)
}

func warmSmokeRuntime(t *testing.T, runtime string) {
	t.Helper()

	cmd := exec.Command(runtime, "run", "--rm", "alpine", "echo", "ok")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("warm %s: %v: %s", runtime, err, out)
	}
}

func writeSmokeGitConfig(homeDir, remoteDir string) error {
	gitConfig := fmt.Sprintf("[url %q]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n[user]\n\tname = %s\n\temail = %s\n", "file://"+remoteDir, smokeGitName, smokeGitEmail)
	return os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitConfig), 0644)
}

func copySmokeAuthLayout(t *testing.T, realHome, tempHome string, paths []string) bool {
	t.Helper()

	copied := false
	for _, rel := range paths {
		src := homePath(realHome, rel)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := homePath(tempHome, rel)
		if err := copySmokePath(src, dst); err != nil {
			t.Fatalf("copy %s: %v", rel, err)
		}
		copied = true
	}
	for _, rel := range paths {
		if strings.HasSuffix(rel, ".json") {
			continue
		}
		dst := homePath(tempHome, rel)
		if err := os.MkdirAll(dst, 0755); err != nil {
			t.Fatalf("ensure %s: %v", rel, err)
		}
		if err := os.Chmod(dst, 0777); err != nil {
			t.Fatalf("chmod %s: %v", rel, err)
		}
	}
	cacheDir := homePath(tempHome, "~/.cache")
	if err := os.MkdirAll(cacheDir, 0777); err != nil {
		t.Fatalf("ensure cache dir: %v", err)
	}
	if err := os.Chmod(cacheDir, 0777); err != nil {
		t.Fatalf("chmod cache dir: %v", err)
	}
	return copied
}

func hasSmokeAuth(realHome string, requiredPaths, fallbackPaths []string) bool {
	paths := requiredPaths
	if len(paths) == 0 {
		paths = fallbackPaths
	}
	for _, rel := range paths {
		if _, err := os.Stat(homePath(realHome, rel)); err == nil {
			return true
		}
	}
	return false
}

func homePath(home, rel string) string {
	if strings.HasPrefix(rel, "~") {
		rel = strings.TrimPrefix(rel, "~")
	}
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	return filepath.Join(home, rel)
}

func copySmokePath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copySmokeDir(src, dst)
	}
	return copySmokeFile(src, dst, info.Mode())
}

func copySmokeDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if err := copySmokePath(srcPath, dstPath); err != nil {
			return err
		}
	}
	return os.Chmod(dst, 0777)
}

func copySmokeFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode.Perm())
}

func addSmokeDockerDeps(repoDir, provider string) error {
	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return err
	}
	if provider == "opencode" {
		data = append(data, []byte("RUN command -v opencode >/dev/null\n")...)
	}
	return os.WriteFile(dockerfilePath, data, 0644)
}

func customizeSmokeConfig(repoDir, provider, model string) (*config.Config, error) {
	configPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	resolved, err := cfg.ResolveAgentProvider(provider)
	if err != nil {
		return nil, err
	}
	resolved.Model = model
	if provider == "opencode" {
		resolved.Command = strings.Join([]string{
			fmt.Sprintf(`test "$(git config user.name)" = %q`, smokeGitName),
			fmt.Sprintf(`test "$(git config user.email)" = %q`, smokeGitEmail),
			fmt.Sprintf(`opencode run --pure --dangerously-skip-permissions -m %s "$(cat {{.PromptFile}})"`, model),
		}, " && ")
		for _, dir := range []string{"~/.cache/opencode", "~/.cache/opencode/bin"} {
			if !containsSmokePath(resolved.ConfigDirs, dir) {
				resolved.ConfigDirs = append(resolved.ConfigDirs, dir)
			}
		}
	} else if provider == "pi" {
		resolved.Command = `PATH=/workspace/.sandman/bin:${PATH} pi --print --provider {{.ModelProvider}}{{if .ModelName}} --model {{.ModelName}}{{end}} "$(cat {{.PromptFile}})"`
	}
	if cfg.AgentProviders == nil {
		cfg.AgentProviders = map[string]config.Agent{}
	}
	cfg.AgentProviders[provider] = resolved
	if cfg.Agents == nil {
		cfg.Agents = map[string]config.Agent{}
	}
	cfg.Agents[provider] = resolved
	return cfg, nil
}

func ensureSmokeHostCLI(t *testing.T, tc smokeProviderCase) {
	t.Helper()

	if _, err := exec.LookPath(tc.hostCLI); err != nil {
		t.Skipf("skip %s smoke: host CLI %q not installed", tc.name, tc.hostCLI)
	}
}

func containsSmokePath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func ensureSmokeWritableDirs(t *testing.T, homeDir, provider string) {
	t.Helper()

	dirs := []string{"~/.cache"}
	if provider == "opencode" {
		dirs = append(dirs, "~/.cache/opencode", "~/.cache/opencode/bin")
	}
	for _, rel := range dirs {
		path := homePath(homeDir, rel)
		if err := os.MkdirAll(path, 0777); err != nil {
			t.Fatalf("ensure %s: %v", rel, err)
		}
		if err := os.Chmod(path, 0777); err != nil {
			t.Fatalf("chmod %s: %v", rel, err)
		}
	}
}

func ensurePiFreePackage(t *testing.T, homeDir string) {
	t.Helper()

	cmd := exec.Command("pi", "install", "git:github.com/apmantza/pi-free")
	cmd.Env = append(os.Environ(), "HOME="+homeDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install pi-free: %v: %s", err, out)
	}
}

func preflightSmokeImage(t *testing.T, runtime, repoDir, provider string) string {
	t.Helper()

	tag := fmt.Sprintf("sandman-smoke-%s-%d:latest", provider, time.Now().UnixNano())
	cmd := exec.Command(runtime, "build", "-t", tag, "-f", filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("skip %s smoke: container image build unavailable: %v\n%s", provider, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command(runtime, "rmi", "-f", tag).Run()
	})
	return tag
}

func preflightSmokeContainer(t *testing.T, runtime, imageTag, repoDir, homeDir, provider, buildTools string, authPaths []string) {
	t.Helper()

	startOpts := sandbox.StartOptions{
		UserID:       fmt.Sprintf("%d", os.Getuid()),
		SSH:          true,
		RemoteScheme: "ssh",
	}
	gitConfigPath := filepath.Join(homeDir, ".gitconfig")
	if _, err := os.Stat(gitConfigPath); err == nil {
		startOpts.GitConfigPath = gitConfigPath
	}
	gitConfigDir := filepath.Join(homeDir, ".config", "git")
	if _, err := os.Stat(gitConfigDir); err == nil {
		startOpts.AgentConfigDirs = append(startOpts.AgentConfigDirs, gitConfigDir)
	}
	for _, rel := range authPaths {
		path := homePath(homeDir, rel)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			startOpts.AgentConfigDirs = append(startOpts.AgentConfigDirs, path)
			continue
		}
		startOpts.AgentConfigFiles = append(startOpts.AgentConfigFiles, path)
	}
	if provider == "opencode" {
		startOpts.AgentConfigDirs = append(startOpts.AgentConfigDirs,
			homePath(homeDir, "~/.cache/opencode"),
			homePath(homeDir, "~/.cache/opencode/bin"),
		)
	}
	cleanup, err := batch.PrepareContainerConfigMounts(repoDir, &startOpts)
	if err != nil {
		t.Skipf("skip %s smoke: prepare config mounts unavailable: %v", provider, err)
	}
	defer cleanup()

	rt := sandbox.NewContainerRuntime(runtime)
	container, err := rt.Start(imageTag, repoDir, startOpts)
	if err != nil {
		t.Skipf("skip %s smoke: container start unavailable: %v", provider, err)
	}
	defer container.Stop()

	assertCmd := "set -eu; command -v gh >/dev/null; test -w /.cache; test -w /.local; test -w /.config"
	if buildTools == "go" {
		assertCmd += "; command -v go >/dev/null; test \"$(go env GOPATH)\" = \"/.local/share/go\"; test \"$(go env GOMODCACHE)\" = \"/.cache/go/pkg/mod\"; mkdir -p /.cache/go/pkg/mod /.local/share/go; test -w /.cache/go/pkg/mod; test -w /.local/share/go"
	}
	if buildTools == "python" {
		assertCmd += "; command -v python >/dev/null || command -v python3 >/dev/null"
	}
	check := exec.Command(runtime, "exec", container.ID(), "sh", "-c", assertCmd)
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("preflight assertions failed for %s/%s: %v\n%s", provider, buildTools, err, out)
	}
}

func preflightSmokeWorktree(t *testing.T, repoDir, branch string) {
	t.Helper()

	preflightBranch := branch + "-preflight"

	if err := sandbox.SyncDefaultBranch(repoDir, "main"); err != nil {
		t.Skipf("skip smoke: default branch sync unavailable: %v", err)
	}

	wt := sandbox.NewWorktreeSandbox(repoDir, filepath.Join(repoDir, ".sandman", "worktrees"), preflightBranch, "main")
	if err := wt.Start(); err != nil {
		t.Skipf("skip smoke: worktree start unavailable: %v", err)
	}
	_ = wt.Stop()
	if out, err := exec.Command("git", "branch", "-D", preflightBranch).CombinedOutput(); err != nil {
		t.Skipf("skip smoke: worktree cleanup unavailable: %v\n%s", err, out)
	}
}
