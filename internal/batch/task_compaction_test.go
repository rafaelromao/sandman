package batch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

const testTaskCompactBytes = 48 * 1024

// testTaskNonce is the marker nonce of the test Tasks.
const testTaskNonce = "test-nonce"

// checkpointTask is a fresh marked render of the built-in template, for an
// issue with an empty body, followed by count agent checkpoint sections of
// about 2 KiB each, the shape a long continuation grows into.
func checkpointTask(count int) string {
	rendered, err := (&prompt.Engine{}).RenderMarked(prompt.RenderConfig{}, prompt.IssueData{Number: 7, Title: "Compaction"}, testTaskNonce)
	if err != nil {
		panic(err)
	}
	return withCheckpoints(rendered, count)
}

// unmarkedTask is checkpointTask as a Task rendered before markers.
func unmarkedTask(count int) string {
	return withCheckpoints(strings.Replace(prompt.DefaultPrompt(), "{{ISSUE_BODY}}", "", 1), count)
}

func withCheckpoints(rendered string, count int) string {
	var b strings.Builder
	b.WriteString(rendered)
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&b, "\n## Checkpoint %d\n\n%s\n", i, strings.Repeat(fmt.Sprintf("checkpoint %d detail. ", i), 100))
	}
	return b.String()
}

// recordTestNonce records checkpointTask's markers in a worktree, as a fresh
// launch does.
func recordTestNonce(t *testing.T, workDir string) {
	t.Helper()
	path := filepath.Join(workDir, ".sandman", taskAnchorsFile)
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := atomicfs.WriteAtomic(path, []byte(testTaskNonce+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// compactionLaunch runs one implementation launch of task through a worktree
// sandbox and a fake agent CLI that records its standard input.
type compactionLaunch struct {
	workDir, runFolder, stdin string
	writes                    []string
	result                    AgentRunResult
}

func launchWithCompaction(t *testing.T, task string, threshold int) compactionLaunch {
	t.Helper()
	return launchInWorktree(t, t.TempDir(), prompt.RenderConfig{TaskPrompt: task}, threshold, nil)
}

// launchInWorktree launches in root's "compaction" worktree, creating the
// repository on first use, and renders the Task with renderer when the
// configuration carries no Task.
func launchInWorktree(t *testing.T, root string, renderCfg prompt.RenderConfig, threshold int, renderer prompt.IssueRenderer, customize ...func(*AgentRun)) compactionLaunch {
	t.Helper()
	if renderer == nil {
		renderer = &spyRenderer{}
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		initGitRepo(t, root)
	}
	out, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(fakePromptCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SANDMAN_TEST_OUT", out)
	sb := sandbox.NewWorktreeSandbox(root, filepath.Join(root, ".sandman", "worktrees"), "compaction", "main")
	_, statErr := os.Stat(sb.WorkDir())
	if err := sb.Start(sandbox.SandboxStart{Continue: statErr == nil}); err != nil {
		t.Fatal(err)
	}
	recordTestNonce(t, sb.WorkDir())
	launch := compactionLaunch{workDir: sb.WorkDir(), runFolder: filepath.Join(root, "run")}
	run := NewAgentRunWithLayout(&github.Issue{Number: 7, Title: "Compaction"}, "compaction", sb, paths.NewLayout(&config.Config{}, root))
	run.preset, run.runID = "opencode-v2", "261127100000-abcd-7"
	run.runFolder, run.outputWriter, run.sessionWarning = launch.runFolder, &bytes.Buffer{}, &bytes.Buffer{}
	run.taskCompactBytes = threshold
	run.now = func() time.Time { return time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC) }
	run.taskWriter = func(path string, data []byte, mode os.FileMode) error {
		launch.writes = append(launch.writes, path)
		return atomicfs.WriteAtomic(path, data, mode)
	}
	run.archiveWriter = func(path string, data []byte, mode os.FileMode) error {
		launch.writes = append(launch.writes, path)
		return atomicfs.WriteAtomicNew(path, data, mode)
	}
	for _, apply := range customize {
		apply(run)
	}

	launch.result = run.Run(context.Background(), renderer, config.BuiltInAgentPresets["opencode-v2"].Command, renderCfg)

	if launch.result.Status != "success" {
		t.Fatalf("launch failed: %+v\n%s", launch.result, run.outputWriter)
	}
	stdin, err := os.ReadFile(filepath.Join(out, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	launch.stdin = string(stdin)
	return launch
}

func TestImplementationLaunch_CompactsAnOversizedTask(t *testing.T) {
	task := checkpointTask(30)
	if len(task) <= testTaskCompactBytes {
		t.Fatalf("test Task is only %d bytes", len(task))
	}

	launch := launchWithCompaction(t, task, testTaskCompactBytes)

	if target := testTaskCompactBytes * 3 / 4; len(launch.stdin) > target {
		t.Fatalf("launched Task is %d bytes, want at most %d", len(launch.stdin), target)
	}
	written, err := os.ReadFile(filepath.Join(launch.workDir, ".sandman", "task.md"))
	if err != nil || string(written) != launch.stdin {
		t.Fatalf("worktree Task differs from the launched one (%v)", err)
	}
	for _, kept := range []string{"## Execution Checklist", "## Runtime Context", "## Checkpoint 30\n"} {
		if !strings.Contains(launch.stdin, kept) {
			t.Errorf("launched Task lost %q", kept)
		}
	}
	if strings.Contains(launch.stdin, "## Checkpoint 1\n") {
		t.Error("the oldest checkpoint was kept")
	}
	archives, _ := filepath.Glob(filepath.Join(launch.workDir, ".sandman", "task-history", "*.md"))
	if len(archives) != 1 || filepath.Base(archives[0]) != "261127100000-abcd-7-20261127T100000Z.md" {
		t.Fatalf("archives = %v", archives)
	}
	archived, err := os.ReadFile(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 30; i++ {
		if !strings.Contains(string(archived), fmt.Sprintf("## Checkpoint %d\n", i)) {
			t.Fatalf("archive lost checkpoint %d", i)
		}
	}
	if !strings.Contains(launch.stdin, ".sandman/task-history/"+filepath.Base(archives[0])) {
		t.Error("the launched Task does not name its archive")
	}
	if len(launch.writes) < 2 || launch.writes[0] != archives[0] || filepath.Base(launch.writes[len(launch.writes)-1]) != "task.md" {
		t.Fatalf("writes = %v, want the archive before the Task", launch.writes)
	}
	log, err := os.ReadFile(filepath.Join(launch.runFolder, "run.log"))
	if err != nil || !strings.Contains(string(log), fmt.Sprintf("task compacted: %d -> %d bytes", len(archived), len(launch.stdin))) {
		t.Fatalf("run.log lacks the compaction line: %s (%v)", log, err)
	}
	if summary, output := strings.Index(string(log), "task compacted:"), strings.Index(string(log), `"is_error"`); output < 0 || summary >= output {
		t.Fatalf("compaction summary must precede agent output:\n%s", log)
	}
}

// A Task rendered before Sandman marked Tasks is compacted only on request,
// from the built-in layout with the current issue body; automatic compaction
// leaves it alone and says how to ask.
func TestImplementationLaunch_CompactsAnUnmarkedBuiltInTaskOnlyOnRequest(t *testing.T) {
	issue := "## Goal\n\nKeep the goal.\n\n## Acceptance criteria\n\nKeep every criterion."
	task := strings.Replace(unmarkedTask(30), "## Issue Context\n\n", "## Issue Context\n\n"+issue, 1)
	task = strings.Replace(task, "## Next Step\n", "## Next Step\n\nCurrent checkpoint.\n"+strings.Repeat("Obsolete step.\n", 1000), 1)
	for name, force := range map[string]bool{"automatic": false, "on request": true} {
		t.Run(name, func(t *testing.T) {
			launch := launchInWorktree(t, t.TempDir(), prompt.RenderConfig{TaskPrompt: task}, testTaskCompactBytes, nil, func(run *AgentRun) {
				run.issue.Body = issue
				run.forceTaskCompaction = force
			})

			log, _ := os.ReadFile(filepath.Join(launch.runFolder, "run.log"))
			if !force {
				if !strings.Contains(launch.stdin, "## Checkpoint 1\n") || !strings.Contains(string(log), "--continue --compact") {
					t.Fatalf("an unmarked Task was compacted automatically, or the log does not say how to:\n%s", log)
				}
				return
			}
			if len(launch.stdin) >= testTaskCompactBytes || !strings.Contains(launch.stdin, issue) ||
				!strings.Contains(launch.stdin, "Current checkpoint.") || strings.Count(launch.stdin, "Obsolete step.") >= 300 {
				t.Fatalf("the requested compaction did not keep the issue and cap the step: %d bytes\n%s", len(launch.stdin), log)
			}
		})
	}
}

func TestImplementationLaunch_LeavesATaskUnderTheThresholdAlone(t *testing.T) {
	task := checkpointTask(5)

	launch := launchWithCompaction(t, task, testTaskCompactBytes)

	if strings.Contains(launch.stdin, "## Sandman Task History") || !strings.Contains(launch.stdin, "## Checkpoint 1\n") {
		t.Fatal("a Task under the threshold was compacted")
	}
	if _, err := os.Stat(filepath.Join(launch.workDir, ".sandman", "task-history")); !os.IsNotExist(err) {
		t.Fatalf("archive directory exists: %v", err)
	}
}

// A fresh render marks its structure and records the markers' nonce, so a
// later launch compacts what the agent added and nothing else.
func TestFreshRenderMarksTheTaskForLaterCompaction(t *testing.T) {
	root := t.TempDir()
	fresh := launchInWorktree(t, root, prompt.RenderConfig{}, testTaskCompactBytes, &prompt.Engine{})
	nonce, err := os.ReadFile(filepath.Join(fresh.workDir, ".sandman", "task-anchors"))
	if err != nil {
		t.Fatalf("fresh render recorded no nonce: %v", err)
	}
	bounds, ok := prompt.MarkedTaskBounds(fresh.stdin, strings.TrimSpace(string(nonce)))
	if !ok || bounds.NextStepEnd <= bounds.NextStepStart {
		t.Fatalf("fresh Task has no located structure: %+v %t", bounds, ok)
	}
	if strings.Contains(fresh.stdin, "## Sandman Task History") {
		t.Fatal("a fresh render was compacted")
	}

	var task strings.Builder
	task.WriteString(fresh.stdin)
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&task, "\n## Checkpoint %d\n\n%s\n", i, strings.Repeat("detail. ", 250))
	}
	continued := prompt.ContinuationTaskPromptWithReviewSettings(task.String(), "/sandman review", true, 1800)

	launch := launchInWorktree(t, root, prompt.RenderConfig{TaskPrompt: continued}, testTaskCompactBytes, nil)

	if strings.Contains(launch.stdin, "## Checkpoint 1\n") || !strings.Contains(launch.stdin, "## Checkpoint 30\n") {
		t.Fatal("checkpoints were not treated as history")
	}
	if len(launch.stdin) > testTaskCompactBytes*3/4 {
		t.Fatalf("launched Task is %d bytes", len(launch.stdin))
	}
}

func TestImplementationLaunch_ArchiveFailureLaunchesTheTaskUnchanged(t *testing.T) {
	task := checkpointTask(30)

	launch := launchInWorktree(t, t.TempDir(), prompt.RenderConfig{TaskPrompt: task}, testTaskCompactBytes, nil, func(run *AgentRun) {
		run.archiveWriter = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
	})

	if !strings.Contains(launch.stdin, "## Checkpoint 1\n") || strings.Contains(launch.stdin, "## Sandman Task History") {
		t.Fatal("the Task was compacted without an archive")
	}
	log, _ := os.ReadFile(filepath.Join(launch.runFolder, "run.log"))
	if !strings.Contains(string(log), "task not compacted:") || !strings.Contains(string(log), "disk full") {
		t.Fatalf("run.log does not explain the failure:\n%s", log)
	}
}

func TestOnlyImplementationLaunchesAreCompacted(t *testing.T) {
	task := checkpointTask(30)
	for name, customize := range map[string]func(*AgentRun){
		"review":                      func(run *AgentRun) { run.review = true },
		"prompt-only (probe, repair)": func(run *AgentRun) { run.issue = nil },
	} {
		t.Run(name, func(t *testing.T) {
			launch := launchInWorktree(t, t.TempDir(), prompt.RenderConfig{TaskPrompt: task}, testTaskCompactBytes, nil, customize)

			if strings.Contains(launch.stdin, "## Sandman Task History") || !strings.Contains(launch.stdin, "## Checkpoint 1\n") {
				t.Fatal("compacted a launch that serves no implementation")
			}
		})
	}
}

func TestImplementationLaunch_LogsATaskItCannotShrink(t *testing.T) {
	task := checkpointTask(0) + "\n## Plan\n\n" + strings.Repeat("a long plan line\n", 4000)

	launch := launchWithCompaction(t, task, testTaskCompactBytes)

	log, _ := os.ReadFile(filepath.Join(launch.runFolder, "run.log"))
	if strings.Count(string(log), "task not compacted:") != 1 || !strings.Contains(string(log), "protected content exceeds the target") {
		t.Fatalf("run.log =\n%s", log)
	}
	if !strings.Contains(launch.stdin, "a long plan line") {
		t.Fatal("the plan was changed")
	}
}

type capturingRunnableFactory struct {
	sandbox sandbox.Sandbox
	runs    []*AgentRun
}

func (f *capturingRunnableFactory) NewRunnable(issue *github.Issue, branch string, _ sandbox.Sandbox) Runnable {
	run := NewAgentRun(issue, branch, f.sandbox)
	f.runs = append(f.runs, run)
	return run
}

// Every implementation launch of a Run receives the configured threshold.
func TestRunSession_PassesTheConfiguredTaskCompactThreshold(t *testing.T) {
	forty := 40000
	zero := 0
	for name, tc := range map[string]struct {
		configured *int
		want       int
	}{"default": {nil, config.DefaultTaskCompactBytes}, "configured": {&forty, 40000}, "disabled": {&zero, 0}} {
		t.Run(name, func(t *testing.T) {
			workDir := t.TempDir()
			t.Chdir(workDir)
			sb := &cleanupErrorSandbox{workDir: filepath.Join(workDir, "worktree"), cleanupFn: func() error { return nil }}
			factory := &capturingRunnableFactory{sandbox: sb}
			client := &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Threshold", State: "closed"}}}
			o := NewOrchestrator(client, &retryRenderer{result: "# Task\n\nInitial task."}, nil,
				&events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")},
				WithErrorLog(io.Discard), WithSandboxFactory(&cleanupErrorSandboxFactory{sandbox: sb}), WithRunnableFactory(factory))
			cfg := &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}, TaskCompactBytes: tc.configured}
			bc := BatchConfig{Cfg: cfg, AgentName: "opencode", AgentCfg: config.BuiltInAgentPresets["opencode"].Agent("opencode"),
				IdentityResolver: noopIdentityResolver()}
			row := RowSpec{IssueNumber: 42, Branches: map[int]string{42: "42-threshold"}, BaseBranch: "main", RunTS: "260814071754", RunShortID: "5d21"}

			o.newRunExecutor(context.Background(), bc, &cleanupErrorSandboxFactory{sandbox: sb}, nil).Execute(context.Background(), row)

			if len(factory.runs) == 0 {
				t.Fatal("nothing was launched")
			}
			for i, run := range factory.runs {
				if run.taskCompactBytes != tc.want {
					t.Errorf("launch %d: threshold = %d, want %d", i+1, run.taskCompactBytes, tc.want)
				}
			}
		})
	}
}

// Two compactions of one Run in the same second, as a launch and its prompt
// retry can be, each keep their own archive and pointer.
func TestCompactWorktreeTask_SameSecondCompactionsKeepEveryArchive(t *testing.T) {
	workDir := t.TempDir()
	recordTestNonce(t, workDir)
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	compact := func(content string) prompt.TaskCompactionResult {
		t.Helper()
		result, err := CompactWorktreeTask(content, TaskCompactionRequest{WorkDir: workDir, Label: "run-1", Threshold: testTaskCompactBytes, Now: now})
		if err != nil || !result.Compacted {
			t.Fatalf("compacted = %t, reason %q, err %v", result.Compacted, result.Reason, err)
		}
		return result
	}
	first := checkpointTask(30)
	once := compact(first)
	second := once.Content + checkpointTask(30)[len(checkpointTask(0)):]
	twice := compact(second)

	archives := map[string]string{
		".sandman/task-history/run-1-20261127T100000Z.md":   first,
		".sandman/task-history/run-1-20261127T100000Z-2.md": second,
	}
	for rel, want := range archives {
		got, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Fatalf("%s does not hold its original (%v)", rel, err)
		}
	}
	if once.ArchivePath == twice.ArchivePath || !strings.Contains(twice.Content, "`"+twice.ArchivePath+"`") || !strings.Contains(second, "`"+once.ArchivePath+"`") {
		t.Fatalf("pointers: first %q, second %q", once.ArchivePath, twice.ArchivePath)
	}
}

// A Task without markers whose layout cannot be verified is left alone, with
// the reason, whatever its size.
func TestCompactWorktreeTask_LeavesUnverifiedLayoutsAlone(t *testing.T) {
	issue := "## Acceptance criteria\n\n- [ ] keep me\n"
	history := unmarkedTask(30)[len(unmarkedTask(0)):]
	custom := "# Task\n\n## Next Step\n\nStart.\n\n## Issue Context\n\n{{ISSUE_BODY}}\n"
	for name, tc := range map[string]struct {
		task string
		cfg  prompt.RenderConfig
		body string
	}{
		"custom template ending with the issue body": {strings.Replace(custom, "{{ISSUE_BODY}}", issue, 1) + history, prompt.RenderConfig{PromptFlag: custom}, issue},
		"custom template with the built-in Task":     {unmarkedTask(30), prompt.RenderConfig{PromptFlag: custom}, ""},
		"built-in Task with an edited issue":         {unmarkedTask(30), prompt.RenderConfig{}, issue},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			result, err := CompactWorktreeTask(tc.task, TaskCompactionRequest{WorkDir: dir, Threshold: testTaskCompactBytes, Force: true,
				Template: tc.cfg, IssueBody: tc.body, Now: time.Now()})

			if err != nil || result.Compacted || result.Content != tc.task || result.Reason != "no recorded Task structure" {
				t.Fatalf("compacted = %t, reason %q, err %v", result.Compacted, result.Reason, err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".sandman", "task-history")); !os.IsNotExist(err) {
				t.Fatalf("an archive directory exists: %v", err)
			}
		})
	}
}

