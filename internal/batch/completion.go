package batch

import (
	"fmt"
	"os"
	"os/exec"
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
			if !strings.HasPrefix(line, "- [✓]") {
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

func buildRetryPrompt(priorContext string) string {
	var b strings.Builder
	b.WriteString("## Prior Context\n\n")
	b.WriteString(strings.TrimSpace(priorContext))
	b.WriteString("\n\n## New Instruction\n\n")
	b.WriteString("Continue the work. Resume from the prior context and finish the remaining implementation steps.\n\n")
	b.WriteString("## Update Continuation Context\n\n")
	b.WriteString("Before exiting, overwrite `.sandman/continuation-context.md` with an updated summary using this template:\n\n")
	b.WriteString("```markdown\n## Completed\n(what was implemented, committed, or merged)\n\n## Pending\n(what remains unfinished)\n\n## Blockers\n(anything preventing completion)\n\n## Key Decisions\n(significant design choices made)\n\n## Next Step\n(single most important next action)\n```\n")
	return b.String()
}

func buildPRReviewContinuationPrompt(priorContext string) string {
	var b strings.Builder
	b.WriteString("## Prior Context\n\n")
	b.WriteString(strings.TrimSpace(priorContext))
	b.WriteString("\n\n## New Instruction\n\n")
	b.WriteString("Continue with sandman-pr-review until the PR is merged.\n\n")
	b.WriteString("## Update Continuation Context\n\n")
	b.WriteString("Before exiting, overwrite `.sandman/continuation-context.md` with an updated summary using this template:\n\n")
	b.WriteString("```markdown\n## Completed\n(what was implemented, committed, or merged)\n\n## Pending\n(what remains unfinished)\n\n## Blockers\n(anything preventing completion)\n\n## Key Decisions\n(significant design choices made)\n\n## Next Step\n(single most important next action)\n```\n")
	return b.String()
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
