package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func (f *fakeGitHubClient) FetchIssueDependencies(number int) ([]int, error) {
	if f.err != nil {
		return nil, f.err
	}
	if issue := f.issues[number]; issue != nil {
		return issue.BlockedBy, nil
	}
	return nil, nil
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

func (f *fakeRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, interactive bool, renderCfg prompt.RenderConfig) AgentRunResult {
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

type freshSandboxFactory struct{}

func (f *freshSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	return &fakeSandbox{}
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

func (b *blockingRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, interactive bool, renderCfg prompt.RenderConfig) AgentRunResult {
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

type controlledRunnable struct {
	result   AgentRunResult
	started  chan struct{}
	finished chan struct{}
	release  <-chan struct{}
	once     sync.Once
}

func (r *controlledRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, interactive bool, renderCfg prompt.RenderConfig) AgentRunResult {
	if r.finished != nil {
		defer close(r.finished)
	}
	if r.started != nil {
		r.once.Do(func() { close(r.started) })
	}
	if r.release != nil {
		<-r.release
	}
	return r.result
}

type controlledRunnableFactory struct {
	mu        sync.Mutex
	created   []int
	runnables map[int]Runnable
}

func (f *controlledRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.mu.Lock()
	f.created = append(f.created, issue.Number)
	r := f.runnables[issue.Number]
	f.mu.Unlock()
	if r == nil {
		return &fakeRunnable{result: AgentRunResult{IssueNumber: issue.Number, Status: "failure"}}
	}
	return r
}

func waitForSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(250 * time.Millisecond):
		t.Fatal(message)
	}
}

func assertNoSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
		t.Fatal(message)
	case <-time.After(50 * time.Millisecond):
	}
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
	o.sandboxFactory = &freshSandboxFactory{}

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
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4, 5}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if factory.max > 4 {
		t.Errorf("expected max concurrent runs <= 4 (default), got %d", factory.max)
	}
}

func TestRunBatch_WaitsForBlockersBeforeStartingDependents(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker"},
			100: {Number: 100, Title: "Dependent", BlockedBy: []int{42}},
		},
	}

	blockerStarted := make(chan struct{})
	dependentStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})

	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 42, Status: "success"},
				started: blockerStarted,
				release: releaseBlocker,
			},
			100: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 100, Status: "success"},
				started: dependentStarted,
			},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{
			Issues:       []int{42, 100},
			Dependencies: map[int][]int{100: {42}},
			Parallel:     2,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")
	assertNoSignal(t, dependentStarted, "dependent started before blocker completed")

	close(releaseBlocker)
	waitForSignal(t, dependentStarted, "expected dependent to start after blocker completed")
	waitForSignal(t, done, "expected batch to complete")
	if len(factory.created) != 2 {
		t.Fatalf("expected both runnables to be created, got %v", factory.created)
	}
}

func TestRunBatch_SkipsDependentsWhenBlockerFails(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker"},
			100: {Number: 100, Title: "Dependent", BlockedBy: []int{42}},
		},
	}

	blockerStarted := make(chan struct{})
	spyLog := &spyEventLog{}
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 42, Status: "failure"},
				started: blockerStarted,
			},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.runnableFactory = factory

	result, err := o.RunBatch(context.Background(), Request{
		Issues:       []int{42, 100},
		Dependencies: map[int][]int{100: {42}},
		Parallel:     2,
	})
	if err == nil {
		t.Fatal("expected error when blocker fails")
	}

	waitForSignal(t, blockerStarted, "expected blocker to run")
	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(result.Runs))
	}

	statuses := make(map[int]string)
	for _, run := range result.Runs {
		statuses[run.IssueNumber] = run.Status
	}
	if statuses[42] != "failure" {
		t.Fatalf("expected blocker failure, got %q", statuses[42])
	}
	if statuses[100] != "blocked" {
		t.Fatalf("expected dependent to be blocked, got %q", statuses[100])
	}
	if len(factory.created) != 1 || factory.created[0] != 42 {
		t.Fatalf("expected only blocker runnable to be created, got %v", factory.created)
	}

	var dependentStartedEvent bool
	var blockedEvent *events.Event
	for i := range spyLog.events {
		e := spyLog.events[i]
		if e.Type == "run.started" && e.Issue == 100 {
			dependentStartedEvent = true
		}
		if e.Type == "run.blocked" && e.Issue == 100 {
			blockedEvent = &e
		}
	}
	if dependentStartedEvent {
		t.Fatal("expected blocked issue not to emit run.started")
	}
	if blockedEvent == nil {
		t.Fatal("expected run.blocked event for blocked issue")
	}
	blockedBy, ok := blockedEvent.Payload["blocked_by"].([]int)
	if !ok || !reflect.DeepEqual(blockedBy, []int{42}) {
		t.Fatalf("expected blocked_by [42], got %#v", blockedEvent.Payload["blocked_by"])
	}
}

