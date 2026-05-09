package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInit_CreatesSandmanFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("1\n"))

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "config.yaml")); err != nil {
		t.Errorf("config.yaml not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "Dockerfile")); err != nil {
		t.Errorf("Dockerfile not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "prompt.md")); err != nil {
		t.Errorf("prompt.md not created: %v", err)
	}
}

func TestInit_LangFlag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--lang", "go"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "FROM golang:latest") {
		t.Errorf("expected golang image, got:\n%s", data)
	}
}

func TestInit_FromImageFlag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--lang", "go", "--from-image", "custom:latest"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "FROM custom:latest") {
		t.Errorf("expected custom image, got:\n%s", data)
	}
}

func TestInit_ExistingDirectoryPrompts(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatalf("create .sandman: %v", err)
	}

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs([]string{"--lang", "go"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when declining overwrite")
	}
}
