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
	"regexp"
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

// redeliveryGrace is how long the probe pauses after re-delivering an
// operator signal. Delivery to the process is asynchronous, so without a
// handler sandman dies during the pause instead of resuming the caller
// (for example `sandman init` writing its Dockerfile). A caller with a
// handler pays the pause once, after the operator already asked to stop.
const redeliveryGrace = 100 * time.Millisecond

// operatorSignals are the signals that stop sandman from a terminal or
// a supervisor. The probe watches them because its own process group
// receives neither a terminal Ctrl-C nor the hangup when the terminal
// closes.
var operatorSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// VersionProbe shells `opencode --version` and returns a validated,
// normalized version. Production wires the bounded host probe at
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
// Both bare V1 output and the named `opencode v2.1.2` format are
// recognized. The executable name must never become an npm version.
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
// sandman dies by that signal shortly after, as if no probe ran.
func probeVersionWithin(timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stopWatching := cancelOnOperatorSignal(cancel)
	cmd := exec.CommandContext(ctx, "opencode", "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProbeGroup(cmd) }
	cmd.WaitDelay = versionProbeWaitDelay
	out, err := cmd.Output()
	sig := stopWatching()
	if (err != nil || sig != nil) && cmd.Process != nil {
		// Cancel only runs on timeout or interrupt. This also stops a
		// helper that kept stdout open past WaitDelay, outlived a failed
		// probe, or was still running when a signal arrived just after
		// the probe exited.
		_ = killProbeGroup(cmd)
	}
	if sig != nil {
		// Re-deliver the signal now that the group is gone; without a
		// handler of its own, sandman dies shortly after.
		if s, ok := sig.(syscall.Signal); ok {
			_ = syscall.Kill(os.Getpid(), s)
			time.Sleep(redeliveryGrace)
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
	fields := strings.Fields(versionPrefix.ReplaceAllString(strings.TrimSpace(string(out)), ""))
	if len(fields) == 0 {
		return "", fmt.Errorf("opencode --version returned no version")
	}
	version := strings.TrimPrefix(fields[0], "v")
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("opencode --version returned an invalid version")
	}
	return version, nil
}

// killProbeGroup kills the process group the probe leads, including
// members that outlived the probe process itself. It is harmless if the
// group is already gone.
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

const versionNumber = `(0|[1-9][0-9]*)`
const prereleaseIdentifier = `(` + versionNumber + `|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)`

var versionPattern = regexp.MustCompile(`^` + versionNumber + `\.` + versionNumber + `\.` + versionNumber +
	`(-` + prereleaseIdentifier + `(\.` + prereleaseIdentifier + `)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
var versionPrefix = regexp.MustCompile(`^opencode\s+`)
