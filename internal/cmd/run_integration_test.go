package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

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

func waitForLineCount(t *testing.T, path string, want int) {
	t.Helper()
	for i := 0; i < 300; i++ {
		data, err := os.ReadFile(path)
		if err == nil && len(strings.Split(strings.TrimSpace(string(data)), "\n")) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d lines in %s", want, path)
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

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

func (l *recordingEventLog) RemoveEventsByIssue(issueNumber int) error {
	var kept []events.Event
	for _, e := range l.events {
		if e.Issue == issueNumber {
			continue
		}
		if e.IssueRef != nil && *e.IssueRef == issueNumber {
			continue
		}
		kept = append(kept, e)
	}
	l.events = kept
	return nil
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
	return newRunIntegrationDepsWithSandboxAndGit(agent, sandboxMode, config.GitConfig{BaseBranch: "main"}, gh)
}

func newRunIntegrationDepsWithSandboxAndGit(agent config.Agent, sandboxMode string, gitCfg config.GitConfig, gh *fakeGitHubClient) Dependencies {
	if agent.Command == "" {
		agent.Command = "true"
	}
	binDir, err := os.MkdirTemp("", "sandman-agent-bin-")
	if err != nil {
		panic(err)
	}
	script := "#!/bin/sh\nset -e\n" + renderShellExports(agent.Env) + strings.TrimSpace(agent.Command) + "\n"
	for _, name := range []string{"opencode"} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(script), 0755); err != nil {
			panic(err)
		}
	}
	_ = os.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	if gitCfg.BaseBranch == "" {
		gitCfg.BaseBranch = "main"
	}

	store := &fakeStore{config: &config.Config{
		DefaultAgent:  "opencode",
		Agent:         "opencode",
		ReviewCommand: "/oc review",
		WorktreeDir:   ".sandman/worktrees",
		Sandbox:       sandboxMode,
		Git:           gitCfg,
		AgentProviders: map[string]config.Agent{
			"opencode": {Command: agent.Command, Env: agent.Env},
		},
	}}

	runner := batch.NewOrchestrator(gh, &prompt.Engine{}, store, nil)
	return Dependencies{
		BatchRunner:  runner,
		ConfigStore:  store,
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IsTTY:        func() bool { return false },
	}
}

func TestRun_ExplicitZeroParallelRunsThroughOrchestratorEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	sharedDir := t.TempDir()
	startFile := filepath.Join(sharedDir, "starts.log")
	releaseFile := filepath.Join(sharedDir, "release")
	agentCommand := fmt.Sprintf(`echo start >> %q; while [ ! -f %q ]; do sleep 0.05; done`, startFile, releaseFile)

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "A"},
			2: {Number: 2, Title: "B"},
			3: {Number: 3, Title: "C"},
			4: {Number: 4, Title: "D"},
			5: {Number: 5, Title: "E"},
		},
		prs: map[string]*github.PR{
			"sandman/1-a": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-a", HeadRefOid: ""},
			"sandman/2-b": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-b", HeadRefOid: ""},
			"sandman/3-c": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-c", HeadRefOid: ""},
			"sandman/4-d": {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-d", HeadRefOid: ""},
			"sandman/5-e": {Number: 5, State: "closed", Merged: true, HeadRefName: "sandman/5-e", HeadRefOid: ""},
		},
	}

	deps := newRunIntegrationDeps(agentCommand, gh)
	store := &fakeStore{config: &config.Config{
		DefaultAgent:    "opencode",
		Agent:           "opencode",
		DefaultParallel: 8,
		ReviewCommand:   "/oc review",
		WorktreeDir:     ".sandman/worktrees",
		Sandbox:         "worktree",
		Git:             config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"opencode": {Command: agentCommand},
		},
	}}
	deps.ConfigStore = store
	deps.BatchRunner = batch.NewOrchestrator(gh, &prompt.Engine{}, store, nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := executeRunCommand(t, deps, "--parallel", "0", "1", "2", "3", "4", "5")
		errCh <- err
	}()

	waitForLineCount(t, startFile, 5)
	if err := os.WriteFile(releaseFile, []byte("go\n"), 0644); err != nil {
		t.Fatalf("release runs: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func renderShellExports(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		out.WriteString("export ")
		out.WriteString(key)
		out.WriteString("=")
		out.WriteString(shellQuote(env[key]))
		out.WriteString("\n")
	}
	return out.String()
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
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

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{7}, State: "open"},
			7:   {Number: 7, Title: "Groundwork", State: "open"},
		},
		prs: map[string]*github.PR{
			"sandman/100-feature":  {Number: 100, State: "closed", Merged: true, HeadRefName: "sandman/100-feature", HeadRefOid: ""},
			"sandman/42-refactor":  {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-refactor", HeadRefOid: ""},
			"sandman/7-groundwork": {Number: 7, State: "closed", Merged: true, HeadRefName: "sandman/7-groundwork", HeadRefOid: ""},
		},
	}
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
`), client)

	resultCh := make(chan struct {
		out string
		err error
	}, 1)
	go func() {
		out, err := executeRunCommand(t, deps, "--include-dependencies", "100")
		resultCh <- struct {
			out string
			err error
		}{out: out, err: err}
	}()

	waitForPath(t, filepath.Join(dir, ".sandman", "chain", "7.done"))
	client.setIssueState(7, "closed")
	waitForPath(t, filepath.Join(dir, ".sandman", "chain", "42.done"))
	client.setIssueState(42, "closed")

	result := <-resultCh
	out, err := result.out, result.err
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 3 succeeded") {
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
		wantOut []string
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
			name: "external open blocker without include-dependencies",
			issues: map[int]*github.Issue{
				100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
				42:  {Number: 42, Title: "Blocker"},
			},
			args:    []string{"100"},
			wantOut: []string{"Summary:", "blocked"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			initRunIntegrationRepoWithRemote(t, dir)

			deps := newRunIntegrationDeps(issueAwareAgentCommand(`
