package batch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	remoteDir := t.TempDir()
	cmd := exec.Command("git", "init", "--bare")
	cmd.Dir = remoteDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
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
	addRemote := exec.Command("git", "remote", "add", "origin", remoteDir)
	addRemote.Dir = dir
	if out, err := addRemote.CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v: %s", err, out)
	}
	push := exec.Command("git", "push", "-u", "origin", "main")
	push.Dir = dir
	if out, err := push.CombinedOutput(); err != nil {
		t.Fatalf("push main: %v: %s", err, out)
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

type retryRenderer struct {
	renderCalls int
	result      string
	err         error
}

func (r *retryRenderer) Render(cfg prompt.RenderConfig, data prompt.IssueData) (string, error) {
	r.renderCalls++
	return r.result, r.err
}

type fakeGitHubClient struct {
	issues       map[int]*github.Issue
	fetchRelease map[int]<-chan struct{}
	prs          map[string]*github.PR
	err          error
	findPRErr    error
}

func (f *fakeGitHubClient) FetchIssue(number int) (*github.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	if release := f.fetchRelease[number]; release != nil {
		<-release
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

func (f *fakeGitHubClient) FindPRByBranch(branch string) (*github.PR, error) {
	if f.findPRErr != nil {
		return nil, f.findPRErr
	}
	if f.prs != nil {
		if pr, ok := f.prs[branch]; ok {
			return pr, nil
		}
		return nil, nil
	}
	if f.err != nil {
		return nil, f.err
	}
	return &github.PR{Number: 1, State: "closed", Merged: true, HeadRefName: branch}, nil
}

func mergedPR(branch, sha string) *github.PR {
	return &github.PR{Number: 1, State: "closed", Merged: true, HeadRefName: branch, HeadRefOid: sha}
}

type fakeRunnable struct {
	result      AgentRunResult
	delay       time.Duration
	activeCount *int
	maxActive   *int
	mu          *sync.Mutex
}

func (f *fakeRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
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

type retrySandbox struct {
	startCalled      bool
	writePromptCount int
	execCount        int
	execCommand      string
	execErrors       []error
	workDir          string
}

func (s *retrySandbox) Start() error {
	s.startCalled = true
	return nil
}

func (s *retrySandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	s.execCount++
	s.execCommand = command
	if s.execCount <= len(s.execErrors) {
		if err := s.execErrors[s.execCount-1]; err != nil {
			return err
		}
	}
	return nil
}

func (s *retrySandbox) ExecInteractive(ctx context.Context, command string) error { return nil }
func (s *retrySandbox) Stop() error                                               { return nil }
func (s *retrySandbox) WorkDir() string                                           { return s.workDir }
func (s *retrySandbox) WritePrompt(content string) error {
	s.writePromptCount++
	return nil
}
func (s *retrySandbox) Process() sandbox.Process { return nil }

type retrySandboxFactory struct {
	sandbox *retrySandbox
}

func (f *retrySandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	return f.sandbox
}

type syncTrackingSandboxFactory struct {
	mu         sync.Mutex
	synced     bool
	beforeSync bool
}

func (f *syncTrackingSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.synced {
		f.beforeSync = true
	}
	return &fakeSandbox{}
}

type baseBranchSyncTracker struct {
	mu          sync.Mutex
	syncCalls   int
	startCalls  int
	beforeStart bool
	branches    []string
}

type syncAwareSandboxFactory struct {
	tracker *baseBranchSyncTracker
}

func (f *syncAwareSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	f.tracker.mu.Lock()
	f.tracker.branches = append(f.tracker.branches, sourceBranch)
	f.tracker.mu.Unlock()
	return &syncAwareSandbox{fakeSandbox: &fakeSandbox{}, tracker: f.tracker}
}

type syncAwareSandbox struct {
	*fakeSandbox
	tracker *baseBranchSyncTracker
}

func (s *syncAwareSandbox) Start() error {
	s.tracker.mu.Lock()
	s.tracker.startCalls++
	if s.tracker.syncCalls < s.tracker.startCalls {
		s.tracker.beforeStart = true
	}
	s.tracker.mu.Unlock()
	return s.fakeSandbox.Start()
}

type freshSandboxFactory struct{}

func (f *freshSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	return &fakeSandbox{}
}

type spyEventLog struct {
	events                   []events.Event
	removedIssueNumber       int
	removeEventsByIssueCalls int
}

func (s *spyEventLog) Log(e events.Event) error {
	s.events = append(s.events, e)
	return nil
}

func (s *spyEventLog) Read() ([]events.Event, error) {
	return s.events, nil
}

func (s *spyEventLog) RemoveEventsByIssue(issueNumber int) error {
	s.removeEventsByIssueCalls++
	s.removedIssueNumber = issueNumber
	var kept []events.Event
	for _, e := range s.events {
		if e.Issue == issueNumber {
			continue
		}
		if e.IssueRef != nil && *e.IssueRef == issueNumber {
			continue
		}
		kept = append(kept, e)
	}
	s.events = kept
	return nil
}

type threadSafeSpyEventLog struct {
	mu     sync.Mutex
	events []events.Event
}

func (s *threadSafeSpyEventLog) Log(e events.Event) error {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
	return nil
}

func (s *threadSafeSpyEventLog) Read() ([]events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]events.Event, len(s.events))
	copy(out, s.events)
	return out, nil
}

func (s *threadSafeSpyEventLog) RemoveEventsByIssue(issueNumber int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []events.Event
	for _, e := range s.events {
		if e.Issue == issueNumber {
			continue
		}
		if e.IssueRef != nil && *e.IssueRef == issueNumber {
			continue
		}
		kept = append(kept, e)
	}
	s.events = kept
	return nil
}

func (s *threadSafeSpyEventLog) Snapshot() []events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]events.Event, len(s.events))
	copy(out, s.events)
	return out
}

type blockingRunnable struct {
	delayAfterCancel time.Duration
}

func (b *blockingRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
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

func (r *controlledRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
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
	issueNumber := 0
	if issue != nil {
		issueNumber = issue.Number
	}
	f.created = append(f.created, issueNumber)
	r := f.runnables[issueNumber]
	f.mu.Unlock()
	if r == nil {
		return &fakeRunnable{result: AgentRunResult{IssueNumber: issueNumber, Status: "failure"}}
	}
	return r
}

type promptOnlyRunnableFactory struct {
	hook func(issue *github.Issue, branch string) AgentRunResult
}

func (f *promptOnlyRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	return &fakeRunnable{result: f.hook(issue, branch)}
}

type continuationFlowState struct {
	mu       sync.Mutex
	prompts  []string
	contexts []string
	step     int
}

func (s *continuationFlowState) recordPrompt(promptText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, promptText)
}

func (s *continuationFlowState) nextContext() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step >= len(s.contexts) {
		return ""
	}
	context := s.contexts[s.step]
	s.step++
	return context
}

type continuationFlowRunnable struct {
	state *continuationFlowState
	sb    sandbox.Sandbox
}

func (r *continuationFlowRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	if renderCfg.ContinuePrompt != "" {
		promptPath := filepath.Join(r.sb.WorkDir(), ".sandman", "continue-prompt.md")
		if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err == nil {
			_ = os.WriteFile(promptPath, []byte(renderCfg.ContinuePrompt), 0644)
		}
		r.state.recordPrompt(renderCfg.ContinuePrompt)
	}
	contextPath := filepath.Join(r.sb.WorkDir(), ".sandman", "continuation-context.md")
	if content := r.state.nextContext(); content != "" {
		if err := os.MkdirAll(filepath.Dir(contextPath), 0755); err == nil {
			_ = os.WriteFile(contextPath, []byte(content), 0644)
		}
	}
	return AgentRunResult{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug", WorktreePath: r.sb.WorkDir()}
}

type continuationFlowRunnableFactory struct {
	state *continuationFlowState
}

func (f *continuationFlowRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	return &continuationFlowRunnable{state: f.state, sb: sb}
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

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestResolveRetries(t *testing.T) {
	cfg := &config.Config{Retries: 3}

	if got := resolveRetries(Request{Retries: -1}, cfg); got != 3 {
		t.Fatalf("resolveRetries fallback = %d, want 3", got)
	}
	if got := resolveRetries(Request{Retries: 0}, cfg); got != 0 {
		t.Fatalf("resolveRetries override = %d, want 0", got)
	}
	if got := resolveRetries(Request{Retries: 5}, cfg); got != 5 {
		t.Fatalf("resolveRetries explicit = %d, want 5", got)
	}
}

func TestParseLogForCompletion_UsesLastTodosSection(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "42.log")
	content := "--- run 1/3 ---\n# Todos\n- [✓] done\n\n# Notes\nignored\n\n--- retry 2/3 ---\n# Todos\n- [ ] still open\n"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if parseLogForCompletion(logPath) {
		t.Fatal("expected incomplete last Todos section to return false")
	}
}

func TestParseLogForCompletion_ReturnsTrueForCheckedTodos(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "42.log")
	content := "--- run 1/3 ---\npreamble\n# Todos\n- [✓] done\n- [✓] done too\n"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if !parseLogForCompletion(logPath) {
		t.Fatal("expected checked Todos section to return true")
	}
}

func TestParseLogForCompletion_ReturnsFalseWithoutTodos(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "42.log")
	if err := os.WriteFile(logPath, []byte("no todos here\n"), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if parseLogForCompletion(logPath) {
		t.Fatal("expected missing Todos section to return false")
	}
}

func TestParseLogForCompletion_IgnoresEarlierRunSections(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "42.log")
	content := "--- run 1/3 ---\n# Todos\n- [✓] old done\n--- retry 2/3 ---\n# Todos\n- [ ] current open\n"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if parseLogForCompletion(logPath) {
		t.Fatal("expected current run section to control completion")
	}
}

func TestParseLogForCompletion_AcceptsGFMCheckboxSyntax(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "42.log")
	content := "--- run 1/3 ---\n# Todos\n- [x] done\n- [X] done too\n"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if !parseLogForCompletion(logPath) {
		t.Fatal("expected GFM checkbox syntax to return true")
	}
}

func TestCheckPRMergedAtHead(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{
		"open":     {Number: 1, State: "open", Merged: false, HeadRefName: "open", HeadRefOid: "open-sha"},
		"merged":   {Number: 2, State: "open", Merged: true, HeadRefName: "merged", HeadRefOid: "merged-sha"},
		"closed":   {Number: 3, State: "closed", Merged: false, HeadRefName: "closed", HeadRefOid: "closed-sha"},
		"explicit": {Number: 4, State: "merged", Merged: false, HeadRefName: "explicit", HeadRefOid: "explicit-sha"},
	}}

	if merged, err := checkPRMergedAtHead(client, "open", "open-sha"); err != nil || merged {
		t.Fatalf("expected open PR to be false, got merged=%v err=%v", merged, err)
	}
	if merged, err := checkPRMergedAtHead(client, "merged", "merged-sha"); err != nil || !merged {
		t.Fatalf("expected merged PR to be true, got merged=%v err=%v", merged, err)
	}
	if merged, err := checkPRMergedAtHead(client, "merged", "stale-sha"); err != nil || merged {
		t.Fatalf("expected stale merged PR to be false, got merged=%v err=%v", merged, err)
	}
	if merged, err := checkPRMergedAtHead(client, "closed", "closed-sha"); err != nil || merged {
		t.Fatalf("expected closed-unmerged PR to be false, got merged=%v err=%v", merged, err)
	}
	if merged, err := checkPRMergedAtHead(client, "explicit", "explicit-sha"); err != nil || !merged {
		t.Fatalf("expected merged state to be true, got merged=%v err=%v", merged, err)
	}
}