// `--continue --compact` forces the first launch to compact, even under the
// threshold and with automatic compaction disabled.
func TestImplementationLaunch_ForcedCompactionIgnoresTheThreshold(t *testing.T) {
	task := checkpointTask(8)
	if len(task) > config.DefaultTaskCompactBytes || len(task) <= config.DefaultTaskCompactBytes*3/4 {
		t.Fatalf("test Task is %d bytes", len(task))
	}
	for name, force := range map[string]bool{"forced": true, "not forced": false} {
		t.Run(name, func(t *testing.T) {
			launch := launchInWorktree(t, t.TempDir(), prompt.RenderConfig{TaskPrompt: task}, 0, nil, func(run *AgentRun) {
				run.forceTaskCompaction = force
			})

			if compacted := !strings.Contains(launch.stdin, "## Checkpoint 1\n"); compacted != force {
				t.Fatalf("compacted = %t, want %t", compacted, force)
			}
		})
	}
}

func TestImplementationLaunch_RefusalLeavesTaskUnchanged(t *testing.T) {
	for _, name := range []string{"invalid command", "cannot open log", "cannot spawn shell", "cannot write Task", "cannot spawn shell with nothing to compact", "cannot spawn shell after an archive failure"} {
		t.Run(name, func(t *testing.T) {
			testRefusedCompactionLaunch(t, name)
		})
	}
}

