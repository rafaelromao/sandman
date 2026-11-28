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
		fmt.Fprintln(cmd.ErrOrStderr(), "[--compact] no continued issue is selected, so no Task is compacted")
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
// number, label, query or picker, in continuation order. Selecting a
// Specification selects the issues it expands to, which children maps.
func selectedIssues(continued, selected []int, children map[int][]int) []int {
	chosen := map[int]bool{}
	pending := slices.Clone(selected)
	for len(pending) > 0 {
		num := pending[0]
		pending = pending[1:]
		if !chosen[num] {
			chosen[num] = true
			pending = append(pending, children[num]...)
		}
	}
	var out []int
	for _, num := range continued {
		if chosen[num] {
			out = append(out, num)
		}
	}
	return out
}
