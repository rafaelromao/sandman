package batch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func parseLogForCompletion(logPath string) bool {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return false
	}

	lines := strings.Split(string(data), "\n")
	start := -1
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "--- run ") || strings.HasPrefix(line, "--- retry ") {
			start = i + 1
			break
		}
	}
	if start == -1 {
		return false
	}

	for i := len(lines) - 1; i >= start; i-- {
		if strings.TrimSpace(lines[i]) == "# Todos" {
			start = i
			break
		}
	}
	if start < 0 || start >= len(lines) || strings.TrimSpace(lines[start]) != "# Todos" {
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
			if !strings.HasPrefix(line, "- [✓]") && !strings.HasPrefix(line, "- [x]") && !strings.HasPrefix(line, "- [X]") {
				return false
			}
		}
	}

	return hasItem
}

func checkPRMerged(client github.Client, branch string) bool {
	merged, err := checkPRMergedAtHead(client, branch, "")
	return err == nil && merged
}

func CheckPRMergedAtHead(client github.Client, branch, headSHA string) (bool, error) {
	return checkPRMergedAtHead(client, branch, headSHA)
}

func checkPRMergedAtHead(client github.Client, branch, headSHA string) (bool, error) {
	if client == nil || strings.TrimSpace(branch) == "" {
		return false, nil
	}
	pr, err := client.FindPRByBranch(branch)
	if err != nil || pr == nil {
		return false, err
	}
	if !pr.Merged && !strings.EqualFold(pr.State, "merged") {
		return false, nil
	}
	if strings.TrimSpace(headSHA) != "" && strings.TrimSpace(pr.HeadRefOid) != "" && !strings.EqualFold(pr.HeadRefOid, headSHA) {
		return false, nil
	}
	return true, nil
}

func findOpenPRByBranch(client github.Client, branch string) (*github.PR, error) {
	if client == nil || strings.TrimSpace(branch) == "" {
		return nil, nil
	}
	pr, err := client.FindPRByBranch(branch)
	if err != nil || pr == nil {
		return nil, err
	}
	if !strings.EqualFold(pr.State, "open") {
		return nil, nil
	}
	return pr, nil
}

// EmptyHandoffTemplate is the fallback prompt used when the handoff document
// is missing (the file does not exist). It tells the agent to continue the
// work without prior context.
const EmptyHandoffTemplate = `## Completed


## Pending


## Blockers


## Key Decisions


## Next Step
Continue the work.`

// ReadHandoffContent reads the handoff document at the given path and returns
// its verbatim content. The second return value is true when the file was read
// successfully. When the file is missing (os.IsNotExist), EmptyHandoffTemplate
// is returned and exists is false. Other read errors (permission denied, I/O
// failure) are surfaced through the error return.
func ReadHandoffContent(path string) (content string, exists bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return EmptyHandoffTemplate, false, nil
		}
		return "", false, err
	}
	return string(raw), true, nil
}

func LogRetryMarker(logPath string, attempt, maxRetries int) {
	_ = logRetryMarker(logPath, attempt, maxRetries)
}

var logRetryMarkerFn = logRetryMarker

func logRetryMarker(logPath string, attempt, maxRetries int) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer file.Close()
	if _, err := fmt.Fprintf(file, "--- retry %d/%d ---\n", attempt+1, maxRetries+1); err != nil {
		return fmt.Errorf("write retry marker: %w", err)
	}
	return nil
}

func currentBranchHead(workDir string) (string, error) {
	cmd := exec.Command("git", "-C", workDir, "rev-parse", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

var currentBranchHeadFn = currentBranchHead

func logRunMarker(logPath string, attempt, maxRetries int) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer file.Close()
	if _, err := fmt.Fprintf(file, "--- run %d/%d ---\n", attempt+1, maxRetries+1); err != nil {
		return fmt.Errorf("write run marker: %w", err)
	}
	return nil
}

var logRunMarkerFn = logRunMarker

// BuildHandoffPrompt wraps raw handoff markdown content into a structured
// resume prompt using the shared prompt package helpers.
func BuildHandoffPrompt(content string) string {
	doc := prompt.ParseHandoff(content)
	return prompt.BuildResumePrompt(doc)
}
