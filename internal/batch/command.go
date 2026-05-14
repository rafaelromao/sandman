package batch

import (
	"bytes"
	"fmt"
	"text/template"
)

// CommandData holds template variables available for agent command rendering.
type CommandData struct {
	Worktree   string
	PromptFile string
}

// RenderCommand renders an agent command template with the given data.
func RenderCommand(command string, data CommandData) (string, error) {
	tmpl, err := template.New("command").Option("missingkey=error").Parse(command)
	if err != nil {
		return "", fmt.Errorf("parse command template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute command template: %w", err)
	}
	return buf.String(), nil
}
