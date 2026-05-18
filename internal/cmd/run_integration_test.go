package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func initRunIntegrationRepo(t *testing.T, dir string) {
	t.Helper()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "checkout", "-b", "main"},
		{"git", "commit", "--allow-empty", "-m", "init"},
	}
	for _, cmd := range cmds {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func initRunIntegrationRepoWithRemote(t *testing.T, dir string) string {
	t.Helper()
	initRunIntegrationRepo(t, dir)

	remoteDir := t.TempDir()
	cmd := exec.Command("git", "init", "--bare")
	cmd.Dir = remoteDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare remote: %v: %s", err, out)
	}

	addRemote := exec.Command("git", "remote", "add", "origin", remoteDir)
	addRemote.Dir = dir
	if out, err := addRemote.CombinedOutput(); err != nil {
		t.Fatalf("add origin remote: %v: %s", err, out)
	}

	push := exec.Command("git", "push", "-u", "origin", "main")
	push.Dir = dir
	if out, err := push.CombinedOutput(); err != nil {
		t.Fatalf("push main: %v: %s", err, out)
	}

	return remoteDir
}

func newRunIntegrationDepsWithAgent(agent config.Agent, gh *fakeGitHubClient) Dependencies {
	return newRunIntegrationDepsWithSandbox(agent, "worktree", gh)
}

func newRunIntegrationDeps(agentCommand string, gh *fakeGitHubClient) Dependencies {
	return newRunIntegrationDepsWithAgent(config.Agent{
		Name:    "test-agent",
		Command: agentCommand,
	}, gh)
}

func executeRunCommand(t *testing.T, deps Dependencies, args ...string) (string, error) {
	t.Helper()

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)

	err := cmd.Execute()
	return buf.String(), err
}

var podmanWarmupOnce sync.Once

func podmanAvailable(t *testing.T) bool {
	t.Helper()
	cmd := exec.Command("podman", "version")
	if err := cmd.Run(); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("podman not available in CI: %v", err)
		}
		t.Skip("podman not available")
		return false
	}
	podmanWarmupOnce.Do(func() {
		if out, err := exec.Command("podman", "run", "--rm", "alpine", "echo", "ok").CombinedOutput(); err != nil {
			t.Logf("podman warmup failed: %v: %s", err, out)
		}
	})
	return true
}

type recordingEventLog struct {
	events []events.Event
}

func (l *recordingEventLog) Log(event events.Event) error {
	l.events = append(l.events, event)
	return nil
}

func (l *recordingEventLog) Read() ([]events.Event, error) {
	return append([]events.Event(nil), l.events...), nil
}