func testRefusedCompactionLaunch(t *testing.T, refusal string) {
	t.Helper()
	root := t.TempDir()
	initGitRepo(t, root)
	sb := sandbox.NewWorktreeSandbox(root, filepath.Join(root, "worktrees"), "compaction", "main")
	if err := sb.Start(sandbox.SandboxStart{}); err != nil {
		t.Fatal(err)
	}
	recordTestNonce(t, sb.WorkDir())
	task := checkpointTask(30)
	if refusal == "cannot spawn shell with nothing to compact" {
		task = checkpointTask(0)
	}
	if err := sb.WritePrompt(task); err != nil {
		t.Fatal(err)
	}
	run := NewAgentRun(&github.Issue{Number: 7}, "compaction", sb)
	run.forceTaskCompaction = true
	command := "fake-agent {{.Unknown}}"
	switch refusal {
	case "cannot open log":
		if err := os.Mkdir(filepath.Join(sb.WorkDir(), "run.log"), 0o755); err != nil {
			t.Fatal(err)
		}
		command = "true"
	case "cannot spawn shell", "cannot spawn shell with nothing to compact":
		command = "true"
		t.Setenv("PATH", t.TempDir())
	case "cannot spawn shell after an archive failure":
		command = "true"
		run.archiveWriter = func(string, []byte, os.FileMode) error { return errors.New("archive refused") }
		t.Setenv("PATH", t.TempDir())
	case "cannot write Task":
		command = "true"
		run.taskWriter = func(string, []byte, os.FileMode) error { return errors.New("write refused") }
	}
	result := run.Run(context.Background(), &prompt.Engine{}, command, prompt.RenderConfig{TaskPrompt: task})
	if result.Status != "failure" {
		t.Fatalf("status = %s, want failure", result.Status)
	}
	if run.launcherStarted {
		t.Fatal("a refused launch spent the compaction request")
	}
	got, err := os.ReadFile(filepath.Join(sb.WorkDir(), ".sandman", "task.md"))
	if err != nil || string(got) != task {
		t.Fatalf("the refused launch changed its Task: %v", err)
	}
	if archives, err := filepath.Glob(filepath.Join(sb.WorkDir(), ".sandman", "task-history", "*.md")); err != nil || len(archives) != 0 {
		t.Fatalf("the refused launch left archives: %v (%v)", archives, err)
	}
	if refusal == "cannot spawn shell" || refusal == "cannot write Task" {
		log, err := os.ReadFile(filepath.Join(sb.WorkDir(), "run.log"))
		if err != nil || !strings.Contains(string(log), "task not compacted:") || strings.Contains(string(log), "task compacted:") {
			t.Fatalf("incorrect compaction log: %s (%v)", log, err)
		}
	}
}

