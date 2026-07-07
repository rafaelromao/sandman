package batch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/runid"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// TestRunBatch_ModeContinueCopiesOriginalTaskToRunFolder covers slice 9 B3:
// when launching a ModeContinue run, the orchestrator copies the
// worktree's existing .sandman/task.md into the new per-row run folder
// as <runFolder>/task.md before the agent overwrites the worktree file
// with the continuation prompt. The snapshot is a historical artifact
// so the prior wording survives the prompt overwrite.
func TestRunBatch_ModeContinueCopiesOriginalTaskToRunFolder(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	branch := "sandman/42-fix-bug"
	worktreePath := filepath.Join(".sandman", "worktrees", branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	// Stage a stable worktree task.md so the orchestrator's snapshot
	// has known content to capture.
	originalTask := "## Completed\nStaged wording for snapshot.\n"
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte(originalTask), 0644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	log := &spyEventLog{}
	o := NewOrchestrator(
		&fakeGitHubClient{
			issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}},
			prs:    map[string]*github.PR{branch: {Merged: true, HeadRefName: branch}},
		},
		&noopRenderer{},
		&fakeConfigStore{
			config: &config.Config{
				Agent:          "opencode",
				Sandbox:        "worktree",
				WorktreeDir:    filepath.Join(".sandman", "worktrees"),
				Git:            config.GitConfig{BaseBranch: "main"},
				AgentProviders: map[string]config.Agent{"opencode": {Preset: "opencode", Command: "true"}},
			},
		},
		log,
	)
	o.sandboxFactory = &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: worktreePath}}
	// Reuse fakeRunnableFactory with one success result per RunBatch
	// call (initial run + continuation) so the Runnable does not
	// overwrite the worktree's task.md before the snapshot block captures it.
	o.runnableFactory = &fakeRunnableFactory{
		results: []AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: branch, WorktreePath: worktreePath},
			{IssueNumber: 42, Status: "success", Branch: branch, WorktreePath: worktreePath},
		},
	}

	if _, err := o.RunBatch(context.Background(), Request{Issues: []int{42}}); err != nil {
		t.Fatalf("initial run failed: %v", err)
	}

	// Run the continuation with a fresh (ts, shortid) so the new per-row
	// RunID is distinct from the prior run and lands in a fresh batch folder.
	continuedReq := Request{
		Issues:         []int{42},
		Mode:           map[int]IssueMode{42: ModeContinue},
		BaseBranch:     "main",
		PreviousRunIDs: map[int]string{42: log.events[0].RunID},
		RunTS:          orchTestRunTS,
		RunShortID:     orchTestRunShortID,
		PromptConfig:   prompt.RenderConfig{TaskPrompt: prompt.ContinuationTaskPrompt(originalTask)},
	}
	if _, err := o.RunBatch(context.Background(), continuedReq); err != nil {
		t.Fatalf("first continue failed: %v", err)
	}

	// Resolve the expected run folder through the canonical layout so the
	// assertion stays correct if the path scheme ever changes.
	expectedBatchID := runid.NewBatchID(runid.KindIssue, 1, "42", orchTestRunTS, orchTestRunShortID)
	expectedRunID := runid.NewRunID(runid.KindIssue, "42", orchTestRunTS, orchTestRunShortID)
	layout := paths.NewLayout(&config.Config{}, ".")
	expectedRunFolder := layout.RunFolder(expectedBatchID, expectedRunID)

	// The copied task.md must exist in the new run folder with the original
	// content preserved byte-for-byte. The orchestrator writes this file
	// BEFORE launching the agent so the original wording survives even
	// though the worktree's task.md will be overwritten by the new prompt.
	copiedPath := filepath.Join(expectedRunFolder, "task.md")
	content, err := os.ReadFile(copiedPath)
	if err != nil {
		t.Fatalf("read copied task.md at %q: %v", copiedPath, err)
	}
	if string(content) != originalTask {
		t.Errorf("copied task.md content = %q, want %q (must match original worktree task.md)", string(content), originalTask)
	}
}