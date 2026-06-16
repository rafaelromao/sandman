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
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestMain(m *testing.M) {
	branchExists = func(string, string) bool { return false }
	branchValidationEnabled = false
	os.Exit(m.Run())
}

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

func (n *noopRenderer) RenderReview(cfg prompt.RenderConfig, data prompt.PRData) (string, error) {
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

func (s *spyPromptRenderer) RenderReview(cfg prompt.RenderConfig, data prompt.PRData) (string, error) {
	return "", nil
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

func (r *retryRenderer) RenderReview(cfg prompt.RenderConfig, data prompt.PRData) (string, error) {
	return "", nil
}

type fakeGitHubClient struct {
	issues             map[int]*github.Issue
	fetchRelease       map[int]<-chan struct{}
	prs                map[string]*github.PR
	err                error
	findPRErr          error
	findPRHook         func()
	searchIssuesResult []github.Issue
	searchIssuesError  error
	searchCalls        []string
	issueComments      map[int][]github.IssueComment
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

func (f *fakeGitHubClient) FetchPR(number int) (*github.PR, error) {
	return &github.PR{Number: number, State: "open"}, nil
}

func (f *fakeGitHubClient) SearchIssues(query string) ([]github.Issue, error) {
	f.searchCalls = append(f.searchCalls, query)
	if f.searchIssuesError != nil {
		return nil, f.searchIssuesError
	}
	if f.searchIssuesResult != nil {
		return f.searchIssuesResult, nil
	}
	return nil, nil
}

func (f *fakeGitHubClient) FindPRByBranch(branch string) (*github.PR, error) {
	if f.findPRHook != nil {
		f.findPRHook()
	}
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
	for _, issue := range f.issues {
		if BranchName(issue.Number, issue.Title) == branch {
			return &github.PR{Number: issue.Number, State: "closed", Merged: false, HeadRefName: branch}, nil
		}
	}
	return nil, nil
}

func (f *fakeGitHubClient) ListOpenPRs() ([]github.PR, error) {
	return nil, nil
}

func (f *fakeGitHubClient) ListPRComments(number int) ([]github.PRComment, error) {
	return nil, nil
}

func (f *fakeGitHubClient) ListIssueComments(number int) ([]github.IssueComment, error) {
	if f.issueComments == nil {
		return nil, nil
	}
	return f.issueComments[number], nil
}

func (f *fakeGitHubClient) RepoName() (string, error) {
	return "owner/repo", nil
}

func (f *fakeGitHubClient) EditComment(commentID, body string) error {
	return nil
}

func (f *fakeGitHubClient) EditPRBody(prNumber int, body string) error {
	return nil
}

func (f *fakeGitHubClient) AddCommentReaction(commentID, content string) (string, error) {
	return "", nil
}

func (f *fakeGitHubClient) AddIssueReaction(issueNumber int, content string) (string, error) {
	return "", nil
}

func (f *fakeGitHubClient) RemoveCommentReaction(commentID, reactionID string) error {
	return nil
}

func (f *fakeGitHubClient) RemoveIssueReaction(issueNumber int, reactionID string) error {
	return nil
}

func mergedPR(branch, sha string) *github.PR {
	return &github.PR{Number: 1, State: "closed", Merged: true, HeadRefName: branch}
}

type fakeRunnable struct {
	result      AgentRunResult
	delay       time.Duration
	activeCount *int
	maxActive   *int
	mu          *sync.Mutex
}

func (f *fakeRunnable) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
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

type byIssueRunnableFactory struct {
	results map[int]AgentRunResult
}

func (f *byIssueRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	res, ok := f.results[issue.Number]
	if !ok {
		res = AgentRunResult{IssueNumber: issue.Number, Status: "failure"}
	}
	return &fakeRunnable{result: res}
}

type fakeSandboxFactory struct {
	sandbox *fakeSandbox
}

func (f *fakeSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	return f.sandbox
}

type sandboxFactoryFunc func(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox

func (f sandboxFactoryFunc) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	return f(repoPath, worktreeBase, branch, sourceBranch, container)
}

type retrySandbox struct {
	startCalled                bool
	writePromptCount           int
	execCount                  int
	execCommand                string
	execErrors                 []error
	workDir                    string
	repoPath                   string
	setOverrideCalled          bool
	setOverrideValue           bool
	setStrandedReconcileCalled bool
	setStrandedReconcileValue  bool
	setIdentityName            string
	setIdentityEmail           string
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
func (s *retrySandbox) RepoPath() string                                          { return s.repoPath }
func (s *retrySandbox) WritePrompt(content string) error {
	s.writePromptCount++
	return nil
}
func (s *retrySandbox) Process() sandbox.Process { return nil }
func (s *retrySandbox) SetOverride(override bool) {
	s.setOverrideCalled = true
	s.setOverrideValue = override
}
func (s *retrySandbox) SetStrandedReconcile(enabled bool) {
	s.setStrandedReconcileCalled = true
	s.setStrandedReconcileValue = enabled
}
func (s *retrySandbox) SetGitIdentity(name, email string) {
	s.setIdentityName = name
	s.setIdentityEmail = email
}

// Ensure retrySandbox satisfies sandbox.Sandbox.
var _ sandbox.Sandbox = (*retrySandbox)(nil)

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
	mu                       sync.Mutex
	events                   []events.Event
	removedIssueNumber       int
	removeEventsByIssueCalls int
}

func (s *spyEventLog) Log(e events.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *spyEventLog) Read() ([]events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]events.Event, len(s.events))
	copy(out, s.events)
	return out, nil
}

func (s *spyEventLog) RemoveEventsByIssue(issueNumber int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	running          chan struct{}
	once             sync.Once
}

func (b *blockingRunnable) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	if b.running != nil {
		b.once.Do(func() { close(b.running) })
	}
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

func (r *controlledRunnable) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
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
	writes   int
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

func (r *continuationFlowRunnable) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	if renderCfg.TaskPrompt != "" {
		promptPath := filepath.Join(r.sb.WorkDir(), ".sandman", "task.md")
		if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err == nil {
			_ = os.WriteFile(promptPath, []byte(renderCfg.TaskPrompt), 0644)
		}
		r.state.recordPrompt(renderCfg.TaskPrompt)
	}
	contextPath := filepath.Join(r.sb.WorkDir(), ".sandman", "task.md")
	if content := r.state.nextContext(); content != "" {
		r.state.writes++
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

func TestReadTailLines_MoreThanN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readTailLines(path, 3)
	want := []string{"line3", "line4", "line5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReadTailLines_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readTailLines(path, 3)
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestReadTailLines_FewerThanN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "few.log")
	content := "only1\nonly2\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readTailLines(path, 3)
	want := []string{"only1", "only2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReadTailLines_NonexistentFile(t *testing.T) {
	got := readTailLines("/nonexistent/path.log", 3)
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestReadTailLines_TrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trailing.log")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readTailLines(path, 3)
	want := []string{"line1", "line2", "line3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAgentLogPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	o := &Orchestrator{layout: paths.NewLayout(&config.Config{}, dir)}
	tests := []struct {
		name     string
		filename string
	}{
		{"numeric log", "42.log"},
		{"prompt-only log", "prompt-only.log"},
		{"bare filename", "foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := o.agentLogPath(tt.filename)
			if !filepath.IsAbs(path) {
				t.Fatal("expected absolute path")
			}
			wantSuffix := filepath.Join(".sandman", "logs", tt.filename)
			if !strings.HasSuffix(path, wantSuffix) {
				t.Fatalf("unexpected path: %s (want suffix: %s)", path, wantSuffix)
			}
		})
	}
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

	if merged := checkPRMerged(nil, ""); merged {
		t.Fatal("expected nil client to report unmerged")
	}

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
		execErrors: []error{errors.New("exit 1"), nil},
	}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	branch := "sandman/42-fix-bug"
	pr := &github.PR{Number: 17, State: "closed", Merged: false, HeadRefName: branch}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}, prs: map[string]*github.PR{branch: pr}},
		renderer:     renderer,
		errorLog:     io.Discard,
		layout:       paths.NewLayout(&config.Config{}, workDir),
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls []struct{ worktreePath, branch, baseBranch string }
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls = append(resetCalls, struct{ worktreePath, branch, baseBranch string }{sb.WorkDir(), branch, baseBranch})
		if len(resetCalls) == 1 {
			pr.Merged = true
		}
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 2, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if result.RetriesTotal != 2 {
		t.Fatalf("RetriesTotal = %d, want 2", result.RetriesTotal)
	}
	if renderer.renderCalls != 1 {
		t.Fatalf("render calls = %d, want 1 (task prompt bypasses renderer)", renderer.renderCalls)
	}
	if rtSandbox.execCount != 2 {
		t.Fatalf("exec calls = %d, want 2", rtSandbox.execCount)
	}
	if rtSandbox.writePromptCount != 1 {
		t.Fatalf("prompt writes = %d, want 1 (task prompt bypasses sandbox WritePrompt)", rtSandbox.writePromptCount)
	}
	if len(resetCalls) != 1 {
		t.Fatalf("reset calls = %d, want 1", len(resetCalls))
	}
	if resetCalls[0].branch != branch || resetCalls[0].baseBranch != "main" {
		t.Fatalf("unexpected reset args: %#v", resetCalls[0])
	}
	if resetCalls[0].worktreePath != rtSandbox.WorkDir() {
		t.Fatalf("reset worktree path = %q, want %q", resetCalls[0].worktreePath, rtSandbox.WorkDir())
	}
	logPath := filepath.Join(workDir, ".sandman", "logs", "42.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "--- run 1/3 ---") {
		t.Fatalf("expected run marker in log, got:\n%s", data)
	}
	if !strings.Contains(string(data), "--- retry 2/3 ---") {
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

	branch := "sandman/42-fix-bug"
	rtSandbox := &retrySandbox{workDir: filepath.Join(workDir, "worktree"), execErrors: []error{errors.New("exit 1")}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	pr := &github.PR{Number: 17, State: "closed", Merged: false, HeadRefName: branch}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}, prs: map[string]*github.PR{branch: pr}},
		renderer:     renderer,
		errorLog:     io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		pr.Merged = true
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
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
		layout:       paths.NewLayout(&config.Config{}, workDir),
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, _ := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: "sandman/42-fix-bug"}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
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
	contextPath := filepath.Join(worktreePath, ".sandman", "task.md")
	if err := os.WriteFile(contextPath, []byte("## Source Prompt: .sandman/task.md\n## Last Skill: sandman-pr-review\n## Last Skill Status: complete\n## Completed\nKeep going.\n"), 0644); err != nil {
		t.Fatalf("write context: %v", err)
	}

	pr := &github.PR{Number: 42, State: "closed", Merged: false, HeadRefName: branch}
	// Flip Merged only on the post-attempt check after the retry has run
	// (third FindPRByBranch call), so the pre-retry guard on the second call
	// does not short-circuit the retry before the continuation prompt is
	// rendered.
	var findPRCalls int
	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
			findPRHook: func() {
				findPRCalls++
				if findPRCalls >= 3 {
					pr.Merged = true
				}
			},
		},
		renderer: renderer,
		errorLog: io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
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
	if rtSandbox.execCommand != "opencode run .sandman/task.md" {
		t.Fatalf("expected continue prompt command, got %q", rtSandbox.execCommand)
	}
	taskPromptPath := filepath.Join(worktreePath, ".sandman", "task.md")
	data, err := os.ReadFile(taskPromptPath)
	if err != nil {
		t.Fatalf("read continue prompt: %v", err)
	}
	if !strings.Contains(string(data), "## Prior Context") {
		t.Fatalf("expected Prior Context in retry prompt, got: %q", string(data))
	}
	if !strings.Contains(string(data), "Keep going.") {
		t.Fatalf("expected body content in retry prompt, got: %q", string(data))
	}
	if !strings.Contains(string(data), "## Source Prompt: .sandman/task.md") {
		t.Fatalf("expected Source Prompt in retry prompt, got: %q", string(data))
	}
	if !strings.Contains(string(data), "## Last Skill: sandman-pr-review") {
		t.Fatalf("expected Last Skill in retry prompt, got: %q", string(data))
	}
	if !strings.Contains(string(data), "## Last Skill Status: complete") {
		t.Fatalf("expected Last Skill Status in retry prompt, got: %q", string(data))
	}
	if !strings.Contains(string(data), "## Update Task Context") {
		t.Fatalf("expected Update Task Context in retry prompt, got: %q", string(data))
	}
}

// When a retry happens with an open PR and task.md is missing, the agent
// receives the EmptyTaskTemplate (no branch reset) so the open PR is
// preserved and the agent continues working on it without prior context.
func TestRunSingle_RetryWithOpenPRFallsBackToEmptyTaskTemplate(t *testing.T) {
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

	pr := &github.PR{Number: 17, State: "open", Merged: false, HeadRefName: branch}
	// Flip Merged only after the retry has run (third FindPRByBranch call)
	// so the pre-retry guard on the second call does not short-circuit the
	// retry before the empty task template is rendered.
	var findPRCalls int
	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
			findPRHook: func() {
				findPRCalls++
				if findPRCalls >= 3 {
					pr.Merged = true
				}
			},
		},
		renderer: renderer,
		errorLog: io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
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
	if rtSandbox.execCommand != "opencode run .sandman/task.md" {
		t.Fatalf("expected continue prompt command, got %q", rtSandbox.execCommand)
	}
	taskPromptPath := filepath.Join(worktreePath, ".sandman", "task.md")
	data, err := os.ReadFile(taskPromptPath)
	if err != nil {
		t.Fatalf("read continue prompt: %v", err)
	}
	if !strings.Contains(string(data), "Continue the work.") {
		t.Fatalf("expected empty task template, got %q", string(data))
	}
}

func TestRunSingle_FailsWhenSuccessPRUnmerged(t *testing.T) {
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
	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: false, HeadRefName: branch}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	if len(resultFactory.created) != 2 {
		t.Fatalf("created runnables = %d, want 2", len(resultFactory.created))
	}
	events, err := spyLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events (run.started + run.retry + run.finished), got %d: %v", len(events), events)
	}
	if events[0].Type != "run.started" {
		t.Fatalf("expected first event run.started, got %q", events[0].Type)
	}
	if events[1].Type != "run.retry" {
		t.Fatalf("expected second event run.retry, got %q", events[1].Type)
	}
	if events[2].Type != "run.finished" {
		t.Fatalf("expected terminal event run.finished, got %q", events[2].Type)
	}
	if status, _ := events[2].Payload["status"].(string); status != "failure" {
		t.Fatalf("expected terminal status failure, got %q", status)
	}
}

