package cmd

import (
	"strconv"
	"strings"
)

func formatIssueList(issues []int) string {
	if len(issues) == 0 {
		return "(none)"
	}
	parts := make([]string, len(issues))
	for i, issue := range issues {
		parts[i] = "#" + strconv.Itoa(issue)
	}
	return strings.Join(parts, ", ")
}
