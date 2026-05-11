package batch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
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
	issues               map[int]*github.Issue
	err                  error
	createPRCalled       bool
	createPRBranch       string
	createPRTargetBranch string
	createPRTitle        string
	createPRBody         string
	createPRResult       string
	createPRError        error
}

func (f *fakeGitHubClient) FetchIssue(number int) (*github.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.issues[number], nil
}

func (f *fakeGitHubClient) CreatePR(branch, targetBranch, title, body string) (string, error) {
	f.createPRCalled = true
	f.createPRBranch = branch
	f.createPRTargetBranch = targetBranch
	f.createPRTitle = title
	f.createPRBody = body
	return f.createPRResult, f.createPRError
}

type fakeRunnable struct {
	result      AgentRunResult
	delay       time.Duration
	activeCount *int
	maxActive   *int
	mu          *sync.Mutex
}

func (f *fakeRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, client github.Client, defaultBranch string) AgentRunResult {
	if f.mu != nil {
		f.mu.Lock()
		*f.activeCount++
		if *f.activeCount > *f.maxActive {
			*f.maxActive = *f.activeCount
		}
		f.mu.Unlock()
	}

	if f.delay > 0 {
		time.Sleep(f.delay)
	}

	if f.mu != nil {
		f.mu.Lock()
		*f.activeCount--
		f.mu.Unlock()
	}
	return f.result
}

type fakeRunnableFactory struct {
	created []Runnable
	results []AgentRunResult
	delays  []time.Duration
	active  int
	max     int
	mu      sync.Mutex
}

func (f *fakeRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.mu.Lock()
	idx := len(f.created)
	res := f.results[idx]
	delay := time.Duration(0)
	if idx < len(f.delays) {
		delay = f.delays[idx]
	}
	r := &fakeRunnable{result: res, delay: delay, activeCount: &f.active, maxActive: &f.max, mu: &f.mu}
	f.created = append(f.created, r)
	f.mu.Unlock()
	return r
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error when fetch fails")
	}
}

func TestRunBatch_NoIssues(t *testing.T) {
	client := &fakeGitHubClient{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, spy, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, spy, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
			Git:         config.GitConfig{DefaultBranch: "main"},
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

func TestRunBatch_PRCreationFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		createPRError: errors.New("gh pr create failed"),
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error when PR creation fails")
	}
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "failure" {
		t.Errorf("expected failure status in result, got %+v", result)
	}
	if result.Runs[0].PRURL != "" {
		t.Errorf("expected empty PRURL, got %q", result.Runs[0].PRURL)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		t.Errorf("expected worktree to be preserved")
	}
}

func TestRunBatch_PopulatesPRURL(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		createPRResult: "https://github.com/owner/repo/pull/99",
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(result.Runs))
	}
	if result.Runs[0].PRURL != "https://github.com/owner/repo/pull/99" {
		t.Errorf("expected PRURL https://github.com/owner/repo/pull/99, got %q", result.Runs[0].PRURL)
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
			Git:         config.GitConfig{DefaultBranch: "main"},
			AgentProviders: map[string]config.Agent{
				"test-agent": {Command: "exit 1"},
			},
		},
	}

	o := NewOrchestrator(client, renderer, store, nil)
	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error when agent fails")
	}
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "failure" {
		t.Errorf("expected failure status in result, got %+v", result)
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

func TestRunBatch_UsesRunResultJSONForPR(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: `mkdir -p .sandman && echo '{"title":"Custom Title","body":"Custom Body"}' > .sandman/run-result.json`}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !client.createPRCalled {
		t.Fatal("expected CreatePR to be called")
	}
	if client.createPRTitle != "Custom Title" {
		t.Errorf("expected title 'Custom Title', got %q", client.createPRTitle)
	}
	if client.createPRBody != "Custom Body\n\nFixes #42" {
		t.Errorf("expected body 'Custom Body\n\nFixes #42', got %q", client.createPRBody)
	}
}

func TestRunBatch_PRBodyReferencesIssue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(client.createPRBody, "Fixes #42") {
		t.Errorf("expected PR body to reference issue, got %q", client.createPRBody)
	}
}

func TestRunBatch_CreatesPRAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !client.createPRCalled {
		t.Fatal("expected CreatePR to be called")
	}
	if client.createPRBranch != "sandman/42-fix-bug" {
		t.Errorf("expected branch sandman/42-fix-bug, got %s", client.createPRBranch)
	}
	if client.createPRTargetBranch != "main" {
		t.Errorf("expected target branch main, got %s", client.createPRTargetBranch)
	}
	if client.createPRTitle != "Fix bug" {
		t.Errorf("expected title 'Fix bug', got %q", client.createPRTitle)
	}
	if client.createPRBody != "Users cannot log in.\n\nFixes #42" {
		t.Errorf("expected body 'Users cannot log in.\n\nFixes #42', got %q", client.createPRBody)
	}
}

func TestRunBatch_RespectsParallelLimit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "A"},
			2: {Number: 2, Title: "B"},
			3: {Number: 3, Title: "C"},
			4: {Number: 4, Title: "D"},
		},
	}

	factory := &fakeRunnableFactory{
		results: []AgentRunResult{
			{IssueNumber: 1, Status: "success"},
			{IssueNumber: 2, Status: "success"},
			{IssueNumber: 3, Status: "success"},
			{IssueNumber: 4, Status: "success"},
		},
		delays: []time.Duration{50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4}, Parallel: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if factory.max > 2 {
		t.Errorf("expected max concurrent runs <= 2, got %d", factory.max)
	}
}

func TestRunBatch_OneFailureDoesNotAbortOthers(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "A"},
			2: {Number: 2, Title: "B"},
			3: {Number: 3, Title: "C"},
		},
	}

	factory := &fakeRunnableFactory{
		results: []AgentRunResult{
			{IssueNumber: 1, Status: "success"},
			{IssueNumber: 2, Status: "failure"},
			{IssueNumber: 3, Status: "success"},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3}, Parallel: 3})
	if err == nil {
		t.Fatal("expected error when some runs fail")
	}

	if len(result.Runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(result.Runs))
	}

	statuses := make(map[int]string)
	for _, r := range result.Runs {
		statuses[r.IssueNumber] = r.Status
	}
	if statuses[1] != "success" {
		t.Errorf("expected issue 1 to succeed, got %s", statuses[1])
	}
	if statuses[2] != "failure" {
		t.Errorf("expected issue 2 to fail, got %s", statuses[2])
	}
	if statuses[3] != "success" {
		t.Errorf("expected issue 3 to succeed, got %s", statuses[3])
	}
}

func TestRunBatch_DefaultParallelIsFour(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "A"},
			2: {Number: 2, Title: "B"},
			3: {Number: 3, Title: "C"},
			4: {Number: 4, Title: "D"},
			5: {Number: 5, Title: "E"},
		},
	}

	factory := &fakeRunnableFactory{
		results: []AgentRunResult{
			{IssueNumber: 1, Status: "success"},
			{IssueNumber: 2, Status: "success"},
			{IssueNumber: 3, Status: "success"},
			{IssueNumber: 4, Status: "success"},
			{IssueNumber: 5, Status: "success"},
		},
		delays: []time.Duration{50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4, 5}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if factory.max > 4 {
		t.Errorf("expected max concurrent runs <= 4 (default), got %d", factory.max)
	}
}
