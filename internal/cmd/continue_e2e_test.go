package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestRun_ContinueFlag_WarnsWhenPromptOnlyHandoffMissing(t *testing.T) {
	dir := t.TempDir()
	branch := "sandman/prompt-only-456"
	if err := os.MkdirAll(filepath.Join(dir, branch, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent:         "opencode",
		WorktreeDir:   dir,
		ReviewCommand: "/oc review",
		AgentProviders: map[string]config.Agent{
			"opencode": {Preset: "opencode", Command: "true"},
		},
	}}
	deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: "run-0-abc", Issue: 0, Payload: map[string]any{"agent": "opencode", "branch": branch, "base_branch": "main", "prompt_source_type": "prompt"}}}}

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--continue", "--run-id", "my-run"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "warning: no handoff found") {
		t.Fatalf("expected missing-handoff warning, got %q", buf.String())
	}
}