func TestRunSingle_RetriesResetBranchAndRerender(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	rtSandbox := &retrySandbox{
		workDir:    filepath.Join(workDir, "worktree"),
		execErrors: []error{errors.New("exit 1"), errors.New("exit 1"), nil},
	}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}, prs: map[string]*github.PR{"sandman/42-fix-bug": {Number: 17, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug", HeadRefOid: "current-sha"}}},
		renderer:     renderer,
		errorLog:     io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls []struct{ worktreePath, branch, baseBranch string }
	o.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls = append(resetCalls, struct{ worktreePath, branch, baseBranch string }{sb.WorkDir(), branch, baseBranch})
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, func() (gitIdentity, error) {
		return gitIdentity{}, nil
	}, map[int]string{42: "sandman/42-fix-bug"}, prompt.RenderConfig{}, nil, map[int]sandbox.Sandbox{}, &sync.Mutex{}, &retrySandboxFactory{sandbox: rtSandbox}, nil, "main", nil, nil, 3, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if result.RetriesTotal != 3 {
		t.Fatalf("RetriesTotal = %d, want 3", result.RetriesTotal)
	}
	if renderer.renderCalls != 3 {
		t.Fatalf("render calls = %d, want 3", renderer.renderCalls)
	}
	if rtSandbox.execCount != 3 {
		t.Fatalf("exec calls = %d, want 3", rtSandbox.execCount)
	}
	if rtSandbox.writePromptCount != 3 {
		t.Fatalf("prompt writes = %d, want 3", rtSandbox.writePromptCount)
	}
	if len(resetCalls) != 2 {
		t.Fatalf("reset calls = %d, want 2", len(resetCalls))
	}
	if resetCalls[0].branch != "sandman/42-fix-bug" || resetCalls[0].baseBranch != "main" {
		t.Fatalf("unexpected reset args: %#v", resetCalls[0])
	}
	logPath := filepath.Join(workDir, ".sandman", "logs", "42.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "--- retry 2/4 ---") {
		t.Fatalf("expected retry marker in log, got:\n%s", data)
	}
}

func TestRunSingle_RetryClosedPRResetsBranch(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	rtSandbox := &retrySandbox{workDir: filepath.Join(workDir, "worktree"), execErrors: []error{errors.New("exit 1")}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}, prs: map[string]*github.PR{"sandman/42-fix-bug": {Number: 17, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug", HeadRefOid: "current-sha"}}},
		renderer:     renderer,
		errorLog:     io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, func() (gitIdentity, error) {
		return gitIdentity{}, nil
	}, map[int]string{42: "sandman/42-fix-bug"}, prompt.RenderConfig{}, nil, map[int]sandbox.Sandbox{}, &sync.Mutex{}, &retrySandboxFactory{sandbox: rtSandbox}, nil, "main", nil, nil, 1, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if resetCalls != 1 {
		t.Fatalf("reset calls = %d, want 1", resetCalls)
	}
}

func TestRunSingle_RetryLookupErrorPreservesBranch(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	rtSandbox := &retrySandbox{workDir: filepath.Join(workDir, "worktree"), execErrors: []error{errors.New("exit 1")}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}, findPRErr: errors.New("lookup failed")},
		renderer:     renderer,
		errorLog:     io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, _ := o.runSingle(context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, func() (gitIdentity, error) {
		return gitIdentity{}, nil
	}, map[int]string{42: "sandman/42-fix-bug"}, prompt.RenderConfig{}, nil, map[int]sandbox.Sandbox{}, &sync.Mutex{}, &retrySandboxFactory{sandbox: rtSandbox}, nil, "main", nil, nil, 1, false)
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure on lookup error", result.Status)
	}
	if resetCalls != 0 {
		t.Fatalf("reset calls = %d, want 0 (branch should be preserved on lookup error)", resetCalls)
	}
}

func TestRunSingle_RetryUsesContinuationContextWithoutOpenPR(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	contextPath := filepath.Join(worktreePath, ".sandman", "continuation-context.md")
	if err := os.WriteFile(contextPath, []byte("## Completed\nKeep going.\n"), 0644); err != nil {
		t.Fatalf("write context: %v", err)
	}

	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}},
		renderer:     renderer,
		errorLog:     io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, func() (gitIdentity, error) {
		return gitIdentity{}, nil
	}, map[int]string{42: branch}, prompt.RenderConfig{}, nil, map[int]sandbox.Sandbox{}, &sync.Mutex{}, &retrySandboxFactory{sandbox: rtSandbox}, nil, "main", nil, nil, 1, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if renderer.renderCalls != 1 {
		t.Fatalf("render calls = %d, want 1", renderer.renderCalls)
	}
	if resetCalls != 0 {
		t.Fatalf("reset calls = %d, want 0", resetCalls)
	}
	if rtSandbox.execCommand != "opencode run .sandman/continue-prompt.md" {
		t.Fatalf("expected continue prompt command, got %q", rtSandbox.execCommand)
	}
	continuePromptPath := filepath.Join(worktreePath, ".sandman", "continue-prompt.md")
	data, err := os.ReadFile(continuePromptPath)
	if err != nil {
		t.Fatalf("read continue prompt: %v", err)
	}
	wantPrompt := "## Prior Context\n\n## Completed\nKeep going.\n\n## New Instruction\n\nContinue the work. Resume from the prior context and finish the remaining implementation steps.\n\n## Update Continuation Context\n\nBefore exiting, overwrite `.sandman/continuation-context.md` with an updated summary using this template:\n\n```markdown\n## Completed\n(what was implemented, committed, or merged)\n\n## Pending\n(what remains unfinished)\n\n## Blockers\n(anything preventing completion)\n\n## Key Decisions\n(significant design choices made)\n\n## Next Step\n(single most important next action)\n```\n"
	if string(data) != wantPrompt {
		t.Fatalf("unexpected continue prompt content: %q", string(data))
	}
}

func TestRunSingle_RetryUsesPRReviewPrompt(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	branch := "issue-386/smart-completion-detection-phase-aware-retry"
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: true, HeadRefName: branch, HeadRefOid: "current-sha"}},
		},
		renderer: renderer,
		errorLog: io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, func() (gitIdentity, error) {
		return gitIdentity{}, nil
	}, map[int]string{42: branch}, prompt.RenderConfig{}, nil, map[int]sandbox.Sandbox{}, &sync.Mutex{}, &retrySandboxFactory{sandbox: rtSandbox}, nil, "main", nil, nil, 1, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if renderer.renderCalls != 1 {
		t.Fatalf("render calls = %d, want 1", renderer.renderCalls)
	}
	if resetCalls != 0 {
		t.Fatalf("reset calls = %d, want 0", resetCalls)
	}
	if rtSandbox.execCommand != "opencode run .sandman/continue-prompt.md" {
		t.Fatalf("expected continue prompt command, got %q", rtSandbox.execCommand)
	}
	continuePromptPath := filepath.Join(worktreePath, ".sandman", "continue-prompt.md")
	data, err := os.ReadFile(continuePromptPath)
	if err != nil {
		t.Fatalf("read continue prompt: %v", err)
	}
	if string(data) != "Continue with sandman-pr-review until the PR is merged" {
		t.Fatalf("unexpected continue prompt content: %q", string(data))
	}
}

func TestRunSingle_RetrySkipsClosedPRReview(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	branch := "issue-386/smart-completion-detection-phase-aware-retry"
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "closed", Merged: true, HeadRefName: branch, HeadRefOid: "current-sha"}},
		},
		renderer: renderer,
		errorLog: io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, func() (gitIdentity, error) {
		return gitIdentity{}, nil
	}, map[int]string{42: branch}, prompt.RenderConfig{}, nil, map[int]sandbox.Sandbox{}, &sync.Mutex{}, &retrySandboxFactory{sandbox: rtSandbox}, nil, "main", nil, nil, 1, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if resetCalls != 1 {
		t.Fatalf("reset calls = %d, want 1", resetCalls)
	}
	if rtSandbox.execCommand == "opencode run .sandman/continue-prompt.md" {
		t.Fatal("expected closed PR to skip review continuation")
	}
}

func TestRunSingle_LogsRetryCounters(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	log := &spyEventLog{}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}},
		renderer:     &retryRenderer{result: "rendered prompt"},
		errorLog:     io.Discard,
		eventLog:     log,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, func() (gitIdentity, error) {
		return gitIdentity{}, nil
	}, map[int]string{42: branch}, prompt.RenderConfig{}, nil, map[int]sandbox.Sandbox{}, &sync.Mutex{}, &retrySandboxFactory{sandbox: rtSandbox}, nil, "main", nil, nil, 1, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.RetriesTotal != 2 {
		t.Fatalf("RetriesTotal = %d, want 2", result.RetriesTotal)
	}
	if len(log.events) == 0 {
		t.Fatal("expected events")
	}
	finished := log.events[len(log.events)-1]
	if got := finished.Payload["retries_total"]; got != 1 {
		t.Fatalf("retries_total = %#v, want 1", got)
	}
	if got := finished.Payload["retries_done"]; got != 1 {
		t.Fatalf("retries_done = %#v, want 1", got)
	}
}

