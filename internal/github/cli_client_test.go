package github

import (
	"os/exec"
	"testing"
)

type fakeRunner struct {
	calls     []fakeCall
	responses []fakeResponse
}

type fakeCall struct {
	name string
	args []string
}

type fakeResponse struct {
	output string
	err    error
}

func (f *fakeRunner) Run(name string, arg ...string) *exec.Cmd {
	f.calls = append(f.calls, fakeCall{name: name, args: append([]string(nil), arg...)})
	idx := len(f.calls) - 1
	if idx < len(f.responses) && f.responses[idx].err != nil {
		return exec.Command("sh", "-c", "echo error >&2; exit 1")
	}
	if idx < len(f.responses) {
		return exec.Command("echo", f.responses[idx].output)
	}
	return exec.Command("echo")
}

func TestCLIClient_SearchIssues_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `[{"number":1,"title":"Bug","body":"bug body","labels":["bug"]},{"number":2,"title":"Feature","body":"feat body","labels":[]}]`}}}
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
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "gh" {
		t.Errorf("expected command gh, got %q", runner.calls[0].name)
	}
	expectedArgs := []string{"issue", "list", "--search", "is:open label:bug", "--json", "number,title,body,labels", "--limit", "100"}
	if len(runner.calls[0].args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[0].args)
	}
	for i, arg := range expectedArgs {
		if runner.calls[0].args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.calls[0].args[i])
		}
	}
}

func TestCLIClient_SearchIssues_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{err: exec.ErrNotFound}}}
	client := &CLIClient{runner: runner}

	_, err := client.SearchIssues("is:open")
	if err == nil {
		t.Fatal("expected error when gh issue list fails")
	}
}

func TestCLIClient_ResolveRepo_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`}}}
	client := &CLIClient{runner: runner}

	owner, repo, err := client.resolveRepo()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "rafaelromao" {
		t.Fatalf("expected owner rafaelromao, got %q", owner)
	}
	if repo != "sandman" {
		t.Fatalf("expected repo sandman, got %q", repo)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(runner.calls))
	}
	expectedArgs := []string{"repo", "view", "--json", "owner,name"}
	if len(runner.calls[0].args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[0].args)
	}
	for i, arg := range expectedArgs {
		if runner.calls[0].args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.calls[0].args[i])
		}
	}
}

func TestCLIClient_ResolveRepo_CachesResult(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`}}}
	client := &CLIClient{runner: runner}

	owner1, repo1, err := client.resolveRepo()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	owner2, repo2, err := client.resolveRepo()
	if err != nil {
		t.Fatalf("unexpected error on cached lookup: %v", err)
	}
	if owner1 != owner2 || repo1 != repo2 {
		t.Fatalf("expected cached repo to match first lookup, got %q/%q then %q/%q", owner1, repo1, owner2, repo2)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected cached lookup to avoid a second gh call, got %d calls", len(runner.calls))
	}
}

func TestCLIClient_ResolveRepo_UsesRepoOverride(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`}}}
	client := &CLIClient{runner: runner, RepoOverride: "octo/sandman"}

	_, _, err := client.resolveRepo()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(runner.calls))
	}
	expectedArgs := []string{"repo", "view", "--json", "owner,name", "--repo", "octo/sandman"}
	if len(runner.calls[0].args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[0].args)
	}
	for i, arg := range expectedArgs {
		if runner.calls[0].args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.calls[0].args[i])
		}
	}
}

func TestCLIClient_ResolveRepo_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{err: exec.ErrNotFound}}}
	client := &CLIClient{runner: runner}

	_, _, err := client.resolveRepo()
	if err == nil {
		t.Fatal("expected error when gh repo view fails")
	}
}