func writeSandmanDockerfile(t *testing.T, dir string) {
	t.Helper()
	dockerfileDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(dockerfileDir, 0755); err != nil {
		t.Fatalf("create .sandman dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dockerfileDir, "Dockerfile"), []byte("FROM alpine\n"), 0644); err != nil {
		t.Fatalf("write .sandman/Dockerfile: %v", err)
	}
}

func newRunIntegrationDepsWithSandbox(agent config.Agent, sandboxMode string, gh *fakeGitHubClient) Dependencies {
	if agent.Name == "" {
		agent.Name = "test-agent"
	}
	if agent.Command == "" {
		agent.Command = "true"
	}

	store := &fakeStore{config: &config.Config{
		Agent:       "test-agent",
		WorktreeDir: ".sandman/worktrees",
		Sandbox:     sandboxMode,
		Git:         config.GitConfig{DefaultBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"test-agent": agent,
		},
	}}

	runner := batch.NewOrchestrator(gh, &prompt.Engine{}, store, nil)
	return Dependencies{
		BatchRunner:    runner,
		ConfigStore:    store,
		EventLog:       &fakeEventLog{},
		GitHubClient:   gh,
		PromptRenderer: &prompt.Engine{},
		IsTTY:          func() bool { return false },
	}
}

func issueAwareAgentCommand(body string) string {
	return strings.TrimSpace(`
# The agent command is shared across AgentRuns, so infer the issue number
# from the worktree directory each run executes inside.
repo_root=$(dirname "$(dirname "$(dirname "$(dirname "$PWD")")")")
issue_dir=$(basename "$PWD")
issue=${issue_dir%%-*}
` + body)
}

func TestRun_DependencyAwareBatch_IncludeDependenciesExecutesTransitiveChain(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	deps := newRunIntegrationDeps(issueAwareAgentCommand(`
state_dir="$repo_root/.sandman/chain"
mkdir -p "$state_dir"
printf '%s\n' "$issue" >> "$state_dir/start-order"

case "$issue" in
  42)
    if [ ! -f "$state_dir/7.done" ]; then
      exit 1
    fi
    ;;
  100)
    if [ ! -f "$state_dir/42.done" ]; then
      exit 1
    fi
    ;;
esac

touch "$state_dir/$issue.done"
`), &fakeGitHubClient{issues: map[int]*github.Issue{
		100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
		42:  {Number: 42, Title: "Refactor", BlockedBy: []int{7}},
		7:   {Number: 7, Title: "Groundwork"},
	}})

	out, err := executeRunCommand(t, deps, "--include-dependencies", "100")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 3 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	order, err := os.ReadFile(filepath.Join(dir, ".sandman", "chain", "start-order"))
	if err != nil {
		t.Fatalf("read start order: %v", err)
	}
	if got := strings.TrimSpace(string(order)); got != "7\n42\n100" {
		t.Fatalf("expected start order 7 -> 42 -> 100, got %q", got)
	}
}

func TestRun_DependencyAwareBatch_InvalidGraphsFailBeforeExecution(t *testing.T) {
	tests := []struct {
		name    string
		issues  map[int]*github.Issue
		args    []string
		wantErr string
	}{
		{
			name: "cycle detection",
			issues: map[int]*github.Issue{
				100: {Number: 100, Title: "Feature", BlockedBy: []int{101}},
				101: {Number: 101, Title: "Refactor", BlockedBy: []int{100}},
			},
			args:    []string{"100", "101"},
			wantErr: "dependency cycle detected: #100 -> #101 -> #100",
		},
		{
			name: "missing blocker in strict mode",
			issues: map[int]*github.Issue{
				100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
				42:  {Number: 42, Title: "Blocker"},
			},
			args:    []string{"100"},
			wantErr: "missing blockers: #42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			initRunIntegrationRepo(t, dir)

			deps := newRunIntegrationDeps(issueAwareAgentCommand(`
state_dir="$repo_root/.sandman/executed"
mkdir -p "$state_dir"
touch "$state_dir/$issue"
`), &fakeGitHubClient{issues: tc.issues})

			out, err := executeRunCommand(t, deps, tc.args...)
			if err == nil {
				t.Fatal("expected dependency resolution error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
			if strings.Contains(out, "Summary:") {
				t.Fatalf("expected dependency resolution to fail before execution, got:\n%s", out)
			}

			executedDir := filepath.Join(dir, ".sandman", "executed")
			if _, statErr := os.Stat(executedDir); !os.IsNotExist(statErr) {
				t.Fatalf("expected no agent execution, but %s exists", executedDir)
			}
		})
	}
}

func TestRun_DependencyAwareBatch_BlocksDependentsAfterFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	deps := newRunIntegrationDeps(issueAwareAgentCommand(`
state_dir="$repo_root/.sandman/blocked"
mkdir -p "$state_dir"
touch "$state_dir/started-$issue"

if [ "$issue" = "42" ]; then
  exit 1
fi

touch "$state_dir/ran-$issue"
`), &fakeGitHubClient{issues: map[int]*github.Issue{
		42:  {Number: 42, Title: "Blocker"},
		100: {Number: 100, Title: "Dependent", BlockedBy: []int{42}},
	}})

	out, err := executeRunCommand(t, deps, "42", "100")
	if err == nil {
		t.Fatal("expected blocker failure to return an error")
	}
	if !strings.Contains(err.Error(), "1 of 2 runs failed") {
		t.Fatalf("expected partial failure error, got %v", err)
	}
	if !strings.Contains(out, "Summary: 0 succeeded, 1 failed, 1 blocked") {
		t.Fatalf("expected blocked summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#42  failure") {
		t.Fatalf("expected blocker failure in summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#100  blocked") {
		t.Fatalf("expected dependent blocked in summary, got:\n%s", out)
	}

	if _, statErr := os.Stat(filepath.Join(dir, ".sandman", "blocked", "started-42")); statErr != nil {
		t.Fatalf("expected blocker to execute, got %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".sandman", "blocked", "started-100")); !os.IsNotExist(statErr) {
		t.Fatalf("expected blocked issue not to start, got %v", statErr)
	}
}

func TestRun_DependencyAwareBatch_NoDependenciesRemainConcurrent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	deps := newRunIntegrationDeps(issueAwareAgentCommand(`
state_dir="$repo_root/.sandman/no-deps"
mkdir -p "$state_dir"
touch "$state_dir/start-$issue"

attempts=0
count=0
while [ "$attempts" -lt 500 ]; do
  count=0
  for path in "$state_dir"/start-*; do
    if [ -e "$path" ]; then
      count=$((count + 1))
    fi
  done
  if [ "$count" -ge 3 ]; then
    break
  fi
  attempts=$((attempts + 1))
  sleep 0.02
done

if [ "$count" -lt 3 ]; then
  exit 1
fi

touch "$state_dir/finish-$issue"
`), &fakeGitHubClient{issues: map[int]*github.Issue{
		10: {Number: 10, Title: "One"},
		11: {Number: 11, Title: "Two"},
		12: {Number: 12, Title: "Three"},
	}})

	out, err := executeRunCommand(t, deps, "--parallel", "3", "10", "11", "12")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 3 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	for _, issue := range []string{"10", "11", "12"} {
		if _, statErr := os.Stat(filepath.Join(dir, ".sandman", "no-deps", "finish-"+issue)); statErr != nil {
			t.Fatalf("expected issue %s to finish, got %v", issue, statErr)
		}
	}
}

func TestRun_WorktreeSandboxSingleIssuePersistsLogAndRemovesWorktree(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	deps := newRunIntegrationDeps(`printf '%s\n' "agent stdout"`, &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
	}})

	out, err := executeRunCommand(t, deps, "--sandbox", "worktree", "42")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#42  success  sandman/42-fix-bug") {
		t.Fatalf("expected branch string in summary, got:\n%s", out)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "agent stdout") {
		t.Fatalf("expected agent stdout in log, got:\n%s", logData)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree to be removed, got: %v", err)
	}
}

