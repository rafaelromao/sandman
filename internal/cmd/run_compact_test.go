package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// The command only requests compaction: the row's first launch compacts the
// Task after the batch has admitted it, whatever task_compact_bytes says.
func TestRun_ContinueCompact_RequestsCompactionAtTheFirstLaunch(t *testing.T) {
	zero := 0
	for name, compactBytes := range map[string]*int{"default threshold": nil, "automatic compaction disabled": &zero} {
		t.Run(name, func(t *testing.T) {
			f := newCompactFixture(t, compactBytes)

			out, err := f.run(t, "--continue", "--compact", "42")

			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if !f.spy.called || len(f.spy.req.CompactTasks) != 1 || !f.spy.req.CompactTasks[42] {
				t.Fatalf("CompactTasks = %v", f.spy.req.CompactTasks)
			}
			if got, _ := os.ReadFile(f.taskPath); string(got) != f.task {
				t.Fatal("the command wrote the Task before the batch admitted the row")
			}
			if archives, _ := filepath.Glob(filepath.Join(filepath.Dir(f.taskPath), "task-history", "*")); len(archives) != 0 {
				t.Fatalf("the command wrote archives: %v", archives)
			}
			if !strings.Contains(out, "[--compact] #42: the Task is compacted at its first launch") {
				t.Fatalf("no summary line:\n%s", out)
			}
		})
	}
}

func TestRun_ContinueWithoutCompact_LeavesTheTaskAlone(t *testing.T) {
	f := newCompactFixture(t, nil)

	if _, err := f.run(t, "--continue", "42"); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(f.taskPath); string(got) != f.task || len(f.spy.req.CompactTasks) != 0 {
		t.Fatalf("the Task changed or compaction was requested without --compact: %v", f.spy.req.CompactTasks)
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
	if !f.spy.req.CompactTasks[42] || f.spy.req.CompactTasks[43] || strings.Contains(out, "[--compact] #43") {
		t.Fatalf("CompactTasks = %v\n%s", f.spy.req.CompactTasks, out)
	}
	if f.spy.req.Mode[43] != batch.ModeOverride {
		t.Fatalf("mode[43] = %v, want override", f.spy.req.Mode[43])
	}
}

// --compact applies to the continued issues the operator selected. A ready
// continuation resumed alongside them is compacted only when it is selected
// too; either way it keeps its ready routing, and session reuse is kept.
func TestRun_ContinueCompact_ReadyContinuationsOnlyWhenSelected(t *testing.T) {
	for name, tc := range map[string]struct {
		args       []string
		compacts43 bool
	}{
		"resumed automatically": {[]string{"--continue", "--compact", "--reuse-session", "42"}, false},
		"selected":              {[]string{"--continue", "--compact", "--reuse-session", "42", "43"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCompactFixture(t, nil)
			worktrees := filepath.Dir(filepath.Dir(filepath.Dir(f.taskPath)))
			readyTask := filepath.Join(addRegisteredContinuationWorktree(t, f.deps.RepoRoot, worktrees, "43-other"), ".sandman", "task.md")
			if err := os.MkdirAll(filepath.Dir(readyTask), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(readyTask, []byte(f.task), 0o644); err != nil {
				t.Fatal(err)
			}
			f.deps.GitHubClient = &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}, 43: {Number: 43, State: "open", Title: "Other"}}, prs: map[string]*github.PR{}}
			readyRunID := testRunTS + "-" + testRunShortID + "-43-prev"
			started := time.Now().UTC().Add(-2 * time.Minute)
			log := f.deps.EventLog.(*fakeEventLog)
			log.events = append(log.events,
				events.Event{Type: "run.started", Timestamp: started, RunID: readyRunID, Issue: 43, Payload: map[string]any{
					"branch": "43-other", "base_branch": "main", "batch_id": "old-batch", "issue_title": "Other"}},
				events.Event{Type: "run.await", Timestamp: started.Add(time.Minute), RunID: readyRunID, Issue: 43, Payload: map[string]any{"gate": "pending", "await": true}},
				events.Event{Type: "run.capacity_queued", Timestamp: started.Add(2 * time.Minute), RunID: readyRunID, Issue: 43, Payload: map[string]any{
					"ready_continuation": true, "branch": "43-other", "base_branch": "main", "batch_id": "old-batch",
					"previous_run_id": readyRunID, "previous_run_batch_id": "old-batch", "issue_title": "Other"}},
			)

			out, err := f.run(t, tc.args...)

			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if !f.spy.req.ReadyContinuations[43] || f.spy.req.RunIDs[43] != readyRunID {
				t.Fatalf("#43 lost its ready routing:\n%s", out)
			}
			if !f.spy.req.CompactTasks[42] || !f.spy.req.ReuseSession[42] {
				t.Fatalf("#42: compact %v, reuse %v", f.spy.req.CompactTasks, f.spy.req.ReuseSession)
			}
			if f.spy.req.CompactTasks[43] != tc.compacts43 || strings.Contains(out, "[--compact] #43") != tc.compacts43 {
				t.Fatalf("#43 compaction requested = %t, want %t:\n%s", f.spy.req.CompactTasks[43], tc.compacts43, out)
			}
			for _, path := range []string{f.taskPath, readyTask} {
				if got, _ := os.ReadFile(path); string(got) != f.task {
					t.Fatalf("the command wrote %s", path)
				}
			}
		})
	}
}

// A start refused after the continuation is accepted, here because the batch
// ID cannot be generated, leaves the Task as it was. Refusals inside the batch
// are covered where the launch compacts.
func TestRun_ContinueCompact_RefusedStartLeavesTheTaskAlone(t *testing.T) {
	f := newCompactFixture(t, nil)
	if err := os.WriteFile(filepath.Join(".sandman", "batches"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := f.run(t, "--continue", "--compact", "42")

	if err == nil || f.spy.called {
		t.Fatalf("expected the start to be refused: err %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(f.taskPath); string(got) != f.task {
		t.Fatal("a refused start compacted the Task")
	}
	if archives, _ := filepath.Glob(filepath.Join(filepath.Dir(f.taskPath), "task-history", "*")); len(archives) != 0 {
		t.Fatalf("a refused start wrote archives: %v", archives)
	}
}
