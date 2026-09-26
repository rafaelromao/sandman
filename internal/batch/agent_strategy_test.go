package batch

import (
	"bytes"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestStrategyFor_SelectsByPresetAndCommand(t *testing.T) {
	opencodeCommand := config.BuiltInAgentPresets["opencode"].Command
	claudeCommand := config.BuiltInAgentPresets["claude"].Command
	tests := []struct {
		name    string
		preset  string
		command string
		want    agentStrategy
	}{
		{name: "built-in opencode", preset: "opencode", command: opencodeCommand, want: opencodeStrategy{builtInCommand: true}},
		{name: "custom command under opencode", preset: "opencode", command: "opencode run {{.PromptFile}}", want: opencodeStrategy{builtInCommand: false}},
		{name: "built-in claude", preset: "claude", command: claudeCommand, want: claudeStrategy{builtInCommand: true}},
		{name: "custom command under claude", preset: "claude", command: "claude -p {{.PromptFile}}", want: claudeStrategy{builtInCommand: false}},
		{name: "opencode template under another preset", preset: "custom", command: opencodeCommand, want: passthroughStrategy{}},
		{name: "claude template without preset", preset: "", command: claudeCommand, want: passthroughStrategy{}},
		{name: "cross-wired template", preset: "claude", command: opencodeCommand, want: claudeStrategy{builtInCommand: false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strategyFor(tt.preset, tt.command); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("strategyFor(%q, %q) = %#v, want %#v", tt.preset, tt.command, got, tt.want)
			}
		})
	}
}

// Adding an agent means a preset entry and a strategy registry entry; this
// keeps the two registries from drifting apart.
func TestAgentStrategies_CoverEveryBuiltInPreset(t *testing.T) {
	var presets, strategies []string
	for name := range config.BuiltInAgentPresets {
		presets = append(presets, name)
	}
	for name := range agentStrategies {
		strategies = append(strategies, name)
	}
	sort.Strings(presets)
	sort.Strings(strategies)
	if !reflect.DeepEqual(presets, strategies) {
		t.Fatalf("strategy registry %v does not match built-in presets %v", strategies, presets)
	}
	for name := range config.BuiltInAgentPresets {
		if _, ok := strategyForPreset(name).(passthroughStrategy); ok {
			t.Fatalf("built-in preset %q resolves to the passthrough strategy", name)
		}
	}
}