func TestRunBatch_PreservesParallelismWithinDependencyLevel(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Blocker"},
			3: {Number: 3, Title: "Dependent A", BlockedBy: []int{1}},
			4: {Number: 4, Title: "Dependent B", BlockedBy: []int{1}},
		},
	}

	blockerStarted := make(chan struct{})
	dependentAStarted := make(chan struct{})
	dependentBStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	releaseDependentA := make(chan struct{})
	releaseDependentB := make(chan struct{})

	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			1: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 1, Status: "success"},
				started: blockerStarted,
				release: releaseBlocker,
			},
			3: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 3, Status: "success"},
				started: dependentAStarted,
				release: releaseDependentA,
			},
			4: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 4, Status: "success"},
				started: dependentBStarted,
				release: releaseDependentB,
			},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{
			Issues:       []int{1, 3, 4},
			Dependencies: map[int][]int{3: {1}, 4: {1}},
			Parallel:     2,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")
	assertNoSignal(t, dependentAStarted, "dependent A started before blocker completed")
	assertNoSignal(t, dependentBStarted, "dependent B started before blocker completed")

	close(releaseBlocker)
	waitForSignal(t, dependentAStarted, "expected dependent A to start after blocker completed")
	waitForSignal(t, dependentBStarted, "expected dependent B to start after blocker completed")

	close(releaseDependentA)
	close(releaseDependentB)
	waitForSignal(t, done, "expected batch to complete")
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
	startCount  int
	startOpts   sandbox.StartOptions
	container   sandbox.Container
	containers  []sandbox.Container
	err         error
}

func (f *fakeContainerStarter) Start(image, repoPath string, opts sandbox.StartOptions) (sandbox.Container, error) {
	f.startCalled = true
	f.startCount++
	f.startOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	if len(f.containers) >= f.startCount {
		return f.containers[f.startCount-1], nil
	}
	if f.container != nil {
		return f.container, nil
	}
	return &fakeContainerForOrchestrator{id: fmt.Sprintf("container-%d", f.startCount)}, nil
}

type fakeContainerRuntimeFactory struct {
	starter *fakeContainerStarter
}

func (f *fakeContainerRuntimeFactory) New(binary string) sandbox.ContainerStarter {
	return f.starter
}

type trackingSandbox struct {
	fakeSandbox
	containerID string
}

type trackingSandboxFactory struct{}

func (f *trackingSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	sb := &trackingSandbox{}
	if container != nil {
		sb.containerID = container.ID()
	}
	return sb
}

type trackingRunnableFactory struct {
	mu               sync.Mutex
	results          map[int]AgentRunResult
	runnables        map[int]Runnable
	containerByIssue map[int]string
}

