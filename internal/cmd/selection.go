package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func priorityPromptFileExists(sandmanDir string) bool {
	_, err := os.Stat(filepath.Join(sandmanDir, "priority-selection-prompt.md"))
	return err == nil
}

func resolveRalphQuery(label, query string) string {
	if query != "" {
		return query
	}
	if label != "" {
		return "label:" + label + " is:open"
	}
	return "label:ready-for-agent is:open"
}

func runSelectionPhase(ctx context.Context, client github.Client, count int, label, query, sandmanDir, agentName, modelFlag string, cfg *config.Config) ([]int, error) {
	if err := requireReviewDaemon(cfg.EffectiveReviewCommand(), sandmanDir); err != nil {
		return nil, err
	}
	searchQuery := resolveRalphQuery(label, query)
	ghIssues, err := client.SearchIssues(searchQuery)
	if err != nil {
		return nil, fmt.Errorf("search candidate issues: %w", err)
	}
	if len(ghIssues) == 0 {
		return nil, fmt.Errorf("no candidate issues found")
	}

	candidates := formatCandidateIssues(ghIssues)

	promptText := prompt.ApplySubstitutions(prompt.DefaultPriorityPrompt(), prompt.RenderConfig{
		CandidateIssues: candidates,
		MaxCount:        count,
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
	modelProvider, modelName, err := resolvePiModel(agentCfg.Preset, modelFlag)
	if err != nil {
		return nil, fmt.Errorf("resolve model: %w", err)
	}

	renderedCmd, err := batch.RenderCommand(agentCfg.Command, batch.CommandData{
		PromptFile:    promptPath,
		ModelFlag:     modelFlagStr,
		ModelProvider: modelProvider,
		ModelName:     modelName,
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

	return readSelectedIssues(sandmanDir, count)
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

	if len(selected) > maxCount {
		selected = selected[:maxCount]
	}

	return selected, nil
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

func resolvePiModel(preset, model string) (string, string, error) {
	if preset != "pi" || model == "" {
		return "", "", nil
	}
	provider, name, err := config.SplitPiModel(model)
	if err != nil {
		return "", "", err
	}
	return provider, name, nil
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