func TestAgentStrategy_Contract(t *testing.T) {
	permissionEnv := map[string]string{
		"OPENCODE_PERMISSION": config.OpencodePermissionExternalDirectoryAllow,
		"KEEP":                "1",
	}
	type contract struct {
		modelFlag             string
		emptyModelFlag        string
		variantFlag           string
		safeEnv               map[string]string
		dangerousEnv          map[string]string
		reuseContinue         bool
		freshContinue         bool
		parsers               bool
		rolloverDetector      bool
		usageRule             bool
		awaitsUsageLimit      bool
		retriesMissingSession bool
	}
	withoutPermission := map[string]string{"KEEP": "1"}
	tests := []struct {
		name     string
		strategy agentStrategy
		want     contract
	}{
		{
			name:     "opencode built-in",
			strategy: opencodeStrategy{builtInCommand: true},
			want: contract{
				modelFlag: "-m provider/model", variantFlag: "--variant 'high'",
				safeEnv: withoutPermission, dangerousEnv: permissionEnv,
				reuseContinue: true, parsers: true, rolloverDetector: true, usageRule: true,
				awaitsUsageLimit: true, retriesMissingSession: true,
			},
		},
		{
			name:     "opencode custom command",
			strategy: opencodeStrategy{builtInCommand: false},
			want: contract{
				safeEnv: withoutPermission, dangerousEnv: permissionEnv,
				rolloverDetector: true, usageRule: true,
			},
		},
		{
			name:     "claude built-in",
			strategy: claudeStrategy{builtInCommand: true},
			want: contract{
				modelFlag: "--model 'provider/model'", variantFlag: "--effort 'high'",
				safeEnv: permissionEnv, dangerousEnv: permissionEnv,
				reuseContinue: true, parsers: true, usageRule: true, awaitsUsageLimit: true,
			},
		},
		{
			name:     "claude custom command",
			strategy: claudeStrategy{builtInCommand: false},
			want:     contract{safeEnv: permissionEnv, dangerousEnv: permissionEnv, usageRule: true},
		},
		{
			name:     "passthrough",
			strategy: passthroughStrategy{},
			want:     contract{safeEnv: permissionEnv, dangerousEnv: permissionEnv},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.strategy
			if got := s.ModelFlag("provider/model"); got != tt.want.modelFlag {
				t.Errorf("ModelFlag = %q, want %q", got, tt.want.modelFlag)
			}
			if got := s.ModelFlag(""); got != tt.want.emptyModelFlag {
				t.Errorf("ModelFlag(empty) = %q, want %q", got, tt.want.emptyModelFlag)
			}
			if got := s.VariantFlag("high"); got != tt.want.variantFlag {
				t.Errorf("VariantFlag = %q, want %q", got, tt.want.variantFlag)
			}
			if got := s.VariantFlag(""); got != "" {
				t.Errorf("VariantFlag(empty) = %q, want empty", got)
			}
			if got := s.LaunchEnv(permissionEnv, "builtin", "agent run"); !reflect.DeepEqual(got, tt.want.safeEnv) {
				t.Errorf("LaunchEnv(safe) = %v, want %v", got, tt.want.safeEnv)
			}
			if got := s.LaunchEnv(permissionEnv, "builtin", "agent run --dangerously-skip-permissions"); !reflect.DeepEqual(got, tt.want.dangerousEnv) {
				t.Errorf("LaunchEnv(dangerous) = %v, want %v", got, tt.want.dangerousEnv)
			}

			run := NewAgentRunWithLayout(&github.Issue{Number: 1}, "1-branch", &fakeSandbox{workDir: t.TempDir()}, paths.NewLayout(&config.Config{}, t.TempDir()))
			run.previousBatchID = "prior-batch"
			run.previousRunID = "prior-run"
			run.reuseSession = true
			if session, cont := s.PrepareLaunch(run); session != "" || cont != tt.want.reuseContinue {
				t.Errorf("PrepareLaunch(reuse) = (%q, %t), want (\"\", %t)", session, cont, tt.want.reuseContinue)
			}
			run.reuseSession = false
			if session, cont := s.PrepareLaunch(run); session != "" || cont != tt.want.freshContinue {
				t.Errorf("PrepareLaunch(fresh) = (%q, %t), want (\"\", %t)", session, cont, tt.want.freshContinue)
			}

			stdout, stderr := s.NewOutputParsers(&bytes.Buffer{})
			if got := stdout != nil && stderr != nil; got != tt.want.parsers {
				t.Errorf("NewOutputParsers present = %t, want %t", got, tt.want.parsers)
			}
			if !tt.want.parsers && (stdout != nil || stderr != nil) {
				t.Errorf("NewOutputParsers = (%#v, %#v), want untyped nil parsers", stdout, stderr)
			}
			if got := s.ContextRolloverDetector(nil, nil) != nil; got != tt.want.rolloverDetector {
				t.Errorf("ContextRolloverDetector present = %t, want %t", got, tt.want.rolloverDetector)
			}
			if got := s.UsageLimitRule() != nil; got != tt.want.usageRule {
				t.Errorf("UsageLimitRule present = %t, want %t", got, tt.want.usageRule)
			}
			if got := s.AwaitsUsageLimit(); got != tt.want.awaitsUsageLimit {
				t.Errorf("AwaitsUsageLimit = %t, want %t", got, tt.want.awaitsUsageLimit)
			}

			run.reuseSession = true
			missing := newOpenCodeOutput(&bytes.Buffer{}, nil, true)
			_, _ = missing.Write([]byte("Session not found\n"))
			if got := s.RetryAfterMissingSession(run, "ses-old", errors.New("exit 1"), missing, nil); got != tt.want.retriesMissingSession {
				t.Errorf("RetryAfterMissingSession = %t, want %t", got, tt.want.retriesMissingSession)
			}
			if s.RetryAfterMissingSession(run, "ses-old", nil, missing, nil) {
				t.Error("RetryAfterMissingSession retried a successful launch")
			}
		})
	}
}

func TestAwaitsUsageLimit_ResolvesStrategyFromAgentName(t *testing.T) {
	for _, tt := range []struct {
		agent string
		want  bool
	}{
		{agent: "opencode", want: true},
		{agent: " claude ", want: true},
		{agent: "custom", want: false},
		{agent: "", want: false},
	} {
		if got := AwaitsUsageLimit(tt.agent); got != tt.want {
			t.Errorf("AwaitsUsageLimit(%q) = %t, want %t", tt.agent, got, tt.want)
		}
	}
}

func TestIsUsageLimitOutput_UsesTheAgentsOwnRule(t *testing.T) {
	const opencodeLimit = "Error: The usage limit has been reached"
	const claudeLimit = `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your session limit · resets 3pm"}`
	for _, tt := range []struct {
		agent  string
		output string
		want   bool
	}{
		{agent: "opencode", output: opencodeLimit, want: true},
		{agent: "opencode", output: claudeLimit, want: false},
		{agent: "claude", output: claudeLimit, want: true},
		{agent: "claude", output: opencodeLimit, want: false},
		{agent: "custom", output: opencodeLimit + "\n" + claudeLimit, want: false},
	} {
		if got := IsUsageLimitOutput(tt.agent, tt.output); got != tt.want {
			t.Errorf("IsUsageLimitOutput(%q, %q) = %t, want %t", tt.agent, tt.output, got, tt.want)
		}
	}
}
