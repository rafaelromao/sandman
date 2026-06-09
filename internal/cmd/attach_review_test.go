package cmd

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttach_FindsReviewSock(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sandmanDir, "review.sock")
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
		conn.Write([]byte("hello from review daemon"))
		conn.Close()
	}()

	var buf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.String() != "hello from review daemon" {
		t.Fatalf("expected review payload, got %q", buf.String())
	}
}

func TestAttach_PrefersReviewSockWhenBothExist(t *testing.T) {
	// When only the review.sock is live, the attach command should pick
	// it up rather than erroring on multiple sockets.
	dir := t.TempDir()
	t.Chdir(dir)

	sandmanDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0755); err != nil {
		t.Fatal(err)
	}
	reviewSock := filepath.Join(sandmanDir, "review.sock")
	listener, err := net.Listen("unix", reviewSock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		conn.Write([]byte("from review"))
		conn.Close()
	}()

	var buf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "from review") {
		t.Fatalf("expected review payload, got %q", buf.String())
	}
}

func TestAttach_NoSocketsReturnsError(t *testing.T) {
	dir := t.TempDir()
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
