package batch

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func renderClaudeStream(t *testing.T, stream string) (string, outputParser) {
	t.Helper()
	var out bytes.Buffer
	stdout, _ := newClaudeOutputs()
	stdout.setDestination(&out)
	if _, err := stdout.Write([]byte(stream)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := stdout.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return out.String(), stdout
}

func TestClaudeOutput_RendersStreamJSONReadably(t *testing.T) {
	stream := strings.Join([]string{
		`⚠ Sandbox disabled: sandbox is enabled but dependencies are missing`,
		`{"type":"system","subtype":"commands_changed","commands":[{"name":"sandman"}]}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1790701200,"rateLimitType":"seven_day","utilization":0.82}}`,
		`{"type":"system","subtype":"init","cwd":"/workspace","session_id":"ed547cbf","model":"claude-opus-5-5","permissionMode":"bypassPermissions","claude_code_version":"2.1.283"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":50,"session_id":"ed547cbf"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"Reading the issue first."}]},"session_id":"ed547cbf"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh issue view 2514\n--json body","description":"Read issue"}}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"t1","type":"tool_result","content":"{\"body\":\"ok\"}"}]},"tool_use_result":{"stdout":"ok","stderr":""}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/workspace/internal/cmd/run.go"}},{"type":"tool_use","id":"t3","name":"Skill","input":{"skill":"sandman-implement"}}]}}`,
		`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"t4","decision_reason_type":"safety"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"This Bash command contains multiple operations.\nThe following part requires approval","is_error":true,"tool_use_id":"t4"}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta"}}`,
		`{"type":"tool_progress","tool_use_id":"t5-heartbeat-0","tool_name":"Bash","parent_tool_use_id":"t5","elapsed_time_seconds":30,"heartbeat":true,"session_id":"ed547cbf"}`,
		`{"type":"system","subtype":"compact_boundary"}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":12,"duration_ms":61400,"total_cost_usd":1.234,"result":"Done.","session_id":"ed547cbf"}`,
	}, "\n") + "\n"

	got, parser := renderClaudeStream(t, stream)
	want := strings.Join([]string{
		`⚠ Sandbox disabled: sandbox is enabled but dependencies are missing`,
		`Rate limit: allowed_warning (seven_day), 82% used, resets 2026-09-29 17:00 UTC`,
		`Claude Code 2.1.283 · model claude-opus-5-5 · permissions bypassPermissions · session ed547cbf`,
		`Reading the issue first.`,
		`$ gh issue view 2514 --json body`,
		`→ Read /workspace/internal/cmd/run.go`,
		`→ Skill "sandman-implement"`,
		`Permission denied: Bash`,
		`Tool error: This Bash command contains multiple operations. The following part requires approval`,
		`Conversation compacted`,
		`Result: success · 12 turns · 1m1s · $1.23`,
	}, "\n") + "\n"
	if got != want {
		t.Fatalf("rendered stream:\n%s\nwant:\n%s", got, want)
	}
	if parser.SessionID() != "ed547cbf" {
		t.Fatalf("session id = %q", parser.SessionID())
	}
	if parser.UsageLimitReached() {
		t.Fatal("successful stream reported a usage limit")
	}
}

func TestClaudeOutput_RecognisesUsageLimitBeforeRendering(t *testing.T) {
	got, parser := renderClaudeStream(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"You've hit your session limit"}]}}`+"\n"+
			claudeUsageLimitResult+"\n")
	if !parser.UsageLimitReached() {
		t.Fatal("usage-limit result was not recognised")
	}
	if !strings.Contains(got, "Result: error · 1 turns\n") || !strings.Contains(got, "Error: You've hit your session limit · resets 3pm (America/Sao_Paulo)") {
		t.Fatalf("rendered = %q, want the error result summary and message", got)
	}

	_, echo := renderClaudeStream(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"You've hit your session limit"}]}}`+"\n")
	if echo.UsageLimitReached() {
		t.Fatal("assistant echo of the limit message was treated as a usage limit")
	}
}

