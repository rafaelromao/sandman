package prompt

import "github.com/rafaelromao/sandman/internal/github"

// Renderer renders prompt templates with substitutions.
type Renderer interface {
	Render(issue github.Issue, templateName string) (string, error)
}
