package scaffold

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestScaffold_ClaudeAgentScaffoldsClaudeConfigAndDockerfile(t *testing.T) {
	dir := t.TempDir()
	var summary bytes.Buffer
	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "generic", Agent: "claude", Writer: &summary}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	cfg, err := config.Load(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.DefaultAgent != "claude" || cfg.DefaultModel != "sonnet" || cfg.DefaultReviewAgent != "claude" || cfg.DefaultReviewModel != "sonnet" {
		t.Fatalf("agent/model/review_agent/review_model = %q/%q/%q/%q, want claude/sonnet/claude/sonnet",
			cfg.DefaultAgent, cfg.DefaultModel, cfg.DefaultReviewAgent, cfg.DefaultReviewModel)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(data)
	for _, want := range []string{
		"# sandman default-agent: claude\n",
		"# sandman installed-agents: claude\n",
		"RUN npm install -g @anthropic-ai/claude-code@2.1.283\n",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile missing %q, got:\n%s", want, dockerfile)
		}
	}
	if strings.Contains(dockerfile, "opencode") {
		t.Errorf("claude Dockerfile installs or names opencode:\n%s", dockerfile)
	}
	if !strings.Contains(summary.String(), "Agent:    @anthropic-ai/claude-code@2.1.283") {
		t.Errorf("init summary = %q, want the claude package pin", summary.String())
	}
	if err := ValidateDockerfileMetadata(dir, "generic", "claude", []string{"claude"}); err != nil {
		t.Fatalf("scaffolded claude Dockerfile does not validate: %v", err)
	}
}

func TestScaffold_OpenCodeDefaultsAreUnchanged(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.DefaultAgent != config.DefaultAgent || cfg.DefaultModel != config.DefaultModel || cfg.DefaultReviewAgent != config.DefaultReviewAgent || cfg.DefaultReviewModel != config.DefaultReviewModel {
		t.Fatalf("opencode defaults changed: %q/%q/%q/%q", cfg.DefaultAgent, cfg.DefaultModel, cfg.DefaultReviewAgent, cfg.DefaultReviewModel)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "# sandman installed-agents: opencode\n") || !strings.Contains(string(data), "RUN npm install -g opencode-ai@") {
		t.Fatalf("opencode Dockerfile changed:\n%s", data)
	}
}

func TestResolveAgentVersion_ClaudePinsCatalogHeadWithoutHostProbe(t *testing.T) {
	t.Cleanup(func(prev func() (string, error)) func() {
		return func() { probeOpencodeVersion = prev }
	}(probeOpencodeVersion))
	probeOpencodeVersion = func() (string, error) { return "9.9.9", nil }
	if got := resolveAgentVersion("claude"); got != "2.1.283" {
		t.Fatalf("claude version = %q, want the catalog head", got)
	}
	if got := resolveAgentVersion("opencode"); got != "9.9.9" {
		t.Fatalf("opencode version = %q, want the host probe", got)
	}
	probeOpencodeVersion = func() (string, error) { return "", errors.New("not installed") }
	if got := resolveAgentVersion("opencode"); got != DefaultBuiltInAgentVersion("opencode") {
		t.Fatalf("opencode version = %q, want the catalog head when the probe fails", got)
	}
}

func TestAgentInstallers_CoverEveryBuiltInPreset(t *testing.T) {
	for name := range config.BuiltInAgentPresets {
		installer, ok := agentInstallers[name]
		if !ok {
			t.Errorf("built-in preset %q has no Dockerfile installer", name)
			continue
		}
		if DefaultBuiltInAgentVersion(name) == "" {
			t.Errorf("built-in preset %q has no pinned version", name)
		}
		if installer.InstallName() == "" {
			t.Errorf("built-in preset %q installer has no package name", name)
		}
	}
	for name := range agentInstallers {
		if _, ok := config.BuiltInAgentPresets[name]; !ok {
			t.Errorf("installer %q has no built-in preset", name)
		}
	}
}

