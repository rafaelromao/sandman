package batch

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// An implementation run whose agent cannot authenticate ends as a terminal
// failure with its reason and remedy, without spending the ordinary retry
// budget or waiting for a quota reset that would not help.
func TestRunBatch_AuthenticationFailureIsNotRetried(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	launches := 0
	log := &spyEventLog{}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}}, &noopRenderer{},
		&fakeConfigStore{config: &config.Config{
			Agent:          "claude",
			Sandbox:        "worktree",
			Retries:        2,
			WorktreeDir:    filepath.Join(dir, "worktrees"),
			Git:            config.GitConfig{BaseBranch: "main"},
			AgentProviders: map[string]config.Agent{"claude": config.BuiltInAgentPresets["claude"].Agent("claude")},
		}},
		log,
		WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(dir, "worktree")}}),
		WithRunnableFactory(&promptOnlyRunnableFactory{hook: func(*github.Issue, string) AgentRunResult {
			launches++
			return AgentRunResult{IssueNumber: 42, Status: "failure", OperationalError: ErrAgentAuthentication}
		}}),
	)

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}, Retries: 2, RunTS: orchTestRunTS, RunShortID: orchTestRunShortID})
	if launches != 1 {
		t.Fatalf("launches = %d, want 1", launches)
	}
	for _, event := range log.snapshot() {
		if event.Type == "run.retry" || event.Type == "run.await" || event.Type == "run.blocked" {
			t.Fatalf("unexpected %s event: %#v", event.Type, event.Payload)
		}
		if event.Type == "run.finished" {
			if event.Payload["status"] != "failure" {
				t.Fatalf("terminal status = %#v, want failure", event.Payload["status"])
			}
			if event.Payload["reason"] != "AGENT_AUTHENTICATION" {
				t.Fatalf("terminal reason = %#v, want AGENT_AUTHENTICATION", event.Payload["reason"])
			}
			action, _ := event.Payload["next_action"].(string)
			if !strings.Contains(action, "claude /login") || !strings.Contains(action, "claude setup-token") || !strings.Contains(action, "--continue") {
				t.Fatalf("next_action = %q, want the credential remedy and the continuation", action)
			}
			return
		}
	}
	t.Fatalf("no terminal failure event in %#v", log.snapshot())
}
