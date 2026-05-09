package prompt

import "fmt"

// Engine renders prompt templates with issue metadata.
type Engine struct{}

// Render produces a prompt string from a template.
func (e *Engine) Render(input PromptInput, templateName string) (string, error) {
	return "", fmt.Errorf("prompt rendering not yet implemented")
}

// Ensure Engine implements Renderer.
var _ Renderer = (*Engine)(nil)