state_dir="$repo_root/.sandman/executed"
mkdir -p "$state_dir"
touch "$state_dir/$issue"
`), &fakeGitHubClient{issues: tc.issues})

			out, err := executeRunCommand(t, deps, tc.args...)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("expected dependency resolution error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				if strings.Contains(out, "Summary:") {
					t.Fatalf("expected dependency resolution to fail before execution, got:\n%s", out)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				for _, want := range tc.wantOut {
					if !strings.Contains(out, want) {
						t.Fatalf("expected output to contain %q, got:\n%s", want, out)
					}
				}
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
	if !strings.Contains(out, "Summary: 1 failed, 1 blocked") {
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
while [ "$attempts" -lt 100 ]; do
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
`), &fakeGitHubClient{
		issues: map[int]*github.Issue{
			10: {Number: 10, Title: "One"},
			11: {Number: 11, Title: "Two"},
			12: {Number: 12, Title: "Three"},
		},
		prs: map[string]*github.PR{
			"sandman/10-one":   {Number: 10, State: "closed", Merged: true, HeadRefName: "sandman/10-one", HeadRefOid: ""},
			"sandman/11-two":   {Number: 11, State: "closed", Merged: true, HeadRefName: "sandman/11-two", HeadRefOid: ""},
			"sandman/12-three": {Number: 12, State: "closed", Merged: true, HeadRefName: "sandman/12-three", HeadRefOid: ""},
		},
	})

	out, err := executeRunCommand(t, deps, "--parallel", "3", "10", "11", "12")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 3 succeeded") {
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

	deps := newRunIntegrationDeps(`printf '%s\n' "agent stdout"`, &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{
			"sandman/42-fix-bug": {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug", HeadRefOid: ""},
		},
	})

	out, err := executeRunCommand(t, deps, "--sandbox", "worktree", "42")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded") {
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
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected worktree to remain, got: %v", err)
	}
}

