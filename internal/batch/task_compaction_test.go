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

// checkpointTask is the built-in template followed by count agent checkpoint
// sections of about 2 KiB each, the shape a long continuation grows into.
func checkpointTask(count int) string {
	var b strings.Builder
	b.WriteString(prompt.DefaultPrompt())
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&b, "\n## Checkpoint %d\n\n%s\n", i, strings.Repeat(fmt.Sprintf("checkpoint %d detail. ", i), 100))
	}
	return b.String()
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

// A fresh render records its headings. Sandman's own sections in that render
// must not anchor the checkpoints an agent appends later, or nothing would
// ever count as history.
func TestFreshRenderAnchorsTheTaskForLaterCompaction(t *testing.T) {
	root := t.TempDir()
	fresh := launchInWorktree(t, root, prompt.RenderConfig{}, testTaskCompactBytes, &prompt.Engine{})
	anchors, err := os.ReadFile(filepath.Join(fresh.workDir, ".sandman", "task-anchors"))
	if err != nil {
		t.Fatalf("fresh render recorded no anchors: %v", err)
	}
	if want := strings.Join(prompt.TaskHeadings(fresh.stdin), "\n") + "\n"; string(anchors) != want {
		t.Fatalf("anchors =\n%s\nwant\n%s", anchors, want)
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
	task := prompt.DefaultPrompt() + "\n## Plan\n\n" + strings.Repeat("a long plan line\n", 4000)

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
	second := once.Content + checkpointTask(30)[len(prompt.DefaultPrompt()):]
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

// A Task rendered before anchors were recorded, from a template that ends
// with the issue body, has no boundary between the issue's sections and
// agent history, so none of them moves.
func TestCompactWorktreeTask_KeepsIssueSectionsWhenTheTemplateEndsWithTheIssueBody(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "prompt.md")
	template := "# Task\n\n## Next Step\n\nStart.\n\n## Issue Context\n\n{{ISSUE_BODY}}\n"
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	var task strings.Builder
	task.WriteString(strings.Replace(template, "{{ISSUE_BODY}}", "## Acceptance criteria\n\n- [ ] keep me\n", 1))
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&task, "\n## Checkpoint %d\n\n%s\n", i, strings.Repeat("detail. ", 250))
	}

	result, err := CompactWorktreeTask(task.String(), TaskCompactionRequest{WorkDir: dir, Threshold: testTaskCompactBytes, Force: true,
		Template: prompt.RenderConfig{PromptFile: templatePath}, Now: time.Now()})

	if err != nil || result.Compacted || !strings.Contains(result.Content, "- [ ] keep me") {
		t.Fatalf("compacted = %t, err %v", result.Compacted, err)
	}
	if !strings.Contains(result.Reason, "no safe history boundary") {
		t.Fatalf("reason = %q", result.Reason)
	}
}