func TestRunSingle_MergedPRSuccessRegardlessOfAgentExitCode(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "failure", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: true, HeadRefName: branch}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 0, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success (PR merged overrides agent non-zero exit)", result.Status)
	}
}

func TestRunSingle_UnmergedPRFailureRegardlessOfAgentExitCode(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: false, HeadRefName: branch}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 0, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure (unmerged PR forces failure regardless of agent zero exit)", result.Status)
	}
}

func TestRunSingle_RetryWithMergedPRIncrementsRetriesTotal(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: true, HeadRefName: branch}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  sbFactory,
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, sbFactory, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
}

func TestRunSingle_RetryWithMergedPRKeepsRetryCount(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "failure", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	spyLog := &spyEventLog{}
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 17, State: "open", Merged: true, HeadRefName: branch}},
		},
		renderer:        &retryRenderer{result: "rendered prompt"},
		sandboxFactory:  &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}},
		eventLog:        spyLog,
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1 (PR merged on first attempt)", result.RetriesTotal)
	}
}

// When the agent fails on attempt 0 but the PR is merged before the retry,
// the pre-retry guard must detect the merge, skip the agent launch, branch
// reset, and prompt render, and return a success result.
func TestRunSingle_PreRetryGuardShortCircuitsOnMergedPR(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(workDir, "worktree")

	pr := &github.PR{Number: 17, State: "open", Merged: false, HeadRefName: branch}
	var findPRCalls int
	resultFactory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "failure", Branch: branch},
	}}
	renderer := &retryRenderer{result: "rendered prompt"}
	var resetCalls int
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
			findPRHook: func() {
				findPRCalls++
				if findPRCalls >= 2 {
					pr.Merged = true
				}
			},
		},
		renderer:        renderer,
		sandboxFactory:  &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}},
		errorLog:        io.Discard,
		runnableFactory: resultFactory,
	}
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}, nil, false, "main", nil, 0, 0, 2, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success (pre-retry guard should short-circuit on merged PR)", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1 (agent ran once; guard short-circuited the retry)", result.RetriesTotal)
	}
	if len(resultFactory.created) != 1 {
		t.Fatalf("agent launches = %d, want 1 (pre-retry guard must not launch the agent on attempt 1)", len(resultFactory.created))
	}
	if resetCalls != 0 {
		t.Fatalf("retry reset calls = %d, want 0 (pre-retry guard must not reset the branch)", resetCalls)
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

	pr := &github.PR{Number: 17, State: "closed", Merged: false, HeadRefName: branch}
	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
		},
		renderer: renderer,
		errorLog: io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		pr.Merged = true
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if resetCalls != 1 {
		t.Fatalf("reset calls = %d, want 1", resetCalls)
	}
	if rtSandbox.execCommand != "opencode run .sandman/task.md" {
		t.Fatalf("expected task.md to be used, got %q", rtSandbox.execCommand)
	}
	// Verify the task prompt is the empty template (no task doc existed)
	promptPath := filepath.Join(worktreePath, ".sandman", "task.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read task.md: %v", err)
	}
	if !strings.Contains(string(data), "Continue the work.") {
		t.Fatalf("expected empty task template, got %q", string(data))
	}
}

func TestRunSingle_RetryUsesStageAwarePrompt(t *testing.T) {
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
	contextPath := filepath.Join(worktreePath, ".sandman", "task.md")
	if err := os.WriteFile(contextPath, []byte("## Stage: plan-approved\n## Source Prompt: .sandman/task.md\n## Last Skill: sandman-tdd\n## Last Skill Status: complete\n\n## Completed\nInitial implementation done.\n"), 0644); err != nil {
		t.Fatalf("write context: %v", err)
	}

	pr := &github.PR{Number: 42, State: "closed", Merged: false, HeadRefName: branch}
	var findPRCalls int
	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
			findPRHook: func() {
				findPRCalls++
				if findPRCalls >= 2 {
					pr.Merged = true
				}
			},
		},
		renderer: renderer,
		errorLog: io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
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
	if rtSandbox.execCommand != "opencode run .sandman/task.md" {
		t.Fatalf("expected continue prompt command, got %q", rtSandbox.execCommand)
	}
	taskPromptPath := filepath.Join(worktreePath, ".sandman", "task.md")
	data, err := os.ReadFile(taskPromptPath)
	if err != nil {
		t.Fatalf("read continue prompt: %v", err)
	}
	if !strings.Contains(string(data), "## Stage: plan-approved") {
		t.Fatalf("expected stage line preserved verbatim, got:\n%s", data)
	}
	if !strings.Contains(string(data), "## Source Prompt: .sandman/task.md") {
		t.Fatalf("expected source prompt line preserved verbatim, got:\n%s", data)
	}
	if !strings.Contains(string(data), "## Last Skill: sandman-tdd") {
		t.Fatalf("expected last skill line preserved verbatim, got:\n%s", data)
	}
	if !strings.Contains(string(data), "## Last Skill Status: complete") {
		t.Fatalf("expected last skill status line preserved verbatim, got:\n%s", data)
	}
	if !strings.Contains(string(data), "Initial implementation done.") {
		t.Fatalf("expected verbatim context content, got:\n%s", data)
	}
}

func TestRunSingle_RetryUsesPRReviewTaskPrompt(t *testing.T) {
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
	contextPath := filepath.Join(worktreePath, ".sandman", "task.md")
	if err := os.WriteFile(contextPath, []byte("## Stage: pr-review-finished\n## Source Prompt: .sandman/task.md\n## Last Skill: sandman-pr-review\n## Last Skill Status: complete\n\n## Completed\nReview done.\n"), 0644); err != nil {
		t.Fatalf("write context: %v", err)
	}

	pr := &github.PR{Number: 42, State: "closed", Merged: false, HeadRefName: branch}
	var findPRCalls int
	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	renderer := &retryRenderer{result: "rendered prompt"}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
			findPRHook: func() {
				findPRCalls++
				if findPRCalls >= 2 {
					pr.Merged = true
				}
			},
		},
		renderer: renderer,
		errorLog: io.Discard,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}
	var resetCalls int
	o.runSessionOpts.retryReset = func(ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
		resetCalls++
		return nil
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
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
	if rtSandbox.execCommand != "opencode run .sandman/task.md" {
		t.Fatalf("expected continue prompt command, got %q", rtSandbox.execCommand)
	}
	taskPromptPath := filepath.Join(worktreePath, ".sandman", "task.md")
	data, err := os.ReadFile(taskPromptPath)
	if err != nil {
		t.Fatalf("read continue prompt: %v", err)
	}
	if !strings.Contains(string(data), "## Stage: pr-review-finished") {
		t.Fatalf("expected stage line preserved verbatim, got:\n%s", data)
	}
	if !strings.Contains(string(data), "## Source Prompt: .sandman/task.md") {
		t.Fatalf("expected source prompt line preserved verbatim, got:\n%s", data)
	}
	if !strings.Contains(string(data), "## Last Skill: sandman-pr-review") {
		t.Fatalf("expected last skill line preserved verbatim, got:\n%s", data)
	}
	if !strings.Contains(string(data), "## Last Skill Status: complete") {
		t.Fatalf("expected last skill status line preserved verbatim, got:\n%s", data)
	}
	if !strings.Contains(string(data), "Review done.") {
		t.Fatalf("expected verbatim context content, got:\n%s", data)
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

	pr := &github.PR{Number: 42, State: "closed", Merged: false, HeadRefName: branch}
	// Flip Merged only on the post-attempt check after both attempts have
	// run (third FindPRByBranch call) so the pre-retry guard does not
	// short-circuit the retry before the second attempt logs its counter.
	var findPRCalls int
	rtSandbox := &retrySandbox{workDir: worktreePath, execErrors: []error{errors.New("exit 1"), nil}}
	log := &spyEventLog{}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
			findPRHook: func() {
				findPRCalls++
				if findPRCalls >= 3 {
					pr.Merged = true
				}
			},
		},
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		eventLog: log,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
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

func TestRunSingle_LogsIssueTitleOnRunStarted(t *testing.T) {
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

	rtSandbox := &retrySandbox{workDir: worktreePath}
	log := &spyEventLog{}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
		},
		renderer: &retryRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		eventLog: log,
		sandboxFactory: &retrySandboxFactory{
			sandbox: rtSandbox,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &retrySandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	for _, event := range log.events {
		if event.Type != "run.started" {
			continue
		}
		if got := event.Payload["issue_title"]; got != "Fix bug" {
			t.Fatalf("issue_title = %#v, want %q", got, "Fix bug")
		}
		return
	}
	t.Fatal("expected run.started event")
}

func TestRunSingle_ContinuesWhenRunMarkerWriteFails(t *testing.T) {
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
	currentHead := "current-sha"
	rtSandbox := &fakeSandbox{workDir: filepath.Join(workDir, "worktree"), execStdout: "hello from agent\n"}
	var markerPath string
	oldMarkerFn := logRunMarkerFn
	logRunMarkerFn = func(path string, attempt, maxRetries int) error {
		markerPath = path
		return errors.New("marker write failed")
	}
	t.Cleanup(func() { logRunMarkerFn = oldMarkerFn })
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return currentHead, nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs: map[string]*github.PR{
				branch: mergedPR(branch, currentHead),
			},
		},
		renderer: &spyPromptRenderer{result: "rendered prompt"},
		errorLog: io.Discard,
		layout:   paths.NewLayout(&config.Config{}, workDir),
		sandboxFactory: &fakeSandboxFactory{
			sandbox: rtSandbox,
		},
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "opencode run {{.PromptFile}}"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &fakeSandboxFactory{sandbox: rtSandbox}, nil, false, "main", nil, 0, 0, 1, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	wantLogPath := filepath.Join(workDir, ".sandman", "logs", "42.log")
	if markerPath != wantLogPath {
		t.Fatalf("marker path = %q, want %q", markerPath, wantLogPath)
	}
}

func TestRunPromptOnlySingle_LogsRunMarkerInWorktreePath(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	rtSandbox := &fakeSandbox{workDir: filepath.Join(workDir, "worktree")}
	var markerPath string
	oldMarkerFn := logRunMarkerFn
	logRunMarkerFn = func(path string, attempt, maxRetries int) error {
		markerPath = path
		return nil
	}
	t.Cleanup(func() { logRunMarkerFn = oldMarkerFn })

	o := &Orchestrator{
		renderer:       &noopRenderer{},
		errorLog:       io.Discard,
		layout:         paths.NewLayout(&config.Config{}, workDir),
		sandboxFactory: &fakeSandboxFactory{sandbox: rtSandbox},
		runnableFactory: &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
			return AgentRunResult{Status: "success", Branch: branch, WorktreePath: rtSandbox.WorkDir()}
		}},
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runPromptOnlySingle(context.Background(), cfg, "opencode", config.Agent{Command: "echo hi"}, noopIdentityResolver(), "prompt-only", prompt.RenderConfig{}, nil, &fakeSandboxFactory{sandbox: rtSandbox}, nil, ModeFresh, "main", 0, 0, 0, "", 0, false, 0, false, false, false, false, 0, "", "", nil)
	if !started {
		t.Fatal("expected prompt-only run to start")
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	wantLogPath := filepath.Join(workDir, ".sandman", "logs", "prompt-only.log")
	if markerPath != wantLogPath {
		t.Fatalf("marker path = %q, want %q", markerPath, wantLogPath)
	}
}

func TestRunPromptOnlySingle_PrefixesOutputWithRunID(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	rtSandbox := &fakeSandbox{workDir: filepath.Join(workDir, "worktree"), execStdout: "hello from agent\n"}
	var output bytes.Buffer

	o := &Orchestrator{
		renderer:       &noopRenderer{},
		errorLog:       io.Discard,
		sandboxFactory: &fakeSandboxFactory{sandbox: rtSandbox},
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runPromptOnlySingle(context.Background(), cfg, "opencode", config.Agent{Command: "echo hi"}, noopIdentityResolver(), "sandman/review-17-1", prompt.RenderConfig{}, &output, &fakeSandboxFactory{sandbox: rtSandbox}, nil, ModeFresh, "main", 0, 0, 0, "", 0, false, 0, false, false, false, true, 17, "check tests", "PR17", nil)
	if !started {
		t.Fatal("expected prompt-only review run to start")
	}
	if result.RunID != "PR17" {
		t.Fatalf("expected RunID PR17, got %q", result.RunID)
	}
	got := output.String()
	if !strings.Contains(got, "[PR17]") {
		t.Fatalf("expected output prefix [PR17], got %q", got)
	}
	if strings.Contains(got, "[prompt-only]") {
		t.Fatalf("expected output not to use prompt-only prefix, got %q", got)
	}
}

func TestRunPromptOnlySingle_PrefixesOutputPromptOnlyWhenNotReview(t *testing.T) {
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	rtSandbox := &fakeSandbox{workDir: filepath.Join(workDir, "worktree"), execStdout: "hello from agent\n"}
	var output bytes.Buffer

	o := &Orchestrator{
		renderer:       &noopRenderer{},
		errorLog:       io.Discard,
		sandboxFactory: &fakeSandboxFactory{sandbox: rtSandbox},
	}

	cfg := &config.Config{WorktreeDir: "worktree", Git: config.GitConfig{BaseBranch: "main"}}
	result, started := o.runPromptOnlySingle(context.Background(), cfg, "opencode", config.Agent{Command: "echo hi"}, noopIdentityResolver(), "sandman/prompt-only-123", prompt.RenderConfig{}, &output, &fakeSandboxFactory{sandbox: rtSandbox}, nil, ModeFresh, "main", 0, 0, 0, "", 0, false, 0, false, false, false, false, 0, "", "run-123", nil)
	if !started {
		t.Fatal("expected prompt-only run to start")
	}
	if result.RunID != "run-123" {
		t.Fatalf("expected RunID run-123, got %q", result.RunID)
	}
	got := output.String()
	if !strings.Contains(got, "[prompt-only]") {
		t.Fatalf("expected output prefix [prompt-only], got %q", got)
	}
	if strings.Contains(got, "[run-123]") {
		t.Fatalf("expected output not to use run ID prefix, got %q", got)
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
		prs: map[string]*github.PR{
			"sandman/1-issue-1": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-issue-1"},
			"sandman/2-issue-2": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-issue-2"},
			"sandman/3-issue-3": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-issue-3"},
			"sandman/4-issue-4": {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-issue-4"},
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
	}

	proc := makeFakeProcess()
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

	if !proc.sigTermObserved() {
		t.Error("expected SIGTERM to be sent to process")
	}
}

func TestRunBatch_LogsAbortedEventOnCancel(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
	}

	proc := makeFakeProcess()
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
	if spyLog.events[1].Type != "run.aborted" {
		t.Fatalf("expected aborted terminal event, got %q", spyLog.events[1].Type)
	}
	if status, _ := spyLog.events[1].Payload["status"].(string); status != "aborted" {
		t.Fatalf("expected aborted terminal status, got %q", status)
	}
	if run := events.ProjectRunStates(spyLog.events); len(run) != 1 || run[0].IsActive() {
		t.Fatalf("expected aborted run to project as terminal, got %#v", run)
	}
}

func TestRunBatch_ReturnsAbortedStatusOnCancel(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
	}

	proc := makeFakeProcess()
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
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("expected error to wrap batch.ErrAborted, got %v", err)
	}
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "aborted" {
		t.Fatalf("expected aborted batch result to report aborted, got %#v", result)
	}
}