func TestRun_DefaultSandboxSingleIssue_MissingDockerfileFailsBeforeAgentRunBegins(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}

	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	deps := newRunIntegrationDepsWithSandbox(config.Agent{Name: "test-agent", Command: issueAwareAgentCommand(`
touch "$repo_root/.sandman/agent-executed"
`)}, "podman", &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
	}})
	eventLog := &recordingEventLog{}
	deps.EventLog = eventLog

	out, err := executeRunCommand(t, deps, "42")
	if err == nil {
		t.Fatal("expected failure when .sandman/Dockerfile is missing")
	}
	if !strings.Contains(err.Error(), ".sandman/Dockerfile") {
		t.Fatalf("expected error about missing Dockerfile, got: %v", err)
	}
	if strings.Contains(out, "Summary:") {
		t.Fatalf("expected no summary output before agent run, got:\n%s", out)
	}
	if len(eventLog.events) != 0 {
		t.Fatalf("expected no run events, got %d", len(eventLog.events))
	}

	markerPath := filepath.Join(dir, ".sandman", "agent-executed")
	if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected no agent marker, got %v", statErr)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	if _, statErr := os.Stat(logPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected no agent log, got %v", statErr)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("expected no worktree, got %v", statErr)
	}
}

func TestRun_DefaultSandboxSingleIssueUsesContainerWorkdirAndCleansUpWorktree(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}

	dir := t.TempDir()
	t.Chdir(dir)
	remoteDir := initRunIntegrationRepoWithRemote(t, dir)
	runGit(t, dir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	writeSandmanDockerfile(t, dir)

	homeDir, err := os.MkdirTemp("", "sandman-podman-home-")
	if err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0755); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	gitConfig := fmt.Sprintf("[user]\n\tname = Test\n[url %q]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n", "file://"+remoteDir)
	if err := os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitConfig), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	t.Setenv("HOME", homeDir)
	// Re-warm podman with the isolated HOME so the image cache lives outside the repo tree.
	if out, err := exec.Command("podman", "run", "--rm", "alpine", "echo", "ok").CombinedOutput(); err != nil {
		t.Fatalf("warm podman image for test home: %v: %s", err, out)
	}

	gh := &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
	}}
	deps := newRunIntegrationDepsWithSandbox(config.Agent{Name: "test-agent", Command: "pwd"}, "", gh)

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")

	out, err := executeRunCommand(t, deps, "42")
	if err != nil {
		if logData, logErr := os.ReadFile(logPath); logErr == nil {
			t.Fatalf("unexpected error: %v\noutput:\n%s\nlog:\n%s", err, out, logData)
		}
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#42  success  sandman/42-fix-bug") {
		t.Fatalf("expected branch string in summary, got:\n%s", out)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "/workspace/.sandman/worktrees/sandman/42-fix-bug") {
		t.Fatalf("expected container workdir in log, got:\n%s", logData)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree to be removed, got: %v", err)
	}
}

