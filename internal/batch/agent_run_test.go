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

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

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

	readRunResultResult *sandbox.RunResult
	readRunResultError  error

	execCalled  bool
	execCommand string
	execError   error
	execStdout  string
	execStderr  string
	process     *fakeProcess
	stopCalled  bool
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
func (f *fakeSandbox) Stop() error {
	f.stopCalled = true
	return nil
}
func (f *fakeSandbox) WorkDir() string { return "" }
func (f *fakeSandbox) WritePrompt(content string) error {
	f.writePromptCalled = true
	f.writePromptContent = content
	return f.writePromptError
}
func (f *fakeSandbox) ReadRunResult() (*sandbox.RunResult, error) {
	return f.readRunResultResult, f.readRunResultError
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

type fakeGitHubClientForRun struct {
	createPRCalled       bool
	createPRBranch       string
	createPRTargetBranch string
	createPRTitle        string
	createPRBody         string
	createPRResult       string
	createPRError        error
}

func (f *fakeGitHubClientForRun) FetchIssue(number int) (*github.Issue, error) {
	return nil, nil
}

func (f *fakeGitHubClientForRun) CreatePR(branch, targetBranch, title, body string) (string, error) {
	f.createPRCalled = true
	f.createPRBranch = branch
	f.createPRTargetBranch = targetBranch
	f.createPRTitle = title
	f.createPRBody = body
	return f.createPRResult, f.createPRError
}

func TestAgentRun_Prepare_RendersAndWritesPrompt(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Prepare(spy); err != nil {
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
	if err := run.Prepare(spy); err == nil {
		t.Fatal("expected error when render fails")
	}
}

func TestAgentRun_Prepare_WritePromptError(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{writePromptError: errors.New("write failed")}
	spy := &spyRenderer{result: "rendered prompt"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Prepare(spy); err == nil {
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
	if err := run.Execute(context.Background(), "echo hello", &outBuf, io.Discard); err != nil {
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
	if strings.Contains(content, "[issue-42]") {
		t.Errorf("expected log to be un-prefixed, got %q", content)
	}
}

func TestAgentRun_Finalize_CreatesPR(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	client := &fakeGitHubClientForRun{createPRResult: "https://github.com/owner/repo/pull/99"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Finalize(client, "main"); err != nil {
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

func TestAgentRun_Finalize_UsesRunResultForPR(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{readRunResultResult: &sandbox.RunResult{Title: "Custom Title", Body: "Custom Body"}}
	client := &fakeGitHubClientForRun{createPRResult: "https://github.com/owner/repo/pull/99"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Finalize(client, "main"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if client.createPRTitle != "Custom Title" {
		t.Errorf("expected title 'Custom Title', got %q", client.createPRTitle)
	}
	if client.createPRBody != "Custom Body\n\nFixes #42" {
		t.Errorf("expected body 'Custom Body\n\nFixes #42', got %q", client.createPRBody)
	}
}

func TestAgentRun_Finalize_CreatePRError(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	client := &fakeGitHubClientForRun{createPRError: errors.New("gh pr create failed")}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	if err := run.Finalize(client, "main"); err == nil {
		t.Fatal("expected error when CreatePR fails")
	}
}

func TestAgentRun_Run_Success(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug", Body: "Users cannot log in."}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}
	client := &fakeGitHubClientForRun{createPRResult: "https://github.com/owner/repo/pull/99"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "echo hello", client, "main")

	if res.Status != "success" {
		t.Errorf("expected status success, got %s", res.Status)
	}
	if res.IssueNumber != 42 {
		t.Errorf("expected issue 42, got %d", res.IssueNumber)
	}
	if res.PRURL != "https://github.com/owner/repo/pull/99" {
		t.Errorf("expected PRURL, got %q", res.PRURL)
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
	client := &fakeGitHubClientForRun{createPRResult: "https://github.com/owner/repo/pull/99"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "echo hello", client, "main")

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
	client := &fakeGitHubClientForRun{createPRResult: "https://github.com/owner/repo/pull/99"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "exit 1", client, "main")

	if res.Status != "failure" {
		t.Errorf("expected status failure, got %s", res.Status)
	}
	if client.createPRCalled {
		t.Error("expected CreatePR not to be called when Execute fails")
	}
}

func TestAgentRun_Run_FinalizeFailure(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	spy := &spyRenderer{result: "rendered prompt"}
	client := &fakeGitHubClientForRun{createPRError: errors.New("gh pr create failed")}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	res := run.Run(context.Background(), spy, "echo hello", client, "main")

	if res.Status != "failure" {
		t.Errorf("expected status failure, got %s", res.Status)
	}
}

func TestAgentRun_Result(t *testing.T) {
	issue := &github.Issue{Number: 42, Title: "Fix bug"}
	sb := &fakeSandbox{}
	client := &fakeGitHubClientForRun{createPRResult: "https://github.com/owner/repo/pull/99"}

	run := NewAgentRun(issue, "sandman/42-fix-bug", sb)
	_ = run.Finalize(client, "main")

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
	if res.PRURL != "https://github.com/owner/repo/pull/99" {
		t.Errorf("expected PRURL, got %q", res.PRURL)
	}
}