// An automatically compacted Task stays after a refused start, so the next
// attempt still reads what this launch composed, such as the recovery guard.
func TestImplementationLaunch_RefusalKeepsAnAutomaticCompaction(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	sb := sandbox.NewWorktreeSandbox(root, filepath.Join(root, "worktrees"), "compaction", "main")
	if err := sb.Start(sandbox.SandboxStart{}); err != nil {
		t.Fatal(err)
	}
	recordTestNonce(t, sb.WorkDir())
	task := prompt.ContextRecoveryTaskPromptWithReviewSettings(checkpointTask(30), "/sandman review", true, 1800)
	run := NewAgentRun(&github.Issue{Number: 7}, "compaction", sb)
	run.taskCompactBytes = testTaskCompactBytes
	t.Setenv("PATH", t.TempDir())

	result := run.Run(context.Background(), &prompt.Engine{}, "true", prompt.RenderConfig{TaskPrompt: task, ContextRecovery: true})

	got, err := os.ReadFile(filepath.Join(sb.WorkDir(), ".sandman", "task.md"))
	archives, _ := filepath.Glob(filepath.Join(sb.WorkDir(), ".sandman", "task-history", "*.md"))
	if result.Status != "failure" || err != nil || len(got) >= len(task) || !strings.Contains(string(got), "## Context Recovery Guard") || len(archives) != 1 {
		t.Fatalf("status = %s, Task %d of %d bytes, archives %v (%v)", result.Status, len(got), len(task), archives, err)
	}
}

