package cmd

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"
)

// requestTaskCompaction applies `--continue --compact`: each issue's Task is
// compacted, even under the threshold, by its first implementation launch in
// the batch, after the batch has admitted the row. It reports one line per
// issue.
func requestTaskCompaction(cmd *cobra.Command, issues []int) map[int]bool {
	if len(issues) == 0 {
		return nil
	}
	compact := make(map[int]bool, len(issues))
	for _, num := range issues {
		compact[num] = true
		fmt.Fprintf(cmd.ErrOrStderr(), "[--compact] #%d: the Task is compacted at its first launch; see the run log\n", num)
	}
	return compact
}

// selectedIssues returns the continued issues the operator selected, by
// number, label, query or picker, in continuation order.
func selectedIssues(continued, selected []int) []int {
	var out []int
	for _, num := range continued {
		if slices.Contains(selected, num) {
			out = append(out, num)
		}
	}
	return out
}
