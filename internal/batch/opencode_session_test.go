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
	"github.com/rafaelromao/sandman/internal/sandbox"
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
	run.env = map[string]string{"TEST_ENV": "test-value"}
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
	for i, command := range sb.commands {
		if !strings.HasPrefix(command, "export TEST_ENV=test-value; ") {
			t.Errorf("command %d = %q, want configured environment", i, command)
		}
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

func TestAgentRun_PersistsSessionObservedBeforeFallbackFailure(t *testing.T) {
	root := t.TempDir()
	runFolder := filepath.Join(root, "current")
	sb := &opencodeSequenceSandbox{
		fakeSandbox: fakeSandbox{workDir: root},
		results: []opencodeExecResult{
			{stdout: `{"type":"error","sessionID":"old","error":{"message":"Session not found"}}` + "\n", err: errors.New("exact session missing")},
			{stdout: `{"type":"text","sessionID":"new","part":{"text":"before failure"}}` + "\n", err: errors.New("fallback failed")},
		},
	}
	run := NewAgentRunWithLayout(&github.Issue{Number: 42}, "42-fix", sb, paths.NewLayout(&config.Config{}, root))
	run.preset = "opencode"
	run.reuseSession = true
	run.previousBatchID = "prior-batch"
	run.previousRunID = "prior-run"
	run.runFolder = runFolder
	if err := writeOpenCodeSession(filepath.Join(root, ".sandman", "batches", "prior-batch", "runs", "prior-run", "session.json"), "old"); err != nil {
		t.Fatal(err)
	}

	result := run.Run(context.Background(), &spyRenderer{result: "prompt"}, config.BuiltInAgentPresets["opencode"].Command, prompt.RenderConfig{})
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	data, err := os.ReadFile(filepath.Join(runFolder, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"session_id": "new"`) {
		t.Errorf("current metadata = %s, want identity observed before fallback failure", data)
	}
}

func TestAgentRun_PersistsSessionObservedBeforeMissingSessionFailure(t *testing.T) {
	root := t.TempDir()
	runFolder := filepath.Join(root, "current")
	sb := &opencodeSequenceSandbox{
		fakeSandbox: fakeSandbox{workDir: root},
		results: []opencodeExecResult{{
			stdout: `{"type":"text","sessionID":"new","part":{"text":"before failure"}}` + "\n",
			err:    errors.New("fallback failed"),
		}},
	}
	run := NewAgentRunWithLayout(&github.Issue{Number: 42}, "42-fix", sb, paths.NewLayout(&config.Config{}, root))
	run.preset = "opencode"
	run.reuseSession = true
	run.previousBatchID = "prior-batch"
	run.previousRunID = "prior-run"
	run.runFolder = runFolder

	result := run.Run(context.Background(), &spyRenderer{result: "prompt"}, config.BuiltInAgentPresets["opencode"].Command, prompt.RenderConfig{})
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	data, err := os.ReadFile(filepath.Join(runFolder, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"session_id": "new"`) {
		t.Errorf("current metadata = %s, want identity observed before fallback failure", data)
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

func TestRunExecutor_ContinuedRowSelectsOpenCodeSession(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	sb := &fakeSandbox{
		workDir:    worktree,
		execStdout: `{"type":"text","sessionID":"new-session","part":{"text":"continued"}}` + "\n",
	}
	factory := &capturingAgentRunFactory{agentRunCh: make(chan *AgentRun, 1)}
	client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", State: "closed"}}}
	o := NewOrchestrator(client, &noopRenderer{}, nil, nil,
		WithSandboxFactory(&fakeSandboxFactory{sandbox: sb}),
		WithRunnableFactory(factory),
		WithErrorLog(io.Discard),
	)
	layout := paths.NewLayout(&config.Config{}, root)
	if err := writeOpenCodeSession(layout.RunSessionPath("prior-batch", "prior-run"), "prior-session"); err != nil {
		t.Fatal(err)
	}
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: worktree, Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Preset: "opencode", Command: config.BuiltInAgentPresets["opencode"].Command},
		IdentityResolver: noopIdentityResolver(),
		Retries:          0,
	}
	row := RowSpec{
		IssueNumber:         42,
		Mode:                ModeContinue,
		Branches:            map[int]string{42: "42-fix"},
		PreviousRunIDs:      map[int]string{42: "prior-run"},
		PreviousRunBatchIDs: map[int]string{42: "prior-batch"},
		ReuseSession:        true,
		BaseBranch:          "main",
		BatchID:             "current-batch",
		RunTS:               "260827120000",
		RunShortID:          "abcd",
	}
	result, started := o.newRunExecutor(context.Background(), bc, &fakeSandboxFactory{sandbox: sb}, nil).Execute(context.Background(), row)
	if !started {
		t.Fatalf("executor result = %+v, started=%v; want started row", result, started)
	}
	run := <-factory.agentRunCh
	if !run.reuseSession || run.previousRunID != "prior-run" || run.previousBatchID != "prior-batch" {
		t.Fatalf("AgentRun reuse state = reuse=%v run=%q batch=%q", run.reuseSession, run.previousRunID, run.previousBatchID)
	}
	if !strings.Contains(sb.execCommand, "--session 'prior-session'") {
		t.Fatalf("executed command = %q, want exact prior session", sb.execCommand)
	}
}

func TestRunExecutor_LifecycleRelaunchReusesCurrentSession(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	sb := &opencodeSequenceSandbox{
		fakeSandbox: fakeSandbox{workDir: worktree},
		results: []opencodeExecResult{
			{stdout: `{"type":"text","sessionID":"lifecycle-session","part":{"text":"first"}}` + "\n"},
			{stdout: `{"type":"text","sessionID":"lifecycle-session","part":{"text":"second"}}` + "\n"},
		},
	}
	factory := &capturingAgentRunFactory{agentRunCh: make(chan *AgentRun, 2)}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug", State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            7,
			State:             "open",
			HeadRefOid:        "current-sha",
			HeadRefName:       gateTestBranch,
			MergeStateStatus:  "CLEAN",
			StatusCheckRollup: "success",
		}},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	sandboxFactory := sandboxFactoryFunc(func(string, string, string, string, sandbox.Container) sandbox.Sandbox { return sb })
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, &fakeConfigStore{config: &config.Config{
		Agent:          "opencode",
		DefaultAgent:   "opencode",
		WorktreeDir:    worktree,
		Sandbox:        "worktree",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: config.BuiltInAgentPresets["opencode"].Command}},
	}}, nil,
		WithSandboxFactory(sandboxFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runOpts),
		WithErrorLog(io.Discard),
	)
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: worktree, Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Preset: "opencode", Command: config.BuiltInAgentPresets["opencode"].Command},
		IdentityResolver: noopIdentityResolver(),
		Retries:          0,
	}
	row := RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: gateTestBranch},
		BaseBranch:  "main",
		BatchID:     "lifecycle-batch",
		RunTS:       "260827120001",
		RunShortID:  "abcd",
	}
	result, started := o.newRunExecutor(context.Background(), bc, sandboxFactory, nil).Execute(context.Background(), row)
	if !started {
		t.Fatalf("lifecycle executor result = %+v, started=%v", result, started)
	}
	first := <-factory.agentRunCh
	second := <-factory.agentRunCh
	if first.reuseSession {
		t.Fatal("initial lifecycle launch unexpectedly reused a session")
	}
	if !second.reuseSession {
		t.Fatal("lifecycle relaunch did not opt into session reuse")
	}
	if !strings.Contains(sb.commands[1], "--session 'lifecycle-session'") {
		t.Fatalf("lifecycle relaunch command = %q, want current session", sb.commands[1])
	}
}

