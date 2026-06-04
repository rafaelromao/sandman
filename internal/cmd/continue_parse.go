package cmd

import (
	"fmt"
	"strconv"
	"strings"
)

// parseContinueArgs splits raw CLI args into issue numbers and prompt text.
// Leading consecutive numeric args are treated as issue numbers (deduplicated,
// order preserved). Remaining args are joined with spaces as the prompt text.
func parseContinueArgs(args []string) ([]int, string, error) {
	if len(args) == 0 {
		return nil, "", fmt.Errorf("expected at least one issue number")
	}

	issues := make([]int, 0, len(args))
	seen := make(map[int]struct{}, len(args))
	cut := 0
	for i, arg := range args {
		n, err := strconv.Atoi(arg)
		if err != nil {
			break
		}
		if _, ok := seen[n]; !ok {
			seen[n] = struct{}{}
			issues = append(issues, n)
		}
		cut = i + 1
	}

	if len(issues) == 0 {
		return nil, "", fmt.Errorf("invalid issue number %q: expected at least one issue number", args[0])
	}

	prompt := strings.Join(args[cut:], " ")
	if strings.TrimSpace(prompt) == "" {
		return nil, "", fmt.Errorf("no prompt provided")
	}

	return issues, prompt, nil
}