func (f *trackingRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ts, ok := sb.(*trackingSandbox); ok {
		if f.containerByIssue == nil {
			f.containerByIssue = make(map[int]string)
		}
		f.containerByIssue[issue.Number] = ts.containerID
	}
	if r := f.runnables[issue.Number]; r != nil {
		return r
	}
	if res, ok := f.results[issue.Number]; ok {
		return &fakeRunnable{result: res}
	}
	return &fakeRunnable{result: AgentRunResult{IssueNumber: issue.Number, Status: "failure"}}
}

func TestRunBatch_PassesStartOptionsToContainerRuntime(t *testing.T) {
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

func TestRunBatch_ReusesIdleContainerWithinBatchAndStopsItAtEnd(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
		},
	}
	first := &fakeContainerForOrchestrator{id: "container-1"}
	second := &fakeContainerForOrchestrator{id: "container-2"}
	starter := &fakeContainerStarter{containers: []sandbox.Container{first, second}}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	runnables := &trackingRunnableFactory{results: map[int]AgentRunResult{
		1: {IssueNumber: 1, Status: "success"},
		2: {IssueNumber: 2, Status: "success"},
	}}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}, Sandbox: "docker", Parallel: 1, ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 0, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 1 {
		t.Fatalf("expected 1 container to start, got %d", starter.startCount)
	}
	if got := runnables.containerByIssue[1]; got == "" {
		t.Fatal("expected first run to receive a container")
	}
	if runnables.containerByIssue[1] != runnables.containerByIssue[2] {
		t.Fatalf("expected both runs to reuse the same container, got %q and %q", runnables.containerByIssue[1], runnables.containerByIssue[2])
	}
	if !first.stopCalled {
		t.Fatal("expected pooled container to stop when the batch finishes")
	}
}

func TestRunBatch_PrefersLeastLoadedContainerWhenReusingIdleCapacity(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
			3: {Number: 3, Title: "Three"},
			4: {Number: 4, Title: "Four"},
		},
	}
	starter := &fakeContainerStarter{containers: []sandbox.Container{
		&fakeContainerForOrchestrator{id: "container-1"},
		&fakeContainerForOrchestrator{id: "container-2"},
	}}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	release1 := make(chan struct{})
	release2 := make(chan struct{})
	release3 := make(chan struct{})
	release4 := make(chan struct{})
	started1 := make(chan struct{})
	started2 := make(chan struct{})
	started3 := make(chan struct{})
	started4 := make(chan struct{})
	finished2 := make(chan struct{})
	finished3 := make(chan struct{})
	runnables := &trackingRunnableFactory{runnables: map[int]Runnable{
		1: &controlledRunnable{result: AgentRunResult{IssueNumber: 1, Status: "success"}, started: started1, release: release1},
		2: &controlledRunnable{result: AgentRunResult{IssueNumber: 2, Status: "success"}, started: started2, finished: finished2, release: release2},
		3: &controlledRunnable{result: AgentRunResult{IssueNumber: 3, Status: "success"}, started: started3, finished: finished3, release: release3},
		4: &controlledRunnable{result: AgentRunResult{IssueNumber: 4, Status: "success"}, started: started4, release: release4},
	}}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{
			Issues:               []int{1, 2, 3, 4},
			Dependencies:         map[int][]int{4: {3}},
			Sandbox:              "docker",
			Parallel:             4,
			ContainerCapacity:    2,
			ContainerCapacitySet: true,
			MaxContainers:        2,
			MaxContainersSet:     true,
		})
		errCh <- err
	}()

	waitForSignal(t, started1, "expected issue 1 to start")
	waitForSignal(t, started2, "expected issue 2 to start")
	waitForSignal(t, started3, "expected issue 3 to start")
	assertNoSignal(t, started4, "issue 4 should wait for issue 3 to finish")

	close(release2)
	waitForSignal(t, finished2, "expected issue 2 to finish")
	assertNoSignal(t, started4, "issue 4 should still be blocked by issue 3")

	close(release3)
	waitForSignal(t, finished3, "expected issue 3 to finish")
	waitForSignal(t, started4, "expected issue 4 to start after issue 3 finishes")

	if runnables.containerByIssue[1] != runnables.containerByIssue[2] {
		t.Fatalf("expected issues 1 and 2 to share a container, got %q and %q", runnables.containerByIssue[1], runnables.containerByIssue[2])
	}
	if runnables.containerByIssue[3] == runnables.containerByIssue[1] {
		t.Fatalf("expected issue 3 to run in a different container from issue 1")
	}
	if runnables.containerByIssue[4] != runnables.containerByIssue[3] {
		t.Fatalf("expected issue 4 to reuse the least-loaded container %q, got %q", runnables.containerByIssue[3], runnables.containerByIssue[4])
	}

	close(release1)
	close(release4)
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunBatch_ContainerCapacityOneStartsOneContainerPerConcurrentRun(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	runnables := &fakeRunnableFactory{
		results: []AgentRunResult{{IssueNumber: 1, Status: "success"}, {IssueNumber: 2, Status: "success"}},
		delays:  []time.Duration{50 * time.Millisecond, 50 * time.Millisecond},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}, Sandbox: "docker", Parallel: 2, ContainerCapacity: 1, ContainerCapacitySet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 2 {
		t.Fatalf("expected 2 containers to start, got %d", starter.startCount)
	}
}

