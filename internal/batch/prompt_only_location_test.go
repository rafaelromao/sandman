package batch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// A prompt-only or review Request names its Batch by a frozen directory. Its
// identity is the public ID the index records for that directory, never the
// directory's basename, which another Batch may own as its public ID.
func TestRunBatch_PromptOnlyRunDirResolvesItsBatchThroughTheIndex(t *testing.T) {
	const runID = "review-run"
	frozenRel := filepath.Join(".sandman", "batches", "frozen-dir")
	for _, tc := range []struct {
		name   string
		runDir string
	}{
		{name: "batch directory", runDir: frozenRel},
		{name: "review run folder", runDir: filepath.Join(frozenRel, "runs", runID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := testenv.MkdirShort(t, "sm-orch-po-")
			t.Chdir(dir)
			initGitRepo(t, dir)
			layout := paths.NewLayout(&config.Config{}, dir)
			saveBatchIndex(t, layout,
				batchindex.Batch{ID: "public-id", Path: frozenRel, Kind: batchindex.KindPromptOnly, Status: batchindex.StatusActive},
				batchindex.Batch{ID: "frozen-dir", Path: filepath.Join(".sandman", "batches", "elsewhere"), Kind: batchindex.KindPromptOnly, Status: batchindex.StatusActive},
			)

			spyLog := &spyEventLog{}
			agentRunCh := make(chan *AgentRun, 1)
			o := NewOrchestrator(&fakeGitHubClient{err: errors.New("fetch should not run")}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
				Agent:          "test-agent",
				Sandbox:        "worktree",
				WorktreeDir:    ".sandman/worktrees",
				Git:            config.GitConfig{BaseBranch: "main"},
				AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
			}}, spyLog,
				WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(".sandman", "worktrees", "sandman", "prompt-only")}}),
				WithRunnableFactory(&capturingAgentRunFactory{agentRunCh: agentRunCh}),
			)
			if _, err := o.RunBatch(context.Background(), Request{
				PromptConfig: prompt.RenderConfig{PromptFlag: "Do the thing."},
				RunID:        runID,
				RunDir:       tc.runDir,
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			select {
			case ar := <-agentRunCh:
				want := filepath.ToSlash(filepath.Join(frozenRel, "runs", runID))
				if got := filepath.ToSlash(ar.runFolder); !strings.HasSuffix(got, want) {
					t.Errorf("run folder = %q, want it under the frozen directory %q", got, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for AgentRun to be created")
			}
			var started []string
			for _, e := range spyLog.snapshot() {
				if e.Type == "run.started" {
					id, _ := e.Payload["batch_id"].(string)
					started = append(started, id)
				}
			}
			if len(started) != 1 || started[0] != "public-id" {
				t.Errorf("run.started batch_id = %v, want [public-id]", started)
			}
		})
	}
}

// Request.RepoRoot roots Batch location resolution even when the process runs
// from a repository subdirectory, where the orchestrator's default root is.
func TestRunBatch_ResolvesRunDirUnderTheRequestRepositoryRoot(t *testing.T) {
	const runID = "review-run"
	frozenRel := filepath.Join(".sandman", "batches", "frozen-dir")
	dir := testenv.MkdirShort(t, "sm-orch-root-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	saveBatchIndex(t, paths.NewLayout(&config.Config{}, dir),
		batchindex.Batch{ID: "public-id", Path: frozenRel, Kind: batchindex.KindPromptOnly, Status: batchindex.StatusActive},
	)
	subdir := filepath.Join(dir, "nested")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	spyLog := &spyEventLog{}
	agentRunCh := make(chan *AgentRun, 1)
	o := NewOrchestrator(&fakeGitHubClient{err: errors.New("fetch should not run")}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:          "test-agent",
		Sandbox:        "worktree",
		WorktreeDir:    ".sandman/worktrees",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}}, spyLog,
		WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(dir, ".sandman", "worktrees", "sandman", "prompt-only")}}),
		WithRunnableFactory(&capturingAgentRunFactory{agentRunCh: agentRunCh}),
	)
	if _, err := o.RunBatch(context.Background(), Request{
		PromptConfig: prompt.RenderConfig{PromptFlag: "Do the thing."},
		RunID:        runID,
		RunDir:       frozenRel,
		RepoRoot:     dir,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case ar := <-agentRunCh:
		if want := filepath.Join(dir, frozenRel, "runs", runID); filepath.Clean(ar.runFolder) != want {
			t.Errorf("run folder = %q, want %q under the request's repository root", ar.runFolder, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for AgentRun to be created")
	}
	for _, e := range spyLog.snapshot() {
		if e.Type == "run.started" {
			if id, _ := e.Payload["batch_id"].(string); id != "public-id" {
				t.Errorf("run.started batch_id = %q, want public-id", id)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(subdir, ".sandman")); !os.IsNotExist(err) {
		t.Errorf("subdirectory gained Sandman state (%v); want everything under the request's repository root", err)
	}
}

// A Batch-shaped RunDir that the index cannot attribute fails closed, as an
// issue RunDir does, instead of running under an empty identity.
func TestPromptOnlyBatchLocation_FailsClosedForAnUnattributableBatchDirectory(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, root)
	foreign := filepath.Join(string(filepath.Separator), "repo", ".sandman", "batches", "260504123456-ab12-prompt")
	if _, err := promptOnlyBatchLocation(layout, "", "", "", foreign); err == nil {
		t.Fatal("foreign batches directory resolved; want an error")
	}
	if err := os.MkdirAll(filepath.Dir(layout.BatchesIndexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.BatchesIndexPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := promptOnlyBatchLocation(layout, "", "", "", filepath.Join(".sandman", "batches", "frozen-dir")); err == nil {
		t.Fatal("corrupt index resolved; want an error")
	}
}
