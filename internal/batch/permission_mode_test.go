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
	"github.com/rafaelromao/sandman/internal/testenv"
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

func permissionModeStoppedResult() AgentRunResult {
	return AgentRunResult{Status: "failure", OperationalError: &PermissionModeError{Mode: "plan", Diagnostic: claudeReadOnlyModeDiagnostic("plan")}}
}

func newPermissionModeOrchestrator(t *testing.T, dir string, client *fakeGitHubClient, log *spyEventLog, launches *int) *Orchestrator {
	t.Helper()
	return NewOrchestrator(client, &noopRenderer{},
		&fakeConfigStore{config: &config.Config{
			Agent:       "test-agent",
			Sandbox:     "worktree",
			Retries:     2,
			WorktreeDir: filepath.Join(dir, "worktrees"),
			Git:         config.GitConfig{BaseBranch: "main"},
			AgentProviders: map[string]config.Agent{
				"test-agent": {Command: "true"},
			},
		}},
		log,
		WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(dir, "worktree")}}),
		WithRunnableFactory(&promptOnlyRunnableFactory{hook: func(*github.Issue, string) AgentRunResult {
			*launches++
			return permissionModeStoppedResult()
		}}),
	)
}

// A review row stopped for its permission mode is not relaunched, and the
// batch result carries the typed diagnostic once for the review daemon.
func TestRunBatch_ReviewStoppedForPermissionModeIsNotRelaunched(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	launches := 0
	o := newPermissionModeOrchestrator(t, dir, &fakeGitHubClient{}, &spyEventLog{}, &launches)

	result, err := o.RunBatch(context.Background(), Request{
		Agent:        "test-agent",
		Review:       true,
		PRNumber:     42,
		PromptConfig: prompt.RenderConfig{PromptFlag: "review prompt", Branch: "review-branch"},
		RunID:        "review-run",
		RunTS:        orchTestRunTS,
		RunShortID:   orchTestRunShortID,
	})
	if err == nil {
		t.Fatal("RunBatch reported success for a stopped review")
	}
	if launches != 1 {
		t.Fatalf("launches = %d, want 1", launches)
	}
	if result == nil || len(result.Runs) != 1 || !errors.Is(result.Runs[0].OperationalError, ErrUnattendedPermissionMode) {
		t.Fatalf("result = %#v, want the typed permission-mode error on the run", result)
	}
	if strings.Contains(err.Error(), "permission mode plan") {
		t.Fatalf("batch error %q repeats the run's diagnostic", err)
	}
}

// An implementation run stopped for its permission mode ends as a terminal
// failure with its reason, without using the ordinary retry budget.
func TestRunBatch_ImplementationStoppedForPermissionModeIsNotRetried(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	launches := 0
	log := &spyEventLog{}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."}}}
	o := newPermissionModeOrchestrator(t, dir, client, log, &launches)

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}, Retries: 2, RunTS: orchTestRunTS, RunShortID: orchTestRunShortID})
	if launches != 1 {
		t.Fatalf("launches = %d, want 1", launches)
	}
	for _, event := range log.snapshot() {
		if event.Type == "run.retry" || event.Type == "run.await" {
			t.Fatalf("unexpected %s event: %#v", event.Type, event.Payload)
		}
		if event.Type == "run.finished" {
			if event.Payload["status"] != "failure" {
				t.Fatalf("terminal status = %#v, want failure", event.Payload["status"])
			}
			if event.Payload["reason"] != "AGENT_PERMISSION_MODE" {
				t.Fatalf("terminal reason = %#v, want AGENT_PERMISSION_MODE", event.Payload["reason"])
			}
			if action, _ := event.Payload["next_action"].(string); !strings.Contains(action, "permission_mode") {
				t.Fatalf("next_action = %q, want the permission_mode remedy", action)
			}
			return
		}
	}
	t.Fatalf("no terminal failure event in %#v", log.snapshot())
}

// A verified merged completion still outranks a later permission-mode stop.
func TestRunBatch_MergedCompletionOutranksPermissionModeStop(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	launches := 0
	log := &spyEventLog{}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."}},
		prs:    map[string]*github.PR{"42-fix-bug": mergedPR("42-fix-bug", "")},
	}
	o := newPermissionModeOrchestrator(t, dir, client, log, &launches)

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}, Retries: 2, RunTS: orchTestRunTS, RunShortID: orchTestRunShortID})
	for _, event := range log.snapshot() {
		if event.Type == "run.finished" {
			if event.Payload["status"] != "success" || event.Payload["reason"] == "AGENT_PERMISSION_MODE" {
				t.Fatalf("terminal payload = %#v, want the merged success", event.Payload)
			}
			return
		}
	}
	t.Fatalf("no terminal event in %#v", log.snapshot())
}

// The stdout and stderr parsers share one session: a read-only mode seen on
// both streams stops the launch once and is reported once.
func TestClaudeOutput_ReadOnlyModeStopsOnceAcrossStreams(t *testing.T) {
	stdout, stderr := strategyForPreset("claude").NewOutputParsers(io.Discard)
	var log bytes.Buffer
	stops := 0
	for _, parser := range []outputParser{stdout, stderr} {
		parser.setDestination(&log)
		parser.(permissionModeGuard).setPermissionModeStop(func() { stops++ })
	}
	init := `{"type":"system","subtype":"init","session_id":"abc","permissionMode":"plan"}` + "\n"
	for _, parser := range []outputParser{stdout, stderr, stdout} {
		if _, err := parser.Write([]byte(init)); err != nil {
			t.Fatal(err)
		}
	}
	if stops != 1 {
		t.Fatalf("stop callbacks = %d, want 1", stops)
	}
	if got := strings.Count(log.String(), "Stopped: permission mode plan"); got != 1 {
		t.Fatalf("diagnostic lines = %d, want 1:\n%s", got, log.String())
	}
	if stopped := parsersReadOnlyPermissionMode(stdout, stderr); stopped == nil || stopped.Mode != "plan" {
		t.Fatalf("reported mode = %#v, want plan", stopped)
	}
}