func TestRun_DefaultSandboxTwoIssuesReuseContainerAndSeparateWorktrees(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}

	dir := t.TempDir()
	t.Chdir(dir)
	remoteDir := initRunIntegrationRepoWithRemote(t, dir)
	runGit(t, dir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	writeSandmanDockerfile(t, dir)

	homeDir, err := os.MkdirTemp("", "sandman-podman-home-")
	if err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0755); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	gitConfig := fmt.Sprintf("[user]\n\tname = Test\n[url %q]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n", "file://"+remoteDir)
	if err := os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitConfig), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	t.Setenv("HOME", homeDir)
	// Re-warm podman with the isolated HOME so the image cache lives outside the repo tree.
	if out, err := exec.Command("podman", "run", "--rm", "alpine", "echo", "ok").CombinedOutput(); err != nil {
		t.Fatalf("warm podman image for test home: %v: %s", err, out)
	}

	gh := &fakeGitHubClient{issues: map[int]*github.Issue{
		42:  {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		100: {Number: 100, Title: "Add feature", Body: "Two runs should share one container."},
	}}
	deps := newRunIntegrationDepsWithSandbox(config.Agent{Name: "test-agent", Command: `
set -eu
printf 'container-identity=%s\n' "$(hostname)"
printf 'container-workdir=%s\n' "$PWD"
`}, "", gh)

	out, err := executeRunCommand(t, deps, "--parallel", "2", "42", "100")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 2 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	log42, err := os.ReadFile(filepath.Join(dir, ".sandman", "logs", "42.log"))
	if err != nil {
		t.Fatalf("read log for issue 42: %v", err)
	}
	log100, err := os.ReadFile(filepath.Join(dir, ".sandman", "logs", "100.log"))
	if err != nil {
		t.Fatalf("read log for issue 100: %v", err)
	}
	extract := func(logData []byte, prefix string) (string, bool) {
		for _, line := range strings.Split(strings.TrimSpace(string(logData)), "\n") {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
			}
		}
		return "", false
	}
	identity42, ok := extract(log42, "container-identity=")
	if !ok || identity42 == "" {
		t.Fatal("missing container identity for issue 42")
	}
	identity100, ok := extract(log100, "container-identity=")
	if !ok || identity100 == "" {
		t.Fatal("missing container identity for issue 100")
	}
	if got, want := identity42, identity100; got != want {
		t.Fatalf("expected the same container hostname, got %q and %q", got, want)
	}

	workdir42, ok := extract(log42, "container-workdir=")
	if !ok || workdir42 == "" {
		t.Fatal("missing container workdir for issue 42")
	}
	if got, want := workdir42, "/workspace/.sandman/worktrees/sandman/42-fix-bug"; got != want {
		t.Fatalf("expected issue 42 worktree %q, got %q", want, got)
	}

	workdir100, ok := extract(log100, "container-workdir=")
	if !ok || workdir100 == "" {
		t.Fatal("missing container workdir for issue 100")
	}
	if got, want := workdir100, "/workspace/.sandman/worktrees/sandman/100-add-feature"; got != want {
		t.Fatalf("expected issue 100 worktree %q, got %q", want, got)
	}
}