// The row driver hands a forced compaction to the first execution only;
// re-entry after a wait launches without it.
func TestRowDriverForcesCompactionOnlyAtTheFirstLaunch(t *testing.T) {
	for _, launcherStarted := range []bool{false, true} {
		t.Run(fmt.Sprint(launcherStarted), func(t *testing.T) {
			testRowDriverForcesCompactionOnlyAtTheFirstLaunch(t, launcherStarted)
		})
	}
}

func testRowDriverForcesCompactionOnlyAtTheFirstLaunch(t *testing.T, launcherStarted bool) {
	t.Helper()
	run := RunSpec{RunID: "run-42", IssueNumber: 42, BatchID: "batch-a", Branch: "42-fix", BaseBranch: "main"}
	executor := &rowDriverExecutorFake{
		started: true,
		results: []AgentRunResult{
			{IssueNumber: 42, RunID: run.RunID, Branch: run.Branch, Status: "await", launcherStarted: launcherStarted},
			{IssueNumber: 42, RunID: run.RunID, Branch: run.Branch, Status: "success"},
		},
		observations: []lifecycleVerdict{{Action: lifecycleResume}, {Action: lifecycleResume}},
	}

	newRowDriver(executor, &rowDriverGateFake{}, &rowDriverClockFake{}).Drive(context.Background(), run, ContinuationCursor{Mode: ModeContinue, compactTask: true}, rowDriverOptions{
		ReleaseAwaitCapacity: true,
		PollInterval:         func(int) time.Duration { return time.Nanosecond },
	})

	if len(executor.cursors) != 2 || !executor.cursors[0].compactTask || executor.cursors[1].compactTask == launcherStarted {
		t.Fatalf("compactTask per execution = %v", compactFlags(executor.cursors))
	}
}

func compactFlags(cursors []ContinuationCursor) []bool {
	flags := make([]bool, len(cursors))
	for i, cursor := range cursors {
		flags[i] = cursor.compactTask
	}
	return flags
}

// A batch request reaches the row's first implementation launch only; its
// retries follow the threshold.
func TestRunBatch_CompactTasksForcesOnlyTheFirstLaunch(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	sb := &cleanupErrorSandbox{workDir: filepath.Join(workDir, "worktree"), cleanupFn: func() error { return nil }}
	factory := &capturingRunnableFactory{sandbox: sb}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Compact", State: "open"}}}, &retryRenderer{result: "# Task\n\nInitial task."},
		&fakeConfigStore{config: &config.Config{Agent: "opencode", Sandbox: "worktree", WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"},
			AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")}}},
		&events.JSONLLogger{Path: filepath.Join(workDir, "events.jsonl")},
		WithErrorLog(io.Discard), WithSandboxFactory(&cleanupErrorSandboxFactory{sandbox: sb}), WithRunnableFactory(factory))

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}, Retries: 1, CompactTasks: map[int]bool{42: true}, RunTS: orchTestRunTS, RunShortID: orchTestRunShortID})

	if len(factory.runs) < 2 {
		t.Fatalf("launches = %d, want a first launch and a retry", len(factory.runs))
	}
	for i, run := range factory.runs {
		if run.forceTaskCompaction != (i == 0) {
			t.Errorf("launch %d: forced = %t", i+1, run.forceTaskCompaction)
		}
	}
}

// An issue can carry its own `## Next Step`; the render marks only the
// template's, so compaction keeps the issue's whole and caps the template's.
func TestFreshRenderMarksTheTemplatesNextStep(t *testing.T) {
	root := t.TempDir()
	issueStep := strings.Repeat("issue step. ", 500)
	fresh := launchInWorktree(t, root, prompt.RenderConfig{}, testTaskCompactBytes, &prompt.Engine{}, func(run *AgentRun) {
		run.issue.Body = "## Next Step\n\n" + issueStep + "\n"
	})

	at := strings.LastIndex(fresh.stdin, "## Next Step\n")
	task := fresh.stdin[:at] + strings.Replace(fresh.stdin[at:], "## Next Step\n", "## Next Step\n\n"+strings.Repeat("current step. ", 600)+"\n", 1)
	for i := 1; i <= 30; i++ {
		task += fmt.Sprintf("\n## Checkpoint %d\n\n%s\n", i, strings.Repeat("detail. ", 250))
	}
	result, err := CompactWorktreeTask(task, TaskCompactionRequest{WorkDir: fresh.workDir, Threshold: testTaskCompactBytes, Now: time.Now()})

	if err != nil || !result.Compacted || !strings.Contains(result.Content, issueStep) {
		t.Fatalf("compacted = %t, err %v; the issue's Next Step changed", result.Compacted, err)
	}
	if strings.Count(result.Content, "current step.") >= 600 {
		t.Fatal("the template's Next Step was not capped")
	}
}

