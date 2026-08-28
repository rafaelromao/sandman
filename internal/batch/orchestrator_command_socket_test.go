package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestRunBatch_ExternallyBlockedDependencyDoesNotCreateCommandSocket(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	eventLog := &events.JSONLLogger{Path: filepath.Join(dir, ".sandman", "events.jsonl")}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{
				42:  {Number: 42, State: "open", Title: "Blocker"},
				100: {Number: 100, State: "open", Title: "Dependent"},
			},
		},
		&noopRenderer{},
		&fakeConfigStore{config: &config.Config{
			Agent:          "test-agent",
			Sandbox:        "worktree",
			WorktreeDir:    ".sandman/worktrees",
			Git:            config.GitConfig{BaseBranch: "main"},
			AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
		}},
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{}}),
		WithRunnableFactory(&byIssueRunnableFactory{}),
		WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "current-sha", nil }}),
	)

	if _, err := o.RunBatch(context.Background(), Request{
		Issues:     []int{100},
		Blocked:    map[int][]int{100: {42}},
		Parallel:   2,
		RunTS:      "260828120000",
		RunShortID: "test",
	}); err != nil {
		t.Fatalf("run batch: %v; events=%#v", err, mustReadEvents(t, eventLog))
	}

	var blocked events.Event
	for _, event := range mustReadEvents(t, eventLog) {
		if event.Type == "run.blocked" && event.Issue == 100 {
			blocked = event
			break
		}
	}
	if blocked.RunID == "" {
		t.Fatal("expected a blocked event for dependent issue 100")
	}
	batchID, _ := blocked.Payload["batch_id"].(string)
	if batchID == "" {
		t.Fatalf("blocked event has no batch_id: %#v", blocked.Payload)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "batches", batchID, "runs", blocked.RunID)); !os.IsNotExist(err) {
		t.Fatalf("externally blocked dependent unexpectedly has a command-socket folder, stat error=%v", err)
	}
}

func mustReadEvents(t *testing.T, eventLog *events.JSONLLogger) []events.Event {
	t.Helper()
	eventsList, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return eventsList
}