func TestBatchStartGate_HonoursEffectiveParallel(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Issue 1"},
			2: {Number: 2, Title: "Issue 2"},
			3: {Number: 3, Title: "Issue 3"},
			4: {Number: 4, Title: "Issue 4"},
		},
	}
	factory := &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	runnables := &fakeRunnableFactory{
		results: []AgentRunResult{
			{IssueNumber: 1, Status: "success"},
			{IssueNumber: 2, Status: "success"},
			{IssueNumber: 3, Status: "success"},
			{IssueNumber: 4, Status: "success"},
		},
		delays: []time.Duration{50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:          "test-agent",
		Sandbox:        "container",
		WorktreeDir:    ".sandman/worktrees",
		MaxContainers:  2,
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = runnables

	_, err := o.RunBatch(context.Background(), Request{
		Issues:            []int{1, 2, 3, 4},
		ContainerCapacity: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runnables.max != 4 {
		t.Fatalf("expected max 4 concurrent runs, got %d", runnables.max)
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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

func TestRunBatch_LogsCancelledEventOnCancel(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	proc := &fakeProcess{}
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	blockRunnable := &blockingRunnable{delayAfterCancel: 100 * time.Millisecond}
	spyLog := &spyEventLog{}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = factory
	o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _ = o.RunBatch(ctx, Request{Issues: []int{42}})

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	if spyLog.events[1].Type != "run.cancelled" {
		t.Fatalf("expected cancelled terminal event, got %q", spyLog.events[1].Type)
	}
	if status, _ := spyLog.events[1].Payload["status"].(string); status != "failure" {
		t.Fatalf("expected cancelled run to report failure, got %q", status)
	}
	if run := events.ProjectRunStates(spyLog.events); len(run) != 1 || run[0].IsActive() {
		t.Fatalf("expected cancelled run to project as terminal, got %#v", run)
	}
}

func TestRunBatch_ReturnsCancelledStatusOnCancel(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	proc := &fakeProcess{}
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	blockRunnable := &blockingRunnable{delayAfterCancel: 100 * time.Millisecond}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result, err := o.RunBatch(ctx, Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected interrupted batch to return error")
	}
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "failure" {
		t.Fatalf("expected cancelled batch result to report failure, got %#v", result)
	}
}

func TestRunBatch_PreservesSuccessfulRunWhenContextCancelsLate(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	proc := &fakeProcess{}
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	fastSuccess := &fakeRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}, delay: 100 * time.Millisecond}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success"}}, delays: []time.Duration{100 * time.Millisecond}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result, err := o.RunBatch(ctx, Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("expected successful batch result, got error: %v", err)
	}
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		t.Fatalf("expected successful run to stay success, got %#v", result)
	}
	_ = fastSuccess
}

func TestRunBatch_LogsCancelledEventOnPromptOnlyCancel(t *testing.T) {
	client := &fakeGitHubClient{}

	proc := &fakeProcess{}
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	blockRunnable := &blockingRunnable{delayAfterCancel: 100 * time.Millisecond}
	spyLog := &spyEventLog{}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = factory
	o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _ = o.RunBatch(ctx, Request{PromptConfig: prompt.RenderConfig{PromptFlag: "return only ok"}})

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	if spyLog.events[1].Type != "run.cancelled" {
		t.Fatalf("expected cancelled terminal event, got %q", spyLog.events[1].Type)
	}
	if status, _ := spyLog.events[1].Payload["status"].(string); status != "failure" {
		t.Fatalf("expected cancelled run to report failure, got %q", status)
	}
}

func TestRunBatch_ReturnsCancelledStatusOnPromptOnlyCancel(t *testing.T) {
	client := &fakeGitHubClient{}

	proc := &fakeProcess{}
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	started := make(chan struct{})
	release := make(chan struct{})
	signalSent := make(chan struct{})

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{0: &controlledRunnable{result: AgentRunResult{Status: "failure"}, started: started, release: release}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		waitForSignal(t, started, "expected prompt-only run to start")
		cancel()
		go func() {
			for !proc.sigTermCalled {
				time.Sleep(5 * time.Millisecond)
			}
			close(signalSent)
		}()
		waitForSignal(t, signalSent, "expected SIGTERM to be sent to prompt-only process")
		close(release)
	}()

	result, err := o.RunBatch(ctx, Request{PromptConfig: prompt.RenderConfig{PromptFlag: "return only ok"}})
	if err == nil {
		t.Fatal("expected interrupted prompt-only batch to return error")
	}
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "failure" {
		t.Fatalf("expected cancelled prompt-only batch result to report failure, got %#v", result)
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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

func TestRunBatch_PreservesWorktreeOnSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	if _, err := os.Stat(worktreePath); err != nil {
		t.Errorf("expected worktree to be preserved on success, got: %v", err)
	}
}

func TestRunBatch_DoesNotCallStopOnSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	sb := &fakeSandbox{}
	factory := &fakeSandboxFactory{sandbox: sb}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sb.stopCalled {
		t.Error("expected Stop not to be called on successful run")
	}
}

func TestRunBatch_LeavesWorktreeOnSuccess(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	sb := &fakeSandbox{}
	factory := &fakeSandboxFactory{sandbox: sb}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sb.stopCalled {
		t.Error("expected Stop not to be called on successful run")
	}
}

func TestRunBatch_ModelPrecedenceAndDefaultBehavior(t *testing.T) {
	tests := []struct {
		name     string
		agent    string
		cfgModel string
		reqModel string
		wantCmd  string
	}{
		{
			name:     "request overrides config",
			agent:    "opencode",
			cfgModel: "config-model",
			reqModel: "request-model",
			wantCmd:  `opencode run -m request-model "$(cat .sandman/rendered-prompt.md)"`,
		},
		{
			name:     "config model is used",
			agent:    "opencode",
			cfgModel: "config-model",
			wantCmd:  `opencode run -m config-model "$(cat .sandman/rendered-prompt.md)"`,
		},
		{
			name:    "default behavior leaves model out",
			agent:   "opencode",
			wantCmd: `opencode run "$(cat .sandman/rendered-prompt.md)"`,
		},
		{
			name:     "pi splits provider and model",
			agent:    "pi",
			cfgModel: "openai/gpt-4.1",
			wantCmd:  `pi --print --provider openai --model gpt-4.1 "$(cat .sandman/rendered-prompt.md)"`,
		},
		{
			name:     "pi request model overrides config",
			agent:    "pi",
			cfgModel: "anthropic/claude-sonnet-4",
			reqModel: "openai/gpt-4.1",
			wantCmd:  `pi --print --provider openai --model gpt-4.1 "$(cat .sandman/rendered-prompt.md)"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{
				issues: map[int]*github.Issue{
					42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
				},
				prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
			}
			sb := &fakeSandbox{}
			o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
				Agent:       tt.agent,
				Sandbox:     "worktree",
				WorktreeDir: ".sandman/worktrees",
				Git:         config.GitConfig{BaseBranch: "main"},
				AgentProviders: map[string]config.Agent{
					"opencode": {Preset: "opencode", Model: tt.cfgModel},
					"pi":       {Preset: "pi", Model: tt.cfgModel},
				},
			}}, nil)
			o.sandboxFactory = &fakeSandboxFactory{sandbox: sb}

			_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Model: tt.reqModel})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sb.execCommand != tt.wantCmd {
				t.Errorf("expected command %q, got %q", tt.wantCmd, sb.execCommand)
			}
		})
	}
}

func TestRunBatch_PiModelMustUseProviderModelFormat(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:       "pi",
		Sandbox:     "worktree",
		WorktreeDir: ".sandman/worktrees",
		Git:         config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"pi": {Preset: "pi", Model: "gpt-4.1"},
		},
	}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error for invalid pi model value")
	}
	if !strings.Contains(err.Error(), "provider/model format") {
		t.Fatalf("expected error mentioning provider/model format, got %v", err)
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected error when fetch fails")
	}
}

func TestRunBatch_LifecycleErrorsPrintedToStderr(t *testing.T) {
	var buf bytes.Buffer
	client := &fakeGitHubClient{err: errors.New("github api error")}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.errorLog = &buf
	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if !strings.Contains(buf.String(), "github api error") {
		t.Errorf("expected fetch error on stderr, got: %s", buf.String())
	}
}

func TestRunBatch_SandboxStartErrorPrintedToStderr(t *testing.T) {
	var buf bytes.Buffer
	sb := &fakeSandbox{startErr: errors.New("sandbox start failure")}
	factory := &fakeSandboxFactory{sandbox: sb}

	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "test", Body: "body"}}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.errorLog = &buf
	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if !strings.Contains(buf.String(), "sandbox start failure") {
		t.Errorf("expected sandbox start error on stderr, got: %s", buf.String())
	}
}

func TestRunBatch_NoIssues(t *testing.T) {
	client := &fakeGitHubClient{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 0 {
		t.Errorf("expected 0 runs, got %d", len(result.Runs))
	}
}

func TestRunBatch_SyncsBaseBranchBeforeEachAgentRunStarts(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
		},
	}
	store := &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}
	o := NewOrchestrator(client, &noopRenderer{}, store, nil)

	tracker := &baseBranchSyncTracker{}
	o.baseBranchSync = func(repoPath, sourceBranch string) error {
		tracker.mu.Lock()
		tracker.syncCalls++
		tracker.mu.Unlock()
		return nil
	}
	o.sandboxFactory = &syncAwareSandboxFactory{tracker: tracker}
	o.runnableFactory = &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 1, Status: "success"}, {IssueNumber: 2, Status: "success"}}}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}, Parallel: 2, BaseBranch: "trunk"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tracker.mu.Lock()
	if tracker.syncCalls != 2 {
		tracker.mu.Unlock()
		t.Fatalf("expected two syncs, got %d", tracker.syncCalls)
	}
	if tracker.startCalls != 2 {
		tracker.mu.Unlock()
		t.Fatalf("expected two starts, got %d", tracker.startCalls)
	}
	if tracker.beforeStart {
		tracker.mu.Unlock()
		t.Fatal("expected each worktree to start after base branch sync")
	}
	if !reflect.DeepEqual(tracker.branches, []string{"trunk", "trunk"}) {
		tracker.mu.Unlock()
		t.Fatalf("expected base branch to be passed to each worktree, got %v", tracker.branches)
	}
	tracker.mu.Unlock()
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
	o := NewOrchestrator(client, spy, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, spy, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
			Git:         config.GitConfig{BaseBranch: "main"},
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

	promptPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug", ".sandman", "rendered-prompt.md")
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
			Git:         config.GitConfig{BaseBranch: "main"},
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
	if os.Getenv("SANDMAN_E2E") == "" {
		t.Skip("set SANDMAN_E2E=1 to run end-to-end batch test")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	configData := `default_agent: test-agent
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

	promptPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-login-bug", ".sandman", "rendered-prompt.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "You are running inside a Sandman-created worktree.") {
		t.Errorf("prompt missing worktree context, got:\n%s", got)
	}
	if !strings.Contains(got, "Current branch: `sandman/42-fix-login-bug`") {
		t.Errorf("prompt missing branch info, got:\n%s", got)
	}
	if !strings.Contains(got, "Review command: `/oc review`") {
		t.Errorf("prompt missing review command, got:\n%s", got)
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}

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

func TestRunBatch_ZeroParallelAllowsAllRunsToStart(t *testing.T) {
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

	release := make(chan struct{})
	started := make([]chan struct{}, 5)
	runnables := make(map[int]Runnable, 5)
	for i := 1; i <= 5; i++ {
		started[i-1] = make(chan struct{})
		runnables[i] = &controlledRunnable{
			result:  AgentRunResult{IssueNumber: i, Status: "success"},
			started: started[i-1],
			release: release,
		}
	}
	factory := &controlledRunnableFactory{runnables: runnables}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.sandboxFactory = &freshSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4, 5}, Parallel: 0})
		errCh <- err
	}()

	for i, ch := range started {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for run %d to start", i+1)
		}
	}

	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(factory.created) != 5 {
		t.Fatalf("expected 5 created runnables, got %d", len(factory.created))
	}
}

func TestRunBatch_NegativeParallelRejected(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	o := NewOrchestrator(&fakeGitHubClient{}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1}, Parallel: -1})
	if err == nil {
		t.Fatal("expected error for negative parallel")
	}
	if !strings.Contains(err.Error(), "parallel must be 0 or greater") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestRunBatch_WaitsForBlockersBeforeStartingDependents(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker", State: "closed"},
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
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

func TestRunBatch_SkipsIssuesBlockedByOpenExternalBlockers(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Runnable"},
			100: {Number: 100, Title: "Blocked", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "External blocker"},
		},
	}

	spyLog := &spyEventLog{}
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42:  &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}},
			100: &controlledRunnable{result: AgentRunResult{IssueNumber: 100, Status: "failure"}},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.runnableFactory = factory

	result, err := o.RunBatch(context.Background(), Request{
		Issues:   []int{42, 100},
		Blocked:  map[int][]int{100: {7}},
		Parallel: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(result.Runs))
	}

	statuses := make(map[int]string)
	for _, run := range result.Runs {
		statuses[run.IssueNumber] = run.Status
	}
	if statuses[42] != "success" {
		t.Fatalf("expected runnable issue to succeed, got %q", statuses[42])
	}
	if statuses[100] != "blocked" {
		t.Fatalf("expected external-blocked issue to be skipped, got %q", statuses[100])
	}
	if len(factory.created) != 1 || factory.created[0] != 42 {
		t.Fatalf("expected only runnable issue to start, got %v", factory.created)
	}

	var blockedEvent *events.Event
	for i := range spyLog.events {
		e := spyLog.events[i]
		if e.Type == "run.blocked" && e.Issue == 100 {
			blockedEvent = &e
		}
	}
	if blockedEvent == nil {
		t.Fatal("expected run.blocked event for external blocker")
	}
	blockedBy, ok := blockedEvent.Payload["blocked_by"].([]int)
	if !ok || !reflect.DeepEqual(blockedBy, []int{7}) {
		t.Fatalf("expected blocked_by [7], got %#v", blockedEvent.Payload["blocked_by"])
	}
}

func TestRunBatch_RechecksInBatchBlockerStateBeforeDependentStart(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker", State: "open"},
			100: {Number: 100, Title: "Dependent", BlockedBy: []int{42}},
		},
	}

	spyLog := &spyEventLog{}
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	dependentStarted := make(chan struct{})
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42:  &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}, started: blockerStarted, release: releaseBlocker},
			100: &controlledRunnable{result: AgentRunResult{IssueNumber: 100, Status: "success"}, started: dependentStarted},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.runnableFactory = factory

	done := make(chan struct{})
	var result *Result
	var err error
	go func() {
		defer close(done)
		result, err = o.RunBatch(context.Background(), Request{
			Issues:       []int{42, 100},
			Dependencies: map[int][]int{100: {42}},
			Parallel:     2,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")
	assertNoSignal(t, dependentStarted, "dependent should wait for blocker to finish")
	close(releaseBlocker)

	assertNoSignal(t, dependentStarted, "dependent should stay blocked because blocker issue is still open")
	waitForSignal(t, done, "expected batch to finish")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(result.Runs))
	}

	statuses := make(map[int]string)
	for _, run := range result.Runs {
		statuses[run.IssueNumber] = run.Status
	}
	if statuses[42] != "success" {
		t.Fatalf("expected blocker success, got %q", statuses[42])
	}
	if statuses[100] != "blocked" {
		t.Fatalf("expected dependent blocked, got %q", statuses[100])
	}

	var blockedEvent *events.Event
	for i := range spyLog.events {
		e := spyLog.events[i]
		if e.Type == "run.blocked" && e.Issue == 100 {
			blockedEvent = &e
		}
	}
	if blockedEvent == nil {
		t.Fatal("expected run.blocked event for dependent")
	}
	blockedBy, ok := blockedEvent.Payload["blocked_by"].([]int)
	if !ok || !reflect.DeepEqual(blockedBy, []int{42}) {
		t.Fatalf("expected blocked_by [42], got %#v", blockedEvent.Payload["blocked_by"])
	}
}

func TestRunBatch_RechecksExternalBlockerStateBeforeDependentStart(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	releaseExternalFetch := make(chan struct{})
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Runnable", State: "closed"},
			100: {Number: 100, Title: "Blocked", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "External blocker", State: "open"},
		},
		fetchRelease: map[int]<-chan struct{}{
			7: releaseExternalFetch,
		},
	}

	spyLog := &spyEventLog{}
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	dependentStarted := make(chan struct{})
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42:  &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}, started: blockerStarted, release: releaseBlocker},
			100: &controlledRunnable{result: AgentRunResult{IssueNumber: 100, Status: "success"}, started: dependentStarted},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.runnableFactory = factory

	done := make(chan struct{})
	var result *Result
	var err error
	go func() {
		defer close(done)
		result, err = o.RunBatch(context.Background(), Request{
			Issues:       []int{42, 100},
			Blocked:      map[int][]int{100: {7}},
			Dependencies: map[int][]int{100: {42}},
			Parallel:     2,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")
	assertNoSignal(t, dependentStarted, "dependent should wait for blocker to finish")
	close(releaseBlocker)

	assertNoSignal(t, dependentStarted, "dependent should wait for external blocker recheck")
	client.issues[7].State = "closed"
	close(releaseExternalFetch)
	waitForSignal(t, dependentStarted, "expected dependent to start after external blocker closes")
	waitForSignal(t, done, "expected batch to finish")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(result.Runs))
	}

	statuses := make(map[int]string)
	for _, run := range result.Runs {
		statuses[run.IssueNumber] = run.Status
	}
	if statuses[42] != "success" {
		t.Fatalf("expected blocker success, got %q", statuses[42])
	}
	if statuses[100] != "success" {
		t.Fatalf("expected dependent success after blocker closed, got %q", statuses[100])
	}

	for i := range spyLog.events {
		if spyLog.events[i].Type == "run.blocked" && spyLog.events[i].Issue == 100 {
			t.Fatalf("did not expect blocked event for dependent: %#v", spyLog.events[i])
		}
	}
}

