package opencode

import (
	"fmt"
	"testing"
)

func TestVersionProbe_DefaultShellsOut(t *testing.T) {
	// Save and restore the production seam.
	prev := VersionProbe
	defer func() { VersionProbe = prev }()

	VersionProbe = func() (string, error) {
		return "1.17.19", nil
	}

	got, err := VersionProbe()
	if err != nil {
		t.Fatalf("unexpected error from stubbed probe: %v", err)
	}
	if got != "1.17.19" {
		t.Errorf("expected %q, got %q", "1.17.19", got)
	}
}

func TestVersionProbe_ReturnsEmptyOnError(t *testing.T) {
	prev := VersionProbe
	defer func() { VersionProbe = prev }()

	VersionProbe = func() (string, error) {
		return "", fmt.Errorf("exec: no such file")
	}

	got, err := VersionProbe()
	if err == nil {
		t.Errorf("expected error, got nil with version %q", got)
	}
	if got != "" {
		t.Errorf("expected empty version on error, got %q", got)
	}
}

func TestVersionProbe_StubReturnsEmpty(t *testing.T) {
	prev := VersionProbe
	defer func() { VersionProbe = prev }()

	VersionProbe = func() (string, error) {
		return "", nil
	}

	got, err := VersionProbe()
	if err != nil {
		t.Fatalf("unexpected error from stubbed probe: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for absent opencode, got %q", got)
	}
}
