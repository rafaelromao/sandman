package batch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func initGitRepo(t *testing.T, dir string) {
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

type fakeConfigStore struct {
	config *config.Config
	err    error
}

func (f *fakeConfigStore) Load() (*config.Config, error) {
	return f.config, f.err
}

func (f *fakeConfigStore) Save(cfg *config.Config) error {
	return nil
}

type noopRenderer struct{}

func (n *noopRenderer) Render(cfg prompt.RenderConfig, data prompt.IssueData) (string, error) {
	return "", nil
}

type spyPromptRenderer struct {
	called bool
	cfg    prompt.RenderConfig
	data   prompt.IssueData
	result string
	err    error
}

func (s *spyPromptRenderer) Render(cfg prompt.RenderConfig, data prompt.IssueData) (string, error) {
	s.called = true
	s.cfg = cfg
	s.data = data
	return s.result, s.err
}

type fakeGitHubClient struct {
	issues map[int]*github.Issue
	err    error
}

func (f *fakeGitHubClient) FetchIssue(number int) (*github.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.issues[number], nil
}

func (f *fakeGitHubClient) CreatePR(branch, title, body string) (string, error) {
	return "", nil
}

func TestRunBatch_FetchesSingleIssue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(result.Runs))
	}
	if result.Runs[0].IssueNumber != 42 {
		t.Errorf("expected issue 42, got %d", result.Runs[0].IssueNumber)
	}
}

func TestRunBatch_FetchesMultipleIssues(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "A"},
			2: {Number: 2, Title: "B"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(result.Runs))
	}
}

func TestRunBatch_FetchError(t *testing.T) {
	client := &fakeGitHubClient{err: errors.New("github api error")}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error when fetch fails")
	}
}

func TestRunBatch_NoIssues(t *testing.T) {
	client := &fakeGitHubClient{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 0 {
		t.Errorf("expected 0 runs, got %d", len(result.Runs))
	}
}

func TestRunBatch_RendersPromptForIssue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	spy := &spyPromptRenderer{result: "rendered prompt"}
	o := NewOrchestrator(client, spy, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !spy.called {
		t.Fatal("expected renderer to be called")
	}
	if spy.data.Number != 42 {
		t.Errorf("expected issue number 42, got %d", spy.data.Number)
	}
	if spy.data.Title != "Fix bug" {
		t.Errorf("expected title 'Fix bug', got %q", spy.data.Title)
	}
	if spy.data.Body != "Users cannot log in." {
		t.Errorf("expected body 'Users cannot log in.', got %q", spy.data.Body)
	}
}

func TestRunBatch_RenderError(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spy := &spyPromptRenderer{err: errors.New("render failed")}
	o := NewOrchestrator(client, spy, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error when render fails")
	}
}

func TestRunBatch_WritesPromptAndExecutesAgent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	renderer := &spyPromptRenderer{result: "rendered prompt"}
	store := &fakeConfigStore{
		config: &config.Config{
			Agent:       "test-agent",
			WorktreeDir: ".sandman/worktrees",
			AgentProviders: map[string]config.Agent{
				"test-agent": {Command: "touch agent-ran.txt"},
			},
		},
	}

	o := NewOrchestrator(client, renderer, store, nil)
	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	promptPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug", ".sandman", "prompt.md")
	if _, err := os.Stat(promptPath); err != nil {
		t.Errorf("prompt file not found: %v", err)
	}

	markerPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug", "agent-ran.txt")
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("agent marker not found: %v", err)
	}

	if len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		t.Errorf("expected success status, got %+v", result.Runs)
	}
}

func TestRunBatch_AgentFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	renderer := &spyPromptRenderer{result: "rendered prompt"}
	store := &fakeConfigStore{
		config: &config.Config{
			Agent:       "test-agent",
			WorktreeDir: ".sandman/worktrees",
			AgentProviders: map[string]config.Agent{
				"test-agent": {Command: "exit 1"},
			},
		},
	}

	o := NewOrchestrator(client, renderer, store, nil)
	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error when agent fails")
	}
}

func TestRunBatch_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	configData := `agent: test-agent
worktree_dir: .sandman/worktrees
agents:
  test-agent:
    name: test-agent
    command: touch agent-ran.txt
`
	if err := os.WriteFile(configPath, []byte(configData), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix login bug", Body: "Users cannot log in with OAuth."},
		},
	}

	store := &config.FileStore{Path: configPath}
	engine := &prompt.Engine{}
	o := NewOrchestrator(client, engine, store, nil)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(result.Runs))
	}
	if result.Runs[0].IssueNumber != 42 {
		t.Errorf("expected issue 42, got %d", result.Runs[0].IssueNumber)
	}
	if result.Runs[0].Status != "success" {
		t.Errorf("expected success, got %s", result.Runs[0].Status)
	}
	if result.Runs[0].Branch != "sandman/42-fix-login-bug" {
		t.Errorf("expected branch sandman/42-fix-login-bug, got %s", result.Runs[0].Branch)
	}

	promptPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-login-bug", ".sandman", "prompt.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if !strings.Contains(string(data), "Issue #42: Fix login bug") {
		t.Errorf("prompt missing expected content, got:\n%s", string(data))
	}

	markerPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-login-bug", "agent-ran.txt")
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("agent marker not found: %v", err)
	}
}
