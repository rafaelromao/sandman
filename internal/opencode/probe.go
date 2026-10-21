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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// versionProbeTimeout bounds one host `opencode --version` probe. A
// healthy host answers in well under a second (about 0.7 s cold and
// 0.3 s warm on a developer laptop), so five seconds leaves room for a
// cold cache on a slow host while capping how long a hung binary can
// delay `sandman run`, `sandman review` and `sandman init`, which all
// probe synchronously before doing any work.
const versionProbeTimeout = 5 * time.Second

// versionProbeWaitDelay bounds pipe draining after the probe exits or
// its process group is killed, so a helper that escaped the group or
// outlived the probe while holding stdout cannot extend the wait.
const versionProbeWaitDelay = time.Second

// operatorSignals are the signals that stop sandman from a terminal or
// a supervisor. The probe watches them because its own process group
// receives neither a terminal Ctrl-C nor the hangup when the terminal
// closes.
var operatorSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// VersionProbe shells `opencode --version` and returns the leading
// semver-style string. Production wires the bounded host probe at
// package init time; tests inject a stub via the package-level var.
// Returning (empty, err) is the documented "host has no usable opencode
// binary" signal — callers should not warn in that case. A probe that
// does not finish within five seconds, or that the operator interrupts,
// returns that signal too.
//
// An operator signal that arrives during the probe is re-delivered once
// the probe's process group is gone. A caller that registered its own
// handler before probing therefore receives that signal twice.
//
// The parse is intentionally tolerant: opencode has shipped versions
// like `1.17.19`, `1.17.19+abc123`, and bare date stamps across
// releases (we observed both formats in the wild during the issue
// investigation); we accept anything in the first whitespace-separated
// token.
var VersionProbe = func() (string, error) {
	return probeVersionWithin(versionProbeTimeout)
}

// probeVersionWithin runs `opencode --version` in its own process group.
// It kills the whole group when timeout elapses, when an operator signal
// arrives, or when the probe does not finish cleanly, so in those cases
// no process in the group outlives the call; a clean exit leaves the
// group alone. WaitDelay bounds any pipe a helper outside the group
// still holds. An operator signal is handed back once the group is gone,
// so the caller's own disposition still applies: without a handler,
// sandman exits as if no probe ran.
func probeVersionWithin(timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stopWatching := cancelOnOperatorSignal(cancel)
	cmd := exec.CommandContext(ctx, "opencode", "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProbeGroup(cmd) }
	cmd.WaitDelay = versionProbeWaitDelay
	out, err := cmd.Output()
	if err != nil && cmd.Process != nil {
		// Cancel only runs on timeout or interrupt. This also stops a
		// helper that kept stdout open past WaitDelay or outlived a
		// failed probe.
		_ = killProbeGroup(cmd)
	}
	if sig := stopWatching(); sig != nil {
		// Re-deliver the signal now that the group is gone; without a
		// handler of its own, sandman exits here.
		if s, ok := sig.(syscall.Signal); ok {
			_ = syscall.Kill(os.Getpid(), s)
		}
		return "", errors.Join(fmt.Errorf("opencode --version interrupted by %v", sig), err)
	}
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// opencode answered and exited cleanly; only a helper held
		// stdout open, so the version it printed is still valid.
		err = nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("opencode --version did not finish within %s (context: %w): %w", timeout, ctxErr, err)
		}
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", fmt.Errorf("opencode --version returned no output")
	}
	return fields[0], nil
}

// killProbeGroup kills the process group the probe leads. The group can
// outlive the probe process itself, so this stays valid after Wait.
func killProbeGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// cancelOnOperatorSignal calls cancel when an operator signal arrives.
// The returned stop ends the watch and reports the signal that arrived,
// if any. Signals the process ignores are not watched, so they stay
// ignored.
func cancelOnOperatorSignal(cancel context.CancelFunc) (stop func() os.Signal) {
	var watched []os.Signal
	for _, sig := range operatorSignals {
		if !signal.Ignored(sig) {
			watched = append(watched, sig)
		}
	}
	if len(watched) == 0 {
		return func() os.Signal { return nil }
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, watched...)
	done, finished := make(chan struct{}), make(chan struct{})
	var received os.Signal
	go func() {
		defer close(finished)
		select {
		case received = <-signals:
			cancel()
		case <-done:
		}
	}()
	return func() os.Signal {
		close(done)
		<-finished
		signal.Stop(signals)
		if received == nil {
			select {
			case received = <-signals:
			default:
			}
		}
		return received
	}
}