// Protected text stays in both the agent's input and the worktree Task, while
// old checkpoints still move.
func TestImplementationLaunch_KeepsProtectedTextAfterCompaction(t *testing.T) {
	// The issue ends in a heading, which a marker after the template heading
	// line would mistake for that line.
	issue := "Rollback plan\n\n## Acceptance criteria\n\n- [ ] " + strings.Repeat("roll back cleanly. ", 400) + "\n\n## Sign-off"
	template := "# Task\n\n## Next Step\n\nStart.\n\n## {{ISSUE_BODY}}\n\nFooter.\n"
	multiline, err := (&prompt.Engine{}).RenderMarked(prompt.RenderConfig{PromptFlag: template}, prompt.IssueData{Body: issue}, testTaskNonce)
	if err != nil {
		t.Fatal(err)
	}

	once, err := CompactWorktreeTask(checkpointTask(30), TaskCompactionRequest{WorkDir: noncedDir(t), Threshold: testTaskCompactBytes, Now: time.Now()})
	if err != nil || !once.Compacted {
		t.Fatalf("setup: %v %s", err, once.Reason)
	}
	end := "<!-- end of Sandman Task History -->\n"
	plan := "## Plan\n\n- [ ] Verify the rollback.\n\n```\noutput\n```\n"
	noted := strings.Replace(once.Content, end, end+"\n```text\ntruncated log\n"+plan, 1) +
		"\n## Plan\n\nA separate plan.\n" + checkpointTask(30)[len(checkpointTask(0)):]

	for name, tc := range map[string]struct{ task, kept string }{
		"multiline heading after Next Step": {withCheckpoints(multiline, 30), issue},
		"note after the history pointer":    {noted, plan},
	} {
		t.Run(name, func(t *testing.T) {
			launch := launchWithCompaction(t, tc.task, testTaskCompactBytes)

			written, err := os.ReadFile(filepath.Join(launch.workDir, ".sandman", "task.md"))
			if err != nil || string(written) != launch.stdin {
				t.Fatalf("worktree Task differs from the launched one (%v)", err)
			}
			if !strings.Contains(launch.stdin, tc.kept) {
				t.Fatalf("protected text left the Task:\n%s", launch.stdin)
			}
			if strings.Contains(launch.stdin, "## Checkpoint 1\n") {
				t.Fatal("the old checkpoints did not move")
			}
		})
	}
}

// noncedDir is a directory recording the test Tasks' markers.
func noncedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	recordTestNonce(t, dir)
	return dir
}

// Without markers only the Next Step section is editable: a template section
// after it, before the review boundary, stays on stdin and disk.
func TestImplementationLaunch_KeepsATemplateSectionAfterAnUnmarkedNextStep(t *testing.T) {
	gate := "## Deployment Gate\n\n- [ ] " + strings.Repeat("rollback verified. ", 600) + "\n\n"
	template := strings.Replace(prompt.DefaultPrompt(), "## Managed Review Boundary\n", gate+"## Managed Review Boundary\n", 1)
	templatePath := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	task := withCheckpoints(strings.Replace(template, "{{ISSUE_BODY}}", "", 1), 30)
	task = strings.Replace(task, "## Next Step\n", "## Next Step\n\nCurrent checkpoint.\n"+strings.Repeat("Obsolete step.\n", 1000), 1)

	launch := launchInWorktree(t, t.TempDir(), prompt.RenderConfig{TaskPrompt: task, PromptFile: templatePath}, testTaskCompactBytes, nil, func(run *AgentRun) {
		run.forceTaskCompaction = true
	})

	written, err := os.ReadFile(filepath.Join(launch.workDir, ".sandman", "task.md"))
	if err != nil || string(written) != launch.stdin {
		t.Fatalf("worktree Task differs from the launched one (%v)", err)
	}
	if !strings.Contains(launch.stdin, gate) {
		t.Fatalf("the template's section left the Task:\n%s", launch.stdin)
	}
	if strings.Contains(launch.stdin, "## Checkpoint 1\n") || strings.Count(launch.stdin, "Obsolete step.") >= 300 {
		t.Fatal("history did not move or Next Step was not capped")
	}
}

// The agent's input, the worktree Task and the archive agree, protected bytes
// stay, and old checkpoints move.
func requireCompactedLaunchKeeps(t *testing.T, launch compactionLaunch, composed string, kept ...string) {
	t.Helper()
	written, err := os.ReadFile(filepath.Join(launch.workDir, ".sandman", "task.md"))
	if err != nil || string(written) != launch.stdin {
		t.Fatalf("worktree Task differs from the launched one (%v)", err)
	}
	for _, text := range kept {
		if !strings.Contains(launch.stdin, text) {
			t.Errorf("lost %q", text[:min(len(text), 80)])
		}
	}
	if strings.Contains(launch.stdin, "## Checkpoint 1\n") {
		t.Error("the old checkpoints did not move")
	}
	archives, _ := filepath.Glob(filepath.Join(launch.workDir, ".sandman", "task-history", "*.md"))
	if len(archives) != 1 {
		t.Fatalf("archives = %v", archives)
	}
	if archived, _ := os.ReadFile(archives[0]); string(archived) != composed {
		t.Error("the archive is not the composed Task")
	}
}

