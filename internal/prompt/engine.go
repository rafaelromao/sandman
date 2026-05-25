package prompt

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

//go:embed default_prompt.md
var defaultPrompt string

// DefaultPrompt returns Sandman's canonical prompt template.
func DefaultPrompt() string { return defaultPrompt }

var keyPattern = regexp.MustCompile(`\{\{[^{}]+\}\}`)

func loadTemplate(cfg RenderConfig) (string, error) {
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
	return template, nil
}

// ApplySubstitutions renders non-issue placeholders in a prompt template.
func ApplySubstitutions(template string, cfg RenderConfig) string {
	result := template
	for k, v := range cfg.PromptArgs {
		result = strings.ReplaceAll(result, fmt.Sprintf("{{%s}}", k), v)
	}
	reviewCommand := strings.TrimSpace(cfg.ReviewCommand)
	if reviewCommand == "" {
		reviewCommand = config.DefaultReviewCommand
	}
	result = strings.ReplaceAll(result, "{{REVIEW_COMMAND}}", reviewCommand)
	return result
}

// Engine renders prompt templates with issue metadata.
type Engine struct{}

// Render produces a prompt string from a template.
func (e *Engine) Render(cfg RenderConfig, data IssueData) (string, error) {
	template, err := loadTemplate(cfg)
	if err != nil {
		return "", err
	}

	result := template
	result = strings.ReplaceAll(result, "{{ISSUE_NUMBER}}", fmt.Sprintf("%d", data.Number))
	result = strings.ReplaceAll(result, "{{ISSUE_TITLE}}", data.Title)
	result = strings.ReplaceAll(result, "{{ISSUE_BODY}}", data.Body)
	result = strings.ReplaceAll(result, "{{SOURCE_BRANCH}}", data.SourceBranch)
	result = strings.ReplaceAll(result, "{{BASE_BRANCH}}", data.TargetBranch)
	result = strings.ReplaceAll(result, "{{BRANCH}}", data.SourceBranch)
	result = ApplySubstitutions(result, cfg)

	if unmatched := keyPattern.FindAllString(result, -1); len(unmatched) > 0 {
		return "", fmt.Errorf("missing substitution keys: %s", strings.Join(unmatched, ", "))
	}

	return result, nil
}

// MaterializePromptFile creates the project prompt template if it is missing
// and no prompt/template override is active.
func MaterializePromptFile(cfg RenderConfig) error {
	if cfg.PromptFlag != "" || cfg.TemplateFlag != "" {
		return nil
	}
	if cfg.PromptFile == "" {
		return nil
	}
	if _, err := os.Stat(cfg.PromptFile); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check prompt file: %w", err)
	}
	dir := filepath.Dir(cfg.PromptFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create prompt directory: %w", err)
	}
	return os.WriteFile(cfg.PromptFile, []byte(DefaultPrompt()), 0644)
}

// Ensure Engine implements Renderer.
var _ Renderer = (*Engine)(nil)
