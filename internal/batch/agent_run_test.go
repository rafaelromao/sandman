package batch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

type fakeProcess struct {
	mu            sync.Mutex
	sigTermCalled bool
	killCalled    bool
	killed        chan struct{}
}

func makeFakeProcess() *fakeProcess {
	return &fakeProcess{killed: make(chan struct{})}
}

func (p *fakeProcess) Signal(sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sig == syscall.SIGTERM {
		p.sigTermCalled = true
	}
	return nil
}

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.killCalled = true
	killed := p.killed
	p.mu.Unlock()
	if killed != nil {
		select {
		case <-killed:
		default:
			close(killed)
		}
	}
	return nil
}

func (p *fakeProcess) WaitDone() <-chan struct{} {
	if p.killed == nil {
		p.killed = make(chan struct{})
	}
	return p.killed
}

// sigTermObserved returns true if Signal(SIGTERM) has been called. Safe
// to read concurrently with Signal/Kill.
func (p *fakeProcess) sigTermObserved() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sigTermCalled
}

// killObserved returns true if Kill has been called. Safe to read
// concurrently with Signal/Kill.
func (p *fakeProcess) killObserved() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killCalled
}

type fakeSandbox struct {
	mu sync.Mutex

	startCalled        bool
	startErr           error
	writePromptCalled  bool
	writePromptContent string
	writePromptError   error

	execCalled                 bool
	execInteractiveCalled      bool
	execCommand                string
	execError                  error
	execStdout                 string
	execStderr                 string
	process                    *fakeProcess
	stopCalled                 bool
	workDir                    string
	setOverrideCalled          bool
	setOverrideValue           bool
	setStrandedReconcileCalled bool
	setStrandedReconcileValue  bool
	setIdentityName            string
	setIdentityEmail           string
}

func (f *fakeSandbox) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalled = true
	return f.startErr
}
func (f *fakeSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	f.execCalled = true
	f.execCommand = command
	stdoutStr := f.execStdout
	stderrStr := f.execStderr
	execErr := f.execError
	f.mu.Unlock()
	if stdout != nil && stdoutStr != "" {
		stdout.Write([]byte(stdoutStr))
	}
	if stderr != nil && stderrStr != "" {
		stderr.Write([]byte(stderrStr))
	}
	return execErr
}
func (f *fakeSandbox) ExecInteractive(ctx context.Context, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execInteractiveCalled = true
	f.execCommand = command
	return f.execError
}
func (f *fakeSandbox) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalled = true
	return nil
}
func (f *fakeSandbox) WorkDir() string { return f.workDir }
func (f *fakeSandbox) RepoPath() string {
	if f.workDir == "" {
		return ""
	}
	return filepath.Dir(f.workDir)
}
func (f *fakeSandbox) WritePrompt(content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writePromptCalled = true
	f.writePromptContent = content
	return f.writePromptError
}
func (f *fakeSandbox) Process() sandbox.Process {
	if f.process == nil {
		return makeFakeProcess()
	}
	return f.process
}
func (f *fakeSandbox) SetOverride(override bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setOverrideCalled = true
	f.setOverrideValue = override
}
func (f *fakeSandbox) SetStrandedReconcile(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setStrandedReconcileCalled = true
	f.setStrandedReconcileValue = enabled
}
func (f *fakeSandbox) SetGitIdentity(name, email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setIdentityName = name
	f.setIdentityEmail = email
}

// Ensure fakeSandbox satisfies sandbox.Sandbox.
var _ sandbox.Sandbox = (*fakeSandbox)(nil)

type spyRenderer struct {
	called bool
	cfg    prompt.RenderConfig
	data   prompt.IssueData
	result string
	err    error
}

func (s *spyRenderer) Render(cfg prompt.RenderConfig, data prompt.IssueData) (string, error) {
	s.called = true
	s.cfg = cfg
	s.data = data
	return s.result, s.err
}

func (s *spyRenderer) RenderReview(cfg prompt.RenderConfig, data prompt.PRData) (string, error) {
	return "", nil
}

