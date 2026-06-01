package cmd

import "strings"

func resolveModel(model, defaultModel, preset string) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	if preset == "" {
		return ""
	}
	return strings.TrimSpace(defaultModel)
}
