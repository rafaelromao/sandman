package daemon

import (
	"net"
	"path/filepath"
	"testing"
)

func TestControlSocket_StartCreatesSocket(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocket(dir, NewBroadcaster())

	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	info, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err != nil {
		t.Fatalf("connect to socket: %v", err)
	}
	info.Close()
}

func TestControlSocket_StopsAcceptingAfterClose(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocket(dir, NewBroadcaster())

	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if err := sock.Stop(); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}

	_, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err == nil {
		t.Fatal("expected dial error after Stop()")
	}
}

func TestControlSocket_RemovesStaleSocketOnStart(t *testing.T) {
	dir := t.TempDir()
	oldSock := NewControlSocket(dir, NewBroadcaster())
	if err := oldSock.Start(); err != nil {
		t.Fatal(err)
	}
	oldSock.Stop()

	// Start again — should remove stale socket
	newSock := NewControlSocket(dir, NewBroadcaster())
	if err := newSock.Start(); err != nil {
		t.Fatalf("Start() with stale socket should succeed: %v", err)
	}
	defer newSock.Stop()

	conn, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err != nil {
		t.Fatalf("connect after restart: %v", err)
	}
	conn.Close()
}
