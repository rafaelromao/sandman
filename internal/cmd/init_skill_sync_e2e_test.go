package cmd

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/skill"
)

func TestInit_SyncsNonDefaultSkillSettingsE2E(t *testing.T) {
	projectDir := t.TempDir()
	homeDir := t.TempDir()
	t.Chdir(projectDir)
	t.Setenv("HOME", homeDir)
	installInitHostVersion(t, "opencode v1.19.3")

	previousSync := syncSandmanSkill
	syncSandmanSkill = skill.Sync
	t.Cleanup(func() { syncSandmanSkill = previousSync })

	var output bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&output)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{
		"--build-tools", "generic",
		"--agent", "opencode",
		"--review-command", "@codex review",
		"--review-timeout", "600",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sandman init failed: %v\noutput:\n%s", err, output.String())
	}

	cfg, err := config.Load(filepath.Join(projectDir, ".sandman", "config.yaml"))
	if err != nil {
		t.Fatalf("load initialized project config: %v", err)
	}
	if cfg.DefaultAgent != "opencode" {
		t.Errorf("configured agent = %q, want non-default opencode", cfg.DefaultAgent)
	}
	if got := cfg.EffectiveReviewCommand(); got != "@codex review" {
		t.Errorf("configured review command = %q, want %q", got, "@codex review")
	}
	if got := cfg.EffectiveReviewTimeout(); got != 600 {
		t.Errorf("configured review timeout = %d, want 600", got)
	}

	skillRoot := filepath.Join(homeDir, ".agents", "skills", "sandman")
	runSkill, err := os.ReadFile(filepath.Join(skillRoot, "run", "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed run skill: %v", err)
	}
	for _, want := range []string{
		"Review command: `@codex review`",
	} {
		if !strings.Contains(string(runSkill), want) {
			t.Errorf("installed run skill is missing non-default setting %q", want)
		}
	}
	for _, unchanged := range []string{
		"Current branch: {{BRANCH}}",
		"Source branch: {{SOURCE_BRANCH}}",
		"Base branch: `{{BASE_BRANCH}}`",
		"Delegated review response timeout: `{{REVIEW_TIMEOUT}}` seconds",
	} {
		if !strings.Contains(string(runSkill), unchanged) {
			t.Errorf("sandman init changed non-review runtime placeholder %q", unchanged)
		}
	}

	reviewCycle, err := os.ReadFile(filepath.Join(skillRoot, "review-cycle", "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed review-cycle skill: %v", err)
	}
	if !strings.Contains(string(reviewCycle), `--trigger-prefix "@codex review"`) {
		t.Fatal("installed review-cycle skill did not render the configured review command")
	}

	err = filepath.WalkDir(skillRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "{{REVIEW_COMMAND}}") {
			return fmt.Errorf("installed skill %s contains an unresolved review-command placeholder", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