func TestRunBatch_PreservesParallelismWithinDependencyLevel(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Blocker", State: "closed"},
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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

func TestRunBatch_StartDelay_WaitsAfterRunFinishesBeforeNextStart(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "First"},
			2: {Number: 2, Title: "Second"},
		},
	}

	started1 := make(chan struct{})
	started2 := make(chan struct{})
	release1 := make(chan struct{})
	release2 := make(chan struct{})

	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			1: &controlledRunnable{result: AgentRunResult{IssueNumber: 1, Status: "success"}, started: started1, release: release1},
			2: &controlledRunnable{result: AgentRunResult{IssueNumber: 2, Status: "success"}, started: started2, release: release2},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory
	sb := &fakeSandbox{}
	o.sandboxFactory = &fakeSandboxFactory{sandbox: sb}
	var errBuf bytes.Buffer
	o.errorLog = &errBuf

	done := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := o.RunBatch(context.Background(), Request{
			Issues:        []int{1, 2},
			Parallel:      1,
			StartDelay:    100 * time.Millisecond,
			StartDelaySet: true,
		})
		errCh <- err
	}()

	var firstRelease chan struct{}
	var secondRelease chan struct{}
	var secondStarted <-chan struct{}
	select {
	case <-started1:
		firstRelease = release1
		secondRelease = release2
		secondStarted = started2
	case <-started2:
		firstRelease = release2
		secondRelease = release1
		secondStarted = started1
	case <-done:
		t.Fatalf("batch returned early: err=%v logs=%s", <-errCh, errBuf.String())
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("expected first run to start; sandbox started=%v created=%v logs=%s", sb.startCalled, factory.created, errBuf.String())
	}
	assertNoSignal(t, secondStarted, "second run started before first completed")

	close(firstRelease)
	assertNoSignal(t, secondStarted, "second run started before start delay elapsed")
	waitForSignal(t, secondStarted, "expected second run to start after start delay")

	close(secondRelease)
	waitForSignal(t, done, "expected batch to complete")
}

func TestRunBatch_StartDelay_DoesNotStaggerSimultaneousReadyRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Blocker", State: "closed"},
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
			1: &controlledRunnable{result: AgentRunResult{IssueNumber: 1, Status: "success"}, started: blockerStarted, release: releaseBlocker},
			3: &controlledRunnable{result: AgentRunResult{IssueNumber: 3, Status: "success"}, started: dependentAStarted, release: releaseDependentA},
			4: &controlledRunnable{result: AgentRunResult{IssueNumber: 4, Status: "success"}, started: dependentBStarted, release: releaseDependentB},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{
			Issues:        []int{1, 3, 4},
			Dependencies:  map[int][]int{3: {1}, 4: {1}},
			Parallel:      2,
			StartDelay:    100 * time.Millisecond,
			StartDelaySet: true,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")
	releaseAt := time.Now()
	close(releaseBlocker)

	var secondStarted <-chan struct{}
	var firstAt time.Time
	select {
	case <-dependentAStarted:
		secondStarted = dependentBStarted
		firstAt = time.Now()
	case <-dependentBStarted:
		secondStarted = dependentAStarted
		firstAt = time.Now()
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected dependent runs to start after start delay")
	}
	if elapsed := firstAt.Sub(releaseAt); elapsed < 75*time.Millisecond {
		t.Fatalf("expected start delay before dependent runs, got %s", elapsed)
	}

	var secondAt time.Time
	select {
	case <-secondStarted:
		secondAt = time.Now()
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected second dependent to start")
	}
	if diff := secondAt.Sub(firstAt); diff > 50*time.Millisecond {
		t.Fatalf("expected simultaneous ready runs to start together, got %s", diff)
	}

	close(releaseDependentA)
	close(releaseDependentB)
	waitForSignal(t, done, "expected batch to complete")
}

func TestRunBatch_StartDelay_AbortsImmediatelyOnCancel(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "First"},
			2: {Number: 2, Title: "Second"},
		},
	}

	started1 := make(chan struct{})
	started2 := make(chan struct{})
	release1 := make(chan struct{})
	release2 := make(chan struct{})

	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			1: &controlledRunnable{result: AgentRunResult{IssueNumber: 1, Status: "success"}, started: started1, release: release1},
			2: &controlledRunnable{result: AgentRunResult{IssueNumber: 2, Status: "success"}, started: started2, release: release2},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(ctx, Request{
			Issues:        []int{1, 2},
			Parallel:      1,
			StartDelay:    500 * time.Millisecond,
			StartDelaySet: true,
		})
	}()

	var firstRelease chan struct{}
	var secondStarted <-chan struct{}
	select {
	case <-started1:
		firstRelease = release1
		secondStarted = started2
	case <-started2:
		firstRelease = release2
		secondStarted = started1
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected first run to start")
	}
	cancel()
	assertNoSignal(t, secondStarted, "second run started after batch cancellation")
	close(firstRelease)

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected batch to abort immediately after cancellation")
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)

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
	if spyLog.events[1].Payload["base_branch"] != "main" {
		t.Errorf("expected finished base branch main, got %#v", spyLog.events[1].Payload["base_branch"])
	}
}

func TestRunBatch_LogsPromptMetadataOnStartedEvent(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}}}}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, PromptConfig: prompt.RenderConfig{PromptFlag: "inline", PromptArgs: map[string]string{"FOO": "bar"}, ReviewCommand: "/custom review", ReviewCommandSet: true}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) == 0 {
		t.Fatal("expected run.started event")
	}
	started := spyLog.events[0]
	if started.Type != "run.started" {
		t.Fatalf("expected first event run.started, got %q", started.Type)
	}
	if started.Payload["prompt_source_type"] != "prompt" {
		t.Fatalf("expected prompt source type prompt, got %#v", started.Payload["prompt_source_type"])
	}
	if started.Payload["prompt_source_value"] != "inline" {
		t.Fatalf("expected prompt source value inline, got %#v", started.Payload["prompt_source_value"])
	}
	if started.Payload["base_branch"] != "main" {
		t.Fatalf("expected base branch main, got %#v", started.Payload["base_branch"])
	}
	args, ok := started.Payload["prompt_args"].(map[string]string)
	if !ok || args["FOO"] != "bar" {
		t.Fatalf("expected prompt args replay, got %#v", started.Payload["prompt_args"])
	}
	if started.Payload["review_command"] != "/custom review" {
		t.Fatalf("expected review command replay, got %#v", started.Payload["review_command"])
	}
	if started.Payload["agent"] != "test-agent" {
		t.Fatalf("expected agent replay, got %#v", started.Payload["agent"])
	}
}

func TestRunBatch_LogsPromptOnlyTemplateSource(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	templatePath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(templatePath, []byte("Return only OK."), 0644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	client := &fakeGitHubClient{err: errors.New("fetch should not run")}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	tracker := &baseBranchSyncTracker{}
	o.baseBranchSync = func(repoPath, sourceBranch string) error {
		tracker.mu.Lock()
		tracker.syncCalls++
		tracker.mu.Unlock()
		return nil
	}
	o.sandboxFactory = &syncAwareSandboxFactory{tracker: tracker}
	o.runnableFactory = &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		return AgentRunResult{Status: "success", Branch: branch, WorktreePath: filepath.Join(".sandman", "worktrees", branch)}
	}}

	_, err := o.RunBatch(context.Background(), Request{PromptConfig: prompt.RenderConfig{TemplateFlag: templatePath}, BaseBranch: "trunk"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) == 0 {
		t.Fatal("expected run.started event")
	}
	started := spyLog.events[0]
	if started.Payload["prompt_source_type"] != "template" {
		t.Fatalf("expected prompt source type template, got %#v", started.Payload["prompt_source_type"])
	}
	if started.Payload["prompt_source_value"] != templatePath {
		t.Fatalf("expected prompt source value template path, got %#v", started.Payload["prompt_source_value"])
	}
	if started.Payload["base_branch"] != "trunk" {
		t.Fatalf("expected base branch trunk, got %#v", started.Payload["base_branch"])
	}
	tracker.mu.Lock()
	if tracker.syncCalls != 1 {
		tracker.mu.Unlock()
		t.Fatalf("expected one sync, got %d", tracker.syncCalls)
	}
	if tracker.startCalls != 1 {
		tracker.mu.Unlock()
		t.Fatalf("expected one start, got %d", tracker.startCalls)
	}
	if tracker.beforeStart {
		tracker.mu.Unlock()
		t.Fatal("expected prompt-only worktree to start after base branch sync")
	}
	if !reflect.DeepEqual(tracker.branches, []string{"trunk"}) {
		tracker.mu.Unlock()
		t.Fatalf("expected prompt-only worktree to use trunk, got %v", tracker.branches)
	}
	tracker.mu.Unlock()
}

func TestRunBatch_PromptOnlyBaseBranchSyncFailureReturnsError(t *testing.T) {
	client := &fakeGitHubClient{err: errors.New("fetch should not run")}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.baseBranchSync = func(repoPath, sourceBranch string) error { return errors.New("sync failed") }
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		return AgentRunResult{Status: "success", Branch: branch}
	}}

	_, err := o.RunBatch(context.Background(), Request{PromptConfig: prompt.RenderConfig{PromptFlag: "Return only OK."}, BaseBranch: "trunk"})
	if err == nil {
		t.Fatal("expected prompt-only run to fail when base branch sync fails")
	}
	if !strings.Contains(err.Error(), "prompt-only run failed") {
		t.Fatalf("expected prompt-only failure, got %v", err)
	}
}

func TestRunBatch_PromptOnlyRunSkipsIssueLookupAndUsesNullIssue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{err: errors.New("fetch should not run")}
	spyLog := &spyEventLog{}
	var sawIssueNil bool
	var sawBranch string
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(".sandman", "worktrees", "sandman", "return-only-ok-123")}}
	o.runnableFactory = &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		sawIssueNil = issue == nil
		sawBranch = branch
		return AgentRunResult{Status: "success", Branch: branch, WorktreePath: filepath.Join(".sandman", "worktrees", branch)}
	}}

	result, err := o.RunBatch(context.Background(), Request{PromptConfig: prompt.RenderConfig{PromptFlag: "Return only OK."}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || len(result.Runs) != 1 {
		t.Fatalf("expected 1 prompt-only run, got %#v", result)
	}
	if !sawIssueNil {
		t.Fatal("expected prompt-only run to skip issue lookup")
	}
	if !strings.HasPrefix(sawBranch, "sandman/return-only-ok-") {
		t.Fatalf("expected prompt-only branch prefix, got %q", sawBranch)
	}
	if result.Runs[0].IssueNumber != 0 {
		t.Fatalf("expected zero legacy issue number, got %d", result.Runs[0].IssueNumber)
	}
	if result.Runs[0].Issue != nil {
		t.Fatalf("expected nil issue ref in result, got %v", *result.Runs[0].Issue)
	}
	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	for _, evt := range spyLog.events {
		if evt.IssueRef != nil {
			t.Fatalf("expected null issue in event %q, got %d", evt.Type, *evt.IssueRef)
		}
	}
}

func TestRunBatch_LogsContinuedEventWithPreviousRunID(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}}}}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Continuation: true, PreviousRunIDs: map[int]string{42: "run-42-1"}, BaseBranch: "main", PromptConfig: prompt.RenderConfig{ContinuePrompt: "finish the tests"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) == 0 {
		t.Fatal("expected run.continued event")
	}
	continued := spyLog.events[0]
	if continued.Type != "run.continued" {
		t.Fatalf("expected first event run.continued, got %q", continued.Type)
	}
	if continued.Payload["previous_run_id"] != "run-42-1" {
		t.Fatalf("expected previous run ID replay, got %#v", continued.Payload["previous_run_id"])
	}
	if continued.Payload["agent"] != "test-agent" {
		t.Fatalf("expected agent replay, got %#v", continued.Payload["agent"])
	}
	if continued.Payload["base_branch"] != "main" {
		t.Fatalf("expected base branch replay, got %#v", continued.Payload["base_branch"])
	}
}

