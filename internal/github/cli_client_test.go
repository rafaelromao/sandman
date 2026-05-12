package github

import (
	"os/exec"
	"testing"
)

type fakeRunner struct {
	name   string
	args   []string
	output string
	err    error
}

func (f *fakeRunner) Run(name string, arg ...string) *exec.Cmd {
	f.name = name
	f.args = arg
	if f.err != nil {
		return exec.Command("sh", "-c", "echo error >&2; exit 1")
	}
	return exec.Command("echo", f.output)
}

func TestCLIClient_SearchIssues_Success(t *testing.T) {
	runner := &fakeRunner{output: `[{"number":1,"title":"Bug","body":"bug body","labels":["bug"]},{"number":2,"title":"Feature","body":"feat body","labels":[]}]`}
	client := &CLIClient{runner: runner}

	issues, err := client.SearchIssues("is:open label:bug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(issues))
	}
	if issues[0].Number != 1 {
		t.Errorf("expected issue 1, got %d", issues[0].Number)
	}
	if issues[0].Title != "Bug" {
		t.Errorf("expected title 'Bug', got %q", issues[0].Title)
	}
	if issues[0].Body != "bug body" {
		t.Errorf("expected body 'bug body', got %q", issues[0].Body)
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0] != "bug" {
		t.Errorf("expected labels [bug], got %v", issues[0].Labels)
	}
	if runner.name != "gh" {
		t.Errorf("expected command gh, got %q", runner.name)
	}
	expectedArgs := []string{"issue", "list", "--search", "is:open label:bug", "--json", "number,title,body,labels", "--limit", "100"}
	if len(runner.args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.args)
	}
	for i, arg := range expectedArgs {
		if runner.args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.args[i])
		}
	}
}

func TestCLIClient_SearchIssues_Error(t *testing.T) {
	runner := &fakeRunner{err: exec.ErrNotFound}
	client := &CLIClient{runner: runner}

	_, err := client.SearchIssues("is:open")
	if err == nil {
		t.Fatal("expected error when gh issue list fails")
	}
}
