package batch

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// claudeRealAuthFailureResult is a real terminal stream-json record, captured
// from a Claude Code 2.1.283 print-mode run in a container without
// credentials; only the session and record IDs are sanitized. It pins the
// framing Claude Code uses for a run that ends on an API-side error: a
// top-level `result` record with `is_error: true`, `subtype: "success"`,
// `terminal_reason: "api_error"`, and the user-facing message in `result`.
const claudeRealAuthFailureResult = `{"duration_api_ms":0,"stop_reason":"stop_sequence","session_id":"00000000-0000-4000-8000-000000000001","total_cost_usd":0,"usage":{"output_tokens_details":{"thinking_tokens":0},"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"inference_geo":"","iterations":[],"speed":"standard"},"modelUsage":{},"permission_denials":[],"terminal_reason":"api_error","fast_mode_state":"off","is_error":true,"num_turns":1,"subtype":"success","api_error_status":null,"result":"Not logged in · Please run /login","type":"result","duration_ms":203,"uuid":"00000000-0000-4000-8000-000000000002","queued_turn_count":0,"result_index":0}`

// claudeUsageLimitResult is the real framing above with the documented
// subscription-limit message and HTTP 429 status. A subscription limit cannot
// be triggered on demand; replace this fixture with a real capture when one is
// available.
const claudeUsageLimitResult = `{"duration_api_ms":0,"stop_reason":"stop_sequence","session_id":"00000000-0000-4000-8000-000000000001","total_cost_usd":0,"usage":{"output_tokens_details":{"thinking_tokens":0},"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"inference_geo":"","iterations":[],"speed":"standard"},"modelUsage":{},"permission_denials":[],"terminal_reason":"api_error","fast_mode_state":"off","is_error":true,"num_turns":1,"subtype":"success","api_error_status":429,"result":"You've hit your session limit · resets 3pm (America/Sao_Paulo)","type":"result","duration_ms":203,"uuid":"00000000-0000-4000-8000-000000000002","queued_turn_count":0,"result_index":0}`

func TestClaudeUsageLimitLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "session limit result", line: claudeUsageLimitResult, want: true},
		{name: "weekly limit result", line: `{"type":"result","is_error":true,"result":"You've hit your weekly limit · resets Mon 9am"}`, want: true},
		{name: "opus limit result", line: `{"type":"result","is_error":true,"result":"You've hit your Opus limit"}`, want: true},
		{name: "sonnet limit result", line: `{"type":"result","is_error":true,"result":"You've hit your Sonnet limit"}`, want: true},
		{name: "fable consent limit result", line: `{"type":"result","is_error":true,"result":"Fable limit reached"}`, want: true},
		{name: "message in errors array", line: `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["You've hit your session limit"]}`, want: true},
		{name: "spaced json", line: `{"type": "result", "is_error": true, "result": "You've hit your session limit"}`, want: true},
		{name: "sandman prefixed line", line: "[260925-abcd-42] 15:04:05 " + claudeUsageLimitResult, want: true},
		{name: "nested forwarded prefix", line: "[outer] 15:04:05 [inner] 15:04:05 " + claudeUsageLimitResult, want: false},
		{name: "assistant echo", line: `{"type":"assistant","message":{"content":[{"type":"text","text":"You've hit your session limit"}]},"is_error":false}`, want: false},
		{name: "tool result error echo", line: `{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"You've hit your session limit"}]}}`, want: false},
		{name: "successful result", line: `{"type":"result","is_error":false,"result":"You've hit your session limit"}`, want: false},
		{name: "spend limit", line: `{"type":"result","is_error":true,"result":"You've hit your monthly spend limit"}`, want: false},
		{name: "shared budget", line: `{"type":"result","is_error":true,"result":"You've hit your team's shared budget"}`, want: false},
		{name: "opencode literal", line: "Error: The usage limit has been reached", want: false},
		{name: "plain text phrase", line: "You've hit your session limit", want: false},
		{name: "malformed json", line: `{"type":"result","is_error":true,"result":"You've hit your session limit"`, want: false},
		{name: "real authentication failure is not a usage limit", line: claudeRealAuthFailureResult, want: false},
		{name: "legacy usage limit wording", line: `{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached|1790434800"}`, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeUsageLimitLine(tt.line); got != tt.want {
				t.Fatalf("claudeUsageLimitLine(%q) = %t, want %t", tt.line, got, tt.want)
			}
		})
	}
	if opencodeUsageLimitLine(claudeUsageLimitResult) {
		t.Fatal("OpenCode rule recognised the Claude usage-limit result")
	}
}

type claudeLaunch struct {
	command string
	status  string
	result  AgentRunResult
	runLog  string
	root    string
}

