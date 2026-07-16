package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCmd_PrintsSandmanPrefixedVersion(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewVersionCmd(func() string { return "v1.0.0" })
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := buf.String(), "sandman v1.0.0\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestVersionCmd_PrintsDevWhenGetterReturnsDev(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewVersionCmd(func() string { return "dev" })
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := buf.String(), "sandman dev\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestVersionCmd_PrintsBuildInfoPseudoVersionWhenInjected(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewVersionCmd(func() string { return "v0.0.0-20260716184825-0e018c21696d" })
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "v0.0.0-20260716184825-0e018c21696d") {
		t.Errorf("expected pseudo-version in output, got %q", buf.String())
	}
}
