package prompt

import (
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

//go:embed default_prompt.md
var defaultPrompt string

// DefaultPrompt returns Sandman's canonical prompt template.
func DefaultPrompt() string { return defaultPrompt }

var keyPattern = regexp.MustCompile(`\{\{[^{}]+\}\}`)

// Engine renders prompt templates with issue metadata.
type Engine struct{}

// Render produces a prompt string from a template.
func (e *Engine) Render(cfg RenderConfig, data IssueData) (string, error) {
	template := defaultPrompt
	if cfg.PromptFlag != "" {
		template = cfg.PromptFlag
	} else if cfg.TemplateFlag != "" {
		content, err := os.ReadFile(cfg.TemplateFlag)
		if err != nil {
			return "", fmt.Errorf("read template file: %w", err)
		}
		template = string(content)
	} else if cfg.PromptFile != "" {
		content, err := os.ReadFile(cfg.PromptFile)
		if err == nil {
			template = string(content)
		}
		// Missing prompt file silently falls back to default — .sandman/prompt.md is optional.
	}

	result := template
	result = strings.ReplaceAll(result, "{{ISSUE_NUMBER}}", fmt.Sprintf("%d", data.Number))
	result = strings.ReplaceAll(result, "{{ISSUE_TITLE}}", data.Title)
	result = strings.ReplaceAll(result, "{{ISSUE_BODY}}", data.Body)
	result = strings.ReplaceAll(result, "{{SOURCE_BRANCH}}", data.SourceBranch)
	result = strings.ReplaceAll(result, "{{TARGET_BRANCH}}", data.TargetBranch)
	result = strings.ReplaceAll(result, "{{BRANCH}}", data.SourceBranch)
	result = strings.ReplaceAll(result, "{{DEFAULT_BRANCH}}", data.TargetBranch)

	for k, v := range cfg.PromptArgs {
		result = strings.ReplaceAll(result, fmt.Sprintf("{{%s}}", k), v)
	}
	reviewCommand := strings.TrimSpace(cfg.ReviewCommand)
	if reviewCommand == "" {
		reviewCommand = config.DefaultReviewCommand
	}
	result = strings.ReplaceAll(result, "{{REVIEW_COMMAND}}", reviewCommand)

	if unmatched := keyPattern.FindAllString(result, -1); len(unmatched) > 0 {
		return "", fmt.Errorf("missing substitution keys: %s", strings.Join(unmatched, ", "))
	}

	return result, nil
}

// Ensure Engine implements Renderer.
var _ Renderer = (*Engine)(nil)
