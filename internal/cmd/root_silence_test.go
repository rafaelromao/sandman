package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRoot_SilencesUsageAndErrors(t *testing.T) {
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)

	if !rootCmd.SilenceUsage {
		t.Error("root.SilenceUsage must be true so cobra does not auto-print usage on errors")
	}
	if !rootCmd.SilenceErrors {
		t.Error("root.SilenceErrors must be true so cobra does not auto-print errors")
	}
}

func TestRoot_ExecutionDoesNotPrintUsageOnRuntimeError(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"run", "--prompt", "hi"})

	runCmd, _, _ := rootCmd.Find([]string{"run"})
	runCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return errors.New("github API down")
	}

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected runtime error")
	}
	if strings.Contains(buf.String(), "Usage:") || strings.Contains(buf.String(), "Available Commands:") {
		t.Errorf("runtime error should not print usage, got:\n%s", buf.String())
	}
}

func TestRoot_FlagErrorFuncWrapsWithUsageError(t *testing.T) {
	var buf bytes.Buffer
	deps := newTestDeps()
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"run", "--invalid-flag"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error from unknown flag")
	}
	var target *UsageError
	if !errors.As(err, &target) {
		t.Fatalf("expected *UsageError, got %T: %v", err, err)
	}
}
