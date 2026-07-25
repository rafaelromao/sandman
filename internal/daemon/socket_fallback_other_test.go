//go:build !linux

package daemon

import (
	"net"
	"os"
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

func TestCommandServer_StartUsesShortFilesystemSocketOnLongPath(t *testing.T) {
	dir := longCommandSocketDir(t)

	server := NewCommandServer(dir, &fakeCommander{})
	err := server.Start()
	if err != nil {
		t.Fatalf("expected long path to use a short filesystem socket: %v", err)
	}
	defer server.Stop()
	conn, err := net.Dial("unix", CommandSocketPath(dir))
	if err != nil {
		t.Fatalf("dial logical socket path: %v", err)
	}
	conn.Close()
}

func TestControlSocket_StartUsesShortFilesystemSocketOnLongPath(t *testing.T) {
	dir := longCommandSocketDir(t)
	sock := NewControlSocket(dir, NewBroadcaster())
	if err := sock.Start(); err != nil {
		t.Fatalf("expected long path to use a short filesystem socket: %v", err)
	}
	defer sock.Stop()
	conn, err := net.Dial("unix", sock.Path())
	if err != nil {
		t.Fatalf("dial logical socket path: %v", err)
	}
	conn.Close()
}
