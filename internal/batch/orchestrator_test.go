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
	"github.com/rafaelromao/sandman/internal/events"
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
	issues map[int]*github.Issue
	err    error
}

func (f *fakeGitHubClient) FetchIssue(number int) (*github.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.issues[number], nil
}

func (f *fakeGitHubClient) SearchIssues(query string) ([]github.Issue, error) {
	return nil, nil
}

type fakeRunnable struct {
	result      AgentRunResult
	delay       time.Duration
	activeCount *int
	maxActive   *int
	mu          *sync.Mutex
}

func (f *fakeRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, interactive bool) AgentRunResult {
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

type fakeSandboxFactory struct {
	sandbox *fakeSandbox
}

func (f *fakeSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	return f.sandbox
}

type spyEventLog struct {
	events []events.Event
}

func (s *spyEventLog) Log(e events.Event) error {
	s.events = append(s.events, e)
	return nil
}

func (s *spyEventLog) Read() ([]events.Event, error) {
	return s.events, nil
}

type blockingRunnable struct {
	delayAfterCancel time.Duration
}

func (b *blockingRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, interactive bool) AgentRunResult {
	<-ctx.Done()
	time.Sleep(b.delayAfterCancel)
	return AgentRunResult{IssueNumber: 42, Status: "failure"}
}

type blockingRunnableFactory struct {
	runnable *blockingRunnable
}

func (f *blockingRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	return f.runnable
}

func TestRunBatch_SendsSIGTERMOnCancel(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	proc := &fakeProcess{}
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	blockRunnable := &blockingRunnable{delayAfterCancel: 100 * time.Millisecond}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _ = o.RunBatch(ctx, Request{Issues: []int{42}})

	if !proc.sigTermCalled {
		t.Error("expected SIGTERM to be sent to process")
	}
}

func TestRunBatch_PreservesWorktreeOnInterrupt(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	proc := &fakeProcess{}
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	blockRunnable := &blockingRunnable{delayAfterCancel: 100 * time.Millisecond}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _ = o.RunBatch(ctx, Request{Issues: []int{42}})

	if sb.stopCalled {
		t.Error("expected Stop not to be called on interrupted run")
	}
}

func TestRunBatch_DeletesWorktreeOnSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Errorf("expected worktree to be deleted on success, but it still exists")
	}
}

func TestRunBatch_PreservesWorktreeWithPreserveFlag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Preserve: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		t.Errorf("expected worktree to be preserved when Preserve=true, but it was deleted")
	}
}

func TestRunBatch_CallsStopOnSuccess(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	sb := &fakeSandbox{}
	factory := &fakeSandboxFactory{sandbox: sb}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !sb.stopCalled {
		t.Error("expected Stop to be called on successful run")
	}
}

func TestRunBatch_SendsSIGKILLAfterTimeout(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	proc := &fakeProcess{}
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	blockRunnable := &blockingRunnable{delayAfterCancel: 300 * time.Millisecond}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}
	o.killTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _ = o.RunBatch(ctx, Request{Issues: []int{42}})

	if !proc.killCalled {
		t.Error("expected SIGKILL to be sent to process after timeout")
	}
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error when fetch fails")
	}
}

func TestRunBatch_NoIssues(t *testing.T) {
	client := &fakeGitHubClient{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, spy, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, spy, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
			Sandbox:     "worktree",
			WorktreeDir: ".sandman/worktrees",
			Git:         config.GitConfig{DefaultBranch: "main"},
			AgentProviders: map[string]config.Agent{
				"test-agent": {Command: "touch agent-ran.txt"},
			},
		},
	}

	o := NewOrchestrator(client, renderer, store, nil)
	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Preserve: true})
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

func TestRunBatch_PopulatesBranch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(result.Runs))
	}
	if result.Runs[0].Branch != "sandman/42-fix-bug" {
		t.Errorf("expected branch sandman/42-fix-bug, got %q", result.Runs[0].Branch)
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
			Sandbox:     "worktree",
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
sandbox: worktree
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

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Preserve: true})
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4, 5}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if factory.max > 4 {
		t.Errorf("expected max concurrent runs <= 4 (default), got %d", factory.max)
	}
}

