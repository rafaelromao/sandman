package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestInstallScriptIsPortableAndVerifiesDownloads(t *testing.T) {
	script := readRepositoryFile(t, "install.sh")
	for _, required := range []string{
		"#!/bin/sh",
		"--version",
		"--install-dir",
		"--include-prerelease",
		"SANDMAN_VERSION",
		"SANDMAN_INSTALL_DIR",
		"Linux) OS=linux",
		"Darwin) OS=darwin",
		"amd64|x86_64) ARCH=amd64",
		"arm64|aarch64) ARCH=arm64",
		"checksums.txt",
		"sha256sum -c checksum-entry",
		"shasum -a 256 -c checksum-entry",
		"install -m 755",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("installer missing %q", required)
		}
	}
}

func TestInstallScriptHasValidShellSyntax(t *testing.T) {
	if err := exec.Command("sh", "-n", "install.sh").Run(); err != nil {
		t.Fatalf("validate install.sh syntax: %v", err)
	}
}

func TestInstallScriptReportsSupportedPlatformsOnUnsupportedSystem(t *testing.T) {
	script := readRepositoryFile(t, "install.sh")

	supported := []string{
		"linux/amd64",
		"linux/arm64",
		"darwin/amd64",
		"darwin/arm64",
	}

	osFail := unsupportedOSFailLine(script)
	if osFail == "" {
		t.Fatal("installer must define an unsupported-OS failure branch (expected `*) fail \"unsupported operating system:`)")
	}
	for _, platform := range supported {
		if !strings.Contains(osFail, platform) {
			t.Errorf("unsupported-OS failure message must list %q; got %q", platform, osFail)
		}
	}

	archFail := unsupportedArchFailLine(script)
	if archFail == "" {
		t.Fatal("installer must define an unsupported-architecture failure branch (expected `*) fail \"unsupported architecture:`)")
	}
	for _, platform := range supported {
		if !strings.Contains(archFail, platform) {
			t.Errorf("unsupported-architecture failure message must list %q; got %q", platform, archFail)
		}
	}
}

func unsupportedOSFailLine(script string) string {
	return failLineAfter(script, "unsupported operating system")
}

func unsupportedArchFailLine(script string) string {
	return failLineAfter(script, "unsupported architecture")
}

func failLineAfter(script, marker string) string {
	idx := strings.Index(script, marker)
	if idx == -1 {
		return ""
	}
	prefix := script[:idx]
	lineStart := strings.LastIndexByte(prefix, '\n') + 1
	end := strings.IndexByte(script[idx:], '\n')
	if end == -1 {
		return script[lineStart:]
	}
	return script[lineStart : idx+end]
}
