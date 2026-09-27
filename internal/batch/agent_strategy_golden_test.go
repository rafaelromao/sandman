package batch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// The OpenCode launch golden pins what AgentRun renders and records for the
// built-in OpenCode preset, custom commands under that preset, and commands
// outside any built-in preset. It was captured before the agent strategy seam
// existed, so a diff here means the seam changed OpenCode behaviour.
//
// Regenerate the golden only for an intended OpenCode behaviour change:
//
//	SANDMAN_AGENT_STRATEGY_UPDATE=1 go test ./internal/batch/ -run TestAgentStrategyGolden_OpenCodeLaunches

type agentLaunchGoldenCase struct {
	name          string
	preset        string
	command       string
	model         string
	variant       string
	skip          *bool
	env           map[string]string
	permission    string
	sessionName   string
	reuse         bool
	priorSession  string
	results       []opencodeExecResult
	extraLiterals []string
}

const goldenCustomOpenCodeCommand = `opencode run --format json{{if .ModelFlag}} {{.ModelFlag}}{{end}}{{if .VariantFlag}} {{.VariantFlag}}{{end}}{{if .ContinueFlag}} --continue{{end}} "$(cat {{.PromptFile}})"`

func goldenBool(value bool) *bool { return &value }

func agentLaunchGoldenCases() []agentLaunchGoldenCase {
	builtIn := config.BuiltInAgentPresets["opencode"].Command
	permissionEnv := map[string]string{
		"OPENCODE_PERMISSION": config.OpencodePermissionExternalDirectoryAllow,
		"EXTRA_FLAG":          "value with space",
	}
	textOK := opencodeExecResult{stdout: `{"type":"text","sessionID":"ses-new","part":{"text":"working"}}` + "\n"}
	usageLimit := opencodeExecResult{
		stdout: `{"type":"text","sessionID":"ses-new","part":{"text":"working"}}` + "\n",
		stderr: "Error: The usage limit has been reached\n",
		err:    errors.New("agent failed"),
	}
	overflow := opencodeExecResult{
		stderr: "Error: prompt is too long\nError: prompt is too long\n",
		err:    errors.New("agent failed"),
	}
	return []agentLaunchGoldenCase{
		{name: "builtin-defaults", preset: "opencode", command: builtIn, results: []opencodeExecResult{textOK}},
		{name: "builtin-model-variant-session-name", preset: "opencode", command: builtIn, model: "opencode/big-pickle", variant: "high effort", sessionName: "Sandman run-1: ", results: []opencodeExecResult{textOK}},
		{name: "builtin-dangerous-builtin-permission", preset: "opencode", command: builtIn, skip: goldenBool(true), env: permissionEnv, permission: "builtin", results: []opencodeExecResult{textOK}},
		{name: "builtin-safe-builtin-permission", preset: "opencode", command: builtIn, skip: goldenBool(false), env: permissionEnv, permission: "builtin", results: []opencodeExecResult{textOK}},
		{name: "builtin-safe-custom-permission", preset: "opencode", command: builtIn, skip: goldenBool(false), env: permissionEnv, permission: "custom", results: []opencodeExecResult{textOK}},
		{name: "custom-command-opencode-preset", preset: "opencode", command: goldenCustomOpenCodeCommand, model: "opencode/big-pickle", variant: "high", skip: goldenBool(false), env: permissionEnv, permission: "builtin", reuse: true, priorSession: "ses-old", results: []opencodeExecResult{textOK}},
		{name: "builtin-text-other-preset", preset: "custom", command: builtIn, model: "opencode/big-pickle", variant: "high", env: permissionEnv, reuse: true, priorSession: "ses-old", results: []opencodeExecResult{textOK}},
		{name: "builtin-text-no-preset", preset: "", command: builtIn, model: "opencode/big-pickle", variant: "high", results: []opencodeExecResult{textOK}},
		{name: "builtin-reuse-prior-session", preset: "opencode", command: builtIn, reuse: true, priorSession: "ses-old", results: []opencodeExecResult{textOK}},
		{name: "builtin-reuse-without-prior-session", preset: "opencode", command: builtIn, reuse: true, results: []opencodeExecResult{textOK}},
		{name: "builtin-reuse-missing-session-fallback", preset: "opencode", command: builtIn, reuse: true, priorSession: "ses-old", env: map[string]string{"TEST_ENV": "test-value"}, results: []opencodeExecResult{
			{stdout: `{"type":"error","sessionID":"ses-old","error":{"message":"Session not found"}}` + "\n", err: errors.New("exact session missing")},
			{stdout: `{"type":"text","sessionID":"ses-fallback","part":{"text":"resumed"}}` + "\n"},
		}},
		{name: "builtin-reuse-unrelated-failure", preset: "opencode", command: builtIn, reuse: true, priorSession: "ses-old", results: []opencodeExecResult{
			{stdout: `{"type":"error","sessionID":"ses-old","error":{"message":"provider exploded"}}` + "\n", err: errors.New("provider failure")},
		}},
		{name: "builtin-usage-limit", preset: "opencode", command: builtIn, results: []opencodeExecResult{usageLimit}},
		{name: "custom-command-usage-limit", preset: "opencode", command: goldenCustomOpenCodeCommand, results: []opencodeExecResult{usageLimit}},
		{name: "other-preset-usage-limit", preset: "custom", command: goldenCustomOpenCodeCommand, results: []opencodeExecResult{usageLimit}},
		{name: "builtin-context-overflow", preset: "opencode", command: builtIn, results: []opencodeExecResult{overflow}},
		{name: "custom-command-context-overflow", preset: "opencode", command: goldenCustomOpenCodeCommand, results: []opencodeExecResult{overflow}},
		{name: "custom-command-configured-context-phrase", preset: "opencode", command: goldenCustomOpenCodeCommand, extraLiterals: []string{"window full"}, results: []opencodeExecResult{
			{stderr: "Error: window full\nError: window full\n", err: errors.New("agent failed")},
		}},
		{name: "other-preset-context-overflow", preset: "custom", command: goldenCustomOpenCodeCommand, results: []opencodeExecResult{overflow}},
		{name: "builtin-malformed-output", preset: "opencode", command: builtIn, results: []opencodeExecResult{
			{stdout: "not json\n", stderr: "plain stderr\n"},
		}},
	}
}

