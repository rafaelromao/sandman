package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRoot_VersionFlagPrintsSandmanPrefixedVersion(t *testing.T) {
	deps := newTestDeps(t)
	deps.Version = func() string { return "v1.0.0" }

	var buf bytes.Buffer
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"--version"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "sandman v1.0.0") {
		t.Errorf("expected output to contain 'sandman v1.0.0', got %q", got)
	}
	if strings.Contains(got, " version ") {
		t.Errorf("expected cobra default 'version' keyword to be absent, got %q", got)
	}
}

func TestRoot_VersionSubcommandPrintsSandmanPrefixedVersion(t *testing.T) {
	deps := newTestDeps(t)
	deps.Version = func() string { return "v1.0.0" }

	var buf bytes.Buffer
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"version"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "sandman v1.0.0") {
		t.Errorf("expected output to contain 'sandman v1.0.0', got %q", got)
	}
}

func TestRoot_VersionFlagFallsThroughToDevWhenGetterReturnsDev(t *testing.T) {
	deps := newTestDeps(t)
	deps.Version = func() string { return "dev" }

	var buf bytes.Buffer
	rootCmd := NewRootCmd(deps)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"--version"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "sandman dev") {
		t.Errorf("expected output to contain 'sandman dev', got %q", buf.String())
	}
}

// Registration test: the version subcommand must be wired into the root
// command so `sandman version` resolves as a subcommand rather than
// falling through to the root RunE.
func TestRoot_VersionSubcommandRegistered(t *testing.T) {
	deps := newTestDeps(t)
	deps.Version = func() string { return "v1.0.0" }
	rootCmd := NewRootCmd(deps)

	var found *cobra.Command
	for _, sub := range rootCmd.Commands() {
		if sub.Name() == "version" {
			found = sub
			break
		}
	}
	if found == nil {
		t.Fatal("expected 'version' subcommand to be registered on the root command")
	}
}