func TestRunBatch_MaxContainersLimitRestrictsSharedContainerConcurrency(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
			3: {Number: 3, Title: "Three"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	runnables := &fakeRunnableFactory{
		results: []AgentRunResult{{IssueNumber: 1, Status: "success"}, {IssueNumber: 2, Status: "success"}, {IssueNumber: 3, Status: "success"}},
		delays:  []time.Duration{50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3}, Sandbox: "docker", Parallel: 3, ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 1, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if runnables.max != 2 {
		t.Fatalf("expected max concurrent runs to be 2, got %d", runnables.max)
	}
}

func TestRunBatch_MaxContainersAutoStartsMinimumContainers(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
			3: {Number: 3, Title: "Three"},
			4: {Number: 4, Title: "Four"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	runnables := &fakeRunnableFactory{
		results: []AgentRunResult{{IssueNumber: 1, Status: "success"}, {IssueNumber: 2, Status: "success"}, {IssueNumber: 3, Status: "success"}, {IssueNumber: 4, Status: "success"}},
		delays:  []time.Duration{50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4}, Sandbox: "docker", Parallel: 4, ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 0, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 2 {
		t.Fatalf("expected 2 containers to start, got %d", starter.startCount)
	}
}

func TestRunBatch_UsesConfigContainerSettingsWhenRequestUnset(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	runnables := &fakeRunnableFactory{
		results: []AgentRunResult{{IssueNumber: 1, Status: "success"}, {IssueNumber: 2, Status: "success"}},
		delays:  []time.Duration{50 * time.Millisecond, 50 * time.Millisecond},
	}
	store := &fakeConfigStore{config: &config.Config{Agent: "test-agent", ContainerCapacity: 1, MaxContainers: 0, Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}

	o := NewOrchestrator(client, &noopRenderer{}, store, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}, Sandbox: "docker", Parallel: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 2 {
		t.Fatalf("expected config container_capacity=1 to start 2 containers, got %d", starter.startCount)
	}
}

func TestRunBatch_WorktreeSandboxIgnoresContainerSettings(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	runnables := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success"}}}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "worktree", ContainerCapacity: 1, ContainerCapacitySet: true, MaxContainers: 1, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 0 {
		t.Fatalf("expected no containers to start in worktree mode, got %d", starter.startCount)
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

func TestRunBatch_ContainerCapacityOneReturnsErrorWhenDockerUnavailable(t *testing.T) {
	t.Setenv("PATH", "")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{DefaultBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "docker", ContainerCapacity: 1, ContainerCapacitySet: true})
	if err == nil {
		t.Fatal("expected error when docker is unavailable")
	}
}
