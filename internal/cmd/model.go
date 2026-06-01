package cmd

import "strings"

func resolveModel(model, defaultModel string) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(defaultModel)
}
