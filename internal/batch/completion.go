package batch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
)

func parseLogForCompletion(logPath string) bool {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return false
	}

	lines := strings.Split(string(data), "\n")
	start := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "# Todos" {
			start = i
			break
		}
	}
	if start == -1 {
		return false
	}

	hasItem := false
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			break
		}
		if strings.HasPrefix(line, "- [") {
			hasItem = true
			if !strings.HasPrefix(line, "- [✓]") {
				return false
			}
		}
	}

	return hasItem
}

func checkPRMerged(client github.Client, branch string) bool {
	if client == nil || strings.TrimSpace(branch) == "" {
		return false
	}
	pr, err := client.FindPRByBranch(branch)
	if err != nil || pr == nil {
		return false
	}
	return pr.Merged || strings.EqualFold(pr.State, "merged")
}

func LogRetryMarker(logPath string, attempt, maxRetries int) {
	_ = logRetryMarker(logPath, attempt, maxRetries)
}

func logRetryMarker(logPath string, attempt, maxRetries int) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer file.Close()
	if _, err := fmt.Fprintf(file, "--- retry %d/%d ---\n", attempt+1, maxRetries); err != nil {
		return fmt.Errorf("write retry marker: %w", err)
	}
	return nil
}