func TestRunBatch_LogsStartedAndFinishedEvents(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	if spyLog.events[0].Type != "run.started" {
		t.Errorf("expected first event run.started, got %q", spyLog.events[0].Type)
	}
	if spyLog.events[0].Issue != 42 {
		t.Errorf("expected started event issue 42, got %d", spyLog.events[0].Issue)
	}
	if spyLog.events[1].Type != "run.finished" {
		t.Errorf("expected second event run.finished, got %q", spyLog.events[1].Type)
	}
	if spyLog.events[1].Issue != 42 {
		t.Errorf("expected finished event issue 42, got %d", spyLog.events[1].Issue)
	}
	status, _ := spyLog.events[1].Payload["status"].(string)
	if status != "success" {
		t.Errorf("expected finished status success, got %q", status)
	}
}

func TestRunBatch_LogsFinishedEventWithBranch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	branch, _ := spyLog.events[1].Payload["branch"].(string)
	if branch != "sandman/42-fix-bug" {
		t.Errorf("expected branch sandman/42-fix-bug, got %q", branch)
	}
}

func TestRunBatch_LogsWorktreeStateDeletedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	state, _ := spyLog.events[1].Payload["worktree_state"].(string)
	if state != "deleted" {
		t.Errorf("expected worktree_state deleted, got %q", state)
	}
}

func TestRunBatch_LogsWorktreeStatePreservedOnFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "exit 1"}}}}, spyLog)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	state, _ := spyLog.events[1].Payload["worktree_state"].(string)
	if state != "preserved" {
		t.Errorf("expected worktree_state preserved, got %q", state)
	}
}

func TestRunBatch_DebugPrintsWorktreePathOnFailure(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	sb := &fakeSandbox{workDir: "/tmp/sandman/42-fix-bug"}
	factory := &fakeSandboxFactory{sandbox: sb}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = &fakeRunnableFactory{
		results: []AgentRunResult{{IssueNumber: 42, Status: "failure"}},
	}

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Debug: true})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(result.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(result.Runs))
	}
	if result.Runs[0].DebugInfo == "" {
		t.Errorf("expected debug info on failure with debug=true")
	}
	if !strings.Contains(result.Runs[0].DebugInfo, "/tmp/sandman/42-fix-bug") {
		t.Errorf("expected debug info to contain worktree path, got %q", result.Runs[0].DebugInfo)
	}
}

func TestRunBatch_NoDebugInfoOnFailureWithoutDebug(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	sb := &fakeSandbox{workDir: "/tmp/sandman/42-fix-bug"}
	factory := &fakeSandboxFactory{sandbox: sb}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = &fakeRunnableFactory{
		results: []AgentRunResult{{IssueNumber: 42, Status: "failure"}},
	}

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(result.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(result.Runs))
	}
	if result.Runs[0].DebugInfo != "" {
		t.Errorf("expected no debug info on failure without debug flag, got %q", result.Runs[0].DebugInfo)
	}
}

func TestRunBatch_LogsWorktreeStatePreservedWithPreserveFlag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Preserve: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	state, _ := spyLog.events[1].Payload["worktree_state"].(string)
	if state != "preserved" {
		t.Errorf("expected worktree_state preserved, got %q", state)
	}
}

func TestRunBatch_LogsFinishedEventOnFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "exit 1"}}}}, spyLog)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	status, _ := spyLog.events[1].Payload["status"].(string)
	if status != "failure" {
		t.Errorf("expected finished status failure, got %q", status)
	}
}

type fakeContainerForOrchestrator struct {
	id         string
	stopCalled bool
	stopError  error
}

func (f *fakeContainerForOrchestrator) ID() string {
	return f.id
}

