package batch

import (
	"sort"
	"strings"
)

func applyAgentEnv(command string, env map[string]string) string {
	if len(env) == 0 {
		return command
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("export ")
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(shellQuote(env[key]))
	}
	b.WriteString("; ")
	b.WriteString(command)
	return b.String()
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
