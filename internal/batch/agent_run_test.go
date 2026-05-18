package batch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/subagent"
)

type fakeCapture struct {
	wrapCalled   bool
	wrapCmd      string
	wrapWriter   io.Writer
	wrapErr      error
	eventsCh     chan subagent.Event
	stopCalled   bool
	stopSessions []subagent.SessionOutput
	stopErr      error
}

func (f *fakeCapture) WrapCommand(command string) (string, io.Writer, func(), error) {
	f.wrapCalled = true
	f.wrapCmd = "wrapped: " + command
	return f.wrapCmd, f.wrapWriter, func() {}, f.wrapErr
}

func (f *fakeCapture) Events() <-chan subagent.Event {
	return f.eventsCh
}

func (f *fakeCapture) Stop() ([]subagent.SessionOutput, error) {
	f.stopCalled = true
	return f.stopSessions, f.stopErr
}

type fakeProcess struct {
	sigTermCalled bool
	killCalled    bool
}

func (p *fakeProcess) Signal(sig os.Signal) error {
	if sig == syscall.SIGTERM {
		p.sigTermCalled = true
	}
	return nil
}

func (p *fakeProcess) Kill() error {
	p.killCalled = true
	return nil
}

type fakeSandbox struct {
	writePromptCalled  bool
	writePromptContent string
	writePromptError   error

	execCalled  bool
	execCommand string
	execError   error
	execStdout  string
	execStderr  string
	process     *fakeProcess
	stopCalled  bool
	workDir     string
}

func (f *fakeSandbox) Start() error { return nil }
func (f *fakeSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.execCalled = true
	f.execCommand = command
	if stdout != nil && f.execStdout != "" {
		stdout.Write([]byte(f.execStdout))
	}
	if stderr != nil && f.execStderr != "" {
		stderr.Write([]byte(f.execStderr))
	}
	return f.execError
}
func (f *fakeSandbox) ExecInteractive(ctx context.Context, command string) error {
	f.execCalled = true
	f.execCommand = command
	return f.execError
}
func (f *fakeSandbox) Stop() error {
	f.stopCalled = true
	return nil
}
func (f *fakeSandbox) WorkDir() string { return f.workDir }
func (f *fakeSandbox) WritePrompt(content string) error {
	f.writePromptCalled = true
	f.writePromptContent = content
	return f.writePromptError
}
func (f *fakeSandbox) Process() sandbox.Process {
	return f.process
}

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

func TestAgentRun_Prepare_PopulatesBranchFields(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.defaultBranch = "main"

	if err := run.Prepare(spy, prompt.RenderConfig{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spy.data.SourceBranch != "sandman/42-fix-bug" {
		t.Errorf("expected SourceBranch 'sandman/42-fix-bug', got %q", spy.data.SourceBranch)
	}
	if spy.data.TargetBranch != "main" {
		t.Errorf("expected TargetBranch 'main', got %q", spy.data.TargetBranch)
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
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard, nil); err != nil {
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

	res := run.Run(context.Background(), spy, config.BuiltInAgentPresets["opencode"].Command, false, prompt.RenderConfig{})
	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}

	want := `opencode run -m gpt-4.1 "$(cat .sandman/rendered-prompt.md)"`
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

	command := `opencode run --pure "$(cat {{.PromptFile}})"`
	res := run.Run(context.Background(), spy, command, false, prompt.RenderConfig{})
	if res.Status != "success" {
		t.Fatalf("expected success, got %s", res.Status)
	}

	want := `opencode run --pure "$(cat .sandman/rendered-prompt.md)"`
	if sb.execCommand != want {
		t.Errorf("expected command %q, got %q", want, sb.execCommand)
	}
}

func TestAgentRun_Execute_Failure(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execError: errors.New("agent failed")}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Execute(context.Background(), "exit 1", io.Discard, io.Discard, nil); err == nil {
		t.Fatal("expected error when exec fails")
	}
}

func TestAgentRun_Execute_PrefixesOutput(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "hello world\n"}
	var outBuf bytes.Buffer

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Execute(context.Background(), "echo hello", &outBuf, io.Discard, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := outBuf.String()
	if !strings.Contains(output, "[issue-42]") {
		t.Errorf("expected output to contain issue prefix, got %q", output)
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
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard, nil); err != nil {
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
	if strings.Contains(content, "[issue-42]") {
		t.Errorf("expected log to be un-prefixed, got %q", content)
	}
}

func TestAgentRun_Run_Success(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}
	renderCfg := prompt.RenderConfig{}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "echo hello", false, renderCfg)

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
	res := run.Run(context.Background(), spy, "echo hello", false, prompt.RenderConfig{})

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
	res := run.Run(context.Background(), spy, "exit 1", false, prompt.RenderConfig{})

	if res.Status != "failure" {
		t.Errorf("expected status failure, got %s", res.Status)
	}
}

func TestAgentRun_Run_InteractiveUsesExecInteractive(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "echo hello", true, prompt.RenderConfig{})

	if res.Status != "success" {
		t.Errorf("expected status success, got %s", res.Status)
	}
	if !sb.execCalled {
		t.Fatal("expected ExecInteractive to be called")
	}
	if sb.execCommand != "echo hello" {
		t.Errorf("expected command 'echo hello', got %q", sb.execCommand)
	}
}