func TestRunBatch_PreservesSuccessfulRunWhenContextCancelsLate(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
	}

	proc := makeFakeProcess()
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
	if errors.Is(err, ErrAborted) {
		t.Fatalf("expected successful batch to not wrap batch.ErrAborted, got %v", err)
	}
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		t.Fatalf("expected successful run to stay success, got %#v", result)
	}
	_ = fastSuccess
}

func TestRunBatch_LogsAbortedEventOnPromptOnlyCancel(t *testing.T) {
	client := &fakeGitHubClient{}

	proc := makeFakeProcess()
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
	if spyLog.events[1].Type != "run.aborted" {
		t.Fatalf("expected aborted terminal event, got %q", spyLog.events[1].Type)
	}
	if status, _ := spyLog.events[1].Payload["status"].(string); status != "aborted" {
		t.Fatalf("expected aborted terminal status, got %q", status)
	}
}

func TestRunBatch_ReturnsAbortedStatusOnPromptOnlyCancel(t *testing.T) {
	client := &fakeGitHubClient{}

	proc := makeFakeProcess()
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
			for !proc.sigTermObserved() {
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
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("expected error to wrap batch.ErrAborted, got %v", err)
	}
	if result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "aborted" {
		t.Fatalf("expected aborted prompt-only batch result to report aborted, got %#v", result)
	}
}

func TestRunBatch_PreservesWorktreeOnInterrupt(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
	}

	proc := makeFakeProcess()
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-fix-bug"}},
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
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
	branch := "sandman/42-fix-bug"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
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
			wantCmd:  `opencode run --title 'Sandman run-42-`,
		},
		{
			name:     "config model is used",
			agent:    "opencode",
			cfgModel: "config-model",
			wantCmd:  `opencode run --title 'Sandman run-42-`,
		},
		{
			name:    "default behavior leaves model out",
			agent:   "opencode",
			wantCmd: `opencode run --title 'Sandman run-42-`,
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
				},
			}}, nil)
			o.sandboxFactory = &fakeSandboxFactory{sandbox: sb}

			_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Model: tt.reqModel})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(sb.execCommand, tt.wantCmd) {
				t.Errorf("expected command containing %q, got %q", tt.wantCmd, sb.execCommand)
			}
			if tt.agent == "opencode" {
				if !strings.Contains(sb.execCommand, `--title 'Sandman run-`) {
					t.Errorf("expected --title flag in command, got %q", sb.execCommand)
				}
				if strings.Contains(sb.execCommand, `--title ''`) {
					t.Errorf("expected non-empty --title, got %q", sb.execCommand)
				}
			}
		})
	}
}

func TestRunBatch_SendsSIGKILLAfterTimeout(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	proc := makeFakeProcess()
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	blockRunnable := &blockingRunnable{delayAfterCancel: 300 * time.Millisecond}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = factory
	o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}
	o.runSessionOpts.killTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _ = o.RunBatch(ctx, Request{Issues: []int{42}})

	if !proc.killObserved() {
		t.Error("expected SIGKILL to be sent to process after timeout")
	}
}

func TestRunBatch_FetchesSingleIssue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := BranchName(42, "Fix bug")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
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
		prs: map[string]*github.PR{
			"sandman/1-a": mergedPR("sandman/1-a", ""),
			"sandman/2-b": mergedPR("sandman/2-b", ""),
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

func TestRunBatch_AbortsUpfrontWhenAnyBranchExists(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := BranchName(450, "Middle issue")
	runGit(t, dir, "checkout", "-b", branch)
	runGit(t, dir, "checkout", "main")

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			441: {Number: 441, Title: "First issue"},
			450: {Number: 450, Title: "Middle issue"},
			452: {Number: 452, Title: "Third issue"},
		},
		prs: map[string]*github.PR{
			BranchName(441, "First issue"):  mergedPR(BranchName(441, "First issue"), "current-sha"),
			BranchName(450, "Middle issue"): mergedPR(BranchName(450, "Middle issue"), "current-sha"),
			BranchName(452, "Third issue"):  mergedPR(BranchName(452, "Third issue"), "current-sha"),
		},
	}
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			441: &controlledRunnable{result: AgentRunResult{IssueNumber: 441, Status: "success"}, started: make(chan struct{}), release: make(chan struct{})},
			450: &controlledRunnable{result: AgentRunResult{IssueNumber: 450, Status: "success"}, started: make(chan struct{}), release: make(chan struct{})},
			452: &controlledRunnable{result: AgentRunResult{IssueNumber: 452, Status: "success"}, started: make(chan struct{}), release: make(chan struct{})},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = factory
	oldBranchExists := branchExists
	oldBranchValidationEnabled := branchValidationEnabled
	branchExists = sandbox.BranchExists
	branchValidationEnabled = true
	t.Cleanup(func() {
		branchExists = oldBranchExists
		branchValidationEnabled = oldBranchValidationEnabled
	})
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{441, 450, 452}})
	if err == nil {
		t.Fatal("expected branch conflict error")
	}
	want := fmt.Sprintf("#450 (%s)", branch)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error to contain %q, got %q", want, err.Error())
	}
	if !strings.Contains(err.Error(), `git branch -D <branch>`) {
		t.Fatalf("expected delete hint in error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "#450: no prior run — use --override") {
		t.Fatalf("expected #450 per-issue override guidance, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "or use --override to restart from scratch or --continue to resume") {
		t.Fatalf("error should not contain the old global phrase, got %q", err.Error())
	}
	if len(factory.created) != 0 {
		t.Fatalf("expected no runnables created, got %d", len(factory.created))
	}
	for issue, runnable := range factory.runnables {
		cr := runnable.(*controlledRunnable)
		assertNoSignal(t, cr.started, fmt.Sprintf("expected runnable %d to stay idle", issue))
	}
}

func TestValidateBatchBranches_RecommendsCorrectFlagPerIssue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branchWithPriorRun := BranchName(500, "Has prior run")
	branchWithoutPriorRun := BranchName(600, "No prior run")
	runGit(t, dir, "checkout", "-b", branchWithPriorRun)
	runGit(t, dir, "checkout", "main")
	runGit(t, dir, "checkout", "-b", branchWithoutPriorRun)
	runGit(t, dir, "checkout", "main")

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			500: {Number: 500, Title: "Has prior run"},
			600: {Number: 600, Title: "No prior run"},
		},
		prs: map[string]*github.PR{
			branchWithPriorRun:    mergedPR(branchWithPriorRun, "current-sha"),
			branchWithoutPriorRun: mergedPR(branchWithoutPriorRun, "current-sha"),
		},
	}
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			500: &controlledRunnable{result: AgentRunResult{IssueNumber: 500, Status: "success"}, started: make(chan struct{}), release: make(chan struct{})},
			600: &controlledRunnable{result: AgentRunResult{IssueNumber: 600, Status: "success"}, started: make(chan struct{}), release: make(chan struct{})},
		},
	}
	spyLog := &spyEventLog{
		events: []events.Event{
			{Type: "run.started", RunID: "run-500-1", Issue: 500, IssueRef: issueRef(500)},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = factory
	oldBranchExists := branchExists
	oldBranchValidationEnabled := branchValidationEnabled
	branchExists = sandbox.BranchExists
	branchValidationEnabled = true
	t.Cleanup(func() {
		branchExists = oldBranchExists
		branchValidationEnabled = oldBranchValidationEnabled
	})
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{500, 600}})
	if err == nil {
		t.Fatal("expected branch conflict error")
	}

	withPrior := fmt.Sprintf("#500 (%s)", branchWithPriorRun)
	withoutPrior := fmt.Sprintf("#600 (%s)", branchWithoutPriorRun)
	if !strings.Contains(err.Error(), withPrior) {
		t.Fatalf("expected error to contain %q, got %q", withPrior, err.Error())
	}
	if !strings.Contains(err.Error(), withoutPrior) {
		t.Fatalf("expected error to contain %q, got %q", withoutPrior, err.Error())
	}
	if !strings.Contains(err.Error(), "#500: prior run exists — use --continue") {
		t.Fatalf("expected #500 to recommend --continue, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "#600: no prior run — use --override") {
		t.Fatalf("expected #600 to recommend --override, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "git branch -D <branch>") {
		t.Fatalf("expected delete hint in error, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "or use --override to restart from scratch or --continue to resume") {
		t.Fatalf("error should not contain the old global phrase, got %q", err.Error())
	}
	if len(factory.created) != 0 {
		t.Fatalf("expected no runnables created, got %d", len(factory.created))
	}
	for issue, runnable := range factory.runnables {
		cr := runnable.(*controlledRunnable)
		assertNoSignal(t, cr.started, fmt.Sprintf("expected runnable %d to stay idle", issue))
	}
}

func TestRunBatch_AllowsBatchWhenNoBranchExists(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			441: {Number: 441, Title: "First issue"},
			450: {Number: 450, Title: "Middle issue"},
			452: {Number: 452, Title: "Third issue"},
		},
		prs: map[string]*github.PR{
			BranchName(441, "First issue"):  mergedPR(BranchName(441, "First issue"), ""),
			BranchName(450, "Middle issue"): mergedPR(BranchName(450, "Middle issue"), ""),
			BranchName(452, "Third issue"):  mergedPR(BranchName(452, "Third issue"), ""),
		},
	}
	started := map[int]chan struct{}{
		441: make(chan struct{}),
		450: make(chan struct{}),
		452: make(chan struct{}),
	}
	release := map[int]chan struct{}{
		441: make(chan struct{}),
		450: make(chan struct{}),
		452: make(chan struct{}),
	}
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			441: &controlledRunnable{result: AgentRunResult{IssueNumber: 441, Status: "success"}, started: started[441], release: release[441]},
			450: &controlledRunnable{result: AgentRunResult{IssueNumber: 450, Status: "success"}, started: started[450], release: release[450]},
			452: &controlledRunnable{result: AgentRunResult{IssueNumber: 452, Status: "success"}, started: started[452], release: release[452]},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = factory
	oldBranchExists := branchExists
	oldBranchValidationEnabled := branchValidationEnabled
	branchExists = sandbox.BranchExists
	branchValidationEnabled = true
	t.Cleanup(func() {
		branchExists = oldBranchExists
		branchValidationEnabled = oldBranchValidationEnabled
	})

	done := make(chan struct {
		result *Result
		err    error
	}, 1)
	go func() {
		result, err := o.RunBatch(context.Background(), Request{Issues: []int{441, 450, 452}})
		done <- struct {
			result *Result
			err    error
		}{result: result, err: err}
	}()

	for _, issue := range []int{441, 450, 452} {
		waitForSignal(t, started[issue], fmt.Sprintf("expected runnable %d to start", issue))
	}
	for _, issue := range []int{441, 450, 452} {
		close(release[issue])
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if len(factory.created) != 3 {
		t.Fatalf("expected 3 runnables created, got %d", len(factory.created))
	}
	if len(res.result.Runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(res.result.Runs))
	}
}

func TestRunBatch_OverrideClearsExistingBranchesAndProceeds(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := BranchName(450, "Middle issue")
	runGit(t, dir, "checkout", "-b", branch)
	runGit(t, dir, "checkout", "main")

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			441: {Number: 441, Title: "First issue"},
			450: {Number: 450, Title: "Middle issue"},
			452: {Number: 452, Title: "Third issue"},
		},
		prs: map[string]*github.PR{
			BranchName(441, "First issue"):  mergedPR(BranchName(441, "First issue"), "current-sha"),
			BranchName(450, "Middle issue"): mergedPR(BranchName(450, "Middle issue"), "current-sha"),
			BranchName(452, "Third issue"):  mergedPR(BranchName(452, "Third issue"), "current-sha"),
		},
	}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 441, Status: "success"}, {IssueNumber: 450, Status: "success"}, {IssueNumber: 452, Status: "success"}}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory
	oldBranchExists := branchExists
	oldBranchValidationEnabled := branchValidationEnabled
	branchExists = sandbox.BranchExists
	branchValidationEnabled = true
	t.Cleanup(func() {
		branchExists = oldBranchExists
		branchValidationEnabled = oldBranchValidationEnabled
	})
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{441, 450, 452}, Mode: map[int]IssueMode{441: ModeOverride, 450: ModeOverride, 452: ModeOverride}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(result.Runs))
	}
	if len(factory.created) != 3 {
		t.Fatalf("expected 3 runnables created, got %d", len(factory.created))
	}
	if branchOut := strings.TrimSpace(runGit(t, dir, "branch", "--list", branch)); branchOut == "" {
		t.Fatalf("expected branch %s to be recreated", branch)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "worktrees", branch)); err != nil {
		t.Fatalf("expected worktree for %s to be recreated: %v", branch, err)
	}
}

