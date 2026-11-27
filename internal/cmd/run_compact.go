package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/spf13/cobra"
)

// compactContinuedTasks applies `--continue --compact`: each continued
// issue's preserved Task is compacted, even under the threshold, and its
// continuation prompt is composed from the result. The first launch rereads
// the compacted Task from the worktree. A failure leaves that Task as it was.
func compactContinuedTasks(cmd *cobra.Command, cfg *config.Config, req *batch.Request, issues []int, sandmanDir string, compose func(string) string) {
	worktreeBase := strings.TrimSpace(cfg.WorktreeDir)
	if worktreeBase == "" {
		worktreeBase = ".sandman/worktrees"
	}
	template := req.PromptConfig
	if template.PromptFile == "" {
		template.PromptFile = filepath.Join(sandmanDir, "prompt.md")
	}
	for _, num := range issues {
		workDir := filepath.Join(worktreeBase, req.Branches[num])
		taskPath := filepath.Join(workDir, ".sandman", "task.md")
		content, exists, err := batch.ReadTaskContent(taskPath)
		if err != nil || !exists {
			fmt.Fprintf(cmd.ErrOrStderr(), "[--compact] #%d: task not compacted: no readable Task at %s\n", num, taskPath)
			continue
		}
		result, err := batch.CompactWorktreeTask(content, batch.TaskCompactionRequest{
			WorkDir: workDir, Label: req.PreviousRunIDs[num], Threshold: cfg.EffectiveTaskCompactBytes(),
			Force: true, Template: template, Now: time.Now(),
		})
		fmt.Fprintf(cmd.ErrOrStderr(), "[--compact] #%d: %s\n", num, batch.TaskCompactionSummary(result, err))
		if err != nil || !result.Compacted {
			continue
		}
		if err := atomicfs.WriteAtomic(taskPath, []byte(result.Content), 0o644); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "[--compact] #%d: write compacted Task: %v\n", num, err)
			continue
		}
		req.TaskPrompts[num] = compose(result.Content)
	}
}
