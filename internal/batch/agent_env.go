package batch

import (
	"sort"
	"strings"
)

func applyAgentEnv(command string, env map[string]string, opencodePermissionMode string) string {
	if len(env) == 0 {
		return command
	}
	applyOpencodePermission := strings.Contains(command, "--dangerously-skip-permissions")

	keys := make([]string, 0, len(env))
	for key := range env {
		if key == "OPENCODE_PERMISSION" && opencodePermissionMode == "builtin" && !applyOpencodePermission {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return command
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, key := range keys {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString("export ")
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
