package prompt

// RenderConfig holds template source preferences.
type RenderConfig struct {
	PromptFlag         string            // --prompt: inline template
	TemplateFlag       string            // --template: path to template file
	PromptFile         string            // .sandman/prompt.md project template path
	RenderedPromptFile string            // .sandman/rendered-prompt.md rendered prompt path
	ContinuePrompt     string            // raw continuation prompt text
	ReviewCommand      string            // REVIEW_COMMAND substitution value
	ReviewCommandSet   bool              // true when --review-command was provided
	PromptArgs         map[string]string // arbitrary keys from config
}

// IssueData holds the issue metadata needed for prompt rendering.
type IssueData struct {
	Number       int
	Title        string
	Body         string
	SourceBranch string
	TargetBranch string
}

// Renderer renders prompt templates with substitutions.
type Renderer interface {
	Render(cfg RenderConfig, data IssueData) (string, error)
}
