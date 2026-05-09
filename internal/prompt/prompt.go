package prompt

// PromptInput holds the issue metadata needed for prompt rendering.
type PromptInput struct {
	Number int
	Title  string
	Body   string
	Labels []string
}

// Renderer renders prompt templates with substitutions.
type Renderer interface {
	Render(input PromptInput, templateName string) (string, error)
}