func TestRunBatch_PerIssuePreviousRunIDLookup(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
			43: {Number: 43, Title: "Add tests"},
		},
	}
	spyLog := &threadSafeSpyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}},
		43: &controlledRunnable{result: AgentRunResult{IssueNumber: 43, Status: "success"}},
	}}

	_, err := o.RunBatch(context.Background(), Request{
		Issues:         []int{42, 43},
		Continuation:   true,
		PreviousRunIDs: map[int]string{42: "run-42-prev", 43: "run-43-prev"},
		BaseBranch:     "main",
		Parallel:       1,
		PromptConfig:   prompt.RenderConfig{ContinuePrompt: "finish the work"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	continuedByIssue := map[int]events.Event{}
	for _, evt := range spyLog.Snapshot() {
		if evt.Type == "run.continued" {
			continuedByIssue[evt.Issue] = evt
		}
	}
	if len(continuedByIssue) != 2 {
		t.Fatalf("expected run.continued events for issues 42 and 43, got %#v", continuedByIssue)
	}
	if continuedByIssue[42].Payload["previous_run_id"] != "run-42-prev" {
		t.Fatalf("expected issue 42 previous run id run-42-prev, got %#v", continuedByIssue[42].Payload["previous_run_id"])
	}
	if continuedByIssue[43].Payload["previous_run_id"] != "run-43-prev" {
		t.Fatalf("expected issue 43 previous run id run-43-prev, got %#v", continuedByIssue[43].Payload["previous_run_id"])
	}
}

func TestRunBatch_MultiIssueContinuationLogsPerIssuePreviousRunID(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug A"},
			99: {Number: 99, Title: "Fix bug B"},
		},
	}
	spyLog := &threadSafeSpyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}},
		99: &controlledRunnable{result: AgentRunResult{IssueNumber: 99, Status: "success"}},
	}}

	_, err := o.RunBatch(context.Background(), Request{
		Issues:         []int{42, 99},
		Continuation:   true,
		PreviousRunIDs: map[int]string{42: "run-42-7", 99: "run-99-3"},
		BaseBranch:     "main",
		Parallel:       1,
		PromptConfig:   prompt.RenderConfig{ContinuePrompt: "finish them"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prevByIssue := map[int]string{}
	for _, evt := range spyLog.Snapshot() {
		if evt.Type != "run.continued" {
			continue
		}
		got, _ := evt.Payload["previous_run_id"].(string)
		prevByIssue[evt.Issue] = got
	}
	if prevByIssue[42] != "run-42-7" {
		t.Fatalf("expected issue 42 previous_run_id=run-42-7, got %q", prevByIssue[42])
	}
	if prevByIssue[99] != "run-99-3" {
		t.Fatalf("expected issue 99 previous_run_id=run-99-3, got %q", prevByIssue[99])
	}
}

func TestRunBatch_ContinuationSkipsBaseBranchSync(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	tracker := &baseBranchSyncTracker{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "trunk"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.baseBranchSync = func(repoPath, sourceBranch string) error {
		tracker.mu.Lock()
		tracker.syncCalls++
		tracker.mu.Unlock()
		return nil
	}
	o.sandboxFactory = &syncAwareSandboxFactory{tracker: tracker}
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}}}}

	worktreePath := filepath.Join(".sandman", "worktrees", "sandman", "42-fix-bug")
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Continuation: true, BaseBranch: "main", PreviousRunIDs: map[int]string{42: "run-42-1"}, PromptConfig: prompt.RenderConfig{ContinuePrompt: "finish the tests"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.syncCalls != 0 {
		t.Fatalf("expected no base branch sync, got %d", tracker.syncCalls)
	}
	if !reflect.DeepEqual(tracker.branches, []string{"main"}) {
		t.Fatalf("expected stored base branch passed to sandbox, got %v", tracker.branches)
	}
}

func TestRunBatch_ChainedContinuationFlow(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(".sandman", "worktrees", branch)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	state := &continuationFlowState{contexts: []string{
		"## Completed\nInitial run.\n",
		"## Completed\nFirst continue.\n",
		"## Completed\nSecond continue.\n",
	}}
	log := &spyEventLog{}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: filepath.Join(".sandman", "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}}, log)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	o.runnableFactory = &continuationFlowRunnableFactory{state: state}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("initial run failed: %v", err)
	}
	initialContext, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "continuation-context.md"))
	if err != nil {
		t.Fatalf("read initial continuation context: %v", err)
	}
	if !strings.Contains(string(initialContext), "Initial run.") {
		t.Fatalf("expected initial context to be written, got %q", string(initialContext))
	}

	_, err = o.RunBatch(context.Background(), Request{Issues: []int{42}, Continuation: true, BaseBranch: "main", PreviousRunIDs: map[int]string{42: log.events[0].RunID}, PromptConfig: prompt.RenderConfig{ContinuePrompt: "finish the tests"}})
	if err != nil {
		t.Fatalf("first continue failed: %v", err)
	}
	firstContinueContext, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "continuation-context.md"))
	if err != nil {
		t.Fatalf("read first continue context: %v", err)
	}
	if !strings.Contains(string(firstContinueContext), "First continue.") {
		t.Fatalf("expected first continue context to be written, got %q", string(firstContinueContext))
	}

	_, err = o.RunBatch(context.Background(), Request{Issues: []int{42}, Continuation: true, BaseBranch: "main", PreviousRunIDs: map[int]string{42: log.events[2].RunID}, PromptConfig: prompt.RenderConfig{ContinuePrompt: "push the PR"}})
	if err != nil {
		t.Fatalf("second continue failed: %v", err)
	}
	secondContinueContext, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "continuation-context.md"))
	if err != nil {
		t.Fatalf("read second continue context: %v", err)
	}
	if !strings.Contains(string(secondContinueContext), "Second continue.") {
		t.Fatalf("expected second continue context to be written, got %q", string(secondContinueContext))
	}

	if len(state.prompts) != 2 {
		t.Fatalf("expected 2 continue prompts, got %#v", state.prompts)
	}
	if state.prompts[0] != "finish the tests" {
		t.Fatalf("expected first continue prompt to pass through, got %q", state.prompts[0])
	}
	if state.prompts[1] != "push the PR" {
		t.Fatalf("expected second continue prompt to pass through, got %q", state.prompts[1])
	}
	if len(log.events) < 5 {
		t.Fatalf("expected 5 events, got %#v", log.events)
	}
	if log.events[0].Type != "run.started" || log.events[1].Type != "run.finished" || log.events[2].Type != "run.continued" || log.events[3].Type != "run.finished" || log.events[4].Type != "run.continued" {
		t.Fatalf("unexpected event sequence: %#v", []string{log.events[0].Type, log.events[1].Type, log.events[2].Type, log.events[3].Type, log.events[4].Type})
	}
	if log.events[2].Payload["previous_run_id"] != log.events[0].RunID {
		t.Fatalf("expected first continue to reference initial run, got %#v", log.events[2].Payload["previous_run_id"])
	}
	if log.events[4].Payload["previous_run_id"] != log.events[2].RunID {
		t.Fatalf("expected second continue to reference first continue, got %#v", log.events[4].Payload["previous_run_id"])
	}
}

func TestRunBatch_LogsModelOnStartedEvent(t *testing.T) {
	tests := []struct {
		name      string
		cfgModel  string
		reqModel  string
		wantModel string
	}{
		{
			name:      "config model",
			cfgModel:  "gpt-config",
			wantModel: "gpt-config",
		},
		{
			name:      "request override",
			cfgModel:  "gpt-config",
			reqModel:  "gpt-override",
			wantModel: "gpt-override",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{
				issues: map[int]*github.Issue{
					42: {Number: 42, Title: "Fix bug"},
				},
			}
			spyLog := &spyEventLog{}
			o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Model: tt.cfgModel}}}}, spyLog)
			o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
			o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}}}}

			_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Model: tt.reqModel})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			started := spyLog.events[0]
			if got := started.Payload["model"]; got != tt.wantModel {
				t.Fatalf("expected model metadata %q, got %#v", tt.wantModel, got)
			}
		})
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)

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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "exit 1"}}}}, spyLog)

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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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
	if result.Runs[0].Status != "failure" {
		t.Errorf("expected failure status, got %q", result.Runs[0].Status)
	}
}

func TestRunBatch_RecordsWorktreePathOnFailure(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	sb := &fakeSandbox{workDir: "/tmp/sandman/42-fix-bug"}
	factory := &fakeSandboxFactory{sandbox: sb}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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
	if result.Runs[0].Status != "failure" {
		t.Errorf("expected failure status, got %q", result.Runs[0].Status)
	}
}

func TestRunBatch_LogsWorktreeStatePreservedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "exit 1"}}}}, spyLog)

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
	id          string
	stopCalled  bool
	stopError   error
	dead        uint32
	aliveChecks uint32
}

func (f *fakeContainerForOrchestrator) ID() string {
	return f.id
}

func (f *fakeContainerForOrchestrator) Alive() (bool, error) {
	atomic.AddUint32(&f.aliveChecks, 1)
	return atomic.LoadUint32(&f.dead) == 0, nil
}

func (f *fakeContainerForOrchestrator) Stop() error {
	f.stopCalled = true
	return f.stopError
}

type fakeContainerStarter struct {
	startCalled     bool
	startCount      int
	startImage      string
	startDelay      time.Duration
	startOpts       sandbox.StartOptions
	container       sandbox.Container
	containers      []sandbox.Container
	err             error
	buildImageTag   string
	buildImageErr   error
	buildImageCount int
}

func (f *fakeContainerStarter) BuildImage(repoPath string) (string, error) {
	f.buildImageCount++
	if f.buildImageErr != nil {
		return "", f.buildImageErr
	}
	if f.buildImageTag != "" {
		return f.buildImageTag, nil
	}
	return "sandman-custom:latest", nil
}

func (f *fakeContainerStarter) Start(image, repoPath string, opts sandbox.StartOptions) (sandbox.Container, error) {
	f.startCalled = true
	f.startCount++
	f.startImage = image
	f.startOpts = opts
	if f.startDelay > 0 {
		time.Sleep(f.startDelay)
	}
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

func TestOrchestrator_ContainerMetadataDriftFailsBeforeBuild(t *testing.T) {
	if _, err := sandbox.ResolveRuntime("podman"); err != nil {
		t.Skip("container runtime unavailable")
	}
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte("# sandman build-tools: generic\n# sandman default-agent: pi\nFROM debian:bookworm-slim\n"), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	starter := &fakeContainerStarter{}
	o := NewOrchestrator(&fakeGitHubClient{}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:       "opencode",
		Sandbox:     "podman",
		WorktreeDir: ".sandman/worktrees",
		Git:         config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"opencode": {Command: "true"},
		},
	}}, nil)
	o.containerRuntimeFactory = &fakeContainerRuntimeFactory{starter: starter}
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "podman"})
	if err == nil {
		t.Fatal("expected metadata drift error")
	}
	if !strings.Contains(err.Error(), "scaffold metadata drift") {
		t.Fatalf("expected drift error, got: %v", err)
	}
	if starter.buildImageCount != 0 {
		t.Fatalf("expected BuildImage not to run, got %d calls", starter.buildImageCount)
	}
}

