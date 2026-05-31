package cmd

import (
	"fmt"
	"strconv"
	"strings"
)

func parseIssueRange(s string) (start, end int, isRange bool, err error) {
	if !strings.Contains(s, ":") {
		n, err := parsePositiveInt(s)
		if err != nil {
			return 0, 0, false, fmt.Errorf("invalid issue number %q", s)
		}
		return n, 0, false, nil
	}

	parts := strings.SplitN(s, ":", 2)
	if parts[0] == "" && parts[1] == "" {
		return 0, 0, false, fmt.Errorf("invalid range %q: empty range", s)
	}

	if parts[0] == "" {
		end, err := parsePositiveInt(parts[1])
		if err != nil {
			return 0, 0, false, fmt.Errorf("invalid range %q", s)
		}
		return 1, end, true, nil
	}

	start, err = parsePositiveInt(parts[0])
	if err != nil {
		return 0, 0, false, fmt.Errorf("invalid range %q", s)
	}

	if parts[1] == "" {
		return start, 0, true, nil
	}

	end, err = parsePositiveInt(parts[1])
	if err != nil {
		return 0, 0, false, fmt.Errorf("invalid range %q", s)
	}

	if start > end {
		return 0, 0, false, fmt.Errorf("invalid range %q: start %d > end %d", s, start, end)
	}

	return start, end, true, nil
}

func parsePositiveInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("not a positive integer: %q", s)
	}
	return n, nil
}