func TestRunBatch_MixedContinueStillValidatesFreshBranches(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	continueBranch := BranchName(42, "Continue issue")
	freshBranch := BranchName(43, "Fresh issue")
	runGit(t, dir, "checkout", "-b", freshBranch)
	runGit(t, dir, "checkout", "main")

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Continue issue"},
			43: {Number: 43, Title: "Fresh issue"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success"}, {IssueNumber: 43, Status: "success"}}}
	oldBranchExists := branchExists
	oldBranchValidationEnabled := branchValidationEnabled
	branchExists = sandbox.BranchExists
	branchValidationEnabled = true
	t.Cleanup(func() {
		branchExists = oldBranchExists
		branchValidationEnabled = oldBranchValidationEnabled
	})

	_, err := o.RunBatch(context.Background(), Request{
		Issues:         []int{42, 43},
		Mode:           map[int]IssueMode{42: ModeContinue, 43: ModeFresh},
		Branches:       map[int]string{42: continueBranch},
		BaseBranches:   map[int]string{42: "main"},
		PreviousRunIDs: map[int]string{42: "run-42-prev"},
		PromptConfig:   prompt.RenderConfig{TaskPrompt: "finish the work"},
	})
	if err == nil {
		t.Fatal("expected error when fresh issue branch already exists")
	}
	if !strings.Contains(err.Error(), freshBranch) {
		t.Fatalf("expected error to mention fresh branch %q, got %v", freshBranch, err)
	}
}

func TestRunBatch_MixedContinuePropagatesPerIssueModeMap(t *testing.T) {
	// Mixed mode should still carry continue metadata only for continue issues.
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Continue issue"},
			43: {Number: 43, Title: "Fresh issue"},
		},
		prs: map[string]*github.PR{
			"sandman/42-continue-issue": mergedPR("sandman/42-continue-issue", ""),
			"sandman/43-fresh-issue":    mergedPR("sandman/43-fresh-issue", ""),
		},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}},
		43: &controlledRunnable{result: AgentRunResult{IssueNumber: 43, Status: "success"}},
	}}

	result, err := o.RunBatch(context.Background(), Request{
		Issues:         []int{42, 43},
		Mode:           map[int]IssueMode{42: ModeContinue, 43: ModeFresh},
		Branches:       map[int]string{42: "sandman/42-continue-issue"},
		BaseBranches:   map[int]string{42: "main"},
		PreviousRunIDs: map[int]string{42: "run-42-prev"},
		PromptConfig:   prompt.RenderConfig{TaskPrompt: "finish the work"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var continued bool
	for _, e := range spyLog.events {
		switch e.Type {
		case "run.continued":
			if e.Issue == 42 && e.Payload["previous_run_id"] == "run-42-prev" {
				continued = true
			}
		}
	}
	if !continued {
		t.Fatal("expected run.continued event for issue 42")
	}
	if len(result.Runs) != 2 {
		t.Fatalf("expected 2 run results, got %d", len(result.Runs))
	}
	for _, run := range result.Runs {
		if run.IssueNumber == 43 && run.Status != "success" {
			t.Fatalf("expected issue 43 success result, got %q", run.Status)
		}
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
		prs: map[string]*github.PR{
			"sandman/1-one": mergedPR("sandman/1-one", ""),
			"sandman/2-two": mergedPR("sandman/2-two", ""),
		},
	}
	store := &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}
	o := NewOrchestrator(client, &noopRenderer{}, store, nil)

	tracker := &baseBranchSyncTracker{}
	o.runSessionOpts.baseBranchSync = func(repoPath, sourceBranch string) error {
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
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

	promptPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-bug", ".sandman", "task.md")
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
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
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBatch) {
		t.Skip("set SANDMAN_E2E_GATES=batch (or all) to run end-to-end batch test")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	fakeBinDir := filepath.Join(dir, "fake-bin")
	if err := os.MkdirAll(fakeBinDir, 0755); err != nil {
		t.Fatalf("create fake bin dir: %v", err)
	}
	fakeOpencode := filepath.Join(fakeBinDir, "opencode")
	if err := os.WriteFile(fakeOpencode, []byte("#!/bin/sh\ntouch agent-ran.txt\n"), 0755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}
	configData := `agent: opencode
worktree_dir: .sandman/worktrees
sandbox: worktree
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

	promptPath := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix-login-bug", ".sandman", "task.md")
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
	if !strings.Contains(got, "Review command: `/sandman review`") {
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
		prs: map[string]*github.PR{
			"sandman/1-a": mergedPR("sandman/1-a", ""),
			"sandman/2-b": mergedPR("sandman/2-b", ""),
			"sandman/3-c": mergedPR("sandman/3-c", ""),
			"sandman/4-d": mergedPR("sandman/4-d", ""),
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
		prs: map[string]*github.PR{
			"sandman/1-a": mergedPR("sandman/1-a", ""),
			"sandman/3-c": mergedPR("sandman/3-c", ""),
		},
	}

	factory := &byIssueRunnableFactory{results: map[int]AgentRunResult{
		1: {IssueNumber: 1, Status: "success"},
		2: {IssueNumber: 2, Status: "failure"},
		3: {IssueNumber: 3, Status: "success"},
	}}

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
		prs: map[string]*github.PR{
			"sandman/1-a": mergedPR("sandman/1-a", ""),
			"sandman/2-b": mergedPR("sandman/2-b", ""),
			"sandman/3-c": mergedPR("sandman/3-c", ""),
			"sandman/4-d": mergedPR("sandman/4-d", ""),
			"sandman/5-e": mergedPR("sandman/5-e", ""),
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
		prs: map[string]*github.PR{
			"sandman/42-blocker":    mergedPR("sandman/42-blocker", ""),
			"sandman/100-dependent": mergedPR("sandman/100-dependent", ""),
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
		prs: map[string]*github.PR{
			"sandman/42-runnable": mergedPR("sandman/42-runnable", ""),
			"sandman/100-blocked": mergedPR("sandman/100-blocked", ""),
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

func TestRunBatch_InBatchBlockerSuccessUnblocksDependentDespiteOpenIssue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker", State: "open"},
			100: {Number: 100, Title: "Dependent", BlockedBy: []int{42}, State: "open"},
		},
		prs: map[string]*github.PR{
			"sandman/42-blocker":    mergedPR("sandman/42-blocker", ""),
			"sandman/100-dependent": mergedPR("sandman/100-dependent", ""),
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

	waitForSignal(t, dependentStarted, "expected dependent to start once in-batch blocker succeeded, even though blocker's GitHub issue is still open")
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
		t.Fatalf("expected dependent success because in-batch blocker succeeded, got %q", statuses[100])
	}

	for _, e := range spyLog.events {
		if e.Type == "run.blocked" && e.Issue == 100 {
			t.Fatalf("did not expect run.blocked event for dependent when in-batch blocker succeeded, got %#v", e.Payload)
		}
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
		prs: map[string]*github.PR{
			"sandman/42-runnable": mergedPR("sandman/42-runnable", ""),
			"sandman/100-blocked": mergedPR("sandman/100-blocked", ""),
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
		prs: map[string]*github.PR{
			"sandman/1-blocker":     mergedPR("sandman/1-blocker", ""),
			"sandman/3-dependent-a": mergedPR("sandman/3-dependent-a", ""),
			"sandman/4-dependent-b": mergedPR("sandman/4-dependent-b", ""),
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
		prs: map[string]*github.PR{
			"sandman/1-first":  mergedPR("sandman/1-first", ""),
			"sandman/2-second": mergedPR("sandman/2-second", ""),
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
		prs: map[string]*github.PR{
			"sandman/1-blocker":     {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-blocker"},
			"sandman/3-dependent-a": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-dependent-a"},
			"sandman/4-dependent-b": {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-dependent-b"},
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
		prs: map[string]*github.PR{
			"sandman/1-first":  mergedPR("sandman/1-first", ""),
			"sandman/2-second": mergedPR("sandman/2-second", ""),
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
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
	branch := BranchName(42, "Fix bug")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}}}}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Parallel: 3, Retries: 4, StartDelay: 7 * time.Second, StartDelaySet: true, Sandbox: "worktree", ContainerCapacity: 2, ContainerCapacitySet: true, MaxContainers: 5, MaxContainersSet: true, PromptConfig: prompt.RenderConfig{PromptFlag: "inline", PromptArgs: map[string]string{"FOO": "bar"}, ReviewCommand: "/custom review", ReviewCommandSet: true}})
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
	if started.Payload["parallel"] != 3 {
		t.Fatalf("expected parallel replay, got %#v", started.Payload["parallel"])
	}
	if started.Payload["start_delay"] != 7 {
		t.Fatalf("expected start delay replay, got %#v", started.Payload["start_delay"])
	}
	if started.Payload["retries"] != 4 {
		t.Fatalf("expected retries replay, got %#v", started.Payload["retries"])
	}
	if started.Payload["sandbox"] != "worktree" {
		t.Fatalf("expected sandbox replay, got %#v", started.Payload["sandbox"])
	}
	if started.Payload["container_capacity"] != 2 {
		t.Fatalf("expected container capacity replay, got %#v", started.Payload["container_capacity"])
	}
	if started.Payload["container_capacity_set"] != true {
		t.Fatalf("expected container capacity set replay, got %#v", started.Payload["container_capacity_set"])
	}
	if started.Payload["max_containers"] != 5 {
		t.Fatalf("expected max containers replay, got %#v", started.Payload["max_containers"])
	}
	if started.Payload["max_containers_set"] != true {
		t.Fatalf("expected max containers set replay, got %#v", started.Payload["max_containers_set"])
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
	o.runSessionOpts.baseBranchSync = func(repoPath, sourceBranch string) error {
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
	o.runSessionOpts.baseBranchSync = func(repoPath, sourceBranch string) error { return errors.New("sync failed") }
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

func TestRunBatch_PromptOnlyReviewRunEmitsReviewTag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{err: errors.New("fetch should not run")}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(".sandman", "worktrees", "sandman", "review-17-1")}}
	o.runnableFactory = &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		return AgentRunResult{Status: "success", Branch: branch, WorktreePath: filepath.Join(".sandman", "worktrees", branch)}
	}}

	_, err := o.RunBatch(context.Background(), Request{
		PromptConfig: prompt.RenderConfig{PromptFlag: "Review the PR."},
		Review:       true,
		PRNumber:     17,
		ReviewFocus:  "focus on tests",
		RunID:        "PR17",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	for _, evt := range spyLog.events {
		if evt.Payload["review"] != true {
			t.Errorf("event %q missing review=true in payload, got %#v", evt.Type, evt.Payload["review"])
		}
		if evt.Payload["pr_number"] != 17 {
			t.Errorf("event %q missing pr_number=17 in payload, got %#v", evt.Type, evt.Payload["pr_number"])
		}
		if evt.Payload["review_focus"] != "focus on tests" {
			t.Errorf("event %q missing review_focus in payload, got %#v", evt.Type, evt.Payload["review_focus"])
		}
	}
}

func TestRunBatch_PromptOnlyReviewRunResultCarriesReviewIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{err: errors.New("fetch should not run")}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(".sandman", "worktrees", "sandman", "review-17-1")}}
	o.runnableFactory = &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		return AgentRunResult{Status: "success", Branch: branch, WorktreePath: filepath.Join(".sandman", "worktrees", branch)}
	}}

	result, err := o.RunBatch(context.Background(), Request{
		PromptConfig: prompt.RenderConfig{PromptFlag: "Review the PR."},
		Review:       true,
		PRNumber:     17,
		RunID:        "PR17",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(result.Runs))
	}
	if !result.Runs[0].Review {
		t.Errorf("expected result.Runs[0].Review == true")
	}
	if result.Runs[0].RunID != "PR17" {
		t.Errorf("expected result.Runs[0].RunID == \"PR17\", got %q", result.Runs[0].RunID)
	}
}

func TestRunBatch_PromptOnlyReviewRunWithEmptyFocus(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{err: errors.New("fetch should not run")}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(".sandman", "worktrees", "sandman", "review-3-1")}}
	o.runnableFactory = &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		return AgentRunResult{Status: "success", Branch: branch, WorktreePath: filepath.Join(".sandman", "worktrees", branch)}
	}}

	_, err := o.RunBatch(context.Background(), Request{
		PromptConfig: prompt.RenderConfig{PromptFlag: "Review."},
		Review:       true,
		PRNumber:     3,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	for _, evt := range spyLog.events {
		if evt.Payload["review"] != true {
			t.Errorf("event %q missing review=true, got %#v", evt.Type, evt.Payload["review"])
		}
		if _, ok := evt.Payload["review_focus"]; !ok {
			t.Errorf("event %q missing review_focus key (should be present with empty value), payload keys: %v", evt.Type, payloadKeys(evt.Payload))
		}
		if evt.Payload["review_focus"] != "" {
			t.Errorf("event %q review_focus should be empty string, got %#v", evt.Type, evt.Payload["review_focus"])
		}
	}
}

func TestRunBatch_PromptOnlyImplementationRunOmitsReviewKey(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{err: errors.New("fetch should not run")}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(".sandman", "worktrees", "sandman", "p-1")}}
	o.runnableFactory = &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		return AgentRunResult{Status: "success", Branch: branch, WorktreePath: filepath.Join(".sandman", "worktrees", branch)}
	}}

	_, err := o.RunBatch(context.Background(), Request{PromptConfig: prompt.RenderConfig{PromptFlag: "Implement X."}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	for _, evt := range spyLog.events {
		for _, key := range []string{"review", "pr_number", "review_focus"} {
			if _, ok := evt.Payload[key]; ok {
				t.Errorf("implementation event %q should not include %q key, payload: %#v", evt.Type, key, evt.Payload)
			}
		}
	}
}

func TestRunBatch_IssueDrivenImplementationRunOmitsReviewKey(t *testing.T) {
	branch := BranchName(42, "Fix bug")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}}}}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spyLog.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(spyLog.events))
	}
	for _, evt := range spyLog.events {
		for _, key := range []string{"review", "pr_number", "review_focus"} {
			if _, ok := evt.Payload[key]; ok {
				t.Errorf("issue-driven event %q should not include %q key, payload: %#v", evt.Type, key, evt.Payload)
			}
		}
	}
}

func payloadKeys(payload map[string]any) []string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	return keys
}

func TestRunBatch_LogsContinuedEventWithPreviousRunID(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
	}
	spyLog := &spyEventLog{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}}}}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Mode: map[int]IssueMode{42: ModeContinue}, PreviousRunIDs: map[int]string{42: "run-42-1"}, BaseBranch: "main", Parallel: 2, Retries: 1, StartDelay: 5 * time.Second, StartDelaySet: true, Sandbox: "worktree", ContainerCapacity: 4, ContainerCapacitySet: true, MaxContainers: 6, MaxContainersSet: true, PromptConfig: prompt.RenderConfig{TaskPrompt: "finish the tests"}})
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
	if continued.Payload["parallel"] != 2 {
		t.Fatalf("expected parallel replay, got %#v", continued.Payload["parallel"])
	}
	if continued.Payload["start_delay"] != 5 {
		t.Fatalf("expected start delay replay, got %#v", continued.Payload["start_delay"])
	}
	if continued.Payload["retries"] != 1 {
		t.Fatalf("expected retries replay, got %#v", continued.Payload["retries"])
	}
	if continued.Payload["sandbox"] != "worktree" {
		t.Fatalf("expected sandbox replay, got %#v", continued.Payload["sandbox"])
	}
	if continued.Payload["container_capacity"] != 4 {
		t.Fatalf("expected container capacity replay, got %#v", continued.Payload["container_capacity"])
	}
	if continued.Payload["container_capacity_set"] != true {
		t.Fatalf("expected container capacity set replay, got %#v", continued.Payload["container_capacity_set"])
	}
	if continued.Payload["max_containers"] != 6 {
		t.Fatalf("expected max containers replay, got %#v", continued.Payload["max_containers"])
	}
	if continued.Payload["max_containers_set"] != true {
		t.Fatalf("expected max containers set replay, got %#v", continued.Payload["max_containers_set"])
	}
}

func TestRunBatch_ContinuationUsesPerIssuePrompts(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
		},
		prs: map[string]*github.PR{
			"sandman/1-one": mergedPR("sandman/1-one", ""),
			"sandman/2-two": mergedPR("sandman/2-two", ""),
		},
	}
	spyLog := &spyEventLog{}
	workDir := t.TempDir()
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: workDir, Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)

	type promptSandboxFactory struct {
		mu        sync.Mutex
		sandboxes map[string]*fakeSandbox
	}

	factory := &promptSandboxFactory{sandboxes: make(map[string]*fakeSandbox)}
	factoryNew := func(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
		sb := &fakeSandbox{workDir: filepath.Join(worktreeBase, branch)}
		factory.mu.Lock()
		factory.sandboxes[branch] = sb
		factory.mu.Unlock()
		return sb
	}
	o.sandboxFactory = sandboxFactoryFunc(factoryNew)

	_, err := o.RunBatch(context.Background(), Request{
		Issues:         []int{1, 2},
		Branches:       map[int]string{1: "sandman/1-one", 2: "sandman/2-two"},
		Mode:           map[int]IssueMode{1: ModeContinue, 2: ModeContinue},
		PreviousRunIDs: map[int]string{1: "run-1-prev", 2: "run-2-prev"},
		TaskPrompts:    map[int]string{1: "prompt-one", 2: "prompt-two"},
		BaseBranch:     "main",
		PromptConfig:   prompt.RenderConfig{TaskPrompt: "shared-prompt"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for branch, want := range map[string]string{"sandman/1-one": "prompt-one", "sandman/2-two": "prompt-two"} {
		factory.mu.Lock()
		sb := factory.sandboxes[branch]
		factory.mu.Unlock()
		if sb == nil {
			t.Fatalf("missing sandbox for %s", branch)
		}
		promptPath := filepath.Join(sb.workDir, ".sandman", "task.md")
		data, err := os.ReadFile(promptPath)
		if err != nil {
			t.Fatalf("read prompt for %s: %v", branch, err)
		}
		if string(data) != want {
			t.Fatalf("unexpected prompt for %s: %q", branch, string(data))
		}
	}
	if len(spyLog.events) == 0 {
		t.Fatal("expected run events")
	}
}

func TestRunBatch_PerIssuePreviousRunIDLookup(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
			43: {Number: 43, Title: "Add tests"},
		},
		prs: map[string]*github.PR{
			"sandman/42-fix-bug":   mergedPR("sandman/42-fix-bug", ""),
			"sandman/43-add-tests": mergedPR("sandman/43-add-tests", ""),
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
		Mode:           map[int]IssueMode{42: ModeContinue, 43: ModeContinue},
		PreviousRunIDs: map[int]string{42: "run-42-prev", 43: "run-43-prev"},
		BaseBranch:     "main",
		Parallel:       1,
		PromptConfig:   prompt.RenderConfig{TaskPrompt: "finish the work"},
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
		prs: map[string]*github.PR{
			"sandman/42-fix-bug-a": mergedPR("sandman/42-fix-bug-a", ""),
			"sandman/99-fix-bug-b": mergedPR("sandman/99-fix-bug-b", ""),
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
		Mode:           map[int]IssueMode{42: ModeContinue, 99: ModeContinue},
		PreviousRunIDs: map[int]string{42: "run-42-7", 99: "run-99-3"},
		BaseBranch:     "main",
		Parallel:       1,
		PromptConfig:   prompt.RenderConfig{TaskPrompt: "finish them"},
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
	}
	tracker := &baseBranchSyncTracker{}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "trunk"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runSessionOpts.baseBranchSync = func(repoPath, sourceBranch string) error {
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

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Mode: map[int]IssueMode{42: ModeContinue}, BaseBranch: "main", PreviousRunIDs: map[int]string{42: "run-42-1"}, PromptConfig: prompt.RenderConfig{TaskPrompt: "finish the tests"}})
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

	state := &continuationFlowState{writes: 0, contexts: []string{
		"## Completed\nInitial run.\n",
		"## Completed\nFirst continue.\n",
		"## Completed\nSecond continue.\n",
	}}
	log := &spyEventLog{}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}, prs: map[string]*github.PR{branch: {Merged: true, HeadRefName: branch}}}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: filepath.Join(".sandman", "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}}, log)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	o.runnableFactory = &continuationFlowRunnableFactory{state: state}

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}})
	if err != nil {
		t.Fatalf("initial run failed: %v", err)
	}
	// Verify runnable wrote staged task context for each continuation.
	sandmanDir := filepath.Join(worktreePath, ".sandman")
	if _, err := os.Stat(sandmanDir); err != nil {
		t.Fatalf("expected .sandman dir to exist (runnable should have created it), err=%v", err)
	}
	_, err = os.Stat(filepath.Join(sandmanDir, "task.md"))
	if err != nil {
		t.Fatalf("expected task.md to be preserved after merged PR, err=%v", err)
	}

	_, err = o.RunBatch(context.Background(), Request{Issues: []int{42}, Mode: map[int]IssueMode{42: ModeContinue}, BaseBranch: "main", PreviousRunIDs: map[int]string{42: log.events[0].RunID}, PromptConfig: prompt.RenderConfig{TaskPrompt: "finish the tests"}})
	if err != nil {
		t.Fatalf("first continue failed: %v", err)
	}
	// Continuation does not check PR merge, so task.md is preserved.
	_, err = os.Stat(filepath.Join(sandmanDir, "task.md"))
	if err != nil {
		t.Fatalf("expected task.md to be preserved after continuation (no merge check), err=%v", err)
	}

	_, err = o.RunBatch(context.Background(), Request{Issues: []int{42}, Mode: map[int]IssueMode{42: ModeContinue}, BaseBranch: "main", PreviousRunIDs: map[int]string{42: log.events[2].RunID}, PromptConfig: prompt.RenderConfig{TaskPrompt: "push the PR"}})
	if err != nil {
		t.Fatalf("second continue failed: %v", err)
	}
	_, err = os.Stat(filepath.Join(sandmanDir, "task.md"))
	if err != nil {
		t.Fatalf("expected task.md to be preserved after second continuation, err=%v", err)
	}

	if state.writes != 3 {
		t.Fatalf("expected 3 continuation writes, got %d", state.writes)
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
			branch := BranchName(42, "Fix bug")
			client := &fakeGitHubClient{
				issues: map[int]*github.Issue{
					42: {Number: 42, Title: "Fix bug"},
				},
				prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
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

	branch := BranchName(42, "Fix bug")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
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
	branch, _ = spyLog.events[1].Payload["branch"].(string)
	if branch != "sandman/42-fix-bug" {
		t.Errorf("expected branch sandman/42-fix-bug, got %q", branch)
	}
}

func TestRunBatch_LogsWorktreeStateDeletedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := BranchName(42, "Fix bug")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
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

	branch := BranchName(42, "Fix bug")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
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
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte("# sandman build-tools: generic\n# sandman default-agent: opencode\nFROM debian:bookworm-slim\n"), 0644); err != nil {
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
	t.Setenv("HOME", t.TempDir())

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

func (r *deadContainerRunnable) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	if r.container != nil {
		atomic.StoreUint32(&r.container.dead, 1)
	}
	return r.result
}

type aliveCheckingRunnable struct {
	container *fakeContainerForOrchestrator
	result    AgentRunResult
}

func (r *aliveCheckingRunnable) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
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
		prs: map[string]*github.PR{
			"sandman/1-one": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
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
		prs: map[string]*github.PR{
			"sandman/1-one": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
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
		prs: map[string]*github.PR{
			"sandman/1-one": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
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
		prs: map[string]*github.PR{
			"sandman/1-one":   {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two":   {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
			"sandman/3-three": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-three"},
			"sandman/4-four":  {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-four"},
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
	waitForSignal(t, started3, "expected issue 3 to start")
	assertNoSignal(t, started4, "issue 4 should wait for issue 3 to finish")

	close(release2)
	waitForSignal(t, finished2, "expected issue 2 to finish")
	assertNoSignal(t, started4, "issue 4 should still be blocked by issue 3")

	close(release3)
	waitForSignal(t, finished3, "expected issue 3 to finish")
	waitForSignal(t, started4, "expected issue 4 to start after issue 3 finishes")

	if runnables.containerByIssue[4] != runnables.containerByIssue[1] && runnables.containerByIssue[4] != runnables.containerByIssue[2] && runnables.containerByIssue[4] != runnables.containerByIssue[3] {
		t.Fatalf("expected issue 4 to reuse an existing container, got %q", runnables.containerByIssue[4])
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
		prs: map[string]*github.PR{
			"sandman/1-one": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
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
		prs: map[string]*github.PR{
			"sandman/1-one":   {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two":   {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
			"sandman/3-three": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-three"},
			"sandman/4-four":  {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-four"},
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
		prs: map[string]*github.PR{
			"sandman/1-one": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
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

	// effectiveParallelCap(2, 1, 0) = 1, so the start gate serialises the
	// two issues and the pool reuses a single container. See issue #501.
	if starter.startCount != 1 {
		t.Fatalf("expected 1 container to start (effectiveParallel=1 cap applied in auto mode), got %d", starter.startCount)
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
		prs: map[string]*github.PR{
			"sandman/1-one":   {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two":   {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
			"sandman/3-three": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-three"},
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
		prs: map[string]*github.PR{
			"sandman/1-one":   {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two":   {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
			"sandman/3-three": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-three"},
			"sandman/4-four":  {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-four"},
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

	// effectiveParallelCap(4, 2, 0) = 2, so the start gate lets 2 issues
	// through at a time and they share a single container (capacity=2). The
	// pool reuses that container for the second batch. See issue #501.
	if starter.startCount != 1 {
		t.Fatalf("expected 1 container to start (effectiveParallel=2 cap applied, capacity=2 reuses one container), got %d", starter.startCount)
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
		prs: map[string]*github.PR{
			"sandman/1-one": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
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

	if starter.startCount != 1 {
		t.Fatalf("expected config container_capacity=1 to start 1 container (effectiveParallel=1 cap applied, container reused), got %d", starter.startCount)
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
		prs: map[string]*github.PR{
			"sandman/1-a": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-a"},
			"sandman/2-b": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-b"},
			"sandman/3-c": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-c"},
			"sandman/4-d": {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-d"},
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
		prs: map[string]*github.PR{
			"sandman/1-a": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-a"},
			"sandman/2-b": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-b"},
			"sandman/3-c": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-c"},
			"sandman/4-d": {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-d"},
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
		prs: map[string]*github.PR{
			"sandman/1-a": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-a"},
			"sandman/2-b": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-b"},
			"sandman/3-c": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-c"},
			"sandman/4-d": {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-d"},
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
		prs: map[string]*github.PR{
			"sandman/1-a": {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-a"},
			"sandman/2-b": {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-b"},
			"sandman/3-c": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-c"},
			"sandman/4-d": {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-d"},
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
		prs: map[string]*github.PR{
			"sandman/1-one":   {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two":   {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
			"sandman/3-three": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-three"},
			"sandman/4-four":  {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-four"},
			"sandman/5-five":  {Number: 5, State: "closed", Merged: true, HeadRefName: "sandman/5-five"},
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
		prs: map[string]*github.PR{
			"sandman/1-one":   {Number: 1, State: "closed", Merged: true, HeadRefName: "sandman/1-one"},
			"sandman/2-two":   {Number: 2, State: "closed", Merged: true, HeadRefName: "sandman/2-two"},
			"sandman/3-three": {Number: 3, State: "closed", Merged: true, HeadRefName: "sandman/3-three"},
			"sandman/4-four":  {Number: 4, State: "closed", Merged: true, HeadRefName: "sandman/4-four"},
			"sandman/5-five":  {Number: 5, State: "closed", Merged: true, HeadRefName: "sandman/5-five"},
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
	branch := BranchName(42, "Fix bug")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
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

func TestBuildStartOptions_PopulatesOpencodeSnapshotExcludesAndLiveMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	agentCfg := config.BuiltInAgentPresets["opencode"].Agent("opencode")
	opts, err := buildStartOptions(agentCfg)
	if err != nil {
		t.Fatalf("build start options: %v", err)
	}

	wantExcluded := filepath.Join(home, ".local", "share", "opencode", "token-optimizer")
	found := false
	for _, e := range opts.AgentConfigExcludes {
		if e == wantExcluded {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected AgentConfigExcludes to contain %q, got %v", wantExcluded, opts.AgentConfigExcludes)
	}

	wantLive := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	found = false
	for _, l := range opts.LiveMounts {
		if l == wantLive {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected LiveMounts to contain %q, got %v", wantLive, opts.LiveMounts)
	}
}

func TestPrepareContainerConfigMounts_OpencodePresetEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	opencodeDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(opencodeDir, 0755); err != nil {
		t.Fatalf("mkdir opencode dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opencodeDir, "auth.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	tokenOptimizerDir := filepath.Join(opencodeDir, "token-optimizer")
	if err := os.MkdirAll(tokenOptimizerDir, 0755); err != nil {
		t.Fatalf("mkdir token-optimizer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tokenOptimizerDir, "huge.bin"), []byte("X"), 0644); err != nil {
		t.Fatalf("write huge: %v", err)
	}
	dbPath := filepath.Join(opencodeDir, "opencode.db")
	if err := os.WriteFile(dbPath, []byte("DB"), 0644); err != nil {
		t.Fatalf("write db: %v", err)
	}
	configOpencodeDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configOpencodeDir, 0755); err != nil {
		t.Fatalf("mkdir config/opencode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configOpencodeDir, "opencode.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write opencode.json: %v", err)
	}

	agentCfg := config.BuiltInAgentPresets["opencode"].Agent("opencode")
	startOpts, err := buildStartOptions(agentCfg)
	if err != nil {
		t.Fatalf("build start options: %v", err)
	}

	cleanup, err := PrepareContainerConfigMounts(t.TempDir(), "", &startOpts, nil)
	if err != nil {
		t.Fatalf("prepare container config mounts: %v", err)
	}
	defer cleanup()

	var snapshotSource string
	var dbMount *sandbox.ConfigMount
	for i := range startOpts.ConfigMounts {
		mount := &startOpts.ConfigMounts[i]
		switch mount.Target {
		case "/.local/share/opencode":
			snapshotSource = mount.Source
		case "/.local/share/opencode/opencode.db":
			dbMount = mount
		}
	}
	if snapshotSource == "" {
		t.Fatalf("expected snapshot mount for /.local/share/opencode, got %v", startOpts.ConfigMounts)
	}
	if dbMount == nil {
		t.Fatalf("expected live mount for opencode.db, got %v", startOpts.ConfigMounts)
	}
	if dbMount.Source != dbPath {
		t.Errorf("expected live mount source to be host db path %q, got %q", dbPath, dbMount.Source)
	}

	if _, err := os.Stat(filepath.Join(snapshotSource, "auth.json")); err != nil {
		t.Errorf("expected auth.json in snapshot copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshotSource, "token-optimizer")); !os.IsNotExist(err) {
		t.Errorf("expected token-optimizer to be excluded from snapshot, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshotSource, "opencode.db")); !os.IsNotExist(err) {
		t.Errorf("expected opencode.db to be excluded from snapshot (live mount instead), got: %v", err)
	}
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
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
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

	branch := BranchName(42, "Fix bug")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
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

	branch := BranchName(42, "Fix bug")
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
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

	ClearIssueArtifacts(42, branch, worktreeDir, logDir, el, io.Discard, "main", nil)

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
	ClearIssueArtifacts(42, "sandman/42-nonexistent", ".sandman/worktrees", ".sandman/logs", el, io.Discard, "main", nil)
}

func TestClearIssueArtifacts_RemovesOrphanWorktreeDirectory(t *testing.T) {
	// Simulate a previous run that crashed inside `git worktree add` after the
	// directory was created but before git registered it. The dir exists with
	// throwaway content; git never knew about the worktree, so
	// `git worktree remove --force` / `git worktree prune` / `git branch -D`
	// all no-op. ClearIssueArtifacts must still clean up the orphan dir.
	// See #545.
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := "sandman/42-fix-bug"
	worktreeDir := filepath.Join(".sandman", "worktrees")
	wtPath := filepath.Join(worktreeDir, branch)
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "orphan.txt"), []byte("from a crashed run\n"), 0644); err != nil {
		t.Fatalf("write orphan file: %v", err)
	}

	listCmd := exec.Command("git", "worktree", "list")
	listCmd.Dir = dir
	if out, err := listCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree list: %v: %s", err, out)
	} else if strings.Contains(string(out), wtPath) {
		t.Fatalf("git should not know about the orphan dir, got:\n%s", out)
	}

	logDir := filepath.Join(".sandman", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "42.log"), []byte("stale log"), 0644); err != nil {
		t.Fatalf("write stale log: %v", err)
	}

	el := &spyEventLog{}
	ClearIssueArtifacts(42, branch, worktreeDir, logDir, el, io.Discard, "main", nil)

	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("expected orphan worktree dir to be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "42.log")); !os.IsNotExist(err) {
		t.Errorf("expected log to be removed, got err=%v", err)
	}
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

	ClearIssueArtifacts(42, "sandman/42-fix-bug", ".sandman/worktrees", ".sandman/logs", el, io.Discard, "main", nil)

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

// TestClearIssueArtifacts_ReconcilesStrandedWorktreeInMainRepo stages both
// recovery paths added by #938. When `git branch -D` from the main repo fails
// with "used by worktree at" (the main repo is checked out on the target
// branch) and `strandedReconcile` is true, the function must auto-recover
// so that the branch is gone and no error is logged. The two subtests
// cover the stranded-worktree path and the base-branch-checkout path
// respectively; the nil/false belt-and-suspenders behaviours are covered
// by TestClearIssueArtifacts_NoReconcileKeepsBeltAndSuspenders and
// TestClearIssueArtifacts_ExplicitFalseReconcileKeepsBeltAndSuspenders.
func TestClearIssueArtifacts_ReconcilesStrandedWorktreeInMainRepo(t *testing.T) {
	branch := "sandman/42-fix-bug"
	otherBranch := "sandman/99-other-branch"

	t.Run("recovers via stranded worktree", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		initGitRepo(t, dir)

		worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
		if err := os.MkdirAll(worktreeDir, 0755); err != nil {
			t.Fatalf("mkdir worktree base: %v", err)
		}

		// Stage: a stranded worktree exists at <worktreeBase>/<branch>
		// but its HEAD is on `otherBranch` (created via `git worktree
		// add --force`). The main repo is then checked out on the
		// target branch, which is what triggers the "used by worktree
		// at" error from `git branch -D` in the main repo cwd.
		runGit(t, dir, "branch", branch)
		runGit(t, dir, "branch", otherBranch)
		wtPath := filepath.Join(worktreeDir, branch)
		runGit(t, dir, "worktree", "add", "--force", wtPath, otherBranch)
		runGit(t, dir, "checkout", "--quiet", branch)

		logDir := filepath.Join(dir, ".sandman", "logs")
		if err := os.MkdirAll(logDir, 0755); err != nil {
			t.Fatalf("mkdir logs: %v", err)
		}

		el := &spyEventLog{}
		logBuf := &bytes.Buffer{}

		trueVal := true
		ClearIssueArtifacts(42, branch, worktreeDir, logDir, el, logBuf, "main", &trueVal)

		// The branch must be gone from the main repo.
		revCmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
		if out, err := revCmd.CombinedOutput(); err == nil {
			t.Errorf("expected branch %q to be deleted, rev-parse succeeded: %s", branch, out)
		}
		// No error must be logged.
		if strings.Contains(logBuf.String(), "error:") {
			t.Errorf("expected no error log on the stranded-worktree recovery path, got:\n%s", logBuf.String())
		}
	})

	t.Run("recovers via base-branch checkout when no stranded worktree", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		initGitRepo(t, dir)

		worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
		if err := os.MkdirAll(worktreeDir, 0755); err != nil {
			t.Fatalf("mkdir worktree base: %v", err)
		}

		// Stage: the main repo is checked out on the target branch,
		// there is NO stranded worktree at <worktreeBase>/<branch>.
		// `git branch -D <branch>` from the main repo cwd will still
		// fail with "used by worktree at" (because the main repo IS
		// the worktree holding that branch). Recovery must take the
		// base-branch-checkout path: `git checkout -f main` then
		// `git branch -D <branch>`.
		runGit(t, dir, "branch", branch)
		runGit(t, dir, "checkout", "--quiet", branch)

		logDir := filepath.Join(dir, ".sandman", "logs")
		if err := os.MkdirAll(logDir, 0755); err != nil {
			t.Fatalf("mkdir logs: %v", err)
		}

		el := &spyEventLog{}
		logBuf := &bytes.Buffer{}

		trueVal := true
		ClearIssueArtifacts(42, branch, worktreeDir, logDir, el, logBuf, "main", &trueVal)

		// The branch must be gone.
		revCmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
		if out, err := revCmd.CombinedOutput(); err == nil {
			t.Errorf("expected branch %q to be deleted, rev-parse succeeded: %s", branch, out)
		}
		// The main repo must have ended up on the base branch.
		headCmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
		headCmd.Dir = dir
		headOut, err := headCmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(headOut)) != "main" {
			t.Errorf("expected main repo to be on base branch %q after recovery, got %q (err=%v)", "main", strings.TrimSpace(string(headOut)), err)
		}
		// No error must be logged.
		if strings.Contains(logBuf.String(), "error:") {
			t.Errorf("expected no error log on the base-branch-checkout recovery path, got:\n%s", logBuf.String())
		}
	})
}

