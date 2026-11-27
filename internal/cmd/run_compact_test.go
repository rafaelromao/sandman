package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// compactableTask is under the default threshold but has old checkpoints a
// forced compaction moves.
func compactableTask() string {
	var b strings.Builder
	b.WriteString(prompt.DefaultPrompt())
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, "\n## Checkpoint %d\n\n%s\n", i, strings.Repeat(fmt.Sprintf("checkpoint %d. ", i), 200))
	}
	return b.String()
}

type compactFixture struct {
	deps     Dependencies
	spy      *spyBatchRunner
	taskPath string
	task     string
}

func newCompactFixture(t *testing.T, compactBytes *int) compactFixture {
	t.Helper()
	dir := t.TempDir()
	branch := "42-fix-bug"
	spy := &spyBatchRunner{result: &batch.Result{}}
	deps := newRunDeps(t, spy)
	worktreePath := addRegisteredContinuationWorktree(t, deps.RepoRoot, dir, branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := compactFixture{deps: deps, spy: spy, taskPath: filepath.Join(worktreePath, ".sandman", "task.md"), task: compactableTask()}
	if len(f.task) > config.DefaultTaskCompactBytes {
		t.Fatalf("fixture Task is %d bytes, over the threshold", len(f.task))
	}
	if err := os.WriteFile(f.taskPath, []byte(f.task), 0o644); err != nil {
		t.Fatal(err)
	}
	f.deps.ConfigStore = &fakeStore{config: &config.Config{Agent: "opencode", WorktreeDir: dir, ReviewCommand: "/oc review", TaskCompactBytes: compactBytes,
		AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}}}}
	f.deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.started", RunID: testRunID42First, Issue: 42, Payload: map[string]any{"branch": branch, "base_branch": "main", "agent": "opencode"}}}}
	f.deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}, prs: map[string]*github.PR{}}
	return f
}

func (f compactFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd := NewRunCmd(f.deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestRun_ContinueCompact_CompactsTheTaskBeforeContinuing(t *testing.T) {
	zero := 0
	for name, compactBytes := range map[string]*int{"default threshold": nil, "automatic compaction disabled": &zero} {
		t.Run(name, func(t *testing.T) {
			f := newCompactFixture(t, compactBytes)

			out, err := f.run(t, "--continue", "--compact", "42")

			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			compacted, _ := os.ReadFile(f.taskPath)
			if strings.Contains(string(compacted), "## Checkpoint 1\n") || !strings.Contains(string(compacted), "## Checkpoint 8\n") {
				t.Fatal("the worktree Task was not compacted")
			}
			archives, _ := filepath.Glob(filepath.Join(filepath.Dir(f.taskPath), "task-history", testRunID42First+"-*.md"))
			if len(archives) != 1 {
				t.Fatalf("archives = %v", archives)
			}
			if archived, _ := os.ReadFile(archives[0]); string(archived) != f.task {
				t.Fatal("the archive is not the original Task")
			}
			if !strings.Contains(out, "[--compact] #42: task compacted:") {
				t.Fatalf("no summary line:\n%s", out)
			}
			if !f.spy.called || strings.Contains(f.spy.req.TaskPrompts[42], "## Checkpoint 1\n") {
				t.Fatal("the continuation prompt was not composed from the compacted Task")
			}
		})
	}
}

func TestRun_ContinueCompact_ReportsAnAlreadyCompactTask(t *testing.T) {
	f := newCompactFixture(t, nil)
	if _, err := f.run(t, "--continue", "--compact", "42"); err != nil {
		t.Fatal(err)
	}
	once, _ := os.ReadFile(f.taskPath)

	out, err := f.run(t, "--continue", "--compact", "42")

	if err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(f.taskPath); string(again) != string(once) {
		t.Fatal("an already compact Task changed")
	}
	if !strings.Contains(out, "[--compact] #42: task not compacted:") || !strings.Contains(out, "already compact") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestRun_ContinueWithoutCompact_LeavesTheTaskAlone(t *testing.T) {
	f := newCompactFixture(t, nil)

	if _, err := f.run(t, "--continue", "42"); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(f.taskPath); string(got) != f.task {
		t.Fatal("the Task changed without --compact")
	}
}

func TestRun_CompactUsageErrorsTouchNothing(t *testing.T) {
	for name, args := range map[string][]string{
		"without --continue": {"--compact", "42"},
		"prompt-only":        {"--continue", "--compact", "--run-id", "my-run"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCompactFixture(t, nil)

			_, err := f.run(t, args...)

			var usage *UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("err = %v, want a usage error", err)
			}
			if got, _ := os.ReadFile(f.taskPath); string(got) != f.task || f.spy.called {
				t.Fatal("a refused command touched the Task or ran the batch")
			}
		})
	}
}

// A continuation the command refuses, such as one whose pull request already
// merged, leaves the Task as it was.
func TestRun_ContinueCompact_RefusedContinuationLeavesTheTaskAlone(t *testing.T) {
	f := newCompactFixture(t, nil)
	f.deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{"42-fix-bug": {Number: 9, State: "MERGED", Merged: true, HeadRefName: "42-fix-bug"}}}

	if _, err := f.run(t, "--continue", "--compact", "42"); err == nil {
		t.Fatal("expected the merged pull request to refuse the continuation")
	}

	if got, _ := os.ReadFile(f.taskPath); string(got) != f.task {
		t.Fatal("a refused continuation compacted the Task")
	}
}

// Only continued rows are compacted: a row promoted to --override has no
// preserved Task to compact and gets no --compact line.
func TestRun_ContinueCompact_SkipsPromotedRows(t *testing.T) {
	f := newCompactFixture(t, nil)
	f.deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "open"}}, prs: map[string]*github.PR{}}

	out, err := f.run(t, "--continue", "--compact", "42", "43")

	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[--compact] #42: task compacted:") || strings.Contains(out, "[--compact] #43") {
		t.Fatalf("output:\n%s", out)
	}
	if f.spy.req.Mode[43] != batch.ModeOverride {
		t.Fatalf("mode[43] = %v, want override", f.spy.req.Mode[43])
	}
}
