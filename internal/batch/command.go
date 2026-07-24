package batch

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// CommandData holds template variables available for agent command rendering.
type CommandData struct {
	PromptFile                 string
	ModelFlag                  string
	VariantFlag                string
	ModelProvider              string
	ModelName                  string
	DangerouslySkipPermissions bool
	SessionName                string
}

// RenderCommand renders an agent command template with the given data.
func RenderCommand(command string, data CommandData) (string, error) {
	if strings.Contains(data.SessionName, "'") {
		return "", fmt.Errorf("SessionName must not contain single quotes")
	}
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
