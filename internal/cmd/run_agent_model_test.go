package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
)

func TestRun_RefusesClaudeAgentModelOnAnotherAgentBeforeAnyWork(t *testing.T) {
	cases := []struct {
		name   string
		config *config.Config
		args   []string
		wants  []string
	}{
		{
			name:   "--model on an OpenCode default agent",
			config: &config.Config{Agent: "opencode", ReviewCommand: "/oc review"},
			args:   []string{"--model", "claude-agent/sonnet", "42"},
			wants: []string{
				`--model "claude-agent/sonnet" cannot run on agent "opencode"`,
				`set --agent to claude and --model to "sonnet"`,
			},
		},
		{
			name:   "--agent and --model away from a claude default",
			config: &config.Config{DefaultAgent: "claude", DefaultModel: "claude-agent/sonnet", ReviewCommand: "/oc review"},
			args:   []string{"--agent", "opencode-v2", "--model", "claude-agent/sonnet", "42"},
			wants:  []string{`--model "claude-agent/sonnet" cannot run on agent "opencode-v2"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(t, spy)
			deps.ConfigStore = &fakeStore{config: tc.config}
			gh := &fakeGitHubClient{}
			deps.GitHubClient = gh
			cmd := NewRunCmd(deps)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("sandman run %v: expected the pairing to be refused", tc.args)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
			if spy.called {
				t.Error("batch runner was called for a refused pairing")
			}
			if len(gh.fetchCount) != 0 || gh.searchIssuesQuery != "" {
				t.Errorf("GitHub was queried for a refused pairing: fetches %v, search %q", gh.fetchCount, gh.searchIssuesQuery)
			}
		})
	}
}

// --agent alone never inherits another preset's model: the run gets the
// agent's own preset default, so it is not refused.
func TestRun_AgentFlagAloneDoesNotInheritAClaudeAgentModel(t *testing.T) {
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	deps.ConfigStore = &fakeStore{config: &config.Config{DefaultAgent: "claude", DefaultModel: "claude-agent/sonnet", ReviewCommand: "/oc review"}}
	cmd := NewRunCmd(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--agent", "opencode-v2", "42"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("sandman run --agent opencode-v2: %v", err)
	}
	if want := config.DefaultModelForAgent("opencode-v2"); spy.req.Model != want {
		t.Fatalf("model = %q, want the opencode-v2 preset default %q", spy.req.Model, want)
	}
}