func TestOrchestrator_MetadataFreeDockerfileSkipsDriftValidation(t *testing.T) {
	if _, err := sandbox.ResolveRuntime("podman"); err != nil {
		t.Skip("container runtime unavailable")
	}
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte("FROM debian:bookworm-slim\n"), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	starter := &fakeContainerStarter{buildImageTag: "sandman-custom:latest"}
	o := NewOrchestrator(&fakeGitHubClient{}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:       "opencode",
		Sandbox:     "podman",
		WorktreeDir: ".sandman/worktrees",
		Git:         config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"opencode": {Command: "true"},
		},
	}}, nil)
	o.containerRuntimeFactory = &fakeContainerRuntimeFactory{starter: starter}
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}

	_, err := o.RunBatch(context.Background(), Request{Sandbox: "podman"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if starter.buildImageCount != 1 {
		t.Fatalf("expected BuildImage to run once, got %d calls", starter.buildImageCount)
	}
}

type trackingSandbox struct {
	fakeSandbox
	containerID string
	container   *fakeContainerForOrchestrator
}

type trackingSandboxFactory struct{}

func (f *trackingSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	sb := &trackingSandbox{}
	if container != nil {
		sb.containerID = container.ID()
		if fc, ok := container.(*fakeContainerForOrchestrator); ok {
			sb.container = fc
		}
	}
	return sb
}

type deadContainerRunnable struct {
	container *fakeContainerForOrchestrator
	result    AgentRunResult
}

func (r *deadContainerRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	if r.container != nil {
		atomic.StoreUint32(&r.container.dead, 1)
	}
	return r.result
}

type aliveCheckingRunnable struct {
	container *fakeContainerForOrchestrator
	result    AgentRunResult
}

func (r *aliveCheckingRunnable) Run(ctx context.Context, renderer prompt.Renderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	if !containerAlive(r.container) {
		return AgentRunResult{IssueNumber: r.result.IssueNumber, Status: "failure"}
	}
	return r.result
}

type recoveryRunnableFactory struct {
	mu               sync.Mutex
	containerByIssue map[int]string
	issue1           AgentRunResult
	issue2           AgentRunResult
}

func (f *recoveryRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.mu.Lock()
	defer f.mu.Unlock()
	ts, ok := sb.(*trackingSandbox)
	if ok {
		if f.containerByIssue == nil {
			f.containerByIssue = make(map[int]string)
		}
		f.containerByIssue[issue.Number] = ts.containerID
	}
	switch issue.Number {
	case 1:
		return &deadContainerRunnable{container: ts.container, result: f.issue1}
	case 2:
		return &aliveCheckingRunnable{container: ts.container, result: f.issue2}
	default:
		return &fakeRunnable{result: AgentRunResult{IssueNumber: issue.Number, Status: "failure"}}
	}
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
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Ensure a .gitconfig exists so buildStartOptions includes it.
	gitconfigPath := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(gitconfigPath, []byte("[user]\n\tname = Test\n\temail = test@test.com\n"), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}

	// Ensure ~/.config/gh exists so buildStartOptions includes it.
	ghConfigDir := filepath.Join(home, ".config", "gh")
	if err := os.MkdirAll(ghConfigDir, 0755); err != nil {
		t.Fatalf("create gh config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ghConfigDir, "hosts.yml"), []byte("github.com:\n    user: test-user\n    oauth_token: test-token\n"), 0600); err != nil {
		t.Fatalf("write gh hosts.yml: %v", err)
	}

	// Ensure ~/.config/git exists so buildStartOptions includes the XDG git config dir.
	gitConfigDir := filepath.Join(home, ".config", "git")
	if err := os.MkdirAll(gitConfigDir, 0755); err != nil {
		t.Fatalf("create git config dir: %v", err)
	}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	starter := &fakeContainerStarter{container: &fakeContainerForOrchestrator{id: "shared123"}}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true", ConfigDirs: []string{"~/.config/test"}, ConfigFiles: []string{"~/.config/test.json"}}}}}, nil)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.containerRuntimeFactory = factory

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "docker"})

	if !starter.startCalled {
		t.Fatal("expected container starter to be called")
	}
	if starter.startOpts.GitConfigPath != "" {
		t.Errorf("expected GitConfigPath to be moved into ConfigMounts, got %q", starter.startOpts.GitConfigPath)
	}
	if len(starter.startOpts.AgentConfigDirs) != 3 {
		t.Errorf("expected 3 agent config dirs (preset + gh + xdg git), got %d", len(starter.startOpts.AgentConfigDirs))
	}
	if len(starter.startOpts.AgentConfigFiles) != 1 {
		t.Errorf("expected 1 agent config file, got %d", len(starter.startOpts.AgentConfigFiles))
	}
	if starter.startOpts.UserID == "" {
		t.Error("expected UserID to be set")
	}
	if starter.startImage != "sandman-custom:latest" {
		t.Errorf(`expected image "sandman-custom:latest", got %q`, starter.startImage)
	}
	if starter.buildImageCount != 1 {
		t.Errorf("expected BuildImage to be called once, got %d", starter.buildImageCount)
	}
	if len(starter.startOpts.ConfigMounts) == 0 {
		t.Error("expected ConfigMounts to be populated when agent configs exist")
	}
	for _, mount := range starter.startOpts.ConfigMounts {
		if mount.Source == "" {
			t.Error("expected non-empty Source in ConfigMount")
		}
		if mount.Target == "" {
			t.Error("expected non-empty Target in ConfigMount")
		}
		if !filepath.IsAbs(mount.Source) {
			t.Errorf("expected absolute Source path, got %q", mount.Source)
		}
	}
	seenTargets := map[string]bool{}
	for _, mount := range starter.startOpts.ConfigMounts {
		seenTargets[mount.Target] = true
	}
	for _, target := range []string{"/.gitconfig", "/.config/gh", "/.config/git"} {
		if !seenTargets[target] {
			t.Errorf("expected ConfigMount target %q, got %v", target, seenTargets)
		}
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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

func TestRunBatch_ReplacesDeadContainerBeforeNextRun(t *testing.T) {
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
	runnables := &recoveryRunnableFactory{
		issue1: AgentRunResult{IssueNumber: 1, Status: "success"},
		issue2: AgentRunResult{IssueNumber: 2, Status: "success"},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}, Sandbox: "docker", Parallel: 1, ContainerCapacity: 1, ContainerCapacitySet: true, MaxContainers: 0, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if atomic.LoadUint32(&first.dead) == 0 {
		t.Fatal("expected the first container to be marked dead")
	}
	if containerAlive(first) {
		t.Fatal("expected the first container live check to fail")
	}
	if atomic.LoadUint32(&first.aliveChecks) == 0 {
		t.Fatal("expected the pool to probe the first container")
	}
	if runnables.containerByIssue[2] == "" {
		t.Fatal("expected issue 2 to run")
	}
	if runnables.containerByIssue[1] == "" {
		t.Fatal("expected issue 1 to run")
	}
}

func TestRunBatch_MaxContainersAutoDoesNotOverprovisionWhileContainerStarts(t *testing.T) {
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
	// Keep the first container start in flight long enough for a second Acquire call.
	starter := &fakeContainerStarter{startDelay: 200 * time.Millisecond, containers: []sandbox.Container{first, second}}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	runnables := &trackingRunnableFactory{results: map[int]AgentRunResult{
		1: {IssueNumber: 1, Status: "success"},
		2: {IssueNumber: 2, Status: "success"},
	}}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}, Sandbox: "docker", Parallel: 2, ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 0, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 1 {
		t.Fatalf("expected auto mode to start 1 container, got %d", starter.startCount)
	}
	if runnables.containerByIssue[1] != runnables.containerByIssue[2] {
		t.Fatalf("expected concurrent runs to share the same started container, got %q and %q", runnables.containerByIssue[1], runnables.containerByIssue[2])
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
			3: {Number: 3, Title: "Three", State: "closed"},
			4: {Number: 4, Title: "Four"},
		},
		fetchRelease: map[int]<-chan struct{}{},
	}
	releaseIssue3Fetch := make(chan struct{})
	client.fetchRelease[3] = releaseIssue3Fetch
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
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
	close(releaseIssue3Fetch)
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

func TestRunBatch_QueuesEligibleRunsWhenAllContainerSlotsAreOccupied(t *testing.T) {
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

	release1 := make(chan struct{})
	release2 := make(chan struct{})
	started1 := make(chan struct{})
	started2 := make(chan struct{})
	runnables := &trackingRunnableFactory{runnables: map[int]Runnable{
		1: &controlledRunnable{result: AgentRunResult{IssueNumber: 1, Status: "success"}, started: started1, release: release1},
		2: &controlledRunnable{result: AgentRunResult{IssueNumber: 2, Status: "success"}, started: started2, release: release2},
	}}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}, Sandbox: "docker", Parallel: 2, ContainerCapacity: 1, ContainerCapacitySet: true, MaxContainers: 1, MaxContainersSet: true})
		errCh <- err
	}()

	select {
	case <-started1:
		assertNoSignal(t, started2, "expected issue 2 to stay queued while the only container slot is occupied")
	case <-started2:
		t.Fatal("expected issue 1 to start first when only one container slot is available")
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected one issue to start")
	}

	close(release1)
	waitForSignal(t, started2, "expected issue 2 to start after the container slot is released")
	close(release2)

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunBatch_PreservesStartOrderWhenStartCapacityIsOne(t *testing.T) {
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

	releases := map[int]chan struct{}{
		1: make(chan struct{}),
		2: make(chan struct{}),
		3: make(chan struct{}),
		4: make(chan struct{}),
	}
	started := map[int]chan struct{}{
		1: make(chan struct{}),
		2: make(chan struct{}),
		3: make(chan struct{}),
		4: make(chan struct{}),
	}
	runnables := &trackingRunnableFactory{runnables: map[int]Runnable{
		1: &controlledRunnable{result: AgentRunResult{IssueNumber: 1, Status: "success"}, started: started[1], release: releases[1]},
		2: &controlledRunnable{result: AgentRunResult{IssueNumber: 2, Status: "success"}, started: started[2], release: releases[2]},
		3: &controlledRunnable{result: AgentRunResult{IssueNumber: 3, Status: "success"}, started: started[3], release: releases[3]},
		4: &controlledRunnable{result: AgentRunResult{IssueNumber: 4, Status: "success"}, started: started[4], release: releases[4]},
	}}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{
			Issues:               []int{1, 2, 3, 4},
			Sandbox:              "docker",
			Parallel:             4,
			ContainerCapacity:    1,
			ContainerCapacitySet: true,
			MaxContainers:        1,
			MaxContainersSet:     true,
		})
		errCh <- err
	}()

	// When effective start capacity is 1, the input order must be preserved.
	waitForSignal(t, started[1], "expected issue 1 to start first")
	assertNoSignal(t, started[2], "expected issue 2 to stay queued behind issue 1")
	assertNoSignal(t, started[3], "expected issue 3 to stay queued behind issue 1")
	assertNoSignal(t, started[4], "expected issue 4 to stay queued behind issue 1")

	close(releases[1])
	waitForSignal(t, started[2], "expected issue 2 to start after issue 1 releases the slot")
	assertNoSignal(t, started[3], "expected issue 3 to stay queued behind issue 2")
	assertNoSignal(t, started[4], "expected issue 4 to stay queued behind issue 2")

	close(releases[2])
	waitForSignal(t, started[3], "expected issue 3 to start after issue 2 releases the slot")
	assertNoSignal(t, started[4], "expected issue 4 to stay queued behind issue 3")

	close(releases[3])
	waitForSignal(t, started[4], "expected issue 4 to start after issue 3 releases the slot")
	close(releases[4])

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunBatch_StartFailureWakesQueuedWaiters(t *testing.T) {
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
	starter := &fakeContainerStarter{startDelay: 50 * time.Millisecond, err: errors.New("start failed")}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 1, Status: "success"}, {IssueNumber: 2, Status: "success"}, {IssueNumber: 3, Status: "success"}}}
	o.sandboxFactory = &trackingSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3}, Sandbox: "docker", Parallel: 3, ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 1, MaxContainersSet: true})
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected start failure to be returned")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected queued waiters to wake up after container start failure")
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}, Sandbox: "docker", Parallel: 2, ContainerCapacity: 1, ContainerCapacitySet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 2 {
		t.Fatalf("expected 2 containers to start (containerCapacity=1, parallel=2), got %d", starter.startCount)
	}
}

func TestContainerPool_RespectsCapacityAndMaxUnderContention(t *testing.T) {
	starter := &fakeContainerStarter{startDelay: 5 * time.Millisecond}
	pool := newContainerPool(starter, "img", ".", sandbox.StartOptions{}, 3, 2)

	const (
		capacity     = 3
		maxContainer = 2
		acquirers    = 100
		holdFor      = 2 * time.Millisecond
	)

	var (
		observedMaxContainers atomic.Int32
		observedMaxActive     atomic.Int32
		violation             atomic.Value
	)
	violation.Store(false)

	stopSampler := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			select {
			case <-stopSampler:
				return
			default:
			}
			pool.mu.Lock()
			n := int32(len(pool.shared))
			if n > observedMaxContainers.Load() {
				observedMaxContainers.Store(n)
			}
			if n > int32(maxContainer) {
				violation.Store(true)
				t.Errorf("maxContainers=%d violated: pool has %d containers", maxContainer, n)
			}
			for _, entry := range pool.shared {
				if entry.active > int(capacity) {
					violation.Store(true)
					t.Errorf("capacity=%d violated: entry has active=%d", capacity, entry.active)
				}
				if entry.active > int(observedMaxActive.Load()) {
					observedMaxActive.Store(int32(entry.active))
				}
			}
			pool.mu.Unlock()
			runtime.Gosched()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(acquirers)
	for i := 0; i < acquirers; i++ {
		go func() {
			defer wg.Done()
			lease, err := pool.Acquire()
			if err != nil {
				t.Errorf("acquire failed: %v", err)
				return
			}
			time.Sleep(holdFor)
			lease.Release()
		}()
	}
	wg.Wait()

	close(stopSampler)
	<-samplerDone

	if violation.Load().(bool) {
		t.FailNow()
	}
	if got := observedMaxContainers.Load(); got > int32(maxContainer) {
		t.Fatalf("maxContainers=%d violated: sampler observed %d containers", maxContainer, got)
	}
	if got := observedMaxActive.Load(); got > int32(capacity) {
		t.Fatalf("capacity=%d violated: sampler observed active=%d", capacity, got)
	}
	if starter.startCount == 0 {
		t.Fatal("expected the pool to start at least one container")
	}
	if starter.startCount > maxContainer {
		t.Fatalf("pool started %d containers, expected at most %d", starter.startCount, maxContainer)
	}
}

func TestRunBatch_PreservesStartOrderWhenSkippedDependency(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Root", State: "closed"},
			100: {Number: 100, Title: "Dependent"},
			200: {Number: 200, Title: "Lone"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	releaseRoot := make(chan struct{})
	releaseLone := make(chan struct{})
	startedRoot := make(chan struct{})
	startedLone := make(chan struct{})
	runnables := &trackingRunnableFactory{runnables: map[int]Runnable{
		100: &controlledRunnable{result: AgentRunResult{IssueNumber: 100, Status: "blocked"}, started: make(chan struct{}), release: make(chan struct{})},
		200: &controlledRunnable{result: AgentRunResult{IssueNumber: 200, Status: "success"}, started: startedLone, release: releaseLone},
	}}
	runnables.runnables[42] = &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "failure"}, started: startedRoot, release: releaseRoot}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{
			Issues:               []int{42, 100, 200},
			Dependencies:         map[int][]int{100: {42}},
			Sandbox:              "docker",
			Parallel:             4,
			ContainerCapacity:    1,
			ContainerCapacitySet: true,
			MaxContainers:        1,
			MaxContainersSet:     true,
		})
		errCh <- err
	}()

	waitForSignal(t, startedRoot, "expected issue 42 to start")
	close(releaseRoot)
	waitForSignal(t, startedLone, "expected issue 200 to start after issue 100 was skipped due to dependency failure")
	close(releaseLone)

	if err := <-errCh; err == nil {
		t.Fatal("expected batch to surface the failure from issue 42")
	}
}

