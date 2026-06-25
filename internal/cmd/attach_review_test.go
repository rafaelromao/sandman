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
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	sandmanDir := filepath.Join(dir, ".sandman")
	reviewsDir := filepath.Join(sandmanDir, "reviews")
	if err := os.MkdirAll(reviewsDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(reviewsDir, "review.sock")
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

func TestAttach_MultipleSocketsReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	sockDir1 := filepath.Join(dir, ".sandman", "batches", "test-batch-1")
	sockDir2 := filepath.Join(dir, ".sandman", "batches", "test-batch-2")
	if err := os.MkdirAll(sockDir1, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sockDir2, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath1 := filepath.Join(sockDir1, "batch.sock")
	sockPath2 := filepath.Join(sockDir2, "batch.sock")
	listener1, err := net.Listen("unix", sockPath1)
	if err != nil {
		t.Fatal(err)
	}
	defer listener1.Close()
	listener2, err := net.Listen("unix", sockPath2)
	if err != nil {
		t.Fatal(err)
	}
	defer listener2.Close()

	go func() {
		listener1.Accept()
	}()
	go func() {
		listener2.Accept()
	}()

	var buf bytes.Buffer
	cmd := NewAttachCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected error when multiple daemons are running")
	}
	if !strings.Contains(err.Error(), "multiple sandman daemons") {
		t.Fatalf("expected 'multiple sandman daemons' error, got: %v", err)
	}
}

func TestAttach_NoSocketsReturnsError(t *testing.T) {
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
