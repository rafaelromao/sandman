// Package opencode owns the small set of helpers that the opencode CLI
// requires across multiple consumers in the Sandman codebase. Today the
// package is a single package-level seam, VersionProbe, used by:
//   - internal/cmd/opencode_version.go: the run/review mismatch warning.
//   - internal/scaffold/scaffolder.go: the host-current version picker
//     during sandman init.
//
// Both consumers want the same probe (opencode --version), and the
// package exists so neither consumer needs to import the other (cmd
// already imports scaffold, so a cmd import from scaffold would
// create a cycle). Mirrors the role of internal/atomicfs and
// internal/paths: a tiny utility package shared across the rest of the
// repo.
package opencode

import (
	"fmt"
	"os/exec"
	"strings"
)

// VersionProbe shells `opencode --version` and returns the leading
// semver-style string. Production wires exec.Command at package init
// time; tests inject a stub via the package-level var. Returning
// (empty, err) is the documented "host has no opencode binary"
// signal — callers should not warn in that case.
//
// The parse is intentionally tolerant: opencode has shipped versions
// like `1.17.19`, `1.17.19+abc123`, and bare date stamps across
// releases (we observed both formats in the wild during the issue
// investigation); we accept anything in the first whitespace-separated
// token.
var VersionProbe = func() (string, error) {
	out, err := exec.Command("opencode", "--version").Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", fmt.Errorf("opencode --version returned no output")
	}
	return fields[0], nil
}