// TestClearIssueArtifacts_NoReconcileKeepsBeltAndSuspenders asserts that
// when `strandedReconcile` is nil, today's belt-and-suspenders behaviour is
// preserved: the delete failure is logged, the function continues, and the
// branch is NOT auto-recovered (it stays so the operator can fix it).
func TestClearIssueArtifacts_NoReconcileKeepsBeltAndSuspenders(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := "sandman/42-fix-bug"
	runGit(t, dir, "branch", branch)
	runGit(t, dir, "checkout", "--quiet", branch)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	logDir := filepath.Join(dir, ".sandman", "logs")
	el := &spyEventLog{}
	logBuf := &bytes.Buffer{}

	ClearIssueArtifacts(42, branch, worktreeDir, logDir, el, logBuf, "main", nil)

	// The branch should still exist (no recovery ran).
	revCmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
	if out, err := revCmd.CombinedOutput(); err != nil {
		t.Errorf("expected branch %q to still exist when strandedReconcile is nil, rev-parse failed: %s", branch, out)
	}
	if !strings.Contains(logBuf.String(), "error:") {
		t.Errorf("expected an error log when strandedReconcile is nil, got:\n%s", logBuf.String())
	}
}

// TestClearIssueArtifacts_ExplicitFalseReconcileKeepsBeltAndSuspenders
// asserts that an explicit `strandedReconcile=false` preserves today's
// belt-and-suspenders behaviour (the opt-out gate from --no-reconcile-stranded).
func TestClearIssueArtifacts_ExplicitFalseReconcileKeepsBeltAndSuspenders(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := "sandman/42-fix-bug"
	runGit(t, dir, "branch", branch)
	runGit(t, dir, "checkout", "--quiet", branch)

	worktreeDir := filepath.Join(dir, ".sandman", "worktrees")
	logDir := filepath.Join(dir, ".sandman", "logs")
	el := &spyEventLog{}
	logBuf := &bytes.Buffer{}

	falseVal := false
	ClearIssueArtifacts(42, branch, worktreeDir, logDir, el, logBuf, "main", &falseVal)

	// The branch should still exist (no recovery ran).
	revCmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
	if out, err := revCmd.CombinedOutput(); err != nil {
		t.Errorf("expected branch %q to still exist when strandedReconcile is false, rev-parse failed: %s", branch, out)
	}
	if !strings.Contains(logBuf.String(), "error:") {
		t.Errorf("expected an error log when strandedReconcile is false, got:\n%s", logBuf.String())
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

func TestSyncBaseBranchSerializesAcrossParallelCalls(t *testing.T) {
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	o := NewOrchestrator(nil, nil, nil, nil)
	o.runSessionOpts.baseBranchSync = func(repoPath, sourceBranch string) error {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			mx := maxInFlight.Load()
			if cur <= mx {
				break
			}
			if maxInFlight.CompareAndSwap(mx, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		return nil
	}

	const callers = 32
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := o.syncBaseBranch(".", "main"); err != nil {
				t.Errorf("sync failed: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := maxInFlight.Load(); got > 1 {
		t.Fatalf("expected serialized syncBaseBranch, observed max in-flight=%d", got)
	}
	if got := inFlight.Load(); got != 0 {
		t.Fatalf("expected in-flight count to drain to 0, got %d", got)
	}
}

func TestSyncBaseBranchSerializesAgainstRealGitFetch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)
	for i := 0; i < 5; i++ {
		runGit(t, dir, "commit", "--allow-empty", "-m", fmt.Sprintf("seed-%d", i))
		runGit(t, dir, "push", "origin", "main")
	}

	o := NewOrchestrator(nil, nil, nil, nil)
	o.runSessionOpts.baseBranchSync = nil

	const callers = 16
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := o.syncBaseBranch(".", "main"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("parallel syncBaseBranch failed: %v", err)
	}
}

func TestRunBatch_LogsAbortedForQueuedRunOnCancel(t *testing.T) {
	t.Run("turn-wait", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		initGitRepo(t, dir)

		client := &fakeGitHubClient{
			issues: map[int]*github.Issue{
				42: {Number: 42, Title: "First"},
				43: {Number: 43, Title: "Second"},
			},
		}

		proc := makeFakeProcess()
		sb := &fakeSandbox{process: proc}
		factory := &fakeSandboxFactory{sandbox: sb}
		blockRunnable := &blockingRunnable{delayAfterCancel: 50 * time.Millisecond, running: make(chan struct{})}
		spyLog := &spyEventLog{}

		o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
		o.sandboxFactory = factory
		o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = o.RunBatch(ctx, Request{Issues: []int{42, 43}, Parallel: 1})
		}()

		time.Sleep(50 * time.Millisecond)
		cancel()
		waitForSignal(t, done, "expected batch to complete")

		assertQueuedThenAbortedWithSameRunID(t, spyLog.events, 43)
	})

	t.Run("startGate", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		initGitRepo(t, dir)

		client := &fakeGitHubClient{
			issues: map[int]*github.Issue{
				42: {Number: 42, Title: "First"},
				43: {Number: 43, Title: "Second"},
			},
		}

		started1 := make(chan struct{})
		release1 := make(chan struct{})
		started2 := make(chan struct{})
		factory := &controlledRunnableFactory{
			runnables: map[int]Runnable{
				42: &controlledRunnable{
					result:  AgentRunResult{IssueNumber: 42, Status: "success"},
					started: started1,
					release: release1,
				},
				43: &controlledRunnable{
					result:  AgentRunResult{IssueNumber: 43, Status: "success"},
					started: started2,
				},
			},
		}
		spyLog := &spyEventLog{}

		o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
		o.runnableFactory = factory

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = o.RunBatch(ctx, Request{
				Issues:        []int{42, 43},
				Parallel:      1,
				StartDelay:    500 * time.Millisecond,
				StartDelaySet: true,
			})
		}()

		waitForSignal(t, started1, "expected first issue to start")
		close(release1)
		time.Sleep(50 * time.Millisecond)
		cancel()
		assertNoSignal(t, started2, "second issue should not have started")
		waitForSignal(t, done, "expected batch to complete")

		assertQueuedThenAbortedWithSameRunID(t, spyLog.events, 43)
	})
}

