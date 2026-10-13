package batch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// stoppableSandbox streams records one at a time and, like a killed agent
// process, stops streaming and fails once the launch context is cancelled.
type stoppableSandbox struct {
	fakeSandbox
	records    []string
	written    int
	cleanupErr error
}

func (s *stoppableSandbox) Exec(ctx context.Context, _ string, stdout, _ io.Writer) error {
	for _, record := range s.records {
		if ctx.Err() != nil {
			if s.cleanupErr != nil {
				return &sandbox.CleanupError{Err: ctx.Err(), CleanupFail: s.cleanupErr}
			}
			return ctx.Err()
		}
		_, _ = io.WriteString(stdout, record+"\n")
		s.written++
	}
	return ctx.Err()
}

func runStoppableClaudeLaunch(t *testing.T, ctx context.Context, sb *stoppableSandbox, command string) (AgentRunResult, string) {
	t.Helper()
	root := t.TempDir()
	agent := config.BuiltInAgentPresets["claude"].Agent("claude")
	sb.workDir = filepath.Join(root, "worktree")
	run := NewAgentRunWithLayout(&github.Issue{Number: 42, Title: "Claude"}, "42-claude", sb, paths.NewLayout(&config.Config{}, root))
	run.preset = agent.Preset
	run.runID = "run-1"
	run.runFolder = filepath.Join(root, "run")
	run.outputWriter = &bytes.Buffer{}
	if command == "" {
		command = agent.Command
	}
	result := run.Run(ctx, &spyRenderer{result: "task"}, command, prompt.RenderConfig{})
	data, _ := os.ReadFile(filepath.Join(root, "run", "run.log"))
	return result, string(data)
}

func claudeSessionRecords(mode string) []string {
	return []string{
		`{"type":"system","subtype":"init","session_id":"abc","permissionMode":"` + mode + `"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","num_turns":7}`,
	}
}

func TestAgentRun_ClaudePlanModeSessionStopsAtOnce(t *testing.T) {
	sb := &stoppableSandbox{records: claudeSessionRecords("plan")}
	result, runLog := runStoppableClaudeLaunch(t, context.Background(), sb, "")
	if sb.written != 1 {
		t.Fatalf("records streamed = %d, want the launch stopped right after the init record", sb.written)
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	if !errors.Is(result.OperationalError, ErrUnattendedPermissionMode) {
		t.Fatalf("OperationalError = %v, want ErrUnattendedPermissionMode", result.OperationalError)
	}
	diagnostic := result.OperationalError.Error()
	for _, want := range []string{"permission mode plan", "permission_mode", "defaultMode", "managed settings", "bypass mode"} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("diagnostic %q does not mention %q", diagnostic, want)
		}
	}
	if len(diagnostic) > 200 {
		t.Errorf("diagnostic is %d bytes; keep it to one short line for the daemon console", len(diagnostic))
	}
	if !strings.Contains(runLog, diagnostic) {
		t.Fatalf("run.log lacks the diagnostic:\n%s", runLog)
	}
	if strings.Contains(runLog, "Result: success") {
		t.Fatalf("run.log shows records after the stop:\n%s", runLog)
	}
}

func TestAgentRun_ClaudeWritableModesRunNormally(t *testing.T) {
	for _, mode := range []string{"bypassPermissions", "acceptEdits", "dontAsk", "auto", "manual", ""} {
		t.Run(mode, func(t *testing.T) {
			sb := &stoppableSandbox{records: claudeSessionRecords(mode)}
			result, _ := runStoppableClaudeLaunch(t, context.Background(), sb, "")
			if result.Status != "success" || result.OperationalError != nil || sb.written != 3 {
				t.Fatalf("status=%q err=%v written=%d, want a normal session", result.Status, result.OperationalError, sb.written)
			}
		})
	}
}

func TestAgentRun_ClaudeCustomCommandIsNotInspected(t *testing.T) {
	sb := &stoppableSandbox{records: claudeSessionRecords("plan")}
	result, _ := runStoppableClaudeLaunch(t, context.Background(), sb, `claude -p "$(cat {{.PromptFile}})"`)
	if result.Status != "success" || result.OperationalError != nil || sb.written != 3 {
		t.Fatalf("status=%q err=%v written=%d, want a custom command left alone", result.Status, result.OperationalError, sb.written)
	}
}

func TestAgentRun_ClaudePlanModeStopKeepsCleanupFailure(t *testing.T) {
	cleanupFail := errors.New("container kill failed")
	sb := &stoppableSandbox{records: claudeSessionRecords("plan"), cleanupErr: cleanupFail}
	result, _ := runStoppableClaudeLaunch(t, context.Background(), sb, "")
	if !errors.Is(result.OperationalError, ErrUnattendedPermissionMode) || !errors.Is(result.CleanupError, cleanupFail) {
		t.Fatalf("OperationalError=%v CleanupError=%v, want both kept", result.OperationalError, result.CleanupError)
	}
}

func TestAgentRun_OperatorAbortDuringPlanModeStaysAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sb := &stoppableSandbox{records: claudeSessionRecords("plan")}
	result, _ := runStoppableClaudeLaunch(t, ctx, sb, "")
	if errors.Is(result.OperationalError, ErrUnattendedPermissionMode) {
		t.Fatalf("an operator abort became a permission-mode failure: %v", result.OperationalError)
	}
}
