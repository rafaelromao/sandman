package batch

import (
	"context"
	"errors"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

type fakeSandbox struct {
	writePromptCalled  bool
	writePromptContent string
	writePromptError   error

	readRunResultResult *sandbox.RunResult
	readRunResultError  error

	execCalled  bool
	execCommand string
	execError   error
}

func (f *fakeSandbox) Start() error { return nil }
func (f *fakeSandbox) Exec(ctx context.Context, command string) error {
	f.execCalled = true
	f.execCommand = command
	return f.execError
}
func (f *fakeSandbox) Stop() error     { return nil }
func (f *fakeSandbox) WorkDir() string { return "" }
func (f *fakeSandbox) WritePrompt(content string) error {
	f.writePromptCalled = true
	f.writePromptContent = content
	return f.writePromptError
}
func (f *fakeSandbox) ReadRunResult() (*sandbox.RunResult, error) {
	return f.readRunResultResult, f.readRunResultError
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
	if err := run.Execute(context.Background(), "echo hello"); err != nil {
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
	if err := run.Execute(context.Background(), "exit 1"); err == nil {
		t.Fatal("expected error when exec fails")
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