// An operator value can expand to the issue body inside Next Step; the render
// marks what it actually produced.
func TestImplementationLaunch_KeepsAnIssueSubstitutedThroughAPromptArg(t *testing.T) {
	issue := "## Rollback\n\n- [ ] " + strings.Repeat("rollback verified. ", 500)
	rendered, err := (&prompt.Engine{}).RenderMarked(prompt.RenderConfig{
		PromptFlag: "# Task\n\n## Next Step\n\nStart.\n\n{{STARTER}}\n\n## Footer\n\nF.\n",
		PromptArgs: map[string]string{"STARTER": "{{ISSUE_BODY}}"},
	}, prompt.IssueData{Body: issue}, testTaskNonce)
	if err != nil {
		t.Fatal(err)
	}
	task := withCheckpoints(rendered, 30)

	launch := launchWithCompaction(t, task, testTaskCompactBytes)

	requireCompactedLaunchKeeps(t, launch, composedFor(t, task), issue)
}

// Replacing the pointer leaves the sections around it byte for byte.
func TestImplementationLaunch_PointerReplacementKeepsAdjacentSections(t *testing.T) {
	once, err := CompactWorktreeTask(checkpointTask(30), TaskCompactionRequest{WorkDir: noncedDir(t), Threshold: testTaskCompactBytes, Now: time.Now()})
	if err != nil || !once.Compacted {
		t.Fatalf("setup: %v %s", err, once.Reason)
	}
	plan := "## Plan\n\n- [ ] Run the rollback verification.\n\n"
	status := "## Status: already resolved\n"
	task := strings.Replace(once.Content, "## Sandman Task History\n", plan+"## Sandman Task History\n", 1)
	task = strings.Replace(task, "<!-- end of Sandman Task History -->\n", "<!-- end of Sandman Task History -->\n"+status, 1)
	task += checkpointTask(30)[len(checkpointTask(0)):]

	launch := launchWithCompaction(t, task, testTaskCompactBytes)

	requireCompactedLaunchKeeps(t, launch, composedFor(t, task), plan, "\n"+status)
}

// A prompt argument that expands to its own Next Step never becomes the
// editable one: the template's heading is.
func TestImplementationLaunch_CapsTheTemplatesNextStepNotASubstitutedOne(t *testing.T) {
	checklist := "## Next Step\n\n" + strings.Repeat("- [ ] rollback verified.\n", 400)
	rendered, err := (&prompt.Engine{}).RenderMarked(prompt.RenderConfig{
		PromptFlag: "# Task\n\n## Project Requirements\n\n{{RELEASE_DOC}}\n\n## Next Step\n\nStart.\n\n## Footer\n\nF.\n",
		PromptArgs: map[string]string{"RELEASE_DOC": checklist},
	}, prompt.IssueData{}, testTaskNonce)
	if err != nil {
		t.Fatal(err)
	}
	task := withCheckpoints(strings.Replace(rendered, "Start.\n", "Start.\n"+strings.Repeat("Obsolete step.\n", 1000), 1), 30)

	launch := launchWithCompaction(t, task, testTaskCompactBytes)

	requireCompactedLaunchKeeps(t, launch, composedFor(t, task), checklist)
	if strings.Count(launch.stdin, "Obsolete step.") >= 300 {
		t.Error("the template's Next Step was not capped")
	}
}

// A code block that starts in the issue and ends after the structure stays
// whole, with the history pointer after it.
func TestImplementationLaunch_KeepsACodeBlockThatCrossesTheStructure(t *testing.T) {
	rendered, err := (&prompt.Engine{}).RenderMarked(prompt.RenderConfig{}, prompt.IssueData{Number: 7, Body: "Log:\n\n```\nfirst line\n"}, testTaskNonce)
	if err != nil {
		t.Fatal(err)
	}
	closing := "\n## Sample log output\n\n" + strings.Repeat("log line\n", 50) + "```\n"
	task := withCheckpoints(rendered+closing, 30)

	launch := launchWithCompaction(t, task, testTaskCompactBytes)

	requireCompactedLaunchKeeps(t, launch, composedFor(t, task), closing)
	if strings.Index(launch.stdin, prompt.TaskHistoryHeading) < strings.Index(launch.stdin, closing) {
		t.Error("the history pointer is inside the code block")
	}
}

// Compaction keeps whatever a continuation wrote the review settings into,
// and leaves the issue's text as it was.
func TestImplementationLaunch_KeepsTheSettingsWhereverContinuationWroteThem(t *testing.T) {
	template := "# Task\n\n## Issue Context\n\n{{ISSUE_BODY}}\n\n## Runtime Context\n\n- Current branch: `b`\n"
	for name, tc := range map[string]struct{ issue, history string }{
		"later tagged sample":   {history: "\n## Example\n\n```md\n## Runtime Context\n\n- Review command: `/sample-command`\n```\n"},
		"later indented sample": {history: "\n## Log\n\n```\n  ## Runtime Context\n- Review command: `/sample-command`\n```\n"},
		"final section inside the issue's block": {
			issue:   "## Runtime Context\n\n- Review command: `/issue-command`\n\n## Captured output\n\n```text\nlog\n",
			history: "\n```\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rendered, err := (&prompt.Engine{}).RenderMarked(prompt.RenderConfig{PromptFlag: template}, prompt.IssueData{Body: tc.issue}, testTaskNonce)
			if err != nil {
				t.Fatal(err)
			}
			task := withCheckpoints(rendered+tc.history, 30)
			task = prompt.ContinuationTaskPromptWithReviewSettings(task, "/current-review-command", true, 1800)

			launch := launchWithCompaction(t, task, testTaskCompactBytes)

			composed := composedFor(t, task)
			requireCompactedLaunchKeeps(t, launch, composed, tc.issue)
			field := "- Review command: `/current-review-command`"
			if got, want := strings.Count(launch.stdin, field), strings.Count(composed, field); want == 0 || got != want {
				t.Errorf("the launch holds %d of %d current settings", got, want)
			}
		})
	}
}

