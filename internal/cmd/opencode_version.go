package cmd

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/opencode"
	"github.com/rafaelromao/sandman/internal/scaffold"
	"github.com/spf13/cobra"
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

// warnOpencodeVersionMismatch is the runtime helper invoked from the
// `run` and `review` cmd entry points. It is non-fatal on every
// branch: silent for non-opencode agents, missing probe, missing
// Dockerfile, matched versions, or worktree mode (where host IS the
// sandbox). It only emits the warning text when both sides have a
// valid version and they disagree.
//
// The cmd parameter carries the cobra command whose Err stream is
// where the warning is written. sandboxFlag mirrors the cmd flag
// (empty means "fall back to cfg.Sandbox"). cfg supplies the
// resolved default when sandboxFlag is empty. repoRoot is the
// host-absolute repository path used to locate .sandman/Dockerfile.
func warnOpencodeVersionMismatch(cmd *cobra.Command, agentName, sandboxFlag, repoRoot string, cfg *config.Config) {
	if agentName != "opencode" {
		return
	}
	sandboxMode := strings.TrimSpace(sandboxFlag)
	if sandboxMode == "" && cfg != nil {
		sandboxMode = strings.TrimSpace(cfg.Sandbox)
	}
	if sandboxMode == "" {
		return
	}
	pin, _ := readDockerfilePin(repoRoot)
	catalogDefault := scaffoldDefaultOpencodeVersion()
	sandboxVersion := ResolveSandboxVersion(sandboxMode, "", pin, catalogDefault)
	if sandboxVersion == "" {
		return
	}
	hostVersion, err := opencode.VersionProbe()
	if err != nil || hostVersion == "" {
		return
	}
	// Worktree mode means the agent runs the host opencode directly
	// (no separate pinned build). ResolveSandboxVersion already returns
	// the hostVersion for worktree, so this comparison collapses to
	// identity when versions match, but we still guard explicitly in
	// case future catalog-default fallbacks drift.
	if sandboxVersion == hostVersion {
		return
	}
	writeWarning(cmd.ErrOrStderr(), FormatMismatchWarning(hostVersion, sandboxVersion, repoRoot))
}

func writeWarning(w io.Writer, msg string) {
	if w == nil {
		return
	}
	fmt.Fprint(w, msg)
}

// readDockerfilePin is the host-absolute path read for the opencode pin
// produced by `sandman init`. Returns ("", false) when the file is
// missing or unreadable; the caller falls back to the catalog default.
func readDockerfilePin(repoRoot string) (string, bool) {
	if repoRoot == "" {
		return "", false
	}
	path := repoRoot + "/.sandman/Dockerfile"
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return ParseOpencodePinFromDockerfile(string(data))
}

// scaffoldDefaultOpencodeVersion returns the catalog head pinned by
// the scaffolder. Wraps scaffold.DefaultBuiltInAgentVersion so the
// cmd package does not need to hardcode the agent name.
func scaffoldDefaultOpencodeVersion() string {
	return scaffold.DefaultBuiltInAgentVersion("opencode")
}
