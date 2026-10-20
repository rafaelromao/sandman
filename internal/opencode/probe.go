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
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// VersionProbeTimeout bounds one host `opencode --version` probe. A
// healthy host answers in well under a second (about 0.7 s cold and
// 0.3 s warm on a developer laptop), so five seconds leaves room for a
// cold cache on a slow host while capping how long a hung binary can
// delay `sandman run`, `sandman review` and `sandman init`, which all
// probe synchronously before doing any work.
const VersionProbeTimeout = 5 * time.Second

// versionProbeWaitDelay bounds pipe draining after the probe exits or
// its process group is killed, so a helper that escaped the group or
// outlived the probe while holding stdout cannot extend the wait.
const versionProbeWaitDelay = time.Second

// VersionProbe shells `opencode --version` and returns the leading
// semver-style string. Production wires the bounded host probe at
// package init time; tests inject a stub via the package-level var.
// Returning (empty, err) is the documented "host has no usable opencode
// binary" signal — callers should not warn in that case. A probe that
// does not finish within VersionProbeTimeout returns that signal too.
//
// The parse is intentionally tolerant: opencode has shipped versions
// like `1.17.19`, `1.17.19+abc123`, and bare date stamps across
// releases (we observed both formats in the wild during the issue
// investigation); we accept anything in the first whitespace-separated
// token.
var VersionProbe = func() (string, error) {
	return probeVersion(VersionProbeTimeout)
}

// probeVersion runs `opencode --version` in its own process group.
// When timeout elapses the whole group is killed, so neither the probe
// nor helpers it spawned keep running, and WaitDelay bounds any pipe a
// surviving helper still holds.
func probeVersion(timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "opencode", "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = versionProbeWaitDelay
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("opencode --version did not finish within %s: %w", timeout, ctxErr)
		}
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", fmt.Errorf("opencode --version returned no output")
	}
	return fields[0], nil
}
