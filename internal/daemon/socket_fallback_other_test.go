//go:build !linux

package daemon

import (
	"net"
	"os"
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
	dir := longCommandSocketDir(t)

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
	if !strings.Contains(msg, "sun_path limit") {
		t.Errorf("expected error to mention the sun_path limit, got %q", msg)
	}
	if !strings.Contains(msg, "shorten the repo path") {
		t.Errorf("expected error to advise shortening the repo path, got %q", msg)
	}
}
