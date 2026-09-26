package cmd

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/daemon"
)

func readAttachHandshake(conn net.Conn) error {
	var handshake [1]byte
	if _, err := io.ReadFull(conn, handshake[:]); err != nil {
		return err
	}
	if handshake[0] != daemon.AttachStreamHandshake {
		return fmt.Errorf("attach handshake = %d, want %d", handshake[0], daemon.AttachStreamHandshake)
	}
	return nil
}

func TestAttach_NoDaemonReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var buf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no daemon is running")
	}
	if !strings.Contains(err.Error(), "no sandman daemon is running") {
		t.Fatalf("expected 'no sandman daemon is running', got: %v", err)
	}
}

func TestAttach_ReadsFromSocket(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	sockDir := filepath.Join(dir, ".sandman", "batches", "test-batch-1")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if err := readAttachHandshake(conn); err != nil {
			conn.Close()
			return
		}
		conn.Write([]byte("hello from daemon"))
		conn.Close()
	}()

	var buf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.String() != "hello from daemon" {
		t.Fatalf("expected 'hello from daemon', got: %q", buf.String())
	}
}

func TestAttach_ExitsOnEOF(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	sockDir := filepath.Join(dir, ".sandman", "batches", "test-batch-2")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if err := readAttachHandshake(conn); err != nil {
			conn.Close()
			return
		}
		conn.Close()
	}()

	var buf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error on EOF: %v", err)
	}
}