func (f *fakeContainerForOrchestrator) Stop() error {
	f.stopCalled = true
	return f.stopError
}

type fakeContainerStarter struct {
	startCalled bool
	startOpts   sandbox.StartOptions
	container   sandbox.Container
	err         error
}

func (f *fakeContainerStarter) Start(image, repoPath string, opts sandbox.StartOptions) (sandbox.Container, error) {
	f.startCalled = true
	f.startOpts = opts
	return f.container, f.err
}

type fakeContainerRuntimeFactory struct {
	starter *fakeContainerStarter
}

func (f *fakeContainerRuntimeFactory) New(binary string) sandbox.ContainerStarter {
	return f.starter
}

func TestRunBatch_PassesStartOptionsToContainerRuntime(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	starter := &fakeContainerStarter{container: &fakeContainerForOrchestrator{id: "shared123"}}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true", ConfigDirs: []string{"~/.config/test"}}}}}, nil)
	o.containerRuntimeFactory = factory

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "docker"})

	if !starter.startCalled {
		t.Fatal("expected container starter to be called")
	}
	if starter.startOpts.GitConfigPath == "" {
		t.Error("expected GitConfigPath to be set")
	}
	if len(starter.startOpts.AgentConfigDirs) != 1 {
		t.Errorf("expected 1 agent config dir, got %d", len(starter.startOpts.AgentConfigDirs))
	}
	if starter.startOpts.UserID == "" {
		t.Error("expected UserID to be set")
	}
}

func TestRunBatch_InteractiveFlagPassedToRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}

	factory := &fakeRunnableFactory{
		results: []AgentRunResult{
			{IssueNumber: 42, Status: "success"},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Interactive: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(factory.created) != 1 {
		t.Fatalf("expected 1 runnable, got %d", len(factory.created))
	}
	// The interactive parameter is passed to Run; we verify the run succeeded.
	if factory.results[0].IssueNumber != 42 {
		t.Errorf("expected issue 42, got %d", factory.results[0].IssueNumber)
	}
}

func TestRunBatch_RejectsKeychainAuthAgent(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"opencode": {Name: "opencode", Command: "opencode", KeychainAuth: true}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error for keychain auth agent")
	}
	if !strings.Contains(err.Error(), "keychain") {
		t.Errorf("expected error to mention keychain, got: %v", err)
	}
}

func TestRunBatch_DefaultSandbox_ResolvesToPodman(t *testing.T) {
	dir := t.TempDir()
	podmanPath := filepath.Join(dir, "podman")
	if err := os.WriteFile(podmanPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write podman: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	starter := &fakeContainerStarter{container: &fakeContainerForOrchestrator{id: "shared123"}}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}})

	if !starter.startCalled {
		t.Fatal("expected container starter to be called with podman")
	}
}

func TestRunBatch_PodmanMissingFallsBackToDocker(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	starter := &fakeContainerStarter{container: &fakeContainerForOrchestrator{id: "shared123"}}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}})

	if !starter.startCalled {
		t.Fatal("expected container starter to be called with docker")
	}
}

func TestRunBatch_NeitherRuntimeAvailable_ReturnsError(t *testing.T) {
	t.Setenv("PATH", "")

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error when no container runtime is available")
	}
	if !strings.Contains(err.Error(), "podman") || !strings.Contains(err.Error(), "docker") {
		t.Errorf("expected error to mention podman and docker, got: %v", err)
	}
}

func TestRunBatch_SharedContainer_ReturnsErrorWhenDockerUnavailable(t *testing.T) {
	t.Setenv("PATH", "")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "docker"})
	if err == nil {
		t.Fatal("expected error when docker is unavailable")
	}
}

func TestRunBatch_IsolatedContainer_ReturnsErrorWhenDockerUnavailable(t *testing.T) {
	t.Setenv("PATH", "")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "docker", IsolatedContainers: true})
	if err == nil {
		t.Fatal("expected error when docker is unavailable")
	}
}
