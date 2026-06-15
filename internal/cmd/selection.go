package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func autoPromptFileExists(sandmanDir string) bool {
	_, err := os.Stat(filepath.Join(sandmanDir, "auto-selection-prompt.md"))
	return err == nil
}

func resolveAutoQuery(label, query string) string {
	if query != "" {
		return query
	}
	if label != "" {
		return "label:" + label + " is:open"
	}
	return "label:ready-for-agent is:open"
}

func runSelectionPhase(ctx context.Context, client github.Client, count int, sandmanDir, agentName, modelFlag string, cfg *config.Config, candidates []int) ([]int, error) {
	if err := requireReviewDaemon(cfg.EffectiveReviewCommand(), sandmanDir); err != nil {
		return nil, err
	}
	candidateIssues, err := fetchCandidateIssues(client, candidates)
	if err != nil {
		return nil, err
	}
	if len(candidateIssues) == 0 {
		return nil, fmt.Errorf("no candidate issues found")
	}

	candidateText := formatCandidateIssues(candidateIssues)

	effectiveCount := count
	if effectiveCount <= 0 {
		effectiveCount = len(candidateIssues)
	}

	promptText := prompt.ApplySubstitutions(prompt.DefaultPriorityPrompt(), prompt.RenderConfig{
		CandidateIssues: candidateText,
		MaxCount:        effectiveCount,
	})

	promptPath := filepath.Join(sandmanDir, "selection-prompt.md")
	if err := os.WriteFile(promptPath, []byte(promptText), 0o644); err != nil {
		return nil, fmt.Errorf("write selection prompt: %w", err)
	}
	defer os.Remove(promptPath)

	agentCfg, err := cfg.ResolveAgentProvider(agentName)
	if err != nil {
		return nil, fmt.Errorf("resolve agent: %w", err)
	}

	modelFlagStr := resolveModelFlag(modelFlag, agentCfg.Preset)

	renderedCmd, err := batch.RenderCommand(agentCfg.Command, batch.CommandData{
		PromptFile: promptPath,
		ModelFlag:  modelFlagStr,
	})
	if err != nil {
		return nil, fmt.Errorf("render agent command: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", renderedCmd)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("selection agent failed with stderr: %s: %w", strings.TrimSpace(stderrBuf.String()), err)
	}

	return readSelectedIssues(sandmanDir, effectiveCount)
}

func fetchCandidateIssues(client github.Client, candidates []int) ([]github.Issue, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	issues := make([]github.Issue, 0, len(candidates))
	seen := make(map[int]struct{}, len(candidates))
	for _, n := range candidates {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		issue, err := client.FetchIssue(n)
		if err != nil {
			return nil, fmt.Errorf("fetch candidate issue #%d: %w", n, err)
		}
		if issue == nil {
			continue
		}
		issues = append(issues, *issue)
	}
	return issues, nil
}

func readSelectedIssues(sandmanDir string, maxCount int) ([]int, error) {
	selectedPath := filepath.Join(sandmanDir, "selected-issues.json")
	data, err := os.ReadFile(selectedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("selection phase produced no output")
		}
		return nil, fmt.Errorf("read selected issues: %w", err)
	}

	var selected []int
	if err := json.Unmarshal(data, &selected); err != nil {
		return nil, fmt.Errorf("invalid selection format: %w", err)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("agent selected no issues")
	}

	if maxCount > 0 && len(selected) > maxCount {
		selected = selected[:maxCount]
	}

	return selected, nil
}

func resolveAutoIssues(ctx context.Context, client github.Client, count int, candidates []int, sandmanDir, agentName, modelFlag string, cfg *config.Config) ([]int, error) {
	if autoPromptFileExists(sandmanDir) {
		return runSelectionPhase(ctx, client, count, sandmanDir, agentName, modelFlag, cfg, candidates)
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no issues ready for agent")
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i] < candidates[j]
	})

	if count > 0 && count < len(candidates) {
		candidates = candidates[:count]
	}
	return candidates, nil
}

func resolveModelFlag(modelFlag, preset string) string {
	if modelFlag == "" || preset != "opencode" {
		return ""
	}
	builtin, ok := config.BuiltInAgentPresets["opencode"]
	if !ok {
		return ""
	}
	if strings.Contains(builtin.Command, "{{.ModelFlag}}") {
		return "-m " + modelFlag
	}
	return ""
}

func formatCandidateIssues(issues []github.Issue) string {
	var b strings.Builder
	for _, issue := range issues {
		body := strings.TrimSpace(issue.Body)
		if len(body) > 500 {
			body = body[:500] + "..."
		}
		labelStrs := make([]string, len(issue.Labels))
		for i, l := range issue.Labels {
			labelStrs[i] = l
		}
		b.WriteString(fmt.Sprintf("#%d | %s | [%s] | %s\n", issue.Number, issue.Title, strings.Join(labelStrs, ", "), body))
	}
	return b.String()
}