func TestClaudeOutput_RendersRealAuthFailureResult(t *testing.T) {
	got, parser := renderClaudeStream(t, claudeRealAuthFailureResult+"\n")
	if got != "Result: error · 1 turns\nError: Not logged in · Please run /login\n" {
		t.Fatalf("rendered = %q", got)
	}
	if parser.UsageLimitReached() {
		t.Fatal("authentication failure was treated as a usage limit")
	}
}

func TestClaudeOutput_KeepsUnknownAndPartialLines(t *testing.T) {
	got, _ := renderClaudeStream(t, `{"type":"future_record","value":1}`+"\n"+`not json at all`)
	want := `{"type":"future_record","value":1}` + "\n" + `not json at all`
	if got != want {
		t.Fatalf("rendered = %q, want unknown records and a trailing partial line unchanged", got)
	}
}

func TestClaudeOutput_DroppedRecordsReportProgress(t *testing.T) {
	var out bytes.Buffer
	stdout, _ := newClaudeOutputs()
	stdout.setDestination(&out)
	progress := 0
	stdout.(progressObserver).setProgress(func() { progress++ })
	_, _ = stdout.Write([]byte(strings.Join([]string{
		`{"type":"tool_progress","tool_name":"Bash","elapsed_time_seconds":30,"heartbeat":true}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":50}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"visible"}]}}`,
	}, "\n") + "\n"))
	if progress != 2 {
		t.Fatalf("progress calls = %d, want one per dropped record", progress)
	}
	if out.String() != "visible\n" {
		t.Fatalf("rendered = %q", out.String())
	}
}

// The idle-timeout heartbeat watches run.log's modification time. A long tool
// call emits only heartbeat records, which the renderer drops, so they must
// still advance the log's modification time.
func TestAgentRun_ClaudeDroppedHeartbeatsKeepRunLogFresh(t *testing.T) {
	stale := time.Now().Add(-time.Hour)
	var before, after time.Time
	root := t.TempDir()
	sb := &claudeHeartbeatSandbox{fakeSandbox: fakeSandbox{workDir: filepath.Join(root, "worktree")}, logPath: filepath.Join(root, "run", "run.log"), stale: stale, before: &before, after: &after}
	agent := config.BuiltInAgentPresets["claude"].Agent("claude")
	run := NewAgentRunWithLayout(&github.Issue{Number: 42}, "42-claude", sb, paths.NewLayout(&config.Config{}, root))
	run.preset = agent.Preset
	run.runID = "run-1"
	run.runFolder = filepath.Join(root, "run")
	run.outputWriter = &bytes.Buffer{}
	if result := run.Run(context.Background(), &spyRenderer{result: "task"}, agent.Command, prompt.RenderConfig{}); result.Status != "success" {
		t.Fatalf("status = %q", result.Status)
	}
	if !before.Equal(stale) {
		t.Fatalf("setup mtime = %v, want %v", before, stale)
	}
	if !after.After(stale) {
		t.Fatalf("run.log mtime after a dropped heartbeat = %v, want it advanced past %v", after, stale)
	}
}

type claudeHeartbeatSandbox struct {
	fakeSandbox
	logPath       string
	stale         time.Time
	before, after *time.Time
}

func (s *claudeHeartbeatSandbox) Exec(_ context.Context, _ string, stdout, _ io.Writer) error {
	if err := os.Chtimes(s.logPath, s.stale, s.stale); err != nil {
		return err
	}
	if info, err := os.Stat(s.logPath); err == nil {
		*s.before = info.ModTime()
	}
	_, _ = io.WriteString(stdout, `{"type":"tool_progress","tool_name":"Bash","elapsed_time_seconds":30,"heartbeat":true}`+"\n")
	if info, err := os.Stat(s.logPath); err == nil {
		*s.after = info.ModTime()
	}
	_, _ = io.WriteString(stdout, `{"type":"result","subtype":"success","is_error":false,"num_turns":1}`+"\n")
	return nil
}
