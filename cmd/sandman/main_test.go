package main

import (
	"bytes"
	"errors"
	"io"
	"runtime/debug"
	"strings"
	"testing"

	sandmancmd "github.com/rafaelromao/sandman/internal/cmd"
	"github.com/spf13/cobra"
)

// withVersionFixtures swaps the package-level version and buildInfo vars for
// the duration of a test, restoring the originals via t.Cleanup. Keeps the
// tests that touch the global ldflags seam independent and parallel-safe.
func withVersionFixtures(t *testing.T, v string, bi func() (*debug.BuildInfo, bool)) {
	t.Helper()
	prevV, prevBI := version, buildInfo
	t.Cleanup(func() { version, buildInfo = prevV, prevBI })
	version, buildInfo = v, bi
}

func TestVersion_LdflagsOverrideReturnsInjectedValue(t *testing.T) {
	withVersionFixtures(t, "v1.0.0", func() (*debug.BuildInfo, bool) {
		t.Error("buildInfo must not be called when ldflags-injected version is set")
		return nil, false
	})
	if got := Version(); got != "v1.0.0" {
		t.Errorf("Version() = %q, want %q", got, "v1.0.0")
	}
}

func TestVersion_FallsBackToBuildInfoWhenLdflagsUnset(t *testing.T) {
	withVersionFixtures(t, "", func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260716184825-0e018c21696d"}}, true
	})
	if got := Version(); got != "v0.0.0-20260716184825-0e018c21696d" {
		t.Errorf("Version() = %q, want buildinfo pseudo-version", got)
	}
}

func TestVersion_ReturnsDevWhenNeitherLdflagsNorBuildInfoProvidesValue(t *testing.T) {
	withVersionFixtures(t, "", func() (*debug.BuildInfo, bool) { return nil, false })
	if got := Version(); got != "dev" {
		t.Errorf("Version() = %q, want %q", got, "dev")
	}
}

func TestExecuteRoot_NoErrorNoOutput(t *testing.T) {
	root := &cobra.Command{Use: "sandman", SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	var stderr bytes.Buffer
	calls := 0
	executeRoot(root, &stderr, func(code int) {
		calls++
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	})
	if calls != 0 {
		t.Errorf("exit should not be called on success, called %d times", calls)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty, got %q", stderr.String())
	}
}

func TestExecuteRoot_PrintsErrorOnlyForRuntimeError(t *testing.T) {
	root := &cobra.Command{Use: "sandman", SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("github API down")
	}}
	var stderr bytes.Buffer
	calls := 0
	exit := func(code int) { calls = code }
	executeRoot(root, &stderr, exit)
	if calls != 1 {
		t.Errorf("exit code = %d, want 1", calls)
	}
	if !strings.Contains(stderr.String(), "github API down") {
		t.Errorf("expected runtime error in stderr, got %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("runtime error should not print usage, got %q", stderr.String())
	}
}

func TestExecuteRoot_PrintsUsageForUsageError(t *testing.T) {
	root := &cobra.Command{Use: "sandman", Short: "test", SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, args []string) error {
		return sandmancmd.MarkUsage(errors.New("bad input"))
	}}
	var stderr bytes.Buffer
	calls := 0
	exit := func(code int) { calls = code }
	executeRoot(root, &stderr, exit)
	if calls != 1 {
		t.Errorf("exit code = %d, want 1", calls)
	}
	if !strings.Contains(stderr.String(), "Error:") {
		t.Errorf("expected 'Error:' in stderr, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "bad input") {
		t.Errorf("expected 'bad input' in stderr, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("expected usage in stderr, got %q", stderr.String())
	}
}

func TestExecuteRoot_PrintsSubcommandUsage(t *testing.T) {
	root := &cobra.Command{Use: "sandman", Short: "root", SilenceUsage: true, SilenceErrors: true}
	sub := &cobra.Command{
		Use:   "thing",
		Short: "sub",
		RunE: func(cmd *cobra.Command, args []string) error {
			return sandmancmd.MarkUsage(errors.New("bad arg"))
		},
	}
	root.AddCommand(sub)
	var stderr bytes.Buffer
	calls := 0
	exit := func(code int) { calls = code }
	root.SetArgs([]string{"thing"})
	executeRoot(root, &stderr, exit)
	if calls != 1 {
		t.Errorf("exit code = %d, want 1", calls)
	}
	if !strings.Contains(stderr.String(), "thing") {
		t.Errorf("expected subcommand usage mentioning 'thing', got %q", stderr.String())
	}
}

func TestExecuteRoot_ExitCodeForExitCodedError(t *testing.T) {
	root := &cobra.Command{Use: "sandman", SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, args []string) error {
		return &sandmancmd.ExitCodedError{Code: 130, Msg: "aborted"}
	}}
	var stderr bytes.Buffer
	calls := 0
	exit := func(code int) { calls = code }
	executeRoot(root, &stderr, exit)
	if calls != 130 {
		t.Errorf("exit code = %d, want 130", calls)
	}
}

func TestExecuteRoot_HelpDoesNotPrintUsageError(t *testing.T) {
	root := &cobra.Command{Use: "sandman", Short: "test", SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		io.WriteString(cmd.OutOrStderr(), "HELP OUTPUT")
	})
	var stderr bytes.Buffer
	calls := 0
	exit := func(code int) { calls = code }
	root.SetArgs([]string{"--help"})
	executeRoot(root, &stderr, exit)
	if calls != 0 {
		t.Errorf("--help should not exit non-zero, got %d", calls)
	}
	if strings.Contains(stderr.String(), "Error:") {
		t.Errorf("--help should not print 'Error:', got %q", stderr.String())
	}
}