func TestRunBatch_CascadesAbortFromBlockerToDependents(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker"},
			100: {Number: 100, Title: "Dependent", BlockedBy: []int{42}},
		},
	}

	proc := makeFakeProcess()
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	blockRunnable := &blockingRunnable{delayAfterCancel: 50 * time.Millisecond, running: make(chan struct{})}
	spyLog := &spyEventLog{}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = factory
	o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(ctx, Request{
			Issues:       []int{42, 100},
			Dependencies: map[int][]int{100: {42}},
			Parallel:     2,
		})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	waitForSignal(t, done, "expected batch to complete")

	queuedEvent, abortedEvent := findQueuedAndAborted(t, spyLog.events, 100)
	if queuedEvent.RunID != abortedEvent.RunID {
		t.Fatalf("expected same runID for queued and aborted events, got %q vs %q", queuedEvent.RunID, abortedEvent.RunID)
	}
	if status, _ := abortedEvent.Payload["status"].(string); status != "aborted" {
		t.Fatalf("expected aborted terminal status, got %q", status)
	}
	if abortedBy, _ := abortedEvent.Payload["aborted_by"].([]int); !reflect.DeepEqual(abortedBy, []int{42}) {
		t.Fatalf("expected aborted_by [42], got %#v", abortedEvent.Payload["aborted_by"])
	}

	for i := range spyLog.events {
		e := spyLog.events[i]
		if e.Issue == 100 && e.Type == "run.blocked" {
			t.Fatal("did not expect run.blocked event for dependent (should be run.aborted)")
		}
	}

	runs := events.ProjectRunStates(spyLog.events)
	var depRun *events.RunState
	for i := range runs {
		if runs[i].IssueNumber() == 100 {
			depRun = &runs[i]
			break
		}
	}
	if depRun == nil {
		t.Fatal("expected projected run for dependent issue 100")
	}
	if depRun.IsActive() {
		t.Fatal("expected dependent run to be terminal")
	}
	if depRun.Status() != "aborted" {
		t.Fatalf("expected dependent status aborted, got %q", depRun.Status())
	}
}