func TestAgentRun_Run_InjectsPromptFileIntoCommandTemplate(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{workDir: "/tmp/worktrees/fix-bug"}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "opencode --prompt-file {{.PromptFile}}", false, prompt.RenderConfig{
		PromptFile: ".sandman/prompt.md",
	})

	if res.Status != "success" {
		t.Errorf("expected status success, got %s", res.Status)
	}
	if sb.execCommand != "opencode --prompt-file .sandman/rendered-prompt.md" {
		t.Errorf("expected rendered command with prompt file, got %q", sb.execCommand)
	}
}

func TestAgentRun_Run_PassesEnvAndPromptFileThroughFullChain(t *testing.T) {
	issue := &github.Issue{Number: 7, Title: "Fix auth", Body: "OAuth is broken."}
	sb := &fakeSandbox{workDir: "/tmp/worktrees/fix-auth"}
	spy := &spyRenderer{result: "rendered prompt for auth fix"}

	run := NewAgentRun(issue, "sandman/7-fix-auth", sb)
	run.defaultBranch = "main"
	run.env = map[string]string{"API_KEY": "sk-test123", "MODEL": "gpt-4"}

	res := run.Run(context.Background(), spy, "opencode run {{.PromptFile}}", false, prompt.RenderConfig{
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
	if spy.data.TargetBranch != "main" {
		t.Errorf("expected TargetBranch 'main', got %q", spy.data.TargetBranch)
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
	wantPrefix := "export API_KEY='sk-test123'; export MODEL='gpt-4'; opencode run .sandman/rendered-prompt.md"
	if sb.execCommand != wantPrefix {
		t.Errorf("exec command:\ngot:  %q\nwant: %q", sb.execCommand, wantPrefix)
	}
}

func TestAgentRun_Run_TemplateErrorCausesFailure(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "opencode {{.Unknown}}", false, prompt.RenderConfig{})

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

func TestAgentRun_Execute_WithCapture(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "some output\n"}

	eventsCh := make(chan subagent.Event)
	close(eventsCh)

	cap := &fakeCapture{
		wrapWriter:   io.Discard,
		eventsCh:     eventsCh,
		stopSessions: []subagent.SessionOutput{{SessionID: "sess-1"}},
	}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Execute(context.Background(), "opencode run --issue 42", io.Discard, io.Discard, cap); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cap.wrapCalled {
		t.Error("expected WrapCommand to be called")
	}
	if sb.execCommand != cap.wrapCmd {
		t.Errorf("expected wrapped command %q, got %q", cap.wrapCmd, sb.execCommand)
	}
	if !cap.stopCalled {
		t.Error("expected Stop to be called")
	}

	res := run.Result()
	if len(res.SubagentOutput) != 1 || res.SubagentOutput[0].SessionID != "sess-1" {
		t.Errorf("expected subagent output in result, got %+v", res.SubagentOutput)
	}
}

func TestAgentRun_Execute_WithCaptureWrapsOnlyOpencode(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "some output\n"}

	eventsCh := make(chan subagent.Event)
	close(eventsCh)

	cap := &fakeCapture{
		wrapWriter:   io.Discard,
		eventsCh:     eventsCh,
		stopSessions: []subagent.SessionOutput{{SessionID: "sess-1"}},
	}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Execute(context.Background(), "echo hello", io.Discard, io.Discard, cap); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cap.wrapCalled {
		t.Error("expected WrapCommand to be called")
	}
	if sb.execCommand != cap.wrapCmd {
		t.Errorf("expected wrapped command %q, got %q", cap.wrapCmd, sb.execCommand)
	}
}

func TestAgentRun_Execute_WithCaptureStopError(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{execStdout: "some output\n"}

	eventsCh := make(chan subagent.Event)
	close(eventsCh)

	cap := &fakeCapture{
		wrapWriter: io.Discard,
		eventsCh:   eventsCh,
		stopErr:    errors.New("stop failed"),
	}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	err := run.Execute(context.Background(), "opencode run --issue 42", io.Discard, io.Discard, cap)
	if err == nil || !strings.Contains(err.Error(), "stop failed") {
		t.Errorf("expected stop error, got %v", err)
	}
}

func TestAgentRun_Run_WithCaptureOnStruct(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Test capture."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	eventsCh := make(chan subagent.Event)
	close(eventsCh)

	cap := &fakeCapture{
		wrapWriter:   io.Discard,
		eventsCh:     eventsCh,
		stopSessions: []subagent.SessionOutput{{SessionID: "sess-run-1"}},
	}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	run.capture = cap
	res := run.Run(context.Background(), spy, "opencode run {{.PromptFile}}", false, prompt.RenderConfig{})

	if res.Status != "success" {
		t.Errorf("expected success, got %s", res.Status)
	}
	if !cap.wrapCalled {
		t.Error("expected WrapCommand to be called")
	}
	if !cap.stopCalled {
		t.Error("expected Stop to be called")
	}
	if len(res.SubagentOutput) != 1 || res.SubagentOutput[0].SessionID != "sess-run-1" {
		t.Errorf("expected subagent output in result, got %+v", res.SubagentOutput)
	}
}
