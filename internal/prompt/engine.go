package prompt

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

//go:embed default-task-prompt.md
var defaultPrompt string

//go:embed default_pr_review_prompt.md
var defaultPRReviewPrompt string

//go:embed auto_selection_prompt.md
var prioritySelectionPrompt string

//go:embed badge_prompt.md
var badgePrompt string

// promptVersion is the hex-encoded SHA-256 digest of the embedded
// default-task-prompt.md. When the embedded template changes between sandman
// versions the digest changes, signalling that MaterializePromptFile should
// overwrite the project's .sandman/prompt.md.
var promptVersion string

// promptVersionFile is the sidecar file that records the version of the
// materialized prompt template inside .sandman/.
const promptVersionFile = ".prompt-version"

func init() {
	sum := sha256.Sum256([]byte(defaultPrompt))
	promptVersion = hex.EncodeToString(sum[:])
}

// DefaultPrompt returns Sandman's canonical prompt template.
func DefaultPrompt() string { return defaultPrompt }

// DefaultPRReviewPrompt returns the built-in prompt template used by
// `sandman review` (both one-shot and daemon modes).
func DefaultPRReviewPrompt() string { return defaultPRReviewPrompt }

// DefaultPriorityPrompt returns the built-in priority selection prompt template.
func DefaultPriorityPrompt() string { return prioritySelectionPrompt }

// DefaultBadgePrompt returns the built-in prompt template used by the
// post-batch badge-suggestion hook.
func DefaultBadgePrompt() string { return badgePrompt }

var keyPattern = regexp.MustCompile(`\{\{[^{}]+\}\}`)

// issueMapping composes the operator-controlled substitution values for
// the issue keys. ISSUE_BODY is intentionally NOT included here — the
// body is passed as a separate argument to Renderer.Render so the
// body-inert rule applies.
func issueMapping(data IssueData) map[string]string {
	return map[string]string{
		"ISSUE_NUMBER":  fmt.Sprintf("%d", data.Number),
		"ISSUE_TITLE":   data.Title,
		"SOURCE_BRANCH": data.SourceBranch,
		"BASE_BRANCH":   data.BaseBranch,
		"BRANCH":        data.SourceBranch,
	}
}

// configMapping composes the operator-controlled substitution values for
// the render-config keys. REVIEW_COMMAND resolution preserves the
// historical precedence: cfg.ReviewCommand wins over
// PromptArgs["REVIEW_COMMAND"] wins over config.DefaultReviewCommand.
// The other keys come straight from cfg.
func configMapping(cfg RenderConfig) map[string]string {
	mapping := map[string]string{
		"CANDIDATE_ISSUES": cfg.CandidateIssues,
		"MAX_COUNT":        fmt.Sprintf("%d", cfg.MaxCount),
		"REVIEW_COMMAND":   config.DefaultReviewCommand,
	}
	for k, v := range cfg.PromptArgs {
		if k == "REVIEW_COMMAND" {
			continue
		}
		mapping[k] = v
	}
	if v, ok := cfg.PromptArgs["REVIEW_COMMAND"]; ok {
		mapping["REVIEW_COMMAND"] = v
	}
	if reviewCommand := strings.TrimSpace(cfg.ReviewCommand); reviewCommand != "" {
		mapping["REVIEW_COMMAND"] = reviewCommand
	}
	return mapping
}

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
// It is a thin wrapper that builds the operator mapping from cfg and
// runs it against the template. The portal/selection/run call sites
// that pre-compute a prompt for issue-selection detection have no issue
// body on this path; the body-inert rule does not apply. The wrapper
// preserves the historical "always return the partial result" contract
// so callers can still detect unfilled placeholders in the returned
// string, even though Renderer.Render returns "" on missing keys.
func ApplySubstitutions(template string, cfg RenderConfig) string {
	intermediate := template
	for k, v := range configMapping(cfg) {
		intermediate = strings.ReplaceAll(intermediate, "{{"+k+"}}", v)
	}
	return intermediate
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

// Render produces a prompt string from a template. It is a thin wrapper
// over Renderer.Render that composes the operator mapping from
// IssueData and RenderConfig and returns the renderer's error string
// unchanged so the historical "missing substitution keys" contract is
// preserved.
func (e *Engine) Render(cfg RenderConfig, data IssueData) (string, error) {
	template, err := loadTemplate(cfg)
	if err != nil {
		return "", err
	}

	mapping := issueMapping(data)
	for k, v := range configMapping(cfg) {
		mapping[k] = v
	}

	result, _, err := (&Renderer{}).Render(template, data.Body, mapping)
	return result, err
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

// MaterializePromptFile creates the project prompt template when no
// prompt/template override is active. If the template already exists it is
// overwritten when the embedded default-task-prompt.md has changed since the last
// materialization (detected via a version sidecar).
func MaterializePromptFile(cfg RenderConfig) error {
	if cfg.PromptFlag != "" || cfg.TemplateFlag != "" {
		return nil
	}
	if cfg.PromptFile == "" {
		return nil
	}
	dir := filepath.Dir(cfg.PromptFile)
	versionPath := filepath.Join(dir, promptVersionFile)

	needsWrite := true
	if info, err := os.Stat(cfg.PromptFile); err == nil {
		if info.IsDir() {
			return fmt.Errorf("prompt path is a directory: %s", cfg.PromptFile)
		}
		if versionData, err := os.ReadFile(versionPath); err == nil {
			needsWrite = string(versionData) != promptVersion
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check prompt file: %w", err)
	}

	if !needsWrite {
		return nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create prompt directory: %w", err)
	}
	if err := os.WriteFile(cfg.PromptFile, []byte(DefaultPrompt()), 0644); err != nil {
		return fmt.Errorf("write prompt file: %w", err)
	}
	return os.WriteFile(versionPath, []byte(promptVersion), 0644)
}

// Ensure Engine implements IssueRenderer.
var _ IssueRenderer = (*Engine)(nil)
