package batch

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestLinkedReviewContinuationCursorRoundTripsPromptOnlyIdentity(t *testing.T) {
	const previousRunID = "previous-review-run"
	const previousBatchID = "previous-review-batch"
	run, cursor := runSpecFromRow(RowSpec{
		IssueNumber:         1855,
		Review:              true,
		Mode:                ModeContinue,
		PreviousRunIDs:      map[int]string{0: previousRunID},
		PreviousRunBatchIDs: map[int]string{0: previousBatchID},
	})
	if cursor.PreviousRunID != previousRunID || cursor.PreviousRunBatchID != previousBatchID {
		t.Fatalf("linked-review cursor = run:%q batch:%q, want run:%q batch:%q", cursor.PreviousRunID, cursor.PreviousRunBatchID, previousRunID, previousBatchID)
	}

	row := run.row(cursor)
	if row.PreviousRunIDs[0] != previousRunID || row.PreviousRunBatchIDs[0] != previousBatchID {
		t.Fatalf("reconstructed linked-review continuation maps = run:%v batch:%v", row.PreviousRunIDs, row.PreviousRunBatchIDs)
	}
}

func TestLinkedReviewContinuationPreservesPriorRunIDAcrossExecutor(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-review-cursor-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	const priorRunID = "previous-review-run"
	log := &spyEventLog{}
	o := NewOrchestrator(&fakeGitHubClient{err: errors.New("prompt-only review must not fetch an issue")}, &noopRenderer{}, &fakeConfigStore{config: &config.Config{
		Agent:       "test-agent",
		Sandbox:     "worktree",
		WorktreeDir: ".sandman/worktrees",
		Git:         config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"test-agent": {Command: "true"},
		},
	}}, log,
		WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{
			workDir: filepath.Join(dir, ".sandman", "worktrees", "sandman", "review-linked"),
		}}),
		WithRunnableFactory(&fakeRunnableFactory{results: []AgentRunResult{{Status: "success"}}}),
	)

	_, err := o.RunBatch(context.Background(), Request{
		PromptConfig:        prompt.RenderConfig{PromptFlag: "Review the linked pull request.", Branch: "review-17-1855"},
		Review:              true,
		IssueNumber:         1855,
		PRNumber:            17,
		RunID:               "260618113825-abcd-1855-PR17",
		Mode:                map[int]IssueMode{0: ModeContinue},
		PreviousRunIDs:      map[int]string{0: priorRunID},
		PreviousRunBatchIDs: map[int]string{0: "previous-review-batch"},
	})
	if err != nil {
		t.Fatalf("RunBatch linked review continuation: %v", err)
	}

	for _, event := range log.snapshot() {
		if event.Type == "run.continued" {
			if got := event.Payload["previous_run_id"]; got != priorRunID {
				t.Fatalf("run.continued previous_run_id = %#v, want %q", got, priorRunID)
			}
			return
		}
	}
	t.Fatal("RunBatch did not emit run.continued for linked review continuation")
}