func TestRun_DefaultSandboxTwoIssuesQueueWithSingleContainerSlot(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}

	dir := t.TempDir()
	t.Chdir(dir)
	remoteDir := initRunIntegrationRepoWithRemote(t, dir)
	runGit(t, dir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	writeSandmanDockerfile(t, dir)

	homeDir, err := os.MkdirTemp("", "sandman-podman-home-")
	if err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0755); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	gitConfig := fmt.Sprintf("[user]\n\tname = Test\n[url %q]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n", "file://"+remoteDir)
	if err := os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitConfig), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	t.Setenv("HOME", homeDir)
	// Re-warm podman with the isolated HOME so the image cache lives outside the repo tree.
	if out, err := exec.Command("podman", "run", "--rm", "alpine", "echo", "ok").CombinedOutput(); err != nil {
		t.Fatalf("warm podman image for test home: %v: %s", err, out)
	}

	gh := &fakeGitHubClient{issues: map[int]*github.Issue{
		42:  {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		100: {Number: 100, Title: "Add feature", Body: "The second run should wait."},
	}}
	deps := newRunIntegrationDepsWithSandbox(config.Agent{Name: "test-agent", Command: issueAwareAgentCommand(`
	set -eu
	printf 'container-identity=%s\n' "$(hostname)"
	printf 'container-workdir=%s\n' "$PWD"

	state_dir="/tmp/sandman-queueing"
	mkdir -p "$state_dir"
	events="$state_dir/events"
	leader_dir="$state_dir/leader"

	if mkdir "$leader_dir" 2>/dev/null; then
	  role=leader
	  printf '%s\n' "$issue" > "$leader_dir/issue"
	  printf 'started-%s\n' "$issue" >> "$events"
	  sleep 1
	  printf 'finished-%s\n' "$issue" >> "$events"
	  touch "$state_dir/finished-$issue"
	else
	  role=follower
	  while [ ! -f "$leader_dir/issue" ]; do
	    sleep 0.05
	  done
	  read -r leader_issue < "$leader_dir/issue"
	  printf 'started-%s\n' "$issue" >> "$events"
	  while [ ! -f "$state_dir/finished-$leader_issue" ]; do
	    sleep 0.05
	  done
	  printf 'finished-%s\n' "$issue" >> "$events"
	  touch "$state_dir/finished-$issue"
	fi

	printf 'queueing-role=%s\n' "$role"
	while IFS= read -r line; do
	  printf 'queueing-event=%s\n' "$line"
	done < "$events"
	`)}, "", gh)

	out, err := executeRunCommand(t, deps,
		"--sandbox", "podman",
		"--parallel", "2",
		"--container-capacity", "1",
		"--max-containers", "1",
		"42", "100",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 2 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	log42, err := os.ReadFile(filepath.Join(dir, ".sandman", "logs", "42.log"))
	if err != nil {
		t.Fatalf("read log for issue 42: %v", err)
	}
	log100, err := os.ReadFile(filepath.Join(dir, ".sandman", "logs", "100.log"))
	if err != nil {
		t.Fatalf("read log for issue 100: %v", err)
	}
	extract := func(logData []byte, prefix string) (string, bool) {
		for _, line := range strings.Split(strings.TrimSpace(string(logData)), "\n") {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
			}
		}
		return "", false
	}
	identity42, ok := extract(log42, "container-identity=")
	if !ok || identity42 == "" {
		t.Fatal("missing container identity for issue 42")
	}
	identity100, ok := extract(log100, "container-identity=")
	if !ok || identity100 == "" {
		t.Fatal("missing container identity for issue 100")
	}
	if got, want := identity42, identity100; got != want {
		t.Fatalf("expected the same container hostname, got %q and %q", got, want)
	}

	workdir42, ok := extract(log42, "container-workdir=")
	if !ok || workdir42 == "" {
		t.Fatal("missing container workdir for issue 42")
	}
	if got, want := workdir42, "/workspace/.sandman/worktrees/sandman/42-fix-bug"; got != want {
		t.Fatalf("expected issue 42 worktree %q, got %q", want, got)
	}

	workdir100, ok := extract(log100, "container-workdir=")
	if !ok || workdir100 == "" {
		t.Fatal("missing container workdir for issue 100")
	}
	if got, want := workdir100, "/workspace/.sandman/worktrees/sandman/100-add-feature"; got != want {
		t.Fatalf("expected issue 100 worktree %q, got %q", want, got)
	}

	role42, ok := extract(log42, "queueing-role=")
	if !ok || role42 == "" {
		t.Fatal("missing queueing role for issue 42")
	}
	role100, ok := extract(log100, "queueing-role=")
	if !ok || role100 == "" {
		t.Fatal("missing queueing role for issue 100")
	}

	var followerIssue, leaderIssue string
	var followerLog []byte
	switch {
	case role42 == "follower" && role100 == "leader":
		followerIssue = "42"
		leaderIssue = "100"
		followerLog = log42
	case role100 == "follower" && role42 == "leader":
		followerIssue = "100"
		leaderIssue = "42"
		followerLog = log100
	default:
		t.Fatalf("expected one leader and one follower, got issue 42=%q issue 100=%q", role42, role100)
	}

	var events []string
	for _, line := range strings.Split(strings.TrimSpace(string(followerLog)), "\n") {
		if strings.HasPrefix(line, "queueing-event=") {
			events = append(events, strings.TrimPrefix(line, "queueing-event="))
		}
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 queueing markers, got %d:\n%s", len(events), followerLog)
	}
	if got, want := events[0], "started-"+leaderIssue; got != want {
		t.Fatalf("expected leader start first, got %q\n%s", got, followerLog)
	}
	if got, want := events[1], "finished-"+leaderIssue; got != want {
		t.Fatalf("expected leader finish second, got %q\n%s", got, followerLog)
	}
	if got, want := events[2], "started-"+followerIssue; got != want {
		t.Fatalf("expected follower start third, got %q\n%s", got, followerLog)
	}
	if got, want := events[3], "finished-"+followerIssue; got != want {
		t.Fatalf("expected follower finish fourth, got %q\n%s", got, followerLog)
	}
}