func TestRun_WorktreeSandboxOverrideFlagClearsArtifacts(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug"}},
	}
	deps := newRunIntegrationDeps(`printf '%s\n' "agent stdout"`, gh)

	// First run creates artifacts
	out, err := executeRunCommand(t, deps, "--sandbox", "worktree", "42")
	if err != nil {
		t.Fatalf("first run unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("first run expected success, got:\n%s", out)
	}

	verifyPath := func(path string, shouldExist bool) {
		t.Helper()
		_, statErr := os.Stat(path)
		if shouldExist && statErr != nil {
			t.Fatalf("expected %s to exist, got: %v", path, statErr)
		}
		if !shouldExist && !os.IsNotExist(statErr) {
			t.Fatalf("expected %s to not exist, got stat err: %v", path, statErr)
		}
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	verifyPath(worktreePath, true)
	verifyPath(logPath, true)
	staleMarkerPath := filepath.Join(worktreePath, "stale-marker.txt")
	if err := os.WriteFile(staleMarkerPath, []byte("stale\n"), 0644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	// Second run with --override clears old artifacts and creates new ones
	out, err = executeRunCommand(t, deps, "--sandbox", "worktree", "--override", "42")
	if err != nil {
		t.Fatalf("override run unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "#42  success  sandman/42-fix-bug") {
		t.Fatalf("override run expected success, got:\n%s", out)
	}
	verifyPath(worktreePath, true)
	verifyPath(logPath, true)
	if _, err := os.Stat(staleMarkerPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale marker to be removed by --override, got: %v", err)
	}
	if status := runGit(t, worktreePath, "status", "--short"); strings.Contains(status, "stale-marker.txt") {
		t.Fatalf("expected stale marker to be absent from worktree status after --override, got:\n%s", status)
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

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{
			"sandman/42-fix-bug": {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug", HeadRefOid: ""},
		},
	}
	deps := newRunIntegrationDepsWithSandbox(config.Agent{Name: "test-agent", Command: "pwd"}, "", gh)

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")

	out, err := executeRunCommand(t, deps, "42")
	if err != nil {
		if logData, logErr := os.ReadFile(logPath); logErr == nil {
			t.Fatalf("unexpected error: %v\noutput:\n%s\nlog:\n%s", err, out, logData)
		}
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded") {
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
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected worktree to remain, got: %v", err)
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

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
			100: {Number: 100, Title: "Add feature", Body: "Two runs should share one container."},
		},
		prs: map[string]*github.PR{
			"sandman/42-fix-bug":      {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug", HeadRefOid: ""},
			"sandman/100-add-feature": {Number: 100, State: "closed", Merged: true, HeadRefName: "sandman/100-add-feature", HeadRefOid: ""},
		},
	}
	deps := newRunIntegrationDepsWithSandbox(config.Agent{Name: "test-agent", Command: `
set -eu
printf 'container-identity=%s\n' "$(hostname)"
printf 'container-workdir=%s\n' "$PWD"
`}, "", gh)

	out, err := executeRunCommand(t, deps, "--parallel", "2", "42", "100")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 2 succeeded") {
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
			if idx := strings.Index(line, prefix); idx >= 0 {
				return strings.TrimSpace(line[idx+len(prefix):]), true
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

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
			100: {Number: 100, Title: "Add feature", Body: "The second run should wait."},
		},
		prs: map[string]*github.PR{
			"sandman/42-fix-bug":      {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug", HeadRefOid: ""},
			"sandman/100-add-feature": {Number: 100, State: "closed", Merged: true, HeadRefName: "sandman/100-add-feature", HeadRefOid: ""},
		},
	}
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
	if !strings.Contains(out, "Summary: 2 succeeded") {
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
			if idx := strings.Index(line, prefix); idx >= 0 {
				return strings.TrimSpace(line[idx+len(prefix):]), true
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
		if idx := strings.Index(line, "queueing-event="); idx >= 0 {
			events = append(events, strings.TrimSpace(line[idx+len("queueing-event="):]))
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

func TestRun_DefaultSandboxFourIssuesAutoModeSpawnsContainersForCapacityAndKeepsWorktreesSeparate(t *testing.T) {
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

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			11: {Number: 11, Title: "Alpha Task", Body: "First concurrent run."},
			12: {Number: 12, Title: "Beta Task", Body: "Second concurrent run."},
			13: {Number: 13, Title: "Gamma Task", Body: "Third concurrent run."},
			14: {Number: 14, Title: "Delta Task", Body: "Fourth concurrent run."},
		},
		prs: map[string]*github.PR{
			"sandman/11-alpha-task": {Number: 11, State: "closed", Merged: true, HeadRefName: "sandman/11-alpha-task", HeadRefOid: ""},
			"sandman/12-beta-task":  {Number: 12, State: "closed", Merged: true, HeadRefName: "sandman/12-beta-task", HeadRefOid: ""},
			"sandman/13-gamma-task": {Number: 13, State: "closed", Merged: true, HeadRefName: "sandman/13-gamma-task", HeadRefOid: ""},
			"sandman/14-delta-task": {Number: 14, State: "closed", Merged: true, HeadRefName: "sandman/14-delta-task", HeadRefOid: ""},
		},
	}
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

	if !strings.Contains(out, "Summary: 4 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	extract := func(logData []byte, prefix string) (string, bool) {
		for _, line := range strings.Split(strings.TrimSpace(string(logData)), "\n") {
			if idx := strings.Index(line, prefix); idx >= 0 {
				return strings.TrimSpace(line[idx+len(prefix):]), true
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
		t.Fatalf("expected exactly 2 container hostnames (auto mode spawns one container per 2 issues with capacity=2, effectiveParallel=4), got %d: %v", got, hostnames)
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
	}, &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{
			"sandman/42-fix-bug": {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug", HeadRefOid: ""},
		},
	})

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

	if !strings.Contains(out, "Summary: 1 failed") {
		t.Fatalf("expected failure summary, got:\n%s", out)
	}
	if !strings.Contains(out, "worktree: .sandman/worktrees/sandman/42-fix-bug") {
		t.Fatalf("expected worktree hint, got:\n%s", out)
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
prompt_path=".sandman/task.md"
test -f "$prompt_path"
grep -Fq "Issue #42: Fix bug" "$prompt_path"
grep -Fq "Users cannot log in." "$prompt_path"
grep -Fq "Priority: urgent" "$prompt_path"
touch agent-ran.txt
`, &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{
			"sandman/42-fix-bug": {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug", HeadRefOid: ""},
		},
	})

	promptTemplate := `# Task

Issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

{{ISSUE_BODY}}

Priority: {{PRIORITY}}
`

	out, err := executeRunCommand(t, deps,
		"--sandbox", "worktree",
		"--prompt", promptTemplate,
		"--prompt-arg", "PRIORITY=urgent",
		"42",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#42  success  sandman/42-fix-bug") {
		t.Fatalf("expected branch string in summary, got:\n%s", out)
	}
	if !strings.Contains(out, "worktree: .sandman/worktrees/sandman/42-fix-bug") {
		t.Fatalf("expected worktree hint, got:\n%s", out)
	}

	promptPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug", ".sandman", "task.md")
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
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker A", State: "open"},
			43:  {Number: 43, Title: "Blocker B", State: "open"},
			100: {Number: 100, Title: "Dependent A", BlockedBy: []int{42}},
			200: {Number: 200, Title: "Dependent B", BlockedBy: []int{43}},
		},
		prs: map[string]*github.PR{
			"sandman/42-blocker-a":    {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-blocker-a"},
			"sandman/43-blocker-b":    {Number: 43, State: "closed", Merged: true, HeadRefName: "sandman/43-blocker-b"},
			"sandman/100-dependent-a": {Number: 100, State: "closed", Merged: true, HeadRefName: "sandman/100-dependent-a"},
			"sandman/200-dependent-b": {Number: 200, State: "closed", Merged: true, HeadRefName: "sandman/200-dependent-b"},
		},
	}
	deps := newRunIntegrationDeps(issueAwareAgentCommand(`
state_dir="$repo_root/.sandman/dag"
mkdir -p "$state_dir"

	case "$issue" in
	  42|43)
	    touch "$state_dir/blocker-start-$issue"

    attempts=0
    count=0
    while [ "$attempts" -lt 100 ]; do
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
    while [ "$attempts" -lt 100 ]; do
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
`), client)

	go func() {
		waitForPath(t, filepath.Join(dir, ".sandman", "dag", "blocker-finish-42"))
		client.setIssueState(42, "closed")
	}()
	go func() {
		waitForPath(t, filepath.Join(dir, ".sandman", "dag", "blocker-finish-43"))
		client.setIssueState(43, "closed")
	}()

	resultCh := make(chan struct {
		out string
		err error
	}, 1)
	go func() {
		out, err := executeRunCommand(t, deps, "--parallel", "2", "42", "43", "100", "200")
		resultCh <- struct {
			out string
			err error
		}{out: out, err: err}
	}()

	result := <-resultCh
	out, err := result.out, result.err
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 4 succeeded") {
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

func podmanGitIdentityDeps(t *testing.T, dir, remoteDir, dotGitConfig, xdgGitConfig, agentCmd string) Dependencies {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatalf("create .sandman dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte("FROM alpine\nRUN apk add --no-cache git\n"), 0644); err != nil {
		t.Fatalf("write .sandman/Dockerfile: %v", err)
	}

	homeDir, err := os.MkdirTemp("", "sandman-podman-home-")
	if err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0755); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	gitCfg := fmt.Sprintf(dotGitConfig, "file://"+remoteDir)
	if err := os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitCfg), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	if strings.TrimSpace(xdgGitConfig) != "" {
		gitConfigDir := filepath.Join(homeDir, ".config", "git")
		if err := os.MkdirAll(gitConfigDir, 0755); err != nil {
			t.Fatalf("create xdg git dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(gitConfigDir, "config"), []byte(xdgGitConfig), 0644); err != nil {
			t.Fatalf("write xdg git config: %v", err)
		}
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
	if out, err := exec.Command("podman", "run", "--rm", "alpine", "echo", "ok").CombinedOutput(); err != nil {
		t.Fatalf("warm podman image for test home: %v: %s", err, out)
	}

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug"}},
	}

	return newRunIntegrationDepsWithSandboxAndGit(config.Agent{Name: "test-agent", Command: strings.TrimSpace(agentCmd)}, "podman", config.GitConfig{BaseBranch: "main"}, gh)
}

func TestRun_PodmanSandboxUsesDotGitconfigIdentityWithoutMutatingWorktreeConfig(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}

	dir := t.TempDir()
	t.Chdir(dir)
	remoteDir := initRunIntegrationRepoWithRemote(t, dir)
	runGit(t, dir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	agentCmd := `
	git config user.name
	git config user.email
	touch test-file.txt
	git add test-file.txt
	git commit -m "test commit by sandman"
	git log --format="%an <%ae>" -1
	`
	dotGitConfig := `[url %q]
	insteadOf = git@github.com:rafaelromao/sandman.git
[user]
	name = Sandman
	email = sandman@test.com
`
	deps := podmanGitIdentityDeps(t, dir, remoteDir, dotGitConfig, "", agentCmd)

	out, err := executeRunCommand(t, deps, "42")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logContent := string(logData)

	if !strings.Contains(logContent, "Sandman <sandman@test.com>") {
		t.Fatalf("expected commit author 'Sandman <sandman@test.com>' in log, got:\n%s", logContent)
	}
}

func TestRun_PodmanSandboxUsesXDGGitIdentityWithoutMutatingWorktreeConfig(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}

	dir := t.TempDir()
	t.Chdir(dir)
	remoteDir := initRunIntegrationRepoWithRemote(t, dir)
	runGit(t, dir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	agentCmd := `
	git config user.name
	git config user.email
	touch test-file.txt
	git add test-file.txt
	git commit -m "test commit by xdg identity"
	git log --format="%an <%ae>" -1
	`
	dotGitConfig := `[url %q]
	insteadOf = git@github.com:rafaelromao/sandman.git
`
	xdgGitConfig := `[user]
	name = XDG User
	email = xdg@example.com
`
	deps := podmanGitIdentityDeps(t, dir, remoteDir, dotGitConfig, xdgGitConfig, agentCmd)

	out, err := executeRunCommand(t, deps, "42")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "worktree: .sandman/worktrees/sandman/42-fix-bug") {
		t.Fatalf("expected worktree hint, got:\n%s", out)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logContent := string(logData)
	if !strings.Contains(logContent, "XDG User") || !strings.Contains(logContent, "xdg@example.com") {
		t.Fatalf("expected xdg git identity in log, got:\n%s", logContent)
	}
	if !strings.Contains(logContent, "XDG User <xdg@example.com>") {
		t.Fatalf("expected commit author 'XDG User <xdg@example.com>' in log, got:\n%s", logContent)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	cmd := exec.Command("git", "config", "--local", "user.name")
	cmd.Dir = worktreePath
	localName, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config --local user.name: %v", err)
	}
	if got := strings.TrimSpace(string(localName)); got != "Test" {
		t.Fatalf("expected worktree local user.name to stay repo default, got %q", got)
	}

	cmd = exec.Command("git", "config", "--local", "user.email")
	cmd.Dir = worktreePath
	localEmail, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config --local user.email: %v", err)
	}
	if got := strings.TrimSpace(string(localEmail)); got != "test@test.com" {
		t.Fatalf("expected worktree local user.email to stay repo default, got %q", got)
	}
}

func TestRun_WorktreeSandboxUsesHostGitIdentityWithoutMutatingWorktreeConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
	gitConfig := `[user]
	name = Host User
	email = host@example.com
`
	if err := os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte(gitConfig), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}

	agentCmd := `
	git config user.name
	git config user.email
	touch test-file.txt
	git add test-file.txt
	git commit -m "test commit by host identity"
	git log --format="%an <%ae>" -1
	`
	deps := newRunIntegrationDepsWithSandboxAndGit(config.Agent{Name: "test-agent", Command: strings.TrimSpace(agentCmd)}, "worktree", config.GitConfig{BaseBranch: "main"}, &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug"}},
	})

	out, err := executeRunCommand(t, deps, "--sandbox", "worktree", "42")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "worktree: .sandman/worktrees/sandman/42-fix-bug") {
		t.Fatalf("expected worktree hint, got:\n%s", out)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logContent := string(logData)
	if !strings.Contains(logContent, "Host User") || !strings.Contains(logContent, "host@example.com") {
		t.Fatalf("expected host git identity in log, got:\n%s", logContent)
	}
	if !strings.Contains(logContent, "Host User <host@example.com>") {
		t.Fatalf("expected commit author 'Host User <host@example.com>' in log, got:\n%s", logContent)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	cmd := exec.Command("git", "config", "--local", "user.name")
	cmd.Dir = worktreePath
	localName, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config --local user.name: %v", err)
	}
	if got := strings.TrimSpace(string(localName)); got != "Test" {
		t.Fatalf("expected worktree local user.name to stay repo default, got %q", got)
	}

	cmd = exec.Command("git", "config", "--local", "user.email")
	cmd.Dir = worktreePath
	localEmail, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config --local user.email: %v", err)
	}
	if got := strings.TrimSpace(string(localEmail)); got != "test@test.com" {
		t.Fatalf("expected worktree local user.email to stay repo default, got %q", got)
	}
}

func TestRun_PodmanSandboxUsesRepoDefaultIdentityWhenConfigEmpty(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}

	dir := t.TempDir()
	t.Chdir(dir)
	remoteDir := initRunIntegrationRepoWithRemote(t, dir)
	runGit(t, dir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	agentCmd := `
touch test-file.txt
git add test-file.txt
	git commit -m "test commit by repo default"
	git log --format="%an <%ae>" -1
	`
	dotGitConfig := `[url %q]
	insteadOf = git@github.com:rafaelromao/sandman.git
`
	deps := podmanGitIdentityDeps(t, dir, remoteDir, dotGitConfig, "", agentCmd)

	out, err := executeRunCommand(t, deps, "42")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logContent := string(logData)

	if !strings.Contains(logContent, "Test <test@test.com>") {
		t.Fatalf("expected commit author 'Test <test@test.com>' (repo default) in log, got:\n%s", logContent)
	}
}

func TestRun_DependencyAwareBatch_MixedRunnableAndBlockedIssues(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = initRunIntegrationRepoWithRemote(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Runnable"},
			100: {Number: 100, Title: "Blocked by external", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "External blocker", State: "open"},
		},
		prs: map[string]*github.PR{
			"sandman/42-runnable": {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-runnable", HeadRefOid: ""},
		},
	}
	deps := newRunIntegrationDeps(issueAwareAgentCommand(`
state_dir="$repo_root/.sandman/mixed"
mkdir -p "$state_dir"
touch "$state_dir/start-$issue"
`), client)

	resultCh := make(chan struct {
		out string
		err error
	}, 1)
	go func() {
		out, err := executeRunCommand(t, deps, "42", "100")
		resultCh <- struct {
			out string
			err error
		}{out: out, err: err}
	}()

	waitForPath(t, filepath.Join(dir, ".sandman", "mixed", "start-42"))

	result := <-resultCh
	out, err := result.out, result.err
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Summary: 1 succeeded, 1 blocked") {
		t.Fatalf("expected mixed results summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#42  success") {
		t.Fatalf("expected runnable issue success, got:\n%s", out)
	}
	if !strings.Contains(out, "#100  blocked") {
		t.Fatalf("expected blocked issue status, got:\n%s", out)
	}

	for _, marker := range []string{"start-42"} {
		if _, statErr := os.Stat(filepath.Join(dir, ".sandman", "mixed", marker)); statErr != nil {
			t.Fatalf("expected marker %s, got %v", marker, statErr)
		}
	}

	if _, statErr := os.Stat(filepath.Join(dir, ".sandman", "mixed", "start-100")); !os.IsNotExist(statErr) {
		t.Fatalf("expected blocked issue not to start, got %v", statErr)
	}
}
