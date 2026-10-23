package batch

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// The public variant is the same input for implementation and review. Only
// the selected strategy translates it into the agent CLI's native syntax.
func TestAgentRun_VariantTranslationAcrossPresets(t *testing.T) {
	for _, tc := range []struct {
		name, preset, model, variant, want, forbidden string
		custom                                        bool
	}{
		{name: "v1", preset: "opencode", model: "provider/model", variant: "high", want: "-m provider/model --variant 'high'", forbidden: "--effort"},
		{name: "v2", preset: "opencode-v2", model: "provider/model", variant: "high", want: "--model 'provider/model#high'", forbidden: "--variant"},
		{name: "claude", preset: "claude", model: "sonnet", variant: "high", want: "--model 'sonnet' --effort 'high'", forbidden: "--variant"},
		{name: "v2-variant-only", preset: "opencode-v2", variant: "high", want: "--model '" + config.DefaultModel + "#high'", forbidden: "--variant"},
		{name: "v2-no-model-or-variant", preset: "opencode-v2", forbidden: "--model"},
		{name: "v2-replace-suffix", preset: "opencode-v2", model: "provider/model#old", variant: "high", want: "--model 'provider/model#high'", forbidden: "#old"},
		{name: "v2-retain-suffix", preset: "opencode-v2", model: "provider/model#old", want: "--model 'provider/model#old'", forbidden: "--variant"},
		{name: "v2-quote-variant", preset: "opencode-v2", model: "provider/model", variant: "high effort", want: "--model 'provider/model#high effort'", forbidden: "--variant"},
		{name: "v2-custom-command", preset: "opencode-v2", variant: "high", custom: true, forbidden: "--model"},
	} {
		for _, review := range []bool{false, true} {
			phase := "run"
			if review {
				phase = "review"
			}
			t.Run(tc.name+"/"+phase, func(t *testing.T) {
				root := t.TempDir()
				sb := &fakeSandbox{workDir: root}
				run := NewAgentRunWithLayout(nil, "", sb, paths.NewLayout(&config.Config{}, root))
				run.preset, run.model, run.variant, run.review = tc.preset, tc.model, tc.variant, review
				run.runFolder, run.outputWriter = filepath.Join(root, "run"), &bytes.Buffer{}
				command := config.BuiltInAgentPresets[tc.preset].Command
				if tc.custom {
					command = "custom-agent {{.ModelFlag}} {{.VariantFlag}}"
				}
				if result := run.Run(context.Background(), &spyRenderer{}, command, prompt.RenderConfig{TaskPrompt: "task"}); result.Status != "success" {
					t.Fatalf("run failed: %+v", result)
				}
				if !strings.Contains(sb.execCommand, tc.want) || strings.Contains(sb.execCommand, tc.forbidden) {
					t.Fatalf("wrong native invocation: %s; want %q, excluding %q", sb.execCommand, tc.want, tc.forbidden)
				}
			})
		}
	}
}