func TestRun_DefaultSandboxFourIssuesAutoModeStartsMinimumContainersAndKeepsWorktreesSeparate(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}

	dir := t.TempDir()
	t.Chdir(dir)
	remoteDir := initRunIntegrationRepoWithRemote(t, dir)
	runGit(t, dir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	writeSandmanDockerfile(t, dir)

	homeDir, err := os.MkdirTemp("", "sandman-podman-home-")
	if err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0755); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	gitConfig := fmt.Sprintf("[user]\n\tname = Test\n[url %q]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n", "file://"+remoteDir)
	if err := os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitConfig), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	t.Setenv("HOME", homeDir)
	// Re-warm podman with the isolated HOME so the image cache lives outside the repo tree.
	if out, err := exec.Command("podman", "run", "--rm", "alpine", "echo", "ok").CombinedOutput(); err != nil {
		t.Fatalf("warm podman image for test home: %v: %s", err, out)
	}

	gh := &fakeGitHubClient{issues: map[int]*github.Issue{
		11: {Number: 11, Title: "Alpha Task", Body: "First concurrent run."},
		12: {Number: 12, Title: "Beta Task", Body: "Second concurrent run."},
		13: {Number: 13, Title: "Gamma Task", Body: "Third concurrent run."},
		14: {Number: 14, Title: "Delta Task", Body: "Fourth concurrent run."},
	}}
	deps := newRunIntegrationDepsWithSandbox(config.Agent{Name: "test-agent", Command: `
set -eu
printf 'container-identity=%s\n' "$(hostname)"
printf 'container-workdir=%s\n' "$PWD"
sleep 1
`}, "podman", gh)

	out, err := executeRunCommand(t, deps,
		"--sandbox", "podman",
		"--parallel", "4",
		"--container-capacity", "2",
		"--max-containers", "0",
		"11", "12", "13", "14",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 4 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	extract := func(logData []byte, prefix string) (string, bool) {
		for _, line := range strings.Split(strings.TrimSpace(string(logData)), "\n") {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
			}
		}
		return "", false
	}

	expectedSlugs := map[int]string{
		11: "alpha-task",
		12: "beta-task",
		13: "gamma-task",
		14: "delta-task",
	}

	hostnames := map[string]struct{}{}
	for _, issue := range []int{11, 12, 13, 14} {
		logPath := filepath.Join(dir, ".sandman", "logs", fmt.Sprintf("%d.log", issue))
		logData, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read log for issue %d: %v", issue, err)
		}

		hostname, ok := extract(logData, "container-identity=")
		if !ok || hostname == "" {
			t.Fatalf("missing container identity for issue %d", issue)
		}
		hostnames[hostname] = struct{}{}

		workdir, ok := extract(logData, "container-workdir=")
		if !ok || workdir == "" {
			t.Fatalf("missing container workdir for issue %d", issue)
		}
		wantWorkdir := fmt.Sprintf("/workspace/.sandman/worktrees/sandman/%d-%s", issue, expectedSlugs[issue])
		if workdir != wantWorkdir {
			t.Fatalf("expected issue %d worktree %q, got %q", issue, wantWorkdir, workdir)
		}
	}

	if got := len(hostnames); got != 2 {
		t.Fatalf("expected exactly 2 container hostnames, got %d: %v", got, hostnames)
	}
}

