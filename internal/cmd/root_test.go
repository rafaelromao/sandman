package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpListsAllCommands(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"--help"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	commands := []string{"init", "run", "status", "history", "retry", "clean", "config"}
	for _, cmd := range commands {
		if !strings.Contains(out, cmd) {
			t.Errorf("help output missing command %q", cmd)
		}
	}
}

func TestInitPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"init"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "init is not yet implemented") {
		t.Errorf("init did not print placeholder message")
	}
}

func TestRunPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"run"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "run is not yet implemented") {
		t.Errorf("run did not print placeholder message")
	}
}

func TestStatusPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"status"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "status is not yet implemented") {
		t.Errorf("status did not print placeholder message")
	}
}

func TestHistoryPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"history"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "history is not yet implemented") {
		t.Errorf("history did not print placeholder message")
	}
}

func TestRetryPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"retry"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "retry is not yet implemented") {
		t.Errorf("retry did not print placeholder message")
	}
}

func TestCleanPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"clean"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "clean is not yet implemented") {
		t.Errorf("clean did not print placeholder message")
	}
}

func TestConfigPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"config"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "config is not yet implemented") {
		t.Errorf("config did not print placeholder message")
	}
}
