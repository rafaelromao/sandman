package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestInitNewProjectDefaultsToOpenCodeV2(t *testing.T) {
	dir := t.TempDir()
	if err := (&Scaffolder{}).Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultAgent != "opencode-v2" || cfg.DefaultReviewAgent != "opencode-v2" {
		t.Fatalf("agents = %q/%q; want opencode-v2 for implementation and review", cfg.DefaultAgent, cfg.DefaultReviewAgent)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# sandman installed-agents: opencode-v2\n") {
		t.Fatal("Dockerfile does not install the selected OpenCode v2 preset")
	}
}

func TestInitDefaultAgentSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      Options
		preserved *config.Config
		want      string
	}{
		{name: "new project", want: "opencode-v2"},
		{name: "existing v1", preserved: &config.Config{DefaultAgent: "opencode"}, want: "opencode"},
		{name: "existing claude", preserved: &config.Config{DefaultAgent: "claude"}, want: "claude"},
		{name: "explicit v1", opts: Options{Agent: "opencode"}, want: "opencode"},
		{name: "explicit claude", opts: Options{Agent: "claude"}, want: "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&Scaffolder{}).resolveDefaultAgent(tc.opts, tc.preserved)
			if err != nil || got != tc.want {
				t.Fatalf("resolveDefaultAgent() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
