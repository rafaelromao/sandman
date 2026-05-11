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

func TestCLIClient_CreatePR_Success(t *testing.T) {
	runner := &fakeRunner{output: "https://github.com/owner/repo/pull/99"}
	client := &CLIClient{runner: runner}

	url, err := client.CreatePR("feature-branch", "main", "Fix bug", "Fixes the bug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url == "" {
		t.Fatal("expected non-empty URL")
	}
	if runner.name != "gh" {
		t.Errorf("expected command gh, got %q", runner.name)
	}
	expectedArgs := []string{"pr", "create", "--head", "feature-branch", "--base", "main", "--title", "Fix bug", "--body", "Fixes the bug"}
	if len(runner.args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.args)
	}
	for i, arg := range expectedArgs {
		if runner.args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.args[i])
		}
	}
}

func TestCLIClient_CreatePR_Error(t *testing.T) {
	runner := &fakeRunner{err: exec.ErrNotFound}
	client := &CLIClient{runner: runner}

	_, err := client.CreatePR("feature-branch", "main", "Fix bug", "Fixes the bug")
	if err == nil {
		t.Fatal("expected error when gh pr create fails")
	}
}