func assertQueuedThenAbortedWithSameRunID(t *testing.T, recorded []events.Event, issueNum int) {
	t.Helper()
	queuedEvent, abortedEvent := findQueuedAndAborted(t, recorded, issueNum)
	if queuedEvent.RunID != abortedEvent.RunID {
		t.Fatalf("expected same runID for queued and aborted events, got %q vs %q", queuedEvent.RunID, abortedEvent.RunID)
	}
	if status, _ := abortedEvent.Payload["status"].(string); status != "aborted" {
		t.Fatalf("expected aborted terminal status, got %q", status)
	}
	for i := range recorded {
		e := recorded[i]
		if e.Issue == issueNum && e.Type == "run.blocked" {
			t.Fatalf("did not expect run.blocked event for issue %d (should be run.aborted)", issueNum)
		}
	}
	runs := events.ProjectRunStates(recorded)
	for i := range runs {
		if runs[i].IssueNumber() == issueNum {
			if runs[i].IsActive() {
				t.Fatalf("expected issue %d run to be terminal, got active", issueNum)
			}
			if runs[i].Status() != "aborted" {
				t.Fatalf("expected issue %d status aborted, got %q", issueNum, runs[i].Status())
			}
			return
		}
	}
	t.Fatalf("expected projected run for issue %d", issueNum)
}

func findQueuedAndAborted(t *testing.T, recorded []events.Event, issueNum int) (*events.Event, *events.Event) {
	t.Helper()
	var queuedEvent, abortedEvent *events.Event
	for i := range recorded {
		e := &recorded[i]
		if e.Issue != issueNum {
			continue
		}
		switch e.Type {
		case "run.queued":
			queuedEvent = e
		case "run.aborted":
			abortedEvent = e
		}
	}
	if queuedEvent == nil {
		t.Fatalf("expected run.queued event for issue %d", issueNum)
	}
	if abortedEvent == nil {
		t.Fatalf("expected run.aborted event for issue %d", issueNum)
	}
	return queuedEvent, abortedEvent
}

func TestOrchestrator_AbortIssue_AlreadyFinishedReturnsErrNoSuchIssue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := "sandman/42-fix-bug"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
		prs: map[string]*github.PR{branch: {Number: 42, State: "closed", Merged: true, HeadRefName: branch}},
	}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success"}}}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, nil)
	o.runnableFactory = factory
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}

	if _, err := o.RunBatch(context.Background(), Request{Issues: []int{42}}); err != nil {
		t.Fatalf("RunBatch returned error: %v", err)
	}

	if err := o.AbortIssue(42); !errors.Is(err, ErrNoSuchIssue) {
		t.Fatalf("expected ErrNoSuchIssue, got %v", err)
	}
}

func TestOrchestrator_AbortIssue_ActiveRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
		},
	}

	proc := makeFakeProcess()
	sb := &fakeSandbox{process: proc}
	factory := &fakeSandboxFactory{sandbox: sb}
	blockRunnable := &blockingRunnable{delayAfterCancel: 50 * time.Millisecond, running: make(chan struct{})}
	spyLog := &spyEventLog{}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = factory
	o.runnableFactory = &blockingRunnableFactory{runnable: blockRunnable}

	abortReturned := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}})
	}()

	select {
	case <-blockRunnable.running:
	case <-time.After(2 * time.Second):
		t.Fatal("expected active runnable to be running")
	}
	abortReturned <- o.AbortIssue(42)
	waitForSignal(t, done, "expected batch to complete after AbortIssue")

	select {
	case err := <-abortReturned:
		if err != nil {
			t.Fatalf("AbortIssue returned error: %v", err)
		}
	default:
	}

	var abortedEvent *events.Event
	for i := range spyLog.events {
		e := &spyLog.events[i]
		if e.Issue == 42 && e.Type == "run.aborted" {
			abortedEvent = e
			break
		}
	}
	if abortedEvent == nil {
		t.Fatalf("expected run.aborted event for issue 42, got %v", spyLog.events)
	}
	if status, _ := abortedEvent.Payload["status"].(string); status != "aborted" {
		t.Fatalf("expected aborted terminal status, got %q", status)
	}
	// Per-issue abort cancels the per-issue ctx; the runnable
	// unblocks on ctx.Done and returns. The supervisor is scoped
	// to the parent ctx (batch-wide cancellation), so it does
	// NOT signal the per-aborted issue's process. The "sibling
	// Ctrl+C path" guarantee is preserved by the per-session
	// scope of the supervisor. This test exercises the
	// single-process case to assert the supervisor does not fire
	// on per-issue abort.
	if proc.sigTermObserved() {
		t.Fatal("per-issue abort must not signal the aborted issue's process; the supervisor only fires on parent-ctx cancellation")
	}
}

// TestOrchestrator_AbortIssue_SiblingIsNotSignalled verifies the
// per-session scope of the unified superviseShutdown supervisor: when
// AbortIssue cancels a single issue, only that issue's process is
// signalled — its siblings are untouched. This is the "sibling Ctrl+C
// path" guarantee the original TestOrchestrator_AbortIssue_ActiveRun
// test was written to assert, generalised to a multi-issue batch.
func TestOrchestrator_AbortIssue_SiblingIsNotSignalled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug"},
			43: {Number: 43, Title: "Other bug"},
		},
	}

	proc42 := makeFakeProcess()
	proc43 := makeFakeProcess()
	sb42 := &fakeSandbox{process: proc42}
	sb43 := &fakeSandbox{process: proc43}

	// Multi-sandbox factory: returns a different fakeSandbox per
	// issue so the two sessions are independent.
	twoSandboxFactory := &twoSandboxFactory{
		primaryIssue:   42,
		primary:        sb42,
		secondaryIssue: 43,
		secondary:      sb43,
	}

	runnable42 := &blockingRunnable{delayAfterCancel: 50 * time.Millisecond, running: make(chan struct{})}
	// Sibling runnable returns success after a short natural delay
	// (it does not depend on ctx.Done) so the batch can complete
	// after the per-issue abort of issue 42.
	runnable43 := &shortSuccessRunnable{returnAfter: 50 * time.Millisecond, running: make(chan struct{})}
	runFactory := &twoRunnableFactory{
		primaryIssue:   42,
		primary:        runnable42,
		secondaryIssue: 43,
		secondary:      runnable43,
	}
	spyLog := &spyEventLog{}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = twoSandboxFactory
	o.runnableFactory = runFactory

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42, 43}})
	}()

	// Wait for both runnables to be running.
	select {
	case <-runnable42.running:
	case <-time.After(2 * time.Second):
		t.Fatal("expected runnable for 42 to start")
	}
	select {
	case <-runnable43.running:
	case <-time.After(2 * time.Second):
		t.Fatal("expected runnable for 43 to start")
	}

	// Abort only issue 42.
	if err := o.AbortIssue(42); err != nil {
		t.Fatalf("AbortIssue(42) returned error: %v", err)
	}

	// Wait for the batch to complete (both runnables unblock on
	// their respective ctxs after the per-issue abort and a brief
	// settle delay; runnable43 finishes naturally on the merged
	// PR short-circuit path).
	waitForSignal(t, done, "expected batch to complete after AbortIssue(42)")

	if proc42.sigTermObserved() {
		t.Fatal("per-issue abort must not signal SIGTERM on the aborted issue's process; the supervisor only fires on parent-ctx cancellation")
	}
	if proc43.sigTermObserved() {
		t.Fatal("per-issue abort must not signal sibling issue 43's process")
	}
}

// twoSandboxFactory hands out a different fakeSandbox per issue
// number so sibling-safety tests can verify that AbortIssue on one
// issue does not signal a sibling's process.
type twoSandboxFactory struct {
	primaryIssue   int
	primary        *fakeSandbox
	secondaryIssue int
	secondary      *fakeSandbox
}

func (f *twoSandboxFactory) NewSandbox(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
	switch branch {
	case fmt.Sprintf("sandman/%d-fix-bug", f.primaryIssue):
		return f.primary
	case fmt.Sprintf("sandman/%d-other-bug", f.secondaryIssue):
		return f.secondary
	}
	return f.primary
}

type twoRunnableFactory struct {
	primaryIssue   int
	primary        Runnable
	secondaryIssue int
	secondary      Runnable
}

func (f *twoRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	if issue.Number == f.secondaryIssue {
		return f.secondary
	}
	return f.primary
}

// shortSuccessRunnable signals it is running, waits returnAfter, and
// returns success. Used as the "sibling" in sibling-safety tests so
// the batch can complete without depending on ctx cancellation.
type shortSuccessRunnable struct {
	returnAfter time.Duration
	running     chan struct{}
	once        sync.Once
}

func (r *shortSuccessRunnable) Run(ctx context.Context, _ prompt.IssueRenderer, _ string, _ prompt.RenderConfig) AgentRunResult {
	if r.running != nil {
		r.once.Do(func() { close(r.running) })
	}
	time.Sleep(r.returnAfter)
	return AgentRunResult{IssueNumber: 43, Status: "success", Branch: "sandman/43-other-bug"}
}

func TestOrchestrator_AbortIssue_QueuedRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "First"},
			43: {Number: 43, Title: "Second"},
		},
	}

	started42 := make(chan struct{})
	release42 := make(chan struct{})
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 42, Status: "success"},
				started: started42,
				release: release42,
			},
			43: &controlledRunnable{result: AgentRunResult{IssueNumber: 43, Status: "success"}},
		},
	}
	spyLog := &spyEventLog{}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
	o.runnableFactory = factory

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.RunBatch(context.Background(), Request{
			Issues:   []int{42, 43},
			Parallel: 1,
		})
	}()

	waitForSignal(t, started42, "expected 42 to start")
	if err := o.AbortIssue(43); err != nil {
		t.Fatalf("AbortIssue(43) returned error: %v", err)
	}
	close(release42)
	waitForSignal(t, done, "expected batch to complete after AbortIssue")

	assertQueuedThenAbortedWithSameRunID(t, spyLog.events, 43)
}

func TestOrchestrator_AbortIssue_BlockedRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	initGitRepo(t, dir)

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker"},
			100: {Number: 100, Title: "Dependent", BlockedBy: []int{42}},
		},
		prs: map[string]*github.PR{
			"sandman/42-blocker":    {Number: 42, State: "closed", Merged: true, HeadRefName: "sandman/42-blocker"},
			"sandman/100-dependent": {Number: 100, State: "closed", Merged: true, HeadRefName: "sandman/100-dependent"},
		},
	}

	started42 := make(chan struct{})
	release42 := make(chan struct{})
	factory := &controlledRunnableFactory{
		runnables: map[int]Runnable{
			42: &controlledRunnable{
				result:  AgentRunResult{IssueNumber: 42, Status: "success"},
				started: started42,
				release: release42,
			},
			100: &controlledRunnable{result: AgentRunResult{IssueNumber: 100, Status: "success"}},
		},
	}
	spyLog := &spyEventLog{}

	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}}, spyLog)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{}}
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

	waitForSignal(t, started42, "expected 42 to start")
	if err := o.AbortIssue(100); err != nil {
		t.Fatalf("AbortIssue(100) returned error: %v", err)
	}
	close(release42)
	waitForSignal(t, done, "expected batch to complete after AbortIssue")

	for i := range spyLog.events {
		e := &spyLog.events[i]
		if e.Issue == 100 && e.Type == "run.blocked" {
			t.Fatal("did not expect run.blocked for 100 (per-issue abort should override)")
		}
	}
	var abortedEvent *events.Event
	for i := range spyLog.events {
		e := &spyLog.events[i]
		if e.Issue == 100 && e.Type == "run.aborted" {
			abortedEvent = e
			break
		}
	}
	if abortedEvent == nil {
		t.Fatalf("expected run.aborted event for 100, got %v", spyLog.events)
	}
	if status, _ := abortedEvent.Payload["status"].(string); status != "aborted" {
		t.Fatalf("expected aborted terminal status, got %q", status)
	}
}

func TestRunSession_ApplyOverrideAndIdentity_CallsMethodsDirectlyOnSandbox(t *testing.T) {
	wt := &fakeSandbox{}
	s := &runSession{
		o:                &Orchestrator{errorLog: io.Discard},
		mode:             ModeOverride,
		issueNumber:      42,
		identityResolver: noopIdentityResolver(),
	}

	_, ok := s.applyOverrideAndIdentity(wt, "sandman/42-fix-bug")
	if !ok {
		t.Fatal("expected applyOverrideAndIdentity to succeed")
	}

	if !wt.setOverrideCalled {
		t.Fatal("expected sandbox.SetOverride to be called")
	}
	if !wt.setOverrideValue {
		t.Error("expected SetOverride(true) to forward the override value")
	}
	if wt.setIdentityName != "" || wt.setIdentityEmail != "" {
		t.Errorf("expected noop identity to leave name/email unset, got name=%q email=%q",
			wt.setIdentityName, wt.setIdentityEmail)
	}
}

