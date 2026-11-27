package batch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
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
	root := t.TempDir()
	initGitRepo(t, root)
	out, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(fakePromptCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SANDMAN_TEST_OUT", out)
	sb := sandbox.NewWorktreeSandbox(root, filepath.Join(root, ".sandman", "worktrees"), "compaction", "main")
	if err := sb.Start(sandbox.SandboxStart{}); err != nil {
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

	launch.result = run.Run(context.Background(), &spyRenderer{}, config.BuiltInAgentPresets["opencode-v2"].Command, prompt.RenderConfig{TaskPrompt: task})

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