func TestValidateDockerfileMetadata_RequiresEveryRunAgent(t *testing.T) {
	write := func(t *testing.T, installed string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "# sandman build-tools: generic\n# sandman default-agent: opencode\n"
		if installed != "" {
			content += "# sandman installed-agents: " + installed + "\n"
		}
		content += "FROM debian:bookworm-slim\n"
		if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	tests := []struct {
		name      string
		installed string
		required  []string
		wantErr   string
	}{
		{name: "opencode image, opencode run", installed: "opencode", required: []string{"opencode"}},
		{name: "opencode image, claude run", installed: "opencode", required: []string{"opencode", "claude"}, wantErr: `does not include agent "claude"`},
		{name: "both agents installed, claude run", installed: "opencode, claude", required: []string{"opencode", "claude"}},
		{name: "both agents installed, opencode run", installed: "claude,opencode", required: []string{"opencode"}},
		{name: "no requirement falls back to default agent", installed: "opencode", required: nil},
		{name: "missing header", installed: "", required: []string{"opencode"}, wantErr: `does not include agent "opencode"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDockerfileMetadata(write(t, tt.installed), "generic", "opencode", tt.required)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateDockerfileMetadata_RunOnAnotherAgentSkipsDefaultAgentHeader(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# sandman build-tools: go\n# sandman default-agent: claude\n# sandman installed-agents: claude\nFROM debian:bookworm-slim\n"
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDockerfileMetadata(dir, "go", "", []string{"claude"}); err != nil {
		t.Fatalf("claude run against a claude image with an opencode config default: %v", err)
	}
	if err := ValidateDockerfileMetadata(dir, "go", "opencode", []string{"opencode"}); err == nil || !strings.Contains(err.Error(), `default-agent "claude" does not match config default agent "opencode"`) {
		t.Fatalf("default-agent run error = %v, want default-agent drift", err)
	}
	if err := ValidateDockerfileMetadata(dir, "go", "", []string{"opencode"}); err == nil || !strings.Contains(err.Error(), `does not include agent "opencode"`) {
		t.Fatalf("opencode run against a claude-only image error = %v, want installed-agents drift", err)
	}
}

func TestScaffold_ReInitWithAgentSwitchesPreservedConfig(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}
	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetValue("retries", "7"); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := s.Scaffold(dir, Options{BuildTools: "generic", Agent: "claude", Writer: &out}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("re-init with --agent claude: %v", err)
	}
	cfg, err = config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultAgent != "claude" || cfg.DefaultModel != "sonnet" || cfg.DefaultReviewAgent != "claude" || cfg.DefaultReviewModel != "sonnet" {
		t.Fatalf("agent/model/review_agent/review_model = %q/%q/%q/%q, want claude/sonnet/claude/sonnet",
			cfg.DefaultAgent, cfg.DefaultModel, cfg.DefaultReviewAgent, cfg.DefaultReviewModel)
	}
	if cfg.Retries != 7 {
		t.Fatalf("retries = %d, want the preserved operator value 7", cfg.Retries)
	}
	if !strings.Contains(out.String(), "Updated .sandman/config.yaml: agent=claude, model=sonnet, review_agent=claude, review_model=sonnet") {
		t.Fatalf("init output = %q, want the config update line", out.String())
	}
	if err := ValidateDockerfileMetadata(dir, "generic", cfg.DefaultAgent, []string{"claude"}); err != nil {
		t.Fatalf("re-initialised Dockerfile does not match config: %v", err)
	}

	// A plain re-init keeps the preserved agent in the Dockerfile.
	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("plain re-init: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# sandman default-agent: claude\n") || !strings.Contains(string(data), "@anthropic-ai/claude-code@") {
		t.Fatalf("plain re-init Dockerfile lost the claude default:\n%s", data)
	}
}

func TestScaffold_ReInitKeepsSeparatelyChosenReviewAgent(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, ".sandman", "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agents = map[string]config.Agent{"reviewer": {Preset: "opencode", Model: "opencode/fast"}}
	if err := cfg.SetValue("review_agent", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.Scaffold(dir, Options{BuildTools: "generic", Agent: "claude", Model: "opus"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultAgent != "claude" || cfg.DefaultModel != "opus" || cfg.DefaultReviewAgent != "reviewer" {
		t.Fatalf("agent/model/review_agent = %q/%q/%q, want claude/opus/reviewer", cfg.DefaultAgent, cfg.DefaultModel, cfg.DefaultReviewAgent)
	}
}
