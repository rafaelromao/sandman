package cmd

import (
	"fmt"
	"regexp"
	"strings"
)

var opencodeAIPinRe = regexp.MustCompile(`(?m)^RUN\s+npm install -g opencode-ai@([A-Za-z0-9._\-+]+)\s*$`)

func ParseOpencodePinFromDockerfile(content string) (string, bool) {
	match := opencodeAIPinRe.FindStringSubmatch(content)
	if match == nil {
		return "", false
	}
	return strings.TrimSpace(match[1]), true
}

// ResolveSandboxVersion returns the effective opencode version that will
// run inside the sandbox for the active mode. The host version is the
// version opencode would consume when there is no container (worktree
// mode); the Dockerfile pin is what the container image installs;
// the catalog default is the fallback when neither is present
// (mirrors what a fresh `sandman init` would write today). An unknown
// mode yields empty so the caller can short-circuit on "not applicable."
func ResolveSandboxVersion(mode, hostVersion, dockerfilePin, catalogDefault string) string {
	switch mode {
	case "worktree":
		return hostVersion
	case "podman", "docker":
		if dockerfilePin != "" {
			return dockerfilePin
		}
		return catalogDefault
	default:
		return ""
	}
}

// FormatMismatchWarning renders the multi-line warning emitted when the
// host opencode version differs from the sandbox-installed version.
// It is intentionally human-readable (not machine-parseable), names
// the symptom operators search for, and points at the two escape
// paths: re-running sandman init (picks up host version automatically)
// or editing internal/scaffold/scaffolder.go's catalog directly.
func FormatMismatchWarning(hostVersion, sandboxVersion, repoRoot string) string {
	return fmt.Sprintf(
		"warning: opencode host version (%s) does not match sandbox version (%s).\n"+
			"         Run `sandman init` to refresh the pinned image, or update\n"+
			"         `builtInAgentVersionCatalog[\"opencode\"]` in internal/scaffold/scaffolder.go.\n"+
			"         Mismatches can cause agent runs to exit 1 with \"UnknownError:\n"+
			"         Unexpected server error\" before producing a result.\n",
		hostVersion, sandboxVersion,
	)
}
