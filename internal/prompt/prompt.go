package prompt

// RenderConfig holds template source preferences.
type RenderConfig struct {
	Branch             string            // explicit branch name override for prompt-only runs
	PromptFlag         string            // --prompt: inline template
	TemplateFlag       string            // --template: path to template file
	PromptFile         string            // .sandman/prompt.md project template path
	RenderedPromptFile string            // .sandman/task.md rendered prompt path
	TaskPrompt         string            // raw task prompt text
	ReviewCommand      string            // REVIEW_COMMAND substitution value
	ReviewCommandSet   bool              // true when review_command should be recorded in events
	CandidateIssues    string            // CANDIDATE_ISSUES substitution value
	MaxCount           int               // MAX_COUNT substitution value
	PromptArgs         map[string]string // arbitrary keys from config
}

// IssueData holds the issue metadata needed for prompt rendering.
type IssueData struct {
	Number       int
	Title        string
	Body         string
	SourceBranch string
	BaseBranch   string
}

// PRData holds the pull request metadata needed for review prompt rendering.
type PRData struct {
	Number      int
	Title       string
	Body        string
	ReviewFocus string
}

// Renderer renders prompt templates with substitutions.
type Renderer interface {
	Render(cfg RenderConfig, data IssueData) (string, error)
	RenderReview(cfg RenderConfig, data PRData) (string, error)
	// PlanTemplate returns the built-in plan template that defines the
	// output shape for the ## Plan section in task.md.
	PlanTemplate() string
}