func TestAgentRun_Prepare_PopulatesBranchFields(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.baseBranch = "main"

	if err := run.Prepare(spy, prompt.RenderConfig{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.data.SourceBranch != "sandman/42-fix-bug" {
		t.Errorf("expected SourceBranch 'sandman/42-fix-bug', got %q", spy.data.SourceBranch)
	}
	if spy.data.BaseBranch != "main" {
		t.Errorf("expected BaseBranch 'main', got %q", spy.data.BaseBranch)
	}
}

func TestAgentRun_Prepare_RendersAndWritesPrompt(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}
	renderCfg := prompt.RenderConfig{PromptFile: ".sandman/prompt.md"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Prepare(spy, renderCfg); err != nil {
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
	if spy.cfg.PromptFile != ".sandman/prompt.md" {
		t.Errorf("expected PromptFile in render config, got %q", spy.cfg.PromptFile)
	}

	if !sb.writePromptCalled {
		t.Fatal("expected WritePrompt to be called")
	}
	if sb.writePromptContent != "rendered prompt" {
		t.Errorf("expected prompt content 'rendered prompt', got %q", sb.writePromptContent)
	}
}

func TestAgentRun_Prepare_RenderError(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{err: errors.New("render failed")}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Prepare(spy, prompt.RenderConfig{}); err == nil {
		t.Fatal("expected error when render fails")
	}
}

func TestAgentRun_Prepare_WritePromptError(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{writePromptError: errors.New("write failed")}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Prepare(spy, prompt.RenderConfig{}); err == nil {
		t.Fatal("expected error when WritePrompt fails")
	}
}

func TestAgentRun_Execute_RunsCommand(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !sb.execCalled {
		t.Fatal("expected Exec to be called")
	}
	if sb.execCommand != "echo hello" {
		t.Errorf("expected command 'echo hello', got %q", sb.execCommand)
	}
}

func TestAgentRun_Run_IncludesModelFlagForBuiltInPreset(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.preset = "opencode"
	run.model = "gpt-4.1"

	res := run.Run(context.Background(), spy, config.BuiltInAgentPresets["opencode"].Command, prompt.RenderConfig{})
	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}

	want := `opencode run -m gpt-4.1 "$(cat .sandman/task.md)"`
	if sb.execCommand != want {
		t.Errorf("expected command %q, got %q", want, sb.execCommand)
	}
}

func TestAgentRun_Run_DoesNotInjectModelFlagForCustomCommand(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.preset = "opencode"
	run.model = "gpt-4.1"

	command := `opencode run "$(cat {{.PromptFile}})"`
	res := run.Run(context.Background(), spy, command, prompt.RenderConfig{})
	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}

	want := `opencode run "$(cat .sandman/task.md)"`
	if sb.execCommand != want {
		t.Errorf("expected command %q, got %q", want, sb.execCommand)
	}
}

func TestAgentRun_Execute_Failure(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execError: errors.New("agent failed")}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Execute(context.Background(), "exit 1", io.Discard, io.Discard); err == nil {
		t.Fatal("expected error when exec fails")
	}
}

func TestAgentRun_Execute_PrefixesOutput(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "hello world\n"}
	var outBuf bytes.Buffer

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.runID = "issue-42"
	if err := run.Execute(context.Background(), "echo hello", &outBuf, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := outBuf.String()
	if !strings.Contains(output, "[issue-42]") {
		t.Errorf("expected output to contain runID prefix, got %q", output)
	}
	if !strings.Contains(output, "hello world") {
		t.Errorf("expected output to contain agent text, got %q", output)
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "[issue-42]") {
			t.Errorf("expected every line to start with [issue-42], got %q", line)
		}
	}
}

func TestAgentRun_Execute_WritesLogFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "hello world\n", execStderr: "error line\n"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.runID = "issue-42"
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "hello world") {
		t.Errorf("expected log to contain stdout, got %q", content)
	}
	if !strings.Contains(content, "error line") {
		t.Errorf("expected log to contain stderr, got %q", content)
	}
	if !strings.Contains(content, "[issue-42]") {
		t.Errorf("expected log to be prefixed, got %q", content)
	}
}

func TestAgentRun_Execute_UsesLayoutLogDir_IgnoresCWD(t *testing.T) {
	repo := t.TempDir()
	other := t.TempDir()
	t.Chdir(other)

	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "hello world\n"}

	run := NewAgentRunWithLayout(issue, "sandman/42-fix-bug", sb, paths.NewLayout(&config.Config{}, repo))
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := filepath.Join(repo, ".sandman", "logs", "42.log")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected log file at %s: %v", wantPath, err)
	}
	otherLogPath := filepath.Join(other, ".sandman", "logs", "42.log")
	if _, err := os.Stat(otherLogPath); !os.IsNotExist(err) {
		t.Errorf("did not expect log file under CWD %s", otherLogPath)
	}
}

func TestAgentRun_Execute_PromptOnlyUsesSanitizedFilename(t *testing.T) {
	repo := t.TempDir()
	sb := &fakeSandbox{execStdout: "hello world\n"}

	run := NewAgentRunWithLayout(nil, "sandman/42 foo", sb, paths.NewLayout(&config.Config{}, repo))
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := filepath.Join(repo, ".sandman", "logs", "sandman-42-foo.log")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected sanitized log file at %s: %v", wantPath, err)
	}
}