// Rewriting the sections above Next Step neither takes its marker nor leaves
// lines inside the step: it is still capped and the settings stay whole.
func TestImplementationLaunch_CapsNextStepWhateverIsComposedAboveIt(t *testing.T) {
	command := "/review " + strings.TrimSpace(strings.Repeat("focus ", 1100))
	for name, tc := range map[string]struct {
		above, kept string
		compose     func(string) string
	}{
		"managed handoff, at the fresh render": {above: "## Sandman Managed Handoff Context\n\nold"},
		"review evidence, at re-entry": {above: "## Review Evidence\n\nold", compose: func(task string) string {
			return prompt.TaskWithReviewEvidence(task, map[string]any{"reason": "REVIEW_CHANGES_REQUESTED"})
		}},
		"runtime context, with a long review command": {
			above: "## Runtime Context\n\n- Current branch: `b`",
			kept:  "- Review command: `" + command + "`\n",
			compose: func(task string) string {
				return prompt.ContinuationTaskPromptWithReviewSettings(task, command, true, 1800)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			template := "# Task\n\n## Issue Context\n\n{{ISSUE_BODY}}\n\n" + tc.above + "\n\n## Next Step\n\nStart.\n\n## Footer\n\nF.\n"
			fresh := launchInWorktree(t, root, prompt.RenderConfig{PromptFlag: template}, testTaskCompactBytes, &prompt.Engine{})
			task := withCheckpoints(strings.Replace(fresh.stdin, "Start.\n", "Start.\n"+strings.Repeat("Obsolete step.\n", 1000), 1), 30)
			if tc.compose != nil {
				task = tc.compose(task)
			}

			launch := launchInWorktree(t, root, prompt.RenderConfig{TaskPrompt: task}, testTaskCompactBytes, nil)

			requireCompactedLaunchKeeps(t, launch, composedFor(t, task), "## Next Step\n", "\n\nStart.\n", tc.kept)
			if n := strings.Count(launch.stdin, "Obsolete step."); n >= 300 {
				t.Errorf("Next Step was not capped: %d lines remain", n)
			}
		})
	}
}

// A Runtime Context right after Next Step is refreshed below the marker that
// follows its heading, so the step keeps its bounds.
func TestImplementationLaunch_RefreshesARuntimeContextRightAfterNextStep(t *testing.T) {
	for name, settings := range map[string]string{
		"settings missing": "- Current branch: `b`",
		"settings present": "- Review command: `/old`\n- Delegated review response timeout: `600` seconds",
		"empty section":    "",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			template := "# Task\n\n## Issue Context\n\n{{ISSUE_BODY}}\n\n## Next Step\n\nStart.\n\n## Runtime Context\n\n" + settings + "\n\n## Footer\n\nF.\n"
			fresh := launchInWorktree(t, root, prompt.RenderConfig{PromptFlag: template}, testTaskCompactBytes, &prompt.Engine{})
			task := withCheckpoints(strings.Replace(fresh.stdin, "Start.\n", "Start.\n"+strings.Repeat("Obsolete step.\n", 1000), 1), 30)
			task = prompt.ContinuationTaskPromptWithReviewSettings(task, "/current-review-command", true, 1800)

			launch := launchInWorktree(t, root, prompt.RenderConfig{TaskPrompt: task}, testTaskCompactBytes, nil)

			requireCompactedLaunchKeeps(t, launch, composedFor(t, task), "## Next Step\n", "\n\nStart.\n", "- Review command: `/current-review-command`\n")
			if n := strings.Count(launch.stdin, "Obsolete step."); n >= 300 {
				t.Errorf("Next Step was not capped: %d lines remain", n)
			}
			for _, field := range []string{"- Review command:", "- Review mode:", "- Delegated review response timeout:"} {
				if n := strings.Count(launch.stdin, field); n != 1 {
					t.Errorf("%d %q lines, want 1", n, field)
				}
			}
		})
	}
}

// Another Task's marker quoted in the issue is text: the settings refresh
// writes where it did before markers existed.
func TestImplementationLaunch_IgnoresAnotherTasksMarkers(t *testing.T) {
	quoted := "```\n<!-- sandman:task-end 0000 -->\n<!-- sandman:next-step-start 0000 -->\n```\n"
	template := "# Task\n\n## Runtime Context\n\n{{ISSUE_BODY}}\n- Review command: `/stale`\n\n## Next Step\n\nStart.\n\n## Footer\n\nF.\n"
	rendered, err := (&prompt.Engine{}).RenderMarked(prompt.RenderConfig{PromptFlag: template}, prompt.IssueData{Body: quoted}, testTaskNonce)
	if err != nil {
		t.Fatal(err)
	}
	task := prompt.ContinuationTaskPromptWithReviewSettings(withCheckpoints(rendered, 30), "/current-review-command", true, 1800)

	launch := launchWithCompaction(t, task, testTaskCompactBytes)

	requireCompactedLaunchKeeps(t, launch, composedFor(t, task), quoted, "- Review command: `/current-review-command`\n")
	if strings.Contains(launch.stdin, "/stale") || strings.Count(launch.stdin, "- Review command:") != 1 {
		t.Errorf("the settings were not refreshed once:\n%.600s", launch.stdin)
	}
}

// composedFor is the Task a launch composes before compaction.
func composedFor(t *testing.T, task string) string {
	t.Helper()
	return prompt.EnsureManagedTaskContext(prompt.EnsureReviewTimeoutContext(task, 0))
}
