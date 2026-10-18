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
	command  string
	commands []string
	status   string
	result   AgentRunResult
	runLog   string
	root     string
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
	launch.commands = sb.commands
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
			name: "await re-entry without a recorded session starts fresh",
			configure: func(r *AgentRun) {
				r.model = "sonnet"
				r.dangerouslySkipPermissions = &skip
				r.reuseSession = true
				r.previousBatchID = "batch-0"
				r.previousRunID = "run-0"
			},
			want: env + `claude -p --output-format stream-json --verbose --dangerously-skip-permissions --name 'Sandman run-1: ' --model 'sonnet' "$(cat .sandman/task.md)"`,
		},
		{
			name: "worktree launch keeps permission prompts",
			configure: func(r *AgentRun) {
				r.dangerouslySkipPermissions = &noSkip
			},
			want: env + `claude -p --output-format stream-json --verbose --name 'Sandman run-1: ' "$(cat .sandman/task.md)"`,
		},
		{
			name: "repair launch may edit only .sandman",
			configure: func(r *AgentRun) {
				r.dangerouslySkipPermissions = &noSkip
				r.editScope = ".sandman"
			},
			want: env + `claude -p --output-format stream-json --verbose --allowedTools 'Edit(/.sandman/**)' --permission-mode dontAsk --name 'Sandman run-1: ' "$(cat .sandman/task.md)"`,
		},
		{
			name: "skipped permissions need no edit scope",
			configure: func(r *AgentRun) {
				r.dangerouslySkipPermissions = &skip
				r.editScope = ".sandman"
			},
			want: env + `claude -p --output-format stream-json --verbose --dangerously-skip-permissions --name 'Sandman run-1: ' "$(cat .sandman/task.md)"`,
		},
		{
			name: "configured permission mode replaces skipped permissions",
			configure: func(r *AgentRun) {
				r.model = "sonnet"
				r.variant = "high"
				r.dangerouslySkipPermissions = &skip
				r.permissionMode = "acceptEdits"
				r.reuseSession = true
				r.previousBatchID = "batch-0"
				r.previousRunID = "run-0"
			},
			want: env + `claude -p --output-format stream-json --verbose --permission-mode 'acceptEdits' --name 'Sandman run-1: ' --model 'sonnet' --effort 'high' "$(cat .sandman/task.md)"`,
		},
		{
			name: "configured permission mode applies to worktree launches",
			configure: func(r *AgentRun) {
				r.dangerouslySkipPermissions = &noSkip
				r.permissionMode = "dontAsk"
			},
			want: env + `claude -p --output-format stream-json --verbose --permission-mode 'dontAsk' --name 'Sandman run-1: ' "$(cat .sandman/task.md)"`,
		},
		{
			name: "repair edit scope wins over a configured permission mode",
			configure: func(r *AgentRun) {
				r.dangerouslySkipPermissions = &noSkip
				r.editScope = ".sandman"
				r.permissionMode = "acceptEdits"
			},
			want: env + `claude -p --output-format stream-json --verbose --allowedTools 'Edit(/.sandman/**)' --permission-mode dontAsk --name 'Sandman run-1: ' "$(cat .sandman/task.md)"`,
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
			if id, ok := ReadAgentSessionID(filepath.Join(launch.root, "run")); !ok || id != "abc" {
				t.Fatalf("agent session ID = %q, %v; want the observed Claude session abc", id, ok)
			}
			data, err := os.ReadFile(filepath.Join(launch.root, "run", "session.json"))
			if err != nil || !strings.Contains(string(data), `"protocol": "claude-session/v1"`) || !strings.Contains(string(data), `"provider": "claude"`) {
				t.Fatalf("session.json = %s, %v; want a Claude session identity", data, err)
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

// An unattended Claude Code session cannot answer permission prompts, so its
// refused tools are the evidence that it could not act. The final result's
// permission_denials list is authoritative; per-call records are best-effort.
func TestAgentRun_ClaudeReportsPermissionDenials(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"abc"}` + "\n" +
		`{"type":"system","subtype":"permission_denied","tool_name":"Edit","decision_reason":"not allowed"}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"could not edit","permission_denials":[{"tool_name":"Bash","tool_use_id":"t2"},{"tool_name":"Edit","tool_use_id":"t1"}]}` + "\n"
	for _, tc := range []struct {
		name   string
		result opencodeExecResult
		status string
	}{
		{name: "clean exit", result: opencodeExecResult{stdout: stream}, status: "success"},
		{name: "failed exit", result: opencodeExecResult{stdout: stream, err: errors.New("exit status 1")}, status: "failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launch := runClaudeLaunch(t, nil, tc.result)
			if launch.status != tc.status {
				t.Fatalf("status = %q, want %q", launch.status, tc.status)
			}
			if got, want := strings.Join(launch.result.PermissionDenials, ","), "Edit,Bash"; got != want {
				t.Fatalf("PermissionDenials = %q, want %q", got, want)
			}
			if !strings.Contains(launch.runLog, "Permission denied: Edit") {
				t.Fatalf("run.log lost the rendered denial:\n%s", launch.runLog)
			}
		})
	}
}

func TestAgentRun_ClaudeWithoutDenialsReportsNone(t *testing.T) {
	launch := runClaudeLaunch(t, nil, opencodeExecResult{stdout: `{"type":"result","subtype":"success","is_error":false,"result":"done","permission_denials":[]}` + "\n"})
	if launch.status != "success" || len(launch.result.PermissionDenials) != 0 {
		t.Fatalf("status=%q denials=%v", launch.status, launch.result.PermissionDenials)
	}
}

// Only Claude Code takes a permission mode or a scoped edit allowance from the
// command line; other agents and custom commands launch exactly as before.
func TestPermissionFlagIsClaudeOnly(t *testing.T) {
	claude := config.BuiltInAgentPresets["claude"].Command
	opencode := config.BuiltInAgentPresets["opencode"].Command
	for _, tc := range []struct {
		name, preset, command, wantScope, wantMode string
	}{
		{name: "claude", preset: "claude", command: claude, wantScope: "--allowedTools 'Edit(/.sandman/**)' --permission-mode dontAsk", wantMode: "--permission-mode 'acceptEdits'"},
		{name: "claude custom command", preset: "claude", command: `claude -p "$(cat {{.PromptFile}})"`},
		{name: "opencode", preset: "opencode", command: opencode},
		{name: "custom provider", preset: "", command: "my-agent {{.PromptFile}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			strategy := strategyFor(tc.preset, tc.command)
			if got := strategy.PermissionFlag("", ".sandman"); got != tc.wantScope {
				t.Fatalf("PermissionFlag(scope) = %q, want %q", got, tc.wantScope)
			}
			if got := strategy.PermissionFlag("acceptEdits", ".sandman"); got != tc.wantScope {
				t.Fatalf("PermissionFlag(mode, scope) = %q, want the scope %q", got, tc.wantScope)
			}
			if got := strategy.PermissionFlag("acceptEdits", ""); got != tc.wantMode {
				t.Fatalf("PermissionFlag(mode) = %q, want %q", got, tc.wantMode)
			}
			if got := strategy.PermissionFlag("", ""); got != "" {
				t.Fatalf("no mode and no scope rendered %q", got)
			}
		})
	}
}

// A configured permission mode reaches the launched command of both
// implementation and review rows.
func TestRunBatch_ConfiguredPermissionModeReachesClaudeLaunches(t *testing.T) {
	newOrchestrator := func(sb *fakeSandbox) *Orchestrator {
		client := &fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."}},
			prs:    map[string]*github.PR{"42-fix-bug": mergedPR("42-fix-bug", "")},
		}
		return NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
			Agent:        "claude",
			DefaultModel: "sonnet",
			Sandbox:      "worktree",
			WorktreeDir:  ".sandman/worktrees",
			Git:          config.GitConfig{BaseBranch: "main"},
			AgentProviders: map[string]config.Agent{
				"claude": {Preset: "claude", PermissionMode: "acceptEdits"},
			},
		}}, &spyEventLog{}, WithSandboxFactory(&fakeSandboxFactory{sandbox: sb}))
	}
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{name: "implementation", req: Request{Issues: []int{42}}},
		{name: "review", req: Request{
			Agent:        "claude",
			Model:        "sonnet",
			Review:       true,
			PRNumber:     42,
			PromptConfig: prompt.RenderConfig{PromptFlag: "review prompt", Branch: "review-branch"},
			RunID:        "review-run",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &fakeSandbox{}
			tc.req.RunTS, tc.req.RunShortID = orchTestRunTS, orchTestRunShortID
			_, _ = newOrchestrator(sb).RunBatch(context.Background(), tc.req)
			if !strings.Contains(sb.execCommand, "--permission-mode 'acceptEdits'") || strings.Contains(sb.execCommand, "--dangerously-skip-permissions") {
				t.Fatalf("command = %q, want the configured permission mode", sb.execCommand)
			}
		})
	}
}

const recordedClaudeSession = "6f1c2a4e-0b7d-4c1a-9a55-2f0e8d3b7c11"

// reuseRecordedClaudeSession selects session reuse with the prior Run's
// recorded Claude identity set to sessionID.
func reuseRecordedClaudeSession(t *testing.T, sessionID string) func(*AgentRun) {
	return func(r *AgentRun) {
		r.reuseSession = true
		r.previousBatchID = "batch-0"
		r.previousRunID = "run-0"
		if err := writeAgentSession(r.layout.RunSessionPath("batch-0", "run-0"), claudeSessionKind, sessionID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgentRun_ClaudeReuseResumesTheExactRecordedSession(t *testing.T) {
	ok := opencodeExecResult{stdout: `{"type":"system","subtype":"init","session_id":"` + recordedClaudeSession + `"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"OK"}` + "\n"}
	launch := runClaudeLaunch(t, reuseRecordedClaudeSession(t, recordedClaudeSession), ok)
	if launch.status != "success" {
		t.Fatalf("status = %q, want success", launch.status)
	}
	if !strings.Contains(launch.command, "--resume '"+recordedClaudeSession+"'") || strings.Contains(launch.command, "--continue") {
		t.Fatalf("command = %s, want the exact recorded session and no directory-scoped --continue", launch.command)
	}
}

func TestAgentRun_ClaudeIgnoresAnInvalidRecordedSession(t *testing.T) {
	var warnings bytes.Buffer
	ok := opencodeExecResult{stdout: `{"type":"system","subtype":"init","session_id":"` + recordedClaudeSession + `"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"OK"}` + "\n"}
	reuse := reuseRecordedClaudeSession(t, "x'; rm -rf ~ #")
	launch := runClaudeLaunch(t, func(r *AgentRun) {
		reuse(r)
		r.sessionWarning = &warnings
	}, ok)
	if strings.Contains(launch.command, "--resume") || strings.Contains(launch.command, "rm -rf") || strings.Contains(launch.command, "--continue") {
		t.Fatalf("command = %s, want a fresh launch that never renders the recorded value", launch.command)
	}
	if !strings.Contains(warnings.String(), "agent session metadata") {
		t.Fatalf("warnings = %q, want the invalid identity reported", warnings.String())
	}
}

func TestAgentRun_ClaudeMissingSessionRelaunchesFresh(t *testing.T) {
	const missing = `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"` + recordedClaudeSession + `","errors":["No conversation found with session ID: ` + recordedClaudeSession + `"]}`
	const fresh = "0d9e8f7a-6b5c-4d3e-8f1a-2b3c4d5e6f70"
	launch := runClaudeLaunch(t, reuseRecordedClaudeSession(t, recordedClaudeSession),
		opencodeExecResult{stdout: missing + "\n", stderr: "No conversation found with session ID: " + recordedClaudeSession + "\n", err: errors.New("exit status 1")},
		opencodeExecResult{stdout: `{"type":"system","subtype":"init","session_id":"` + fresh + `"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"OK"}` + "\n"},
	)
	if launch.status != "success" {
		t.Fatalf("status = %q, want the fresh relaunch to succeed", launch.status)
	}
	if len(launch.commands) != 2 {
		t.Fatalf("commands = %q, want the exact attempt plus one fresh relaunch", launch.commands)
	}
	if !strings.Contains(launch.commands[0], "--resume '"+recordedClaudeSession+"'") {
		t.Fatalf("first command = %s, want the exact recorded session", launch.commands[0])
	}
	if strings.Contains(launch.commands[1], "--resume") || strings.Contains(launch.commands[1], "--continue") {
		t.Fatalf("relaunch = %s, want a fresh session", launch.commands[1])
	}
	if id, ok := ReadAgentSessionID(filepath.Join(launch.root, "run")); !ok || id != fresh {
		t.Fatalf("recorded session = %q, %v; want the fresh relaunch's session %s", id, ok, fresh)
	}
}

func TestAgentRun_ClaudeMissingSessionRecordIsNotPersisted(t *testing.T) {
	const missing = `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"` + recordedClaudeSession + `","errors":["No conversation found with session ID: ` + recordedClaudeSession + `"]}`
	launch := runClaudeLaunch(t, nil, opencodeExecResult{stdout: missing + "\n", err: errors.New("exit status 1")})
	if launch.status != "failure" {
		t.Fatalf("status = %q, want failure", launch.status)
	}
	if id, ok := ReadAgentSessionID(filepath.Join(launch.root, "run")); ok {
		t.Fatalf("recorded session = %q, want nothing from a missing-session result", id)
	}
}