var goldenLogTimestamp = regexp.MustCompile(`\] \d\d:\d\d:\d\d `)

func runAgentLaunchGoldenCase(t *testing.T, tc agentLaunchGoldenCase) string {
	t.Helper()
	root := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, root)
	runFolder := filepath.Join(root, "run")
	if tc.priorSession != "" {
		if err := writeOpenCodeSession(layout.RunSessionPath("prior-batch", "prior-run"), tc.priorSession); err != nil {
			t.Fatal(err)
		}
	}
	sb := &opencodeSequenceSandbox{
		fakeSandbox: fakeSandbox{workDir: filepath.Join(root, "worktree")},
		results:     append([]opencodeExecResult(nil), tc.results...),
	}
	var warnings bytes.Buffer
	run := NewAgentRunWithLayout(&github.Issue{Number: 42, Title: "Golden"}, "42-golden", sb, layout)
	run.preset = tc.preset
	run.model = tc.model
	run.variant = tc.variant
	run.dangerouslySkipPermissions = tc.skip
	run.env = tc.env
	run.opencodePermissionMode = tc.permission
	run.sessionName = tc.sessionName
	run.reuseSession = tc.reuse
	run.previousBatchID = "prior-batch"
	run.previousRunID = "prior-run"
	run.batchID = "current-batch"
	run.runID = "run-1"
	run.runFolder = runFolder
	run.sessionWarning = &warnings
	run.outputWriter = &bytes.Buffer{}
	run.contextRolloverLiterals = tc.extraLiterals

	result := run.Run(context.Background(), &spyRenderer{result: "task"}, tc.command, prompt.RenderConfig{})

	var b strings.Builder
	fmt.Fprintf(&b, "== %s\n", tc.name)
	for i, command := range sb.commands {
		fmt.Fprintf(&b, "command[%d]: %s\n", i, command)
	}
	fmt.Fprintf(&b, "status: %s\n", result.Status)
	fmt.Fprintf(&b, "usage_limit_reached: %t\n", result.UsageLimitReached)
	fmt.Fprintf(&b, "context_exhausted: %t\n", result.ContextExhausted)
	session, err := os.ReadFile(filepath.Join(runFolder, "session.json"))
	switch {
	case err == nil:
		fmt.Fprintf(&b, "session.json: %s\n", strings.TrimSpace(string(session)))
	case os.IsNotExist(err):
		b.WriteString("session.json: <absent>\n")
	default:
		t.Fatalf("read session.json: %v", err)
	}
	logData, err := os.ReadFile(filepath.Join(runFolder, "run.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read run.log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(logData), "\n"), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintf(&b, "run.log: %s\n", goldenLogTimestamp.ReplaceAllString(line, "] HH:MM:SS "))
	}
	warningLines := strings.Split(strings.TrimRight(warnings.String(), "\n"), "\n")
	sort.Strings(warningLines)
	for _, line := range warningLines {
		if line != "" {
			fmt.Fprintf(&b, "warning: %s\n", line)
		}
	}
	return b.String()
}

func TestAgentStrategyGolden_OpenCodeLaunches(t *testing.T) {
	var captured strings.Builder
	for _, tc := range agentLaunchGoldenCases() {
		captured.WriteString(runAgentLaunchGoldenCase(t, tc))
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file path")
	}
	goldenPath := filepath.Join(filepath.Dir(file), "agent_strategy_test", "opencode_launches.golden")
	if os.Getenv("SANDMAN_AGENT_STRATEGY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(captured.String()), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated golden %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("missing golden %s (run with SANDMAN_AGENT_STRATEGY_UPDATE=1 to create): %v", goldenPath, err)
	}
	if got := captured.String(); got != string(want) {
		t.Fatalf("OpenCode launch golden mismatch\n--- want\n%s\n--- got\n%s", want, got)
	}
}
