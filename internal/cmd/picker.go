package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
)

// SimpleIssuePicker presents a numbered list of issues and reads user selection.
type SimpleIssuePicker struct{}

// Select displays the issues and returns the selected issue numbers.
func (s *SimpleIssuePicker) Select(issues []github.Issue) ([]int, error) {
	if len(issues) == 0 {
		return nil, nil
	}

	fmt.Fprintln(os.Stderr, "Select issues (space-separated numbers):")
	for i, issue := range issues {
		fmt.Fprintf(os.Stderr, "  [%d] #%d  %s\n", i+1, issue.Number, issue.Title)
	}
	fmt.Fprint(os.Stderr, "> ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read selection: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}

	var selected []int
	fields := strings.Fields(line)
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		if n >= 1 && n <= len(issues) {
			selected = append(selected, issues[n-1].Number)
		}
	}
	return selected, nil
}