func runClaudeLaunch(t *testing.T, configure func(*AgentRun), results ...opencodeExecResult) claudeLaunch {
	t.Helper()
	root := t.TempDir()
	agent := config.BuiltInAgentPresets["claude"].Agent("claude")
	sb := &opencodeSequenceSandbox{fakeSandbox: fakeSandbox{workDir: filepath.Join(root, "worktree")}, results: results}
	run := NewAgentRunWithLayout(&github.Issue{Number: 42, Title: "Claude"}, "42-claude", sb, paths.NewLayout(&config.Config{}, root))
	run.preset = agent.Preset
	run.env = agent.Env
	run.opencodePermissionMode = agent.OpencodePermissionMode
	run.runID = "run-1"
	run.batchID = "batch-1"
	run.runFolder = filepath.Join(root, "run")
	run.sessionName = "Sandman run-1: "
	run.outputWriter = &bytes.Buffer{}
	if configure != nil {
		configure(run)
	}
	result := run.Run(context.Background(), &spyRenderer{result: "task"}, agent.Command, prompt.RenderConfig{})
	launch := claudeLaunch{status: result.Status, result: result, root: root}
	if len(sb.commands) > 0 {
		launch.command = sb.commands[len(sb.commands)-1]
	}
	if data, err := os.ReadFile(filepath.Join(root, "run", "run.log")); err == nil {
		launch.runLog = string(data)
	}
	return launch
}

func TestAgentRun_ClaudePresetRendersPrintModeCommand(t *testing.T) {
	const env = "export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1; export CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0; export DISABLE_AUTOUPDATER=1; export IS_SANDBOX=1; "
	skip := true
	noSkip := false
	ok := opencodeExecResult{stdout: `{"type":"system","subtype":"init","session_id":"abc"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"SMOKE_OK"}` + "\n"}
	tests := []struct {
		name      string
		configure func(*AgentRun)
		want      string
	}{
		{
			name: "container launch",
			configure: func(r *AgentRun) {
				r.model = "sonnet"
				r.dangerouslySkipPermissions = &skip
			},
			want: env + `claude -p --output-format stream-json --verbose --dangerously-skip-permissions --name 'Sandman run-1: ' --model 'sonnet' "$(cat .sandman/task.md)"`,
		},
		{
			name: "await re-entry continues the conversation",
			configure: func(r *AgentRun) {
				r.model = "sonnet"
				r.dangerouslySkipPermissions = &skip
				r.reuseSession = true
				r.previousBatchID = "batch-0"
				r.previousRunID = "run-0"
			},
			want: env + `claude -p --output-format stream-json --verbose --continue --dangerously-skip-permissions --name 'Sandman run-1: ' --model 'sonnet' "$(cat .sandman/task.md)"`,
		},
		{
			name: "worktree launch keeps permission prompts",
			configure: func(r *AgentRun) {
				r.dangerouslySkipPermissions = &noSkip
			},
			want: env + `claude -p --output-format stream-json --verbose --name 'Sandman run-1: ' "$(cat .sandman/task.md)"`,
		},
		{
			name: "variant maps to effort",
			configure: func(r *AgentRun) {
				r.model = " opus "
				r.variant = "high"
			},
			want: env + `claude -p --output-format stream-json --verbose --name 'Sandman run-1: ' --model 'opus' --effort 'high' "$(cat .sandman/task.md)"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			launch := runClaudeLaunch(t, tt.configure, ok)
			if launch.status != "success" {
				t.Fatalf("status = %q, want success", launch.status)
			}
			if launch.command != tt.want {
				t.Fatalf("command:\n got %s\nwant %s", launch.command, tt.want)
			}
			if !strings.Contains(launch.runLog, "Result: success") || strings.Contains(launch.runLog, `"type":"result"`) {
				t.Fatalf("run.log = %q, want the rendered result instead of raw stream-json", launch.runLog)
			}
			if _, err := os.Stat(filepath.Join(launch.root, "run", "session.json")); !os.IsNotExist(err) {
				t.Fatalf("session.json stat err = %v, want no OpenCode session identity for claude", err)
			}
		})
	}
}

func TestAgentRun_ClaudeRecognisesUsageLimitOnlyOnFailedResult(t *testing.T) {
	assistantEcho := `{"type":"assistant","message":{"content":[{"type":"text","text":"You've hit your session limit"}]}}`
	tests := []struct {
		name        string
		result      opencodeExecResult
		wantReached bool
	}{
		{name: "limited result", result: opencodeExecResult{stdout: claudeUsageLimitResult + "\n", err: errors.New("exit status 1")}, wantReached: true},
		{name: "successful exit", result: opencodeExecResult{stdout: claudeUsageLimitResult + "\n"}, wantReached: false},
		{name: "assistant echo", result: opencodeExecResult{stdout: assistantEcho + "\n", err: errors.New("exit status 1")}, wantReached: false},
		{name: "opencode literal", result: opencodeExecResult{stderr: "Error: The usage limit has been reached\n", err: errors.New("exit status 1")}, wantReached: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			launch := runClaudeLaunch(t, nil, tt.result)
			if launch.result.UsageLimitReached != tt.wantReached {
				t.Fatalf("UsageLimitReached = %t, want %t (result=%+v)", launch.result.UsageLimitReached, tt.wantReached, launch.result)
			}
			if launch.result.ContextExhausted {
				t.Fatal("claude launch recorded context exhaustion; Claude Code compacts on its own")
			}
		})
	}
}

func TestAgentRun_ClaudeDoesNotRolloverOnContextErrors(t *testing.T) {
	launch := runClaudeLaunch(t, nil, opencodeExecResult{
		stderr: "Error: prompt is too long\nError: prompt is too long\n",
		err:    errors.New("exit status 1"),
	})
	if launch.result.ContextExhausted {
		t.Fatal("claude launch took the OpenCode context-rollover path")
	}
	if launch.status != "failure" {
		t.Fatalf("status = %q, want the ordinary failure path", launch.status)
	}
}
