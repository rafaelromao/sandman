package batch

import (
	"context"
	"errors"
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
