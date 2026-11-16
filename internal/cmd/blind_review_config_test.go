package cmd

import (
	"bytes"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
)

func TestRun_ConfiguredEmptyReviewCommandSurvivesWithoutDaemon(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{
		Agent: "opencode", ReviewCommand: "", ReviewCommandSet: true,
	}}
	var output bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"42"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run without a review daemon: %v\n%s", err, output.String())
	}
	if got := spy.req.PromptConfig.ReviewCommand; got != "" {
		t.Fatalf("review command = %q, want empty", got)
	}
	if !spy.req.PromptConfig.ReviewCommandSet {
		t.Fatal("empty review command must remain explicit in events and rendering")
	}
}

func TestContinuation_ConfiguredEmptyReviewCommandSurvives(t *testing.T) {
	cfg := &config.Config{ReviewCommand: "", ReviewCommandSet: true}
	if got := effectiveReviewCommand(cfg); got != "" {
		t.Fatalf("continuation command = %q, want empty", got)
	}
}