func TestAgentRun_Execute_PromptOnlyEmptyBranchUsesRunID(t *testing.T) {
	repo := t.TempDir()
	sb := &fakeSandbox{execStdout: "hello world\n"}

	run := NewAgentRunWithLayout(nil, "", sb, paths.NewLayout(&config.Config{}, repo))
	run.runID = "prompt-run-123"
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := filepath.Join(repo, ".sandman", "logs", "prompt-run-123.log")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected runID log file at %s: %v", wantPath, err)
	}
}

func TestAgentRun_Run_Success(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}
	renderCfg := prompt.RenderConfig{}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "echo hello", renderCfg)

	if res.Status != "success" {
		t.Errorf("expected status success, got %s", res.Status)
	}
	if res.IssueNumber != 42 {
		t.Errorf("expected issue 42, got %d", res.IssueNumber)
	}
	if !spy.called {
		t.Fatal("expected renderer to be called")
	}
	if !sb.execCalled {
		t.Fatal("expected Exec to be called")
	}
}

func TestAgentRun_Run_PrepareFailure(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{err: errors.New("render failed")}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "echo hello", prompt.RenderConfig{})

	if res.Status != "failure" {
		t.Errorf("expected status failure, got %s", res.Status)
	}
	if sb.execCalled {
		t.Error("expected Exec not to be called when Prepare fails")
	}
}

func TestAgentRun_Run_ExecuteFailure(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execError: errors.New("agent failed")}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "exit 1", prompt.RenderConfig{})

	if res.Status != "failure" {
		t.Errorf("expected status failure, got %s", res.Status)
	}
}

func TestAgentRun_Execute_WritesToOutputWriter(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "hello world\n"}
	var buf bytes.Buffer

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.runID = "issue-42"
	run.outputWriter = &buf
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "hello world") {
		t.Errorf("expected output writer to contain agent output, got %q", output)
	}

	if !strings.Contains(output, "[issue-42]") {
		t.Errorf("expected output writer to contain prefixed output, got %q", output)
	}
}

func TestAgentRun_Run_UsesExecEvenWhenInteractiveRequested(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "echo hello", prompt.RenderConfig{})

	if res.Status != "success" {
		t.Errorf("expected status success, got %s", res.Status)
	}
	if !sb.execCalled {
		t.Fatal("expected Exec to be called")
	}
	if sb.execCommand != "echo hello" {
		t.Errorf("expected command 'echo hello', got %q", sb.execCommand)
	}
	if sb.execInteractiveCalled {
		t.Fatal("expected ExecInteractive not to be called")
	}
}

func TestAgentRun_Run_InjectsPromptFileIntoCommandTemplate(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{workDir: "/tmp/worktrees/fix-bug"}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "opencode --prompt-file {{.PromptFile}}", prompt.RenderConfig{
		PromptFile: ".sandman/prompt.md",
	})

	if res.Status != "success" {
		t.Errorf("expected status success, got %s", res.Status)
	}
	if sb.execCommand != "opencode --prompt-file .sandman/task.md" {
		t.Errorf("expected rendered command with prompt file, got %q", sb.execCommand)
	}
}

