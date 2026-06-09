package cmd

import (
	"fmt"
	"strconv"
)

// parseContinueArgs splits raw CLI args into issue numbers.
// All args are treated as issue numbers. Duplicates are removed,
// order is preserved.
func parseContinueArgs(args []string) ([]int, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("expected at least one issue number")
	}

	issues := make([]int, 0, len(args))
	seen := make(map[int]struct{}, len(args))
	for _, arg := range args {
		n, err := strconv.Atoi(arg)
		if err != nil {
			return nil, fmt.Errorf("invalid issue number %q: expected numeric issue numbers", arg)
		}
		if _, ok := seen[n]; !ok {
			seen[n] = struct{}{}
			issues = append(issues, n)
		}
	}

	if len(issues) == 0 {
		return nil, fmt.Errorf("expected at least one issue number")
	}

	return issues, nil
}