func TestRun_WorktreeSandboxSingleIssuePropagatesAgentEnvToLog(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	deps := newRunIntegrationDepsWithAgent(config.Agent{
		Name:    "test-agent",
		Command: "printenv AGENT_TOKEN",
		Env: map[string]string{
			"AGENT_TOKEN": "token with spaces",
		},
	}, &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
	}})

	out, err := executeRunCommand(t, deps, "--sandbox", "worktree", "42")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "token with spaces") {
		t.Fatalf("expected env-derived value in log, got:\n%s", logData)
	}
}

func TestRun_WorktreeSandboxSingleIssuePreservesWorktreeOnFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	deps := newRunIntegrationDeps(`exit 1`, &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
	}})

	out, err := executeRunCommand(t, deps, "--sandbox", "worktree", "42")
	if err == nil {
		t.Fatal("expected worktree run to fail")
	}
	if !strings.Contains(err.Error(), "1 of 1 runs failed") {
		t.Fatalf("expected batch failure, got: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected failure output with at least two lines, got:\n%s", out)
	}
	if got, want := strings.Join(lines[:2], "\n"), "Summary: 0 succeeded, 1 failed\n  #42  failure  sandman/42-fix-bug"; got != want {
		t.Fatalf("expected failure output %q, got %q", want, got)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected failed worktree to remain, got: %v", err)
	}
}