func TestRunBatch_PreservesStartOrderWhenSkippedDependentHasHigherTurn(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Root", State: "closed"},
			200: {Number: 200, Title: "Independent"},
			100: {Number: 100, Title: "Dependent"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	releaseRoot := make(chan struct{})
	releaseIndependent := make(chan struct{})
	startedRoot := make(chan struct{})
	startedIndependent := make(chan struct{})
	runnables := &trackingRunnableFactory{runnables: map[int]Runnable{
		200: &controlledRunnable{result: AgentRunResult{IssueNumber: 200, Status: "success"}, started: startedIndependent, release: releaseIndependent},
		100: &controlledRunnable{result: AgentRunResult{IssueNumber: 100, Status: "blocked"}, started: make(chan struct{}), release: make(chan struct{})},
	}}
	runnables.runnables[42] = &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "failure"}, started: startedRoot, release: releaseRoot}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{
			Issues:               []int{42, 200, 100},
			Dependencies:         map[int][]int{100: {42}},
			Sandbox:              "docker",
			Parallel:             4,
			ContainerCapacity:    1,
			ContainerCapacitySet: true,
			MaxContainers:        1,
			MaxContainersSet:     true,
		})
		errCh <- err
	}()

	waitForSignal(t, startedRoot, "expected issue 42 to start")
	close(releaseRoot)
	waitForSignal(t, startedIndependent, "expected issue 200 to be served before the skipped dependent 100 jumps the queue")
	close(releaseIndependent)

	if err := <-errCh; err == nil {
		t.Fatal("expected batch to surface the failure from issue 42")
	}
}

func TestRunBatch_PreservesStartOrderWhenOutOfOrderSkips(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Root", State: "closed"},
			300: {Number: 300, Title: "Skipped high turn", State: "closed"},
			200: {Number: 200, Title: "Skipped low turn", State: "closed"},
			400: {Number: 400, Title: "Independent"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	releaseRoot := make(chan struct{})
	releaseIndependent := make(chan struct{})
	startedRoot := make(chan struct{})
	startedIndependent := make(chan struct{})
	runnables := &trackingRunnableFactory{runnables: map[int]Runnable{
		300: &controlledRunnable{result: AgentRunResult{IssueNumber: 300, Status: "blocked"}, started: make(chan struct{}), release: make(chan struct{})},
		200: &controlledRunnable{result: AgentRunResult{IssueNumber: 200, Status: "blocked"}, started: make(chan struct{}), release: make(chan struct{})},
		400: &controlledRunnable{result: AgentRunResult{IssueNumber: 400, Status: "success"}, started: startedIndependent, release: releaseIndependent},
	}}
	runnables.runnables[42] = &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "failure"}, started: startedRoot, release: releaseRoot}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{
			Issues:               []int{42, 300, 200, 400},
			Dependencies:         map[int][]int{300: {42}, 200: {42}},
			Sandbox:              "docker",
			Parallel:             4,
			ContainerCapacity:    1,
			ContainerCapacitySet: true,
			MaxContainers:        1,
			MaxContainersSet:     true,
		})
		errCh <- err
	}()

	waitForSignal(t, startedRoot, "expected issue 42 to start")
	close(releaseRoot)
	waitForSignal(t, startedIndependent, "expected issue 400 to be served even when skipped dependents return out of turn order")
	close(releaseIndependent)

	if err := <-errCh; err == nil {
		t.Fatal("expected batch to surface the failure from issue 42")
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

	release1 := make(chan struct{})
	release2 := make(chan struct{})
	release3 := make(chan struct{})
	started1 := make(chan struct{})
	started2 := make(chan struct{})
	started3 := make(chan struct{})
	runnables := &trackingRunnableFactory{runnables: map[int]Runnable{
		1: &controlledRunnable{result: AgentRunResult{IssueNumber: 1, Status: "success"}, started: started1, release: release1},
		2: &controlledRunnable{result: AgentRunResult{IssueNumber: 2, Status: "success"}, started: started2, release: release2},
		3: &controlledRunnable{result: AgentRunResult{IssueNumber: 3, Status: "success"}, started: started3, release: release3},
	}}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &trackingSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3}, Sandbox: "docker", Parallel: 3, ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 1, MaxContainersSet: true})
		errCh <- err
	}()

	started := map[int]chan struct{}{1: started1, 2: started2, 3: started3}
	released := map[int]chan struct{}{1: release1, 2: release2, 3: release3}
	startedIssues := make([]int, 0, 2)
	for len(startedIssues) < 2 {
		select {
		case <-started1:
			if !containsInt(startedIssues, 1) {
				startedIssues = append(startedIssues, 1)
			}
		case <-started2:
			if !containsInt(startedIssues, 2) {
				startedIssues = append(startedIssues, 2)
			}
		case <-started3:
			if !containsInt(startedIssues, 3) {
				startedIssues = append(startedIssues, 3)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("expected two issues to start")
		}
	}

	queuedIssue := 0
	for _, issue := range []int{1, 2, 3} {
		if !containsInt(startedIssues, issue) {
			queuedIssue = issue
			break
		}
	}
	assertNoSignal(t, started[queuedIssue], "expected the third issue to stay queued while both container slots are occupied")

	close(released[startedIssues[0]])
	waitForSignal(t, started[queuedIssue], "expected the queued issue to start after a container slot is released")

	for _, issue := range []int{1, 2, 3} {
		if issue != startedIssues[0] {
			close(released[issue])
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4}, Sandbox: "docker", Parallel: 4, ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 0, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 2 {
		t.Fatalf("expected 2 containers to start (4 runs at capacity=2), got %d", starter.startCount)
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
	store := &fakeConfigStore{config: &config.Config{Agent: "test-agent", ContainerCapacity: 1, MaxContainers: 0, Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}

	o := NewOrchestrator(client, &noopRenderer{}, store, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2}, Sandbox: "docker", Parallel: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 2 {
		t.Fatalf("expected config container_capacity=1 to start 2 containers (parallel=2, capacity=1), got %d", starter.startCount)
	}
}

func TestEffectiveParallel_AutoContainerMode(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Chdir(dir)
	initGitRepo(t, dir)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = &fakeContainerRuntimeFactory{starter: &fakeContainerStarter{}}
	o.runnableFactory = factory
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4}, Sandbox: "docker", Parallel: 4, ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 0, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if factory.max > 4 {
		t.Errorf("expected max concurrent runs <= 4 (parallel=4 in auto mode, container pool manages capacity), got %d", factory.max)
	}
}

func TestEffectiveParallel_ExplicitMaxContainers(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Chdir(dir)
	initGitRepo(t, dir)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = &fakeContainerRuntimeFactory{starter: &fakeContainerStarter{}}
	o.runnableFactory = factory
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4}, Sandbox: "docker", Parallel: 4, ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 2, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if factory.max > 4 {
		t.Errorf("expected max concurrent runs <= 4 (totalSlots = 2*2), got %d", factory.max)
	}
}

func TestEffectiveParallel_UnlimitedParallel(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Chdir(dir)
	initGitRepo(t, dir)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "A"},
			2: {Number: 2, Title: "B"},
			3: {Number: 3, Title: "C"},
			4: {Number: 4, Title: "D"},
		},
	}

	release := make(chan struct{})
	started := make([]chan struct{}, 4)
	runnables := make(map[int]Runnable, 4)
	for i := 1; i <= 4; i++ {
		started[i-1] = make(chan struct{})
		runnables[i] = &controlledRunnable{
			result:  AgentRunResult{IssueNumber: i, Status: "success"},
			started: started[i-1],
			release: release,
		}
	}
	factory := &controlledRunnableFactory{runnables: runnables}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = &fakeContainerRuntimeFactory{starter: &fakeContainerStarter{}}
	o.runnableFactory = factory
	o.sandboxFactory = &freshSandboxFactory{}

	errCh := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4}, Sandbox: "docker", Parallel: 0, ContainerCapacity: 4, ContainerCapacitySet: true, MaxContainers: 0, MaxContainersSet: true})
		errCh <- err
	}()

	for i, ch := range started {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for run %d to start", i+1)
		}
	}

	if len(factory.created) != 4 {
		t.Fatalf("expected 4 created runnables (parallel=0 means unlimited), got %d", len(factory.created))
	}

	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEffectiveParallel_NoRegressionWhenParallelEqualsCapacity(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Chdir(dir)
	initGitRepo(t, dir)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = &fakeContainerRuntimeFactory{starter: &fakeContainerStarter{}}
	o.runnableFactory = factory
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4}, Sandbox: "docker", Parallel: 4, ContainerCapacity: 4, ContainerCapacitySet: true, MaxContainers: 0, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if factory.max > 4 {
		t.Errorf("expected max concurrent runs <= 4 (parallel equals capacity, no cap needed), got %d", factory.max)
	}
}

func TestRunBatch_ContainerCapacityZeroInConfigMeansUnlimited(t *testing.T) {
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
			5: {Number: 5, Title: "Five"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	runnables := &fakeRunnableFactory{
		results: []AgentRunResult{
			{IssueNumber: 1, Status: "success"},
			{IssueNumber: 2, Status: "success"},
			{IssueNumber: 3, Status: "success"},
			{IssueNumber: 4, Status: "success"},
			{IssueNumber: 5, Status: "success"},
		},
		delays: []time.Duration{
			50 * time.Millisecond,
			50 * time.Millisecond,
			50 * time.Millisecond,
			50 * time.Millisecond,
			50 * time.Millisecond,
		},
	}
	store := &fakeConfigStore{config: &config.Config{Agent: "test-agent", ContainerCapacity: 0, MaxContainers: 0, Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}

	o := NewOrchestrator(client, &noopRenderer{}, store, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4, 5}, Sandbox: "docker", Parallel: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 1 {
		t.Fatalf("expected config container_capacity=0 to mean unlimited and start 1 container, got %d", starter.startCount)
	}
}

func TestRunBatch_ContainerCapacityZeroRequestMeansUnlimited(t *testing.T) {
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
			5: {Number: 5, Title: "Five"},
		},
	}
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	runnables := &fakeRunnableFactory{
		results: []AgentRunResult{
			{IssueNumber: 1, Status: "success"},
			{IssueNumber: 2, Status: "success"},
			{IssueNumber: 3, Status: "success"},
			{IssueNumber: 4, Status: "success"},
			{IssueNumber: 5, Status: "success"},
		},
		delays: []time.Duration{
			50 * time.Millisecond,
			50 * time.Millisecond,
			50 * time.Millisecond,
			50 * time.Millisecond,
			50 * time.Millisecond,
		},
	}
	store := &fakeConfigStore{config: &config.Config{Agent: "test-agent", ContainerCapacity: 2, MaxContainers: 0, Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}

	o := NewOrchestrator(client, &noopRenderer{}, store, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{1, 2, 3, 4, 5}, Sandbox: "docker", Parallel: 5, ContainerCapacity: 0, ContainerCapacitySet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 1 {
		t.Fatalf("expected request container_capacity=0 to mean unlimited and start 1 container, got %d", starter.startCount)
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.runnableFactory = runnables
	o.sandboxFactory = &freshSandboxFactory{}

	// Worktree mode should ignore container-only settings, even when those values
	// would be invalid for a container-backed batch.
	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "worktree", ContainerCapacity: 0, ContainerCapacitySet: true, MaxContainers: -1, MaxContainersSet: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if starter.startCount != 0 {
		t.Fatalf("expected no containers to start in worktree mode, got %d", starter.startCount)
	}
}

func TestResolveSandboxExecutionPolicy_WorktreeModeDoesNotBuildContainerImage(t *testing.T) {
	starter := &fakeContainerStarter{}
	factory := &fakeContainerRuntimeFactory{starter: starter}
	o := &Orchestrator{containerRuntimeFactory: factory}

	policy, err := o.resolveSandboxExecutionPolicy(&config.Config{DefaultAgent: "test-agent", Agent: "test-agent", BuildTools: "generic"}, config.Agent{Command: "true"}, Request{}, "worktree")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if policy == nil {
		t.Fatal("expected policy")
	}
	if policy.mode != "worktree" {
		t.Fatalf("expected worktree mode, got %q", policy.mode)
	}
	if policy.containerAlloc != nil {
		t.Fatal("expected no container allocator in worktree mode")
	}
	if starter.buildImageCount != 0 {
		t.Fatalf("expected no container image build in worktree mode, got %d", starter.buildImageCount)
	}
}

func TestRunBatch_UsesNonInteractiveRunPath(t *testing.T) {
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(factory.created) != 1 {
		t.Fatalf("expected 1 runnable, got %d", len(factory.created))
	}
	// The run succeeded through the normal execution path.
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"opencode": {Name: "opencode", Command: "opencode", KeychainAuth: true}}}}, nil)

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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
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

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

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
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "docker"})
	if err == nil {
		t.Fatal("expected error when docker is unavailable")
	}
}

