package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
)

// SimpleIssuePicker presents a numbered list of issues and reads user selection.
type SimpleIssuePicker struct {
	// In is the reader for user input. Defaults to os.Stdin.
	In io.Reader
}

func (s *SimpleIssuePicker) reader() io.Reader {
	if s.In != nil {
		return s.In
	}
	return os.Stdin
}

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

	reader := bufio.NewReader(s.reader())
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read selection: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}

	var selected []int
	var invalid []string
	fields := strings.Fields(line)
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil {
			invalid = append(invalid, field)
			continue
		}
		if n >= 1 && n <= len(issues) {
			selected = append(selected, issues[n-1].Number)
		} else {
			invalid = append(invalid, field)
		}
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("invalid selection: %s", strings.Join(invalid, ", "))
	}
	return selected, nil
}