func TestOpenCodeOutput_OnlyExactErrorEventTriggersSessionFallback(t *testing.T) {
	var out strings.Builder
	parsed := newOpenCodeOutput(&out, io.Discard, true)
	_, _ = parsed.Write([]byte(" Session not found \n"))
	if parsed.SessionNotFound() {
		t.Fatal("surrounding stderr whitespace must not trigger session fallback")
	}
	_, _ = parsed.Write([]byte(`{"type":"text","part":{"text":"Session not found while explaining"}}` + "\n"))
	if parsed.SessionNotFound() {
		t.Fatal("ordinary text must not trigger session fallback")
	}
	_, _ = parsed.Write([]byte(`{"type":"error","error":{"data":{"message":"Session   not found"}}}` + "\n"))
	if !parsed.SessionNotFound() {
		t.Fatal("normalized exact error event should trigger session fallback")
	}
	if !strings.Contains(out.String(), "Session not found\n") {
		t.Fatalf("normalized error output = %q, want readable normalized message", out.String())
	}
}

func TestOpenCodeOutput_PreservesContextErrorMarker(t *testing.T) {
	var out strings.Builder
	parsed := newOpenCodeOutput(&out, io.Discard, true)
	_, _ = parsed.Write([]byte(`{"type":"error","error":{"message":"prompt is too long"}}` + "\n"))
	if got := out.String(); got != "Error: prompt is too long\n" {
		t.Fatalf("structured error output = %q, want detector-compatible marker", got)
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

func TestOpenCodeOutput_ParsesMultipleEventsInOneWrite(t *testing.T) {
	var out strings.Builder
	parsed := newOpenCodeOutput(&out, io.Discard, false)
	input := []byte(`{"type":"text","sessionID":"first","part":{"text":"hello"}}` + "\n" +
		`{"type":"text","sessionID":"first","part":{"text":"world"}}` + "\n")
	if _, err := parsed.Write(input); err != nil {
		t.Fatalf("write returned error: %v", err)
	}
	if got := parsed.SessionID(); got != "first" {
		t.Fatalf("session id = %q, want first", got)
	}
	if out.String() != "hello\nworld\n" {
		t.Fatalf("output = %q, want both event texts", out.String())
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

func TestOpenCodeOutput_SuppressesStructuralEvents(t *testing.T) {
	var out strings.Builder
	parsed := newOpenCodeOutput(&out, io.Discard, false)
	input := []byte(`{"type":"step_start","sessionID":"ses_42","part":{"type":"step-start"}}` + "\n" +
		`{"type":"tool","part":{"tool":"read"}}` + "\n" +
		`{"type":"step_finish","part":{"reason":"tool-calls","type":"step-finish"}}` + "\n" +
		`{"type":"text","part":{"text":"Finished reviewing."}}` + "\n")
	if _, err := parsed.Write(input); err != nil {
		t.Fatalf("write returned error: %v", err)
	}
	if got := out.String(); got != "→ Read\nFinished reviewing.\n" {
		t.Fatalf("output = %q, want only human-readable events", got)
	}
	if got := parsed.SessionID(); got != "ses_42" {
		t.Fatalf("session id = %q, want session from suppressed event", got)
	}
}

func TestOpenCodeOutput_PreservesUnknownJSONEvent(t *testing.T) {
	var out strings.Builder
	parsed := newOpenCodeOutput(&out, io.Discard, false)
	input := `{"type":"future_event","detail":"keep me"}` + "\n"
	if _, err := parsed.Write([]byte(input)); err != nil {
		t.Fatalf("write returned error: %v", err)
	}
	if got := out.String(); got != input {
		t.Fatalf("output = %q, want unknown event unchanged", got)
	}
}

func TestOpenCodeOutput_RendersToolDetails(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "read with filePath",
			input: `{"type":"tool_use","part":{"tool":"read","state":{"status":"completed","input":{"filePath":"/tmp/foo.txt"}}}}` + "\n",
			want:  "→ Read /tmp/foo.txt\n",
		},
		{
			name:  "read with offset and limit",
			input: `{"type":"tool_use","part":{"tool":"read","state":{"status":"completed","input":{"filePath":"/tmp/foo.txt","offset":20,"limit":10}}}}` + "\n",
			want:  "→ Read /tmp/foo.txt (offset 20, limit 10)\n",
		},
		{
			name:  "grep with pattern and path",
			input: `{"type":"tool_use","part":{"tool":"grep","state":{"status":"completed","input":{"pattern":"hello.*world","path":"/tmp/demo"}}}}` + "\n",
			want:  "✱ Grep \"hello.*world\" in /tmp/demo\n",
		},
		{
			name:  "glob with pattern",
			input: `{"type":"tool_use","part":{"tool":"glob","state":{"status":"completed","input":{"pattern":"**/*.go","path":"/tmp"}}}}` + "\n",
			want:  "✱ Glob \"**/*.go\" in /tmp\n",
		},
		{
			name:  "bash with command",
			input: `{"type":"tool_use","part":{"tool":"bash","state":{"status":"completed","input":{"command":"ls -la /tmp/foo"}}}}` + "\n",
			want:  "$ ls -la /tmp/foo\n",
		},
		{
			name:  "skill with name",
			input: `{"type":"tool_use","part":{"tool":"skill","state":{"status":"completed","input":{"name":"recall"}}}}` + "\n",
			want:  "→ Skill \"recall\"\n",
		},
		{
			name:  "edit with filePath",
			input: `{"type":"tool_use","part":{"tool":"edit","state":{"status":"completed","input":{"filePath":"/tmp/foo.txt"}}}}` + "\n",
			want:  "→ Edit /tmp/foo.txt\n",
		},
		{
			name:  "apply patch with target",
			input: `{"type":"tool_use","part":{"tool":"apply_patch","state":{"status":"completed","input":{"patchText":"*** Begin Patch\n*** Update File: /tmp/foo.txt\n@@\n-old\n+new\n*** End Patch"}}}}` + "\n",
			want:  "→ Edit /tmp/foo.txt\n",
		},
		{
			name:  "todowrite count",
			input: `{"type":"tool_use","part":{"tool":"todowrite","state":{"status":"completed","input":{"todos":[{"content":"a","status":"pending"}]}}}}` + "\n",
			want:  "→ Task 1 todos (pending: 1)\n",
		},
		{
			name:  "bash error status",
			input: `{"type":"tool_use","part":{"tool":"read","state":{"status":"error","input":{"filePath":"/etc/hosts"},"error":"The user rejected permission"}}}` + "\n",
			want:  "→ Read /etc/hosts (error: The user rejected permission)\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			parsed := newOpenCodeOutput(&out, io.Discard, false)
			if _, err := parsed.Write([]byte(tc.input)); err != nil {
				t.Fatalf("write error: %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}