func TestAgentRun_Run_PassesEnvAndPromptFileThroughFullChain(t *testing.T) {
	issue := &github.Issue{Number: 7, Title: "Fix auth", Body: "OAuth is broken."}
	sb := &fakeSandbox{workDir: "/tmp/worktrees/fix-auth"}
	spy := &spyRenderer{result: "rendered prompt for auth fix"}

	run := NewAgentRun(issue, "sandman/7-fix-auth", sb)
	run.baseBranch = "main"
	run.env = map[string]string{"API_KEY": "sk-test123", "MODEL": "gpt-4"}

	res := run.Run(context.Background(), spy, "opencode run {{.PromptFile}}", prompt.RenderConfig{
		PromptFile: ".sandman/prompt.md",
	})

	if res.Status != "success" {
		t.Errorf("expected status success, got %s", res.Status)
	}

	if !spy.called {
		t.Fatal("expected renderer to be called")
	}
	if spy.data.Number != 7 {
		t.Errorf("expected issue number 7, got %d", spy.data.Number)
	}
	if spy.data.Title != "Fix auth" {
		t.Errorf("expected title 'Fix auth', got %q", spy.data.Title)
	}
	if spy.data.Body != "OAuth is broken." {
		t.Errorf("expected body 'OAuth is broken.', got %q", spy.data.Body)
	}
	if spy.data.SourceBranch != "sandman/7-fix-auth" {
		t.Errorf("expected SourceBranch 'sandman/7-fix-auth', got %q", spy.data.SourceBranch)
	}
	if spy.data.BaseBranch != "main" {
		t.Errorf("expected BaseBranch 'main', got %q", spy.data.BaseBranch)
	}

	if !sb.writePromptCalled {
		t.Fatal("expected WritePrompt to be called")
	}
	if sb.writePromptContent != "rendered prompt for auth fix" {
		t.Errorf("expected prompt content %q, got %q", "rendered prompt for auth fix", sb.writePromptContent)
	}

	if !sb.execCalled {
		t.Fatal("expected Exec to be called")
	}
	// shellenv.Build only quotes values that contain shell-special
	// characters; safe alphanumeric env values (letters, digits,
	// dash, dot, slash, colon, plus, comma, at, equals, underscore)
	// appear unquoted in the exported command. This is shell-safe
	// and matches the typed-enum contract, but it is a visible
	// change from the historical "always single-quoted" output the
	// pre-slice-7 applyAgentEnv produced.
	wantPrefix := "export API_KEY=sk-test123; export MODEL=gpt-4; opencode run .sandman/task.md"
	if sb.execCommand != wantPrefix {
		t.Errorf("exec command:\ngot:  %q\nwant: %q", sb.execCommand, wantPrefix)
	}
}

func TestAgentRun_Run_TemplateErrorCausesFailure(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "opencode {{.Unknown}}", prompt.RenderConfig{})

	if res.Status != "failure" {
		t.Errorf("expected status failure, got %s", res.Status)
	}
	if sb.execCalled {
		t.Error("expected Exec not to be called when template rendering fails")
	}
}

func TestAgentRun_Prepare_PassesRenderConfigToRenderer(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}
	renderCfg := prompt.RenderConfig{
		PromptFlag: "custom inline template",
		PromptArgs: map[string]string{"FOO": "bar"},
	}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Prepare(spy, renderCfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.cfg.PromptFlag != "custom inline template" {
		t.Errorf("expected PromptFlag 'custom inline template', got %q", spy.cfg.PromptFlag)
	}
	if spy.cfg.PromptArgs["FOO"] != "bar" {
		t.Errorf("expected PromptArgs FOO=bar, got %q", spy.cfg.PromptArgs["FOO"])
	}
}

func TestAgentRun_Run_WritesRawTaskPrompt(t *testing.T) {
	dir := t.TempDir()
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{workDir: dir}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "opencode run {{.PromptFile}}", prompt.RenderConfig{TaskPrompt: "finish {{ISSUE_NUMBER}}"})

	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}
	if spy.called {
		t.Fatal("expected renderer not to be called for continue prompt")
	}
	if sb.writePromptCalled {
		t.Fatal("expected WritePrompt not to be called for continue prompt")
	}
	promptPath := filepath.Join(dir, ".sandman", "task.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("expected continue prompt file: %v", err)
	}
	if string(data) != "finish {{ISSUE_NUMBER}}" {
		t.Fatalf("expected raw continue prompt, got %q", string(data))
	}
	if sb.execCommand != "opencode run .sandman/task.md" {
		t.Fatalf("expected continue prompt file in command, got %q", sb.execCommand)
	}
}

func TestAgentRun_Result(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)

	res := run.Result()
	if res.IssueNumber != 42 {
		t.Errorf("expected issue 42, got %d", res.IssueNumber)
	}
	if res.Branch != "sandman/42-fix-bug" {
		t.Errorf("expected branch sandman/42-fix-bug, got %s", res.Branch)
	}
	if res.Status != "success" {
		t.Errorf("expected status success, got %s", res.Status)
	}
}

func TestAgentRun_Run_EmptyEnvLeavesCommandUnchanged(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "opencode run {{.PromptFile}}", prompt.RenderConfig{})

	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}
	if sb.execCommand != "opencode run .sandman/task.md" {
		t.Errorf("expected unchanged command, got %q", sb.execCommand)
	}
}

func TestAgentRun_Run_ExportsSortedQuotedVariables(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.env = map[string]string{
		"BETA":  "two words",
		"ALPHA": "it's fine",
	}
	res := run.Run(context.Background(), spy, "opencode run {{.PromptFile}}", prompt.RenderConfig{})

	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}
	want := "export ALPHA='it'\"'\"'s fine'; export BETA='two words'; opencode run .sandman/task.md"
	if sb.execCommand != want {
		t.Errorf("expected sorted quoted exports, got:\n%s", sb.execCommand)
	}
}

