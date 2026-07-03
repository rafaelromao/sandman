//go:build !linux

package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestShouldFallbackToAbstractSocket_AlwaysFalseOnNonLinux(t *testing.T) {
	longPath := "/" + strings.Repeat("a", 108)
	einvalErr := &net.OpError{
		Op:  "listen",
		Net: "unix",
		Err: &os.SyscallError{Syscall: "bind", Err: syscall.EINVAL},
	}
	if shouldFallbackToAbstractSocket(longPath, einvalErr) {
		t.Fatalf("expected shouldFallbackToAbstractSocket=false on non-Linux for long path with EINVAL-shaped error")
	}
}

func TestCommandServer_StartReturnsPlatformSpecificErrorOnLongPath(t *testing.T) {
	dir := t.TempDir()
	for len(CommandSocketPath(dir)) <= 108 {
		dir = filepath.Join(dir, strings.Repeat("long-path-segment", 4))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir long dir: %v", err)
	}

	server := NewCommandServer(dir, &fakeCommander{})
	err := server.Start()
	if err == nil {
		server.Stop()
		t.Fatalf("expected Start to fail on non-Linux with a long path; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, runtime.GOOS) {
		t.Errorf("expected error to name the host platform %q, got %q", runtime.GOOS, msg)
	}
	if !strings.Contains(msg, "107") {
		t.Errorf("expected error to mention the 107-byte sun_path limit, got %q", msg)
	}
	if !strings.Contains(msg, "shorten the repo path") {
		t.Errorf("expected error to advise shortening the repo path, got %q", msg)
	}
	if errors.Is(err, syscall.EINVAL) {
		t.Errorf("expected wrapped error to be a custom message, not just syscall.EINVAL: %q", msg)
	}
}
