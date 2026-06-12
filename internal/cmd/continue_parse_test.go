package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestRun_ContinueFlag_NoPriorPromptOnlyRun_ReturnsError(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review", AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}}
	deps.EventLog = &fakeEventLog{events: []events.Event{}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "--run-id", "my-run"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no prior prompt-only run exists")
	}
	if !strings.Contains(err.Error(), "no previous prompt-only run found") {
		t.Fatalf("expected prompt-only replay error, got %v", err)
	}
	if spy.called {
		t.Fatal("expected batch runner not to be called")
	}
}
