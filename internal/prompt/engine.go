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

//go:embed default_pr_review_prompt.md
var defaultPRReviewPrompt string

//go:embed priority_selection_prompt.md
var prioritySelectionPrompt string

// DefaultPrompt returns Sandman's canonical prompt template.
func DefaultPrompt() string { return defaultPrompt }

// DefaultPRReviewPrompt returns the built-in prompt template used by
// `sandman review` (both one-shot and daemon modes).
func DefaultPRReviewPrompt() string { return defaultPRReviewPrompt }

// DefaultPriorityPrompt returns the built-in priority selection prompt template.
func DefaultPriorityPrompt() string { return prioritySelectionPrompt }

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
	result = strings.ReplaceAll(result, "{{CANDIDATE_ISSUES}}", cfg.CandidateIssues)
	result = strings.ReplaceAll(result, "{{MAX_COUNT}}", fmt.Sprintf("%d", cfg.MaxCount))
	return result
}

// ApplyPRSubstitutions renders {{PR_NUMBER}}, {{PR_TITLE}}, {{PR_BODY}},
// and {{REVIEW_FOCUS}} in a review prompt template. The substitution set is
// deliberately separate from ApplySubstitutions so the issue-running render
// path does not consume PR keys and so the review path stays decoupled from
// the issue metadata shape.
func ApplyPRSubstitutions(template string, data PRData) string {
	result := template
	result = strings.ReplaceAll(result, "{{PR_NUMBER}}", fmt.Sprintf("%d", data.Number))
	result = strings.ReplaceAll(result, "{{PR_TITLE}}", data.Title)
	result = strings.ReplaceAll(result, "{{PR_BODY}}", data.Body)
	result = strings.ReplaceAll(result, "{{REVIEW_FOCUS}}", data.ReviewFocus)
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
	result = strings.ReplaceAll(result, "{{BASE_BRANCH}}", data.BaseBranch)
	result = strings.ReplaceAll(result, "{{BRANCH}}", data.SourceBranch)
	result = ApplySubstitutions(result, cfg)

	if unmatched := keyPattern.FindAllString(result, -1); len(unmatched) > 0 {
		return "", fmt.Errorf("missing substitution keys: %s", strings.Join(unmatched, ", "))
	}

	return result, nil
}

// RenderReview produces a review prompt string. The template is taken from
// cfg (PromptFlag overrides the embedded default); PR substitutions are then
// applied. The issue-running render path never sees these keys.
func (e *Engine) RenderReview(cfg RenderConfig, data PRData) (string, error) {
	template := defaultPRReviewPrompt
	switch {
	case strings.TrimSpace(cfg.PromptFlag) != "":
		template = cfg.PromptFlag
	case strings.TrimSpace(cfg.TemplateFlag) != "":
		content, err := os.ReadFile(cfg.TemplateFlag)
		if err != nil {
			return "", fmt.Errorf("read template file: %w", err)
		}
		template = string(content)
	}
	result := ApplyPRSubstitutions(template, data)

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