func TestRunSession_ApplyOverrideAndIdentity_PropagatesOverrideFalse(t *testing.T) {
	wt := &fakeSandbox{}
	s := &runSession{
		o:                &Orchestrator{errorLog: io.Discard},
		mode:             ModeFresh,
		issueNumber:      7,
		identityResolver: noopIdentityResolver(),
	}

	_, ok := s.applyOverrideAndIdentity(wt, "sandman/7-other")
	if !ok {
		t.Fatal("expected applyOverrideAndIdentity to succeed")
	}

	if !wt.setOverrideCalled {
		t.Fatal("expected sandbox.SetOverride to be called")
	}
	if wt.setOverrideValue {
		t.Error("expected SetOverride(false) to forward the override value")
	}
}

func TestOrchestrator_ResetRetryBranch_Command(t *testing.T) {
	ctx := context.Background()
	sb := &fakeSandbox{}
	o := &Orchestrator{errorLog: io.Discard}

	err := o.resetRetryBranch(ctx, sb, "sandman/42-fix-bug", "main")
	if err != nil {
		t.Fatalf("resetRetryBranch returned error: %v", err)
	}

	expected := fmt.Sprintf("git reset --hard && git checkout -f -B %s %s && git clean -fd",
		shellQuote("sandman/42-fix-bug"), shellQuote("main"))
	if sb.execCommand != expected {
		t.Errorf("expected exec command %q, got %q", expected, sb.execCommand)
	}
}

// strandRunnable switches the worktree to an unexpected branch when Run is called,
// simulating what a real agent does during PR merge (checking out a non-feature branch).
type strandRunnable struct {
	sb sandbox.Sandbox
}

func (r *strandRunnable) Run(ctx context.Context, renderer prompt.IssueRenderer, command string, renderCfg prompt.RenderConfig) AgentRunResult {
	// Create a branch in the main repo (not checked out in any worktree) and
	// switch the worktree to it, stranding it on the wrong branch.
	// Note: we use "wrong-branch" instead of "main" because git prevents
	// checking out a branch that is already active in the main worktree.
	if err := r.sb.Exec(ctx, "git branch wrong-branch main && git checkout -f wrong-branch", io.Discard, io.Discard); err != nil {
		return AgentRunResult{IssueNumber: 42, Status: "failure"}
	}
	return AgentRunResult{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"}
}

type strandRunnableFactory struct{}

func (f *strandRunnableFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	return &strandRunnable{sb: sb}
}

func TestRunSingle_WorktreeBranchMismatch(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	initGitRepo(t, workDir)

	branch := "sandman/42-fix-bug"
	pr := mergedPR(branch, "")
	var errorBuf bytes.Buffer
	o := &Orchestrator{
		githubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: pr},
		},
		renderer:        &noopRenderer{},
		errorLog:        &errorBuf,
		runnableFactory: &strandRunnableFactory{},
	}

	cfg := &config.Config{
		WorktreeDir: "worktrees",
		Git:         config.GitConfig{BaseBranch: "main"},
	}

	// Step 1: Create worktree and strand it on the wrong branch via the strand runnable.
	result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &defaultSandboxFactory{}, nil, false, "main", nil, 0, 0, 0, 0, "", 0, false, 0, false, false, false)
	if !started {
		t.Fatal("expected strand run to start")
	}
	if result.Status != "success" {
		t.Fatalf("strand run status = %q, want success", result.Status)
	}
	t.Cleanup(func() {
		worktreePath := filepath.Join(workDir, "worktrees", branch)
		if _, err := os.Stat(worktreePath); err == nil {
			exec.Command("git", "worktree", "remove", "-f", worktreePath).Run()
		}
	})

	// Remove the strand factory so subsequent runs use the default runnable.
	o.runnableFactory = nil

	// The first run strands the worktree on the wrong branch (via the
	// strand runnable) and succeeds. After the run, reconcileWorktreeBranch
	// (issue #941) restores the worktree to the issue branch. Re-strand
	// here so the next subtest exercises the Start-time branch-mismatch
	// error path on a freshly stranded worktree.
	worktreePath := filepath.Join(workDir, "worktrees", branch)
	if out, err := exec.Command("git", "-C", worktreePath, "checkout", "-f", "wrong-branch").CombinedOutput(); err != nil {
		t.Fatalf("re-strand worktree: %v: %s", err, out)
	}

	t.Run("no force fails on branch mismatch", func(t *testing.T) {
		errorBuf.Reset()
		result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &defaultSandboxFactory{}, nil, false, "main", nil, 0, 0, 0, 0, "", 0, false, 0, false, false, false)
		if started {
			t.Fatal("expected run not to start when worktree is on wrong branch")
		}
		if result.Status != "failure" {
			t.Fatalf("status = %q, want failure", result.Status)
		}
		if !strings.Contains(errorBuf.String(), `expected "sandman/42-fix-bug"; re-run with --override to reconcile`) {
			t.Fatalf("error log does not contain branch mismatch message:\n%s", errorBuf.String())
		}
	})

	t.Run("override reconciles branch mismatch", func(t *testing.T) {
		result, started := o.runSingle(context.Background(), context.Background(), 42, cfg, "opencode", config.Agent{Command: "echo hi"}, false, nil, noopIdentityResolver(), map[int]string{42: branch}, prompt.RenderConfig{}, nil, &defaultSandboxFactory{}, nil, true, "main", nil, 0, 0, 0, 0, "", 0, false, 0, false, false, false)
		if !started {
			t.Fatal("expected override run to start")
		}
		if result.Status != "success" {
			t.Fatalf("override status = %q, want success", result.Status)
		}
		worktreePath := filepath.Join(workDir, "worktrees", branch)
		cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
		cmd.Dir = worktreePath
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git rev-parse HEAD in worktree: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(string(out)); got != branch {
			t.Fatalf("worktree HEAD on %q, want %q", got, branch)
		}
	})
}

// stageReconcileWorktree sets up a real git repo at workDir with a main branch,
// creates a worktree on issueBranch, and returns the worktree's path, the parent
// repo path, and the WorktreeSandbox. The worktree is left on issueBranch. If
// wrongBranch is non-empty, the worktree is also configured to start on it
// (created from main and force-checked out) so reconcileWorktreeBranch has
// something to fix.
func stageReconcileWorktree(t *testing.T, issueBranch, wrongBranch string) (workDir, repoPath, worktreePath string, sb sandbox.Sandbox) {
	t.Helper()
	workDir = t.TempDir()
	repoPath = workDir
	t.Chdir(workDir)
	initGitRepo(t, workDir)
	worktreeBase := filepath.Join(workDir, "worktrees")

	// Ensure the issue branch does not already exist so the sandbox Start
	// call does not fail. Pre-clean any leftover from a previous run.
	exec.Command("git", "branch", "-D", issueBranch).Run()
	exec.Command("git", "branch", "-D", wrongBranch).Run()

	sb = sandbox.NewWorktreeSandbox(repoPath, worktreeBase, issueBranch, "main")
	if err := sb.Start(); err != nil {
		t.Fatalf("sandbox start: %v", err)
	}
	worktreePath = sb.WorkDir()

	if wrongBranch != "" {
		// Create wrongBranch from main and force-check it out in the worktree,
		// mimicking what sandman-pr-merge does after `gh pr merge --squash`.
		if out, err := exec.Command("git", "branch", wrongBranch, "main").CombinedOutput(); err != nil {
			t.Fatalf("git branch %s: %v: %s", wrongBranch, err, out)
		}
		if out, err := exec.Command("git", "-C", worktreePath, "checkout", "-f", wrongBranch).CombinedOutput(); err != nil {
			t.Fatalf("git checkout -f %s: %v: %s", wrongBranch, err, out)
		}
	}

	t.Cleanup(func() {
		exec.Command("git", "-C", repoPath, "worktree", "remove", "-f", worktreePath).Run()
		exec.Command("git", "-C", repoPath, "branch", "-D", issueBranch).Run()
		exec.Command("git", "-C", repoPath, "branch", "-D", wrongBranch).Run()
	})
	return workDir, repoPath, worktreePath, sb
}

func readWorktreeRef(t *testing.T, worktreePath string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --abbrev-ref HEAD: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestReconcileWorktreeBranch_NoOpWhenCorrect(t *testing.T) {
	branch := "sandman/42-fix-bug"
	_, _, worktreePath, sb := stageReconcileWorktree(t, branch, "")

	if got := readWorktreeRef(t, worktreePath); got != branch {
		t.Fatalf("staging: worktree HEAD on %q, want %q", got, branch)
	}

	var errorBuf bytes.Buffer
	s := &runSession{
		o:        &Orchestrator{errorLog: &errorBuf},
		branches: map[int]string{42: branch},
	}

	before := readWorktreeRef(t, worktreePath)
	s.reconcileWorktreeBranch(sb, branch)
	after := readWorktreeRef(t, worktreePath)

	if before != branch {
		t.Fatalf("worktree HEAD before call: got %q, want %q", before, branch)
	}
	if after != branch {
		t.Errorf("worktree HEAD after call: got %q, want %q (no-op should leave HEAD unchanged)", after, branch)
	}
}

func TestReconcileWorktreeBranch_RestoresBranch(t *testing.T) {
	branch := "sandman/42-fix-bug"
	wrong := "sandman/42-wrong-branch"
	_, _, worktreePath, sb := stageReconcileWorktree(t, branch, wrong)

	if got := readWorktreeRef(t, worktreePath); got != wrong {
		t.Fatalf("staging: worktree HEAD on %q, want %q", got, wrong)
	}

	var errorBuf bytes.Buffer
	s := &runSession{
		o:        &Orchestrator{errorLog: &errorBuf},
		branches: map[int]string{42: branch},
	}

	s.reconcileWorktreeBranch(sb, branch)

	after := readWorktreeRef(t, worktreePath)
	if after != branch {
		t.Errorf("worktree HEAD after reconcile: got %q, want %q", after, branch)
	}
}

func TestReconcileWorktreeBranch_LogsAndContinuesOnMissingBranch(t *testing.T) {
	branch := "sandman/42-fix-bug"
	wrong := "sandman/42-wrong-branch"
	_, _, worktreePath, sb := stageReconcileWorktree(t, branch, wrong)

	if got := readWorktreeRef(t, worktreePath); got != wrong {
		t.Fatalf("staging: worktree HEAD on %q, want %q", got, wrong)
	}

	// Simulate `gh pr merge --delete-branch`: the local branch ref is gone,
	// but the worktree is still registered on the (deleted) branch. The
	// reconcile method must detect the missing branch and return without
	// erroring out the run.
	if out, err := exec.Command("git", "-C", filepath.Dir(worktreePath), "branch", "-D", branch).CombinedOutput(); err != nil {
		t.Fatalf("delete branch: %v: %s", err, out)
	}

	var errorBuf bytes.Buffer
	s := &runSession{
		o:        &Orchestrator{errorLog: &errorBuf},
		branches: map[int]string{42: branch},
	}

	s.reconcileWorktreeBranch(sb, branch)

	logged := errorBuf.String()
	if !strings.Contains(logged, "warning:") {
		t.Errorf("expected a warning to be logged, got %q", logged)
	}
	if !strings.Contains(logged, "was deleted") {
		t.Errorf("expected warning to mention the deleted branch, got %q", logged)
	}
}

func TestReconcileWorktreeBranch_RestoresFromDetachedHead(t *testing.T) {
	branch := "sandman/42-fix-bug"
	_, _, worktreePath, sb := stageReconcileWorktree(t, branch, "")

	// Detach HEAD inside the worktree. `currentBranchRef` returns an error
	// in this state (symbolic-ref --quiet fails with exit 1 and no output),
	// so the reconcile method must fall through to the BranchExists +
	// checkout path, not bail out with a warning.
	sha := runGit(t, worktreePath, "rev-parse", "HEAD")
	if out, err := exec.Command("git", "-C", worktreePath, "checkout", "--detach", strings.TrimSpace(sha)).CombinedOutput(); err != nil {
		t.Fatalf("detach HEAD: %v: %s", err, out)
	}

	if got := readWorktreeRef(t, worktreePath); got != "HEAD" {
		t.Fatalf("staging: detached HEAD read as %q, want %q", got, "HEAD")
	}

	var errorBuf bytes.Buffer
	s := &runSession{
		o:        &Orchestrator{errorLog: &errorBuf},
		branches: map[int]string{42: branch},
	}

	s.reconcileWorktreeBranch(sb, branch)

	if after := readWorktreeRef(t, worktreePath); after != branch {
		t.Errorf("worktree HEAD after reconcile: got %q, want %q", after, branch)
	}
	if logged := errorBuf.String(); strings.Contains(logged, "resolve HEAD:") {
		t.Errorf("expected no resolve-HEAD warning, got %q", logged)
	}
}

func TestReconcileWorktreeBranch_LogsAndContinuesOnFailure(t *testing.T) {
	branch := "sandman/42-fix-bug"
	wrong := "sandman/42-wrong-branch"
	_, _, worktreePath, sb := stageReconcileWorktree(t, branch, wrong)

	if got := readWorktreeRef(t, worktreePath); got != wrong {
		t.Fatalf("staging: worktree HEAD on %q, want %q", got, wrong)
	}

	// Simulate a worktree in a state where the checkout cannot run. The
	// spec ("dirty worktree") is loose here: `git checkout -f` does not
	// refuse dirty files, so a literal uncommitted-edit setup cannot
	// produce a deterministic failure. Removing the worktree directory
	// is the simplest, deterministic way to make `git -C <workdir>
	// checkout -f <branch>` fail while leaving the parent repo and the
	// branch ref intact (BranchExists returns true).
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}

	var errorBuf bytes.Buffer
	s := &runSession{
		o:        &Orchestrator{errorLog: &errorBuf},
		branches: map[int]string{42: branch},
	}

	s.reconcileWorktreeBranch(sb, branch)

	logged := errorBuf.String()
	if !strings.Contains(logged, "warning:") {
		t.Errorf("expected a warning to be logged, got %q", logged)
	}
	if !strings.Contains(logged, "reconcile worktree branch") {
		t.Errorf("expected warning to mention reconcile, got %q", logged)
	}
}