func TestRun_WorktreeSandboxSingleIssuePreservesRenderedCliPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	deps := newRunIntegrationDeps(`
set -e
prompt_path=".sandman/rendered-prompt.md"
test -f "$prompt_path"
grep -Fq "Issue #42: Fix bug" "$prompt_path"
grep -Fq "Users cannot log in." "$prompt_path"
grep -Fq "Priority: urgent" "$prompt_path"
touch agent-ran.txt
`, &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
	}})

	promptTemplate := `# Task

Issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

{{ISSUE_BODY}}

Priority: {{PRIORITY}}
`

	out, err := executeRunCommand(t, deps,
		"--sandbox", "worktree",
		"--preserve",
		"--prompt", promptTemplate,
		"--prompt-arg", "PRIORITY=urgent",
		"42",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#42  success  sandman/42-fix-bug") {
		t.Fatalf("expected branch string in summary, got:\n%s", out)
	}

	promptPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug", ".sandman", "rendered-prompt.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}

	wantPrompt := "# Task\n\nIssue #42: Fix bug\n\nUsers cannot log in.\n\nPriority: urgent\n"
	if got := string(data); got != wantPrompt {
		t.Fatalf("unexpected prompt content:\nwant:\n%s\ngot:\n%s", wantPrompt, got)
	}

	markerPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug", "agent-ran.txt")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected agent marker file, got: %v", err)
	}
}

func TestRun_DependencyAwareBatch_TwoLevelDAGPreservesParallelismWithinLevels(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	// ADR-0003 only requires each AgentRun to wait for its own BlockedBy set.
	// This test still proves both blockers start before dependents and that
	// same-level AgentRuns preserve concurrency in both phases.
	deps := newRunIntegrationDeps(issueAwareAgentCommand(`
state_dir="$repo_root/.sandman/dag"
mkdir -p "$state_dir"

	case "$issue" in
	  42|43)
	    touch "$state_dir/blocker-start-$issue"

    attempts=0
    count=0
    while [ "$attempts" -lt 500 ]; do
      count=0
      for path in "$state_dir"/blocker-start-*; do
        if [ -e "$path" ]; then
          count=$((count + 1))
        fi
      done
      if [ "$count" -ge 2 ]; then
        break
      fi
      attempts=$((attempts + 1))
      sleep 0.02
    done

    if [ "$count" -lt 2 ]; then
      exit 1
    fi

	    touch "$state_dir/blocker-finish-$issue"
	    ;;

	  100|200)
	    if [ ! -f "$state_dir/blocker-start-42" ] || [ ! -f "$state_dir/blocker-start-43" ]; then
	      exit 1
	    fi

	    if [ "$issue" = "100" ] && [ ! -f "$state_dir/blocker-finish-42" ]; then
	      exit 1
	    fi

	    if [ "$issue" = "200" ] && [ ! -f "$state_dir/blocker-finish-43" ]; then
	      exit 1
	    fi

    touch "$state_dir/dependent-start-$issue"

    attempts=0
    count=0
    while [ "$attempts" -lt 500 ]; do
      count=0
      for path in "$state_dir"/dependent-start-*; do
        if [ -e "$path" ]; then
          count=$((count + 1))
        fi
      done
      if [ "$count" -ge 2 ]; then
        break
      fi
      attempts=$((attempts + 1))
      sleep 0.02
    done

    if [ "$count" -lt 2 ]; then
      exit 1
    fi

    touch "$state_dir/dependent-finish-$issue"
    ;;
esac
`), &fakeGitHubClient{issues: map[int]*github.Issue{
		42:  {Number: 42, Title: "Blocker A"},
		43:  {Number: 43, Title: "Blocker B"},
		100: {Number: 100, Title: "Dependent A", BlockedBy: []int{42}},
		200: {Number: 200, Title: "Dependent B", BlockedBy: []int{43}},
	}})

	out, err := executeRunCommand(t, deps, "--parallel", "2", "42", "43", "100", "200")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 4 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	for _, marker := range []string{
		"blocker-finish-42",
		"blocker-finish-43",
		"dependent-finish-100",
		"dependent-finish-200",
	} {
		if _, statErr := os.Stat(filepath.Join(dir, ".sandman", "dag", marker)); statErr != nil {
			t.Fatalf("expected marker %s, got %v", marker, statErr)
		}
	}
}
