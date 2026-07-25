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
