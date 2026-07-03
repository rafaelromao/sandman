//go:build darwin

package cmd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolvePortalPeerPIDReturnsCallerPID(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "run.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	connCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		connCh <- c
	}()

	dialer, err := net.DialTimeout("unix", sockPath, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer dialer.Close()

	select {
	case <-connCh:
	case err := <-errCh:
		t.Fatalf("accept: %v", err)
	}

	got, err := resolvePortalPeerPID(sockPath)
	if err != nil {
		t.Fatalf("resolvePortalPeerPID: %v", err)
	}
	if got != os.Getpid() {
		t.Fatalf("peer pid = %d, want %d", got, os.Getpid())
	}
}

func TestPortalAbortSupportedOnDarwin(t *testing.T) {
	if !portalAbortSupported() {
		t.Fatalf("portalAbortSupported() = false on darwin, want true")
	}
}