func TestAgentRun_Run_OpencodePresetExportsPermissionForDangerousRuns(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	preset, ok := config.BuiltInAgentPresets["opencode"]
	if !ok {
		t.Fatal("expected opencode preset to exist")
	}
	agent := preset.Agent("opencode")
	if _, ok := preset.Env["OPENCODE_PERMISSION"]; !ok {
		t.Fatal("expected opencode preset env to carry OPENCODE_PERMISSION")
	}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.preset = "opencode"
	run.env = agent.Env
	run.opencodePermissionMode = agent.OpencodePermissionMode

	res := run.Run(context.Background(), spy, `opencode run --dangerously-skip-permissions {{.PromptFile}}`, prompt.RenderConfig{})

	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}
	wantPrefix := "export OPENCODE_PERMISSION='"
	if !strings.HasPrefix(sb.execCommand, wantPrefix) {
		t.Fatalf("expected rendered opencode command to start with %q, got:\n%s", wantPrefix, sb.execCommand)
	}
	if !strings.HasSuffix(sb.execCommand, "'; opencode run --dangerously-skip-permissions .sandman/task.md") {
		t.Fatalf("expected rendered opencode command to end with the opencode run invocation, got:\n%s", sb.execCommand)
	}
}

func TestAgentRun_Run_OpencodePresetSkipsPermissionForNonDangerousRuns(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	preset, ok := config.BuiltInAgentPresets["opencode"]
	if !ok {
		t.Fatal("expected opencode preset to exist")
	}
	agent := preset.Agent("opencode")

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.preset = "opencode"
	run.env = agent.Env
	run.opencodePermissionMode = agent.OpencodePermissionMode

	res := run.Run(context.Background(), spy, `opencode run {{.PromptFile}}`, prompt.RenderConfig{})

	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}
	want := `opencode run .sandman/task.md`
	if sb.execCommand != want {
		t.Fatalf("expected non-dangerous opencode command to stay unchanged, got:\n%s", sb.execCommand)
	}
}

func TestAgentRun_Run_PreservesUserOpencodePermissionOverride(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.env = map[string]string{
		"OPENCODE_PERMISSION": `{"external_directory":"allow"}`,
	}
	run.opencodePermissionMode = "custom"

	res := run.Run(context.Background(), spy, `opencode run {{.PromptFile}}`, prompt.RenderConfig{})

	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}
	if !strings.HasPrefix(sb.execCommand, "export OPENCODE_PERMISSION='") {
		t.Fatalf("expected user OPENCODE_PERMISSION to be preserved, got:\n%s", sb.execCommand)
	}
}

func TestAgentRun_Run_InvalidEnvKeyFails(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.env = map[string]string{
		"FOO; rm -rf /": "danger",
	}

	res := run.Run(context.Background(), spy, "opencode run {{.PromptFile}}", prompt.RenderConfig{})

	if res.Status != "failure" {
		t.Errorf("expected failure for invalid env key, got %s", res.Status)
	}
	if sb.execCalled {
		t.Error("expected Exec not to be called when env validation fails")
	}
}

func TestAgentRun_Execute_LogFilePrefixed(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "hello world\nsecond line\n", execStderr: "warn line\n"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.runID = "issue-42"
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}

	content := string(data)
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		expectedPrefix := "[issue-42]"
		if !strings.HasPrefix(line, expectedPrefix) {
			t.Errorf("expected line to start with %q, got %q", expectedPrefix, line)
		}
	}
}

func TestAgentRun_Execute_LogFilePrefixed_PromptOnly(t *testing.T) {
	dir := t.TempDir()
	sb := &fakeSandbox{execStdout: "hello world\nsecond line\n", execStderr: "warn line\n"}

	run := NewAgentRunWithLayout(nil, "prompt-test", sb, paths.NewLayout(&config.Config{}, dir))
	run.runID = "prompt-test"
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "prompt-test.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}

	content := string(data)
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		expectedPrefix := "[prompt-test]"
		if !strings.HasPrefix(line, expectedPrefix) {
			t.Errorf("expected line to start with %q, got %q", expectedPrefix, line)
		}
	}
}

func TestAgentRun_Execute_LogFilePrefixed_FlushesPartialLine(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "complete line\npartial line"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.runID = "issue-42"
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logPath := filepath.Join(dir, ".sandman", "logs", "42.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}

	content := string(data)
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		expectedPrefix := "[issue-42]"
		if !strings.HasPrefix(line, expectedPrefix) {
			t.Errorf("expected line to start with %q, got %q", expectedPrefix, line)
		}
	}
	if !strings.Contains(content, "partial line") {
		t.Errorf("expected log to contain partial line, got %q", content)
	}
}