func TestRunBatch_ReturnsErrorWhenBuildImageFails(t *testing.T) {
	if _, err := sandbox.ResolveRuntime("docker"); err != nil {
		t.Skip("docker unavailable")
	}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	starter := &fakeContainerStarter{buildImageErr: errors.New("build failed")}
	factory := &fakeContainerRuntimeFactory{starter: starter}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.containerRuntimeFactory = factory
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "docker"})
	if err == nil {
		t.Fatal("expected error when build image fails")
	}
	if !strings.Contains(err.Error(), "build container image") {
		t.Errorf("expected error to mention build container image, got: %v", err)
	}
	if starter.buildImageCount != 1 {
		t.Errorf("expected BuildImage to be called once, got %d", starter.buildImageCount)
	}
}

func TestBuildStartOptions_IncludesSharedSkillsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	agentCfg := config.BuiltInAgentPresets["opencode"].Agent("opencode")
	opts, err := buildStartOptions(agentCfg)
	if err != nil {
		t.Fatalf("build start options: %v", err)
	}

	want := filepath.Join(home, ".agents")
	for _, dir := range opts.AgentConfigDirs {
		if dir == want {
			return
		}
	}
	t.Fatalf("expected shared skills dir %q in agent config dirs, got %v", want, opts.AgentConfigDirs)
}

func TestRunBatch_UsesDotGitconfigIdentityOverRepoLocalConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Alice\n\temail = alice@example.com\n"), 0644); err != nil {
		t.Fatalf("write .gitconfig: %v", err)
	}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	store := &fakeConfigStore{
		config: &config.Config{
			DefaultAgent: "test-agent",
			Agent:        "test-agent",
			Sandbox:      "worktree",
			WorktreeDir:  ".sandman/worktrees",
			Git: config.GitConfig{
				BaseBranch: "main",
			},
			AgentProviders: map[string]config.Agent{
				"test-agent": {Command: "touch test-file.txt && git add test-file.txt && git commit -m \"test commit\""},
			},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, store, nil)
	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	assertGitCommitAuthor(t, worktreePath, "Alice <alice@example.com>")
	assertLocalGitIdentity(t, worktreePath, "Test", "test@test.com")
}

func TestRunBatch_UsesXDGGitIdentityWhenDotGitconfigLacksIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\teditor = true\n"), 0644); err != nil {
		t.Fatalf("write .gitconfig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0755); err != nil {
		t.Fatalf("mkdir xdg git dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "git", "config"), []byte("[user]\n\tname = XDG User\n\temail = xdg@example.com\n"), 0644); err != nil {
		t.Fatalf("write xdg git config: %v", err)
	}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	store := &fakeConfigStore{
		config: &config.Config{
			DefaultAgent: "test-agent",
			Agent:        "test-agent",
			Sandbox:      "worktree",
			WorktreeDir:  ".sandman/worktrees",
			Git: config.GitConfig{
				BaseBranch: "main",
			},
			AgentProviders: map[string]config.Agent{
				"test-agent": {Command: "touch test-file.txt && git add test-file.txt && git commit -m \"test commit\""},
			},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, store, nil)
	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	assertGitCommitAuthor(t, worktreePath, "XDG User <xdg@example.com>")
	assertLocalGitIdentity(t, worktreePath, "Test", "test@test.com")
}

func TestRunBatch_FallsBackToRepoLocalGitIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	store := &fakeConfigStore{
		config: &config.Config{
			DefaultAgent: "test-agent",
			Agent:        "test-agent",
			Sandbox:      "worktree",
			WorktreeDir:  ".sandman/worktrees",
			Git: config.GitConfig{
				BaseBranch: "main",
			},
			AgentProviders: map[string]config.Agent{
				"test-agent": {Command: "touch test-file.txt && git add test-file.txt && git commit -m \"test commit\""},
			},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, store, nil)
	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	worktreePath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug")
	assertGitCommitAuthor(t, worktreePath, "Test <test@test.com>")
	assertLocalGitIdentity(t, worktreePath, "Test", "test@test.com")
}

func TestRunBatch_FailsWhenNoGitIdentityResolved(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if out, err := exec.Command("git", "config", "--unset", "user.name").CombinedOutput(); err != nil {
		t.Fatalf("unset repo user.name: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "config", "--unset", "user.email").CombinedOutput(); err != nil {
		t.Fatalf("unset repo user.email: %v: %s", err, out)
	}

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
	}
	store := &fakeConfigStore{
		config: &config.Config{
			DefaultAgent: "test-agent",
			Agent:        "test-agent",
			Sandbox:      "worktree",
			WorktreeDir:  ".sandman/worktrees",
			Git: config.GitConfig{
				BaseBranch: "main",
			},
			AgentProviders: map[string]config.Agent{
				"test-agent": {Command: "touch test-file.txt && git add test-file.txt && git commit -m \"test commit\""},
			},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, store, nil)
	var errBuf bytes.Buffer
	o.errorLog = &errBuf
	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err == nil {
		t.Fatal("expected git identity resolution error")
	}
	if !strings.Contains(errBuf.String(), "resolve git identity") {
		t.Fatalf("expected git identity resolution error in stderr, got err=%v stderr=%q", err, errBuf.String())
	}
}

func assertGitCommitAuthor(t *testing.T, worktreePath, want string) {
	t.Helper()
	cmd := exec.Command("git", "log", "--format=%an <%ae>", "-1")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log author: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("commit author: got %q, want %q", got, want)
	}
}

func assertLocalGitIdentity(t *testing.T, worktreePath, wantName, wantEmail string) {
	t.Helper()
	cmd := exec.Command("git", "config", "--local", "user.name")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config --local user.name: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != wantName {
		t.Fatalf("local user.name: got %q, want %q", got, wantName)
	}

	cmd = exec.Command("git", "config", "--local", "user.email")
	cmd.Dir = worktreePath
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("git config --local user.email: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != wantEmail {
		t.Fatalf("local user.email: got %q, want %q", got, wantEmail)
	}
}

func TestRunBatch_ContainerCapacityOneReturnsErrorWhenDockerUnavailable(t *testing.T) {
	t.Setenv("PATH", "")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "docker", ContainerCapacity: 1, ContainerCapacitySet: true})
	if err == nil {
		t.Fatal("expected error when docker is unavailable")
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Fix bug", "fix-bug"},
		{"Hello World 123", "hello-world-123"},
		{"  Special!@#Chars  ", "specialchars"},
		{"Already-slugified", "already-slugified"},
		{"UPPERCASE", "uppercase"},
		{"", ""},
	}
	for _, tt := range tests {
		got := Slugify(tt.input)
		if got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBranchName(t *testing.T) {
	tests := []struct {
		number int
		title  string
		want   string
	}{
		{42, "Fix bug", "sandman/42-fix-bug"},
		{1, "Hello World", "sandman/1-hello-world"},
		{100, "UPPERCASE title", "sandman/100-uppercase-title"},
	}
	for _, tt := range tests {
		got := BranchName(tt.number, tt.title)
		if got != tt.want {
			t.Errorf("BranchName(%d, %q) = %q, want %q", tt.number, tt.title, got, tt.want)
		}
	}
}

func TestClearIssueArtifacts_RemovesWorktree(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := "sandman/42-fix-bug"
	worktreeDir := filepath.Join(".sandman", "worktrees")

	cmd := exec.Command("git", "worktree", "add", "-b", branch, filepath.Join(worktreeDir, branch), "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create worktree: %v: %s", err, out)
	}

	wtPath := filepath.Join(worktreeDir, branch)
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree should exist: %v", err)
	}

	logDir := filepath.Join(".sandman", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	logPath := filepath.Join(logDir, "42.log")
	if err := os.WriteFile(logPath, []byte("test log"), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	// Create events
	el := &spyEventLog{
		events: []events.Event{
			{Type: "run.started", RunID: "run-42-1", Issue: 42, IssueRef: issueRef(42)},
			{Type: "run.finished", RunID: "run-42-1", Issue: 42, IssueRef: issueRef(42)},
		},
	}

	ClearIssueArtifacts(42, branch, worktreeDir, logDir, el, io.Discard)

	// Worktree removed
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("expected worktree to be removed, got %v", err)
	}

	// Log removed
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("expected log to be removed, got %v", err)
	}

	// Branch removed
	revCmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
	if out, err := revCmd.CombinedOutput(); err == nil {
		t.Errorf("expected branch to be removed, rev-parse succeeded: %s", out)
	}

	// Events removed
	events, err := el.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, e := range events {
		if e.Issue == 42 || (e.IssueRef != nil && *e.IssueRef == 42) {
			t.Errorf("expected no events for issue 42, found: %+v", e)
		}
	}
}

func TestClearIssueArtifacts_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	el := &spyEventLog{}
	ClearIssueArtifacts(42, "sandman/42-nonexistent", ".sandman/worktrees", ".sandman/logs", el, io.Discard)
}

func TestClearIssueArtifacts_OnlyRemovesTargetIssue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	// Create two branches and worktrees
	for _, n := range []int{42, 99} {
		branch := fmt.Sprintf("sandman/%d-fix-bug", n)
		worktreeDir := filepath.Join(".sandman", "worktrees")
		cmd := exec.Command("git", "worktree", "add", "-b", branch, filepath.Join(worktreeDir, branch), "main")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create worktree %d: %v: %s", n, err, out)
		}
	}

	el := &spyEventLog{
		events: []events.Event{
			{Type: "run.started", RunID: "run-42-1", Issue: 42, IssueRef: issueRef(42)},
			{Type: "run.finished", RunID: "run-42-1", Issue: 42, IssueRef: issueRef(42)},
			{Type: "run.started", RunID: "run-99-1", Issue: 99, IssueRef: issueRef(99)},
			{Type: "run.finished", RunID: "run-99-1", Issue: 99, IssueRef: issueRef(99)},
		},
	}

	ClearIssueArtifacts(42, "sandman/42-fix-bug", ".sandman/worktrees", ".sandman/logs", el, io.Discard)

	// Issue 99 branch should still exist
	revCmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/sandman/99-fix-bug")
	if out, err := revCmd.CombinedOutput(); err != nil {
		t.Errorf("expected issue 99 branch to remain, err: %v: %s", err, out)
	}

	// Issue 99 events should still exist
	events, err := el.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var found99 bool
	for _, e := range events {
		if e.Issue == 99 || (e.IssueRef != nil && *e.IssueRef == 99) {
			found99 = true
		}
		if e.Issue == 42 || (e.IssueRef != nil && *e.IssueRef == 42) {
			t.Errorf("expected no events for issue 42, found: %+v", e)
		}
	}
	if !found99 {
		t.Error("expected events for issue 99 to remain")
	}
}

func TestOrchestrator_EmitsRunQueuedEventWhenBlocked(t *testing.T) {
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
	releaseBlocker := make(chan struct{})

	spyLog := &spyEventLog{}
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 42, Status: "success"},
				started: blockerStarted,
				release: releaseBlocker,
			},
			100: &controlledRunnable{
				result: AgentRunResult{IssueNumber: 100, Status: "success"},
			},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
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

	var queuedEvent *events.Event
	for i := range spyLog.events {
		if spyLog.events[i].Type == "run.queued" && spyLog.events[i].Issue == 100 {
			queuedEvent = &spyLog.events[i]
			break
		}
	}
	if queuedEvent == nil {
		t.Fatal("expected run.queued event for blocked issue 100")
	}

	var startedEvent bool
	for i := range spyLog.events {
		if spyLog.events[i].Type == "run.started" && spyLog.events[i].Issue == 100 {
			startedEvent = true
			break
		}
	}
	if startedEvent {
		t.Fatal("did not expect run.started for issue 100 (it should remain queued)")
	}

	close(releaseBlocker)
	waitForSignal(t, done, "expected batch to complete")
}

func TestOrchestrator_RunQueuedOnlyForWaitingIssues(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:   {Number: 1, Title: "Unblocked"},
			42:  {Number: 42, Title: "Blocker"},
			100: {Number: 100, Title: "Dependent", BlockedBy: []int{42}},
		},
	}

	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})

	spyLog := &spyEventLog{}
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 42, Status: "success"},
				started: blockerStarted,
				release: releaseBlocker,
			},
			1:   &controlledRunnable{result: AgentRunResult{IssueNumber: 1, Status: "success"}},
			100: &controlledRunnable{result: AgentRunResult{IssueNumber: 100, Status: "success"}},
		},
	}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.runnableFactory = factory

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{
			Issues:       []int{1, 42, 100},
			Dependencies: map[int][]int{100: {42}},
			Parallel:     3,
		})
	}()

	waitForSignal(t, blockerStarted, "expected blocker to start")

	var queuedEvent *events.Event
	for i := range spyLog.events {
		if spyLog.events[i].Type == "run.queued" && spyLog.events[i].Issue == 100 {
			queuedEvent = &spyLog.events[i]
			break
		}
	}
	if queuedEvent == nil {
		t.Fatal("expected run.queued event for blocked issue 100")
	}

	var unblockedQueued bool
	for i := range spyLog.events {
		if spyLog.events[i].Type == "run.queued" && spyLog.events[i].Issue == 1 {
			unblockedQueued = true
			break
		}
	}
	if unblockedQueued {
		t.Fatal("did not expect run.queued for unblocked issue 1")
	}

	close(releaseBlocker)
	waitForSignal(t, done, "expected batch to complete")
}
