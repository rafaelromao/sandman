package batch

import (
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
)

type opencodeExecResult struct {
	stdout string
	stderr string
	err    error
}

type opencodeSequenceSandbox struct {
	fakeSandbox
	results  []opencodeExecResult
	commands []string
}

func (s *opencodeSequenceSandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	s.commands = append(s.commands, command)
	result := s.results[0]
	s.results = s.results[1:]
	if result.stdout != "" {
		_, _ = io.WriteString(stdout, result.stdout)
	}
	if result.stderr != "" {
		_, _ = io.WriteString(stderr, result.stderr)
	}
	return result.err
}

func TestPriorOpenCodeSession_CanonicalInvalidDoesNotUseLegacy(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, root)
	canonical := layout.RunSessionPath("batch-1", "run-1")
	legacy := layout.LegacyRunSessionPath("run-1")
	if err := writeOpenCodeSession(legacy, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(canonical), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte(`{"provider":"wrong"}`), 0644); err != nil {
		t.Fatal(err)
	}

	_, found, err := priorOpenCodeSession(layout, "batch-1", "run-1")
	if err == nil || found {
		t.Fatalf("invalid canonical metadata = found %t, err %v; want no identity and an error", found, err)
	}
}

func TestAgentRun_ReusesMissingSessionWithOneTimeFallback(t *testing.T) {
	root := t.TempDir()
	runFolder := filepath.Join(root, "current")
	sb := &opencodeSequenceSandbox{
		fakeSandbox: fakeSandbox{workDir: root},
		results: []opencodeExecResult{
			{stdout: `{"type":"error","sessionID":"old","error":{"message":"Session not found"}}` + "\n", err: errors.New("exact session missing")},
			{stdout: `{"type":"text","sessionID":"new","part":{"text":"resumed"}}` + "\n"},
		},
	}
	run := NewAgentRunWithLayout(&github.Issue{Number: 42}, "42-fix", sb, paths.NewLayout(&config.Config{}, root))
	run.preset = "opencode"
	run.reuseSession = true
	run.previousBatchID = "prior-batch"
	run.previousRunID = "prior-run"
	run.batchID = "current-batch"
	run.runID = "current-run"
	run.runFolder = runFolder
	layout := paths.NewLayout(&config.Config{}, root)
	if err := writeOpenCodeSession(layout.RunSessionPath("prior-batch", "prior-run"), "old"); err != nil {
		t.Fatal(err)
	}

	result := run.Run(context.Background(), &spyRenderer{result: "prompt"}, config.BuiltInAgentPresets["opencode"].Command, prompt.RenderConfig{})
	if result.Status != "success" {
		t.Fatalf("status = %q, commands=%q, want success", result.Status, sb.commands)
	}
	if len(sb.commands) != 2 {
		t.Fatalf("commands = %d, want exact attempt plus one fallback", len(sb.commands))
	}
	if !strings.Contains(sb.commands[0], "--session 'old'") {
		t.Errorf("exact command = %q, want session selector", sb.commands[0])
	}
	if strings.Contains(sb.commands[1], "--session") || !strings.Contains(sb.commands[1], "--continue") {
		t.Errorf("fallback command = %q, want only --continue", sb.commands[1])
	}
	data, err := os.ReadFile(filepath.Join(runFolder, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"session_id": "new"`) {
		t.Errorf("current metadata = %s, want fallback identity", data)
	}
}

func TestAgentRun_DoesNotFallbackForUnrelatedOpenCodeFailure(t *testing.T) {
	root := t.TempDir()
	sb := &opencodeSequenceSandbox{
		fakeSandbox: fakeSandbox{workDir: root},
		results: []opencodeExecResult{{
			stderr: `{"type":"error","error":{"data":{"message":"Authentication failed"}}}` + "\n",
			err:    errors.New("agent failed"),
		}},
	}
	run := NewAgentRunWithLayout(&github.Issue{Number: 42}, "42-fix", sb, paths.NewLayout(&config.Config{}, root))
	run.preset = "opencode"
	run.reuseSession = true
	run.previousBatchID = "prior-batch"
	run.previousRunID = "prior-run"
	run.runFolder = filepath.Join(root, "current")
	if err := writeOpenCodeSession(filepath.Join(root, ".sandman", "batches", "prior-batch", "runs", "prior-run", "session.json"), "old"); err != nil {
		t.Fatal(err)
	}

	result := run.Run(context.Background(), &spyRenderer{result: "prompt"}, config.BuiltInAgentPresets["opencode"].Command, prompt.RenderConfig{})
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	if len(sb.commands) != 1 {
		t.Fatalf("commands = %d, want no fallback", len(sb.commands))
	}
}

func TestAgentRun_PreservesReadableOutputWhenOpenCodeFails(t *testing.T) {
	root := t.TempDir()
	runFolder := filepath.Join(root, "current")
	sb := &opencodeSequenceSandbox{
		fakeSandbox: fakeSandbox{workDir: root},
		results: []opencodeExecResult{{
			stdout: `{"type":"text","sessionID":"failed-session","part":{"text":"before failure"}}` + "\n",
			err:    errors.New("agent failed"),
		}},
	}
	run := NewAgentRunWithLayout(&github.Issue{Number: 42}, "42-fix", sb, paths.NewLayout(&config.Config{}, root))
	run.preset = "opencode"
	run.runID = "current-run"
	run.batchID = "current-batch"
	run.runFolder = runFolder

	if result := run.Run(context.Background(), &spyRenderer{result: "prompt"}, config.BuiltInAgentPresets["opencode"].Command, prompt.RenderConfig{}); result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	data, err := os.ReadFile(filepath.Join(runFolder, "run.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "before failure") {
		t.Fatalf("run log = %q, want output emitted before failure", data)
	}
}

func TestOpenCodeOutput_OnlyExactErrorEventTriggersSessionFallback(t *testing.T) {
	var out strings.Builder
	parsed := newOpenCodeOutput(&out, io.Discard, false)
	_, _ = parsed.Write([]byte(`{"type":"text","part":{"text":"Session not found while explaining"}}` + "\n"))
	if parsed.SessionNotFound() {
		t.Fatal("ordinary text must not trigger session fallback")
	}
	_, _ = parsed.Write([]byte(`{"type":"error","error":{"data":{"message":"Session   not found"}}}` + "\n"))
	if !parsed.SessionNotFound() {
		t.Fatal("normalized exact error event should trigger session fallback")
	}
}

func TestOpenCodeOutput_PreservesFirstSessionAcrossStreams(t *testing.T) {
	var out, warning strings.Builder
	capture := &opencodeSessionCapture{}
	stdout := newSharedOpenCodeOutput(&out, &warning, false, capture)
	stderr := newSharedOpenCodeOutput(&out, &warning, true, capture)
	_, _ = stdout.Write([]byte(`{"type":"text","sessionID":"first","part":{"text":"hello"}}` + "\n"))
	_, _ = stderr.Write([]byte(`{"type":"text","sessionID":"second","part":{"text":"warning"}}` + "\n"))
	if got := stderr.SessionID(); got != "first" {
		t.Fatalf("session id = %q, want first observed id", got)
	}
	if !strings.Contains(warning.String(), "conflicting OpenCode session IDs") {
		t.Fatalf("expected conflicting-ID warning, got %q", warning.String())
	}
}

func TestOpenCodeOutput_PreservesMalformedLineAndWarns(t *testing.T) {
	var out, warning strings.Builder
	parsed := newOpenCodeOutput(&out, &warning, false)
	_, _ = parsed.Write([]byte("not-json\n"))
	if out.String() != "not-json\n" {
		t.Fatalf("output = %q, want original malformed line", out.String())
	}
	if !strings.Contains(warning.String(), "malformed OpenCode event") {
		t.Fatalf("warning = %q, want malformed-event diagnostic", warning.String())
	}
}
