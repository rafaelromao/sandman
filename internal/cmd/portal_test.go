package cmd

import (
	"bytes"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

func TestPortal_LiveOutputReturnsTailForLongStream(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(repoRoot, ".sandman", "runs", "run-1-1")
	sockPath := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	prefix := "[issue-1] 12:00:00 output\n"
	largeData := make([]byte, portalReadLimit+50)
	copy(largeData, []byte(prefix))
	for i := len(prefix); i < len(largeData); i++ {
		largeData[i] = 'A'
	}
	suffix := "\n[issue-1] 12:59:59 final output\n"

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write(largeData)
		time.Sleep(200 * time.Millisecond)
		_, _ = conn.Write([]byte(suffix))
	}()

	time.Sleep(50 * time.Millisecond)
	output := (&portalRunsView{}).readPortalSocketOutput(sockPath)

	if len(output) != portalReadLimit {
		t.Fatalf("expected output length %d, got %d", portalReadLimit, len(output))
	}

	if !hasSuffix(output, suffix) {
		t.Fatalf("expected output to end with suffix %q, got last 50 chars: %q", suffix, output[len(output)-50:])
	}

	if hasPrefix(output, prefix) {
		t.Fatalf("expected output NOT to start with prefix %q, got %q", prefix, output)
	}
}

func TestPortal_IsSocketAliveReturnsTrueWhenSocketListening(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if !(&portalRunsView{}).isSocketAlive(sockPath) {
		t.Fatal("expected isSocketAlive to return true for listening socket")
	}
}

func TestPortal_IsSocketAliveReturnsFalseWhenSocketMissing(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "nonexistent.sock")

	if (&portalRunsView{}).isSocketAlive(sockPath) {
		t.Fatal("expected isSocketAlive to return false for missing socket")
	}
}

func TestPortal_IsSocketAliveReturnsFalseWhenSocketDead(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "dead.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	if (&portalRunsView{}).isSocketAlive(sockPath) {
		t.Fatal("expected isSocketAlive to return false for dead socket")
	}
}

func TestPortal_IsSocketAliveReturnsFalseForEmptyPath(t *testing.T) {
	if (&portalRunsView{}).isSocketAlive("") {
		t.Fatal("expected isSocketAlive to return false for empty path")
	}
}

func TestPortal_RunFromActiveBatchIssueSetsCompletedWhenSocketDead(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "run.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	active := portalActiveRun{
		Key:          "run-42-1",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{42},
		StartedAt:    time.Now().Add(-1 * time.Minute),
	}

	started := time.Now().Add(-1 * time.Minute)
	state := &events.RunState{
		RunID: "run-42-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, state, nil, "", nil)

	if run.Kind != "completed" {
		t.Fatalf("expected kind 'completed' for run with dead socket, got %q", run.Kind)
	}
}

func TestPortal_RunFromActiveMatchSetsCompletedWhenSocketDead(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "run.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	match := portalRunMatch{
		instance: portalActiveRun{
			Key:         "run-prompt-1",
			SocketPath:  sockPath,
			IssueNumber: 0,
			ModTime:     time.Now().Add(-1 * time.Minute),
		},
	}

	run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, nil)

	if run.Kind != "completed" {
		t.Fatalf("expected kind 'completed' for match with dead socket, got %q", run.Kind)
	}
}

func TestPortal_RunFromStateSetsCompletedWhenActiveButSocketDead(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "run.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	started := time.Now().Add(-1 * time.Minute)
	runState := events.RunState{
		RunID: "run-active-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	active := &portalActiveRun{
		Key:        "run-active-1",
		SocketPath: sockPath,
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, active, nil)

	if run.Kind != "completed" {
		t.Fatalf("expected kind 'completed' for active run with dead socket, got %q", run.Kind)
	}
}

func TestPortal_RunFromActiveBatchIssueKeepsActiveWhenSocketAlive(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "run.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	active := portalActiveRun{
		Key:          "run-42-1",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{42},
		StartedAt:    time.Now().Add(-1 * time.Minute),
	}

	started := time.Now().Add(-1 * time.Minute)
	state := &events.RunState{
		RunID: "run-42-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, state, nil, "", nil)

	if run.Kind != "active" {
		t.Fatalf("expected kind 'active' for run with live socket, got %q", run.Kind)
	}
}

func TestPortal_RunFromActiveMatchKeepsActiveWhenSocketAlive(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "run.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	match := portalRunMatch{
		instance: portalActiveRun{
			Key:         "run-prompt-1",
			SocketPath:  sockPath,
			IssueNumber: 0,
			ModTime:     time.Now().Add(-1 * time.Minute),
		},
	}

	run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, nil)

	if run.Kind != "active" {
		t.Fatalf("expected kind 'active' for match with live socket, got %q", run.Kind)
	}
}

func TestPortal_RunFromStateKeepsActiveWhenSocketAlive(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "run.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	started := time.Now().Add(-1 * time.Minute)
	runState := events.RunState{
		RunID: "run-active-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	active := &portalActiveRun{
		Key:        "run-active-1",
		SocketPath: sockPath,
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, active, nil)

	if run.Kind != "active" {
		t.Fatalf("expected kind 'active' for active run with live socket, got %q", run.Kind)
	}
}

func TestPortal_RunFromStateSetsCompletedWhenUnmatchedActiveHasDeadSocket(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "run.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	runsDir := filepath.Join(repoRoot, ".sandman", "runs", "run-gone-1")
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sockPath, filepath.Join(runsDir, "run.sock")); err != nil {
		t.Fatal(err)
	}

	started := time.Now().Add(-1 * time.Minute)
	runState := events.RunState{
		RunID: "run-gone-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil)

	if run.Kind != "completed" {
		t.Fatalf("expected kind 'completed' for unmatched active state with dead socket, got %q", run.Kind)
	}
}

func TestPortal_DefaultPortFlag(t *testing.T) {
	cmd := NewPortalCmd(Dependencies{})
	port, err := cmd.Flags().GetInt("port")
	if err != nil {
		t.Fatalf("get port flag: %v", err)
	}
	if port != 5000 {
		t.Fatalf("expected default port 5000, got %d", port)
	}
}

func TestPortalStaleCleaner_MessageSuppressedWhenNoRecoveredRuns(t *testing.T) {
	prev := portalRunCleanStale
	t.Cleanup(func() { portalRunCleanStale = prev })

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	t.Run("recovered>0 prints message", func(t *testing.T) {
		buf.Reset()
		portalRunCleanStale = func(_ []events.Event, _ events.EventLog) (int, int, error) {
			return 1, 0, nil
		}
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := portalStaleCleaner(repoRoot); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), "portal: recovered") {
			t.Error("expected log message when recovered > 0")
		}
	})

	t.Run("recovered==0, deadDirs>0 suppresses message", func(t *testing.T) {
		buf.Reset()
		portalRunCleanStale = func(_ []events.Event, _ events.EventLog) (int, int, error) {
			return 0, 1, nil
		}
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := portalStaleCleaner(repoRoot); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "portal: recovered") {
			t.Error("expected no log message when recovered == 0 even if deadDirs > 0")
		}
	})

	t.Run("both zero suppresses message", func(t *testing.T) {
		buf.Reset()
		portalRunCleanStale = func(_ []events.Event, _ events.EventLog) (int, int, error) {
			return 0, 0, nil
		}
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := portalStaleCleaner(repoRoot); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "portal: recovered") {
			t.Error("expected no log message when both are zero")
		}
	})
}

func TestPortal_RunFromActiveMatchReturnsReviewingForPRInstance(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, ".sandman", "runs", "PR42")
	sockPath := filepath.Join(sockDir, "run.sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	match := portalRunMatch{
		instance: portalActiveRun{
			Key:        "PR42",
			SocketPath: sockPath,
			PRNumber:   42,
			ModTime:    time.Now().Add(-1 * time.Minute),
		},
	}

	run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, nil)

	if run.Status != "reviewing" {
		t.Fatalf("expected status 'reviewing' for PR instance, got %q", run.Status)
	}
	if !run.Review {
		t.Fatal("expected Review=true for PR instance")
	}
	if run.PRNumber != 42 {
		t.Fatalf("expected PRNumber=42, got %d", run.PRNumber)
	}
	if run.IssueLabel != "PR42" {
		t.Fatalf("expected IssueLabel 'PR42', got %q", run.IssueLabel)
	}
	if run.Kind != "active" {
		t.Fatalf("expected kind 'active' for PR instance with live socket, got %q", run.Kind)
	}
}

func TestPortal_ParseRunDirPR(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantNum   int
		wantMatch bool
	}{
		{"PR42", "PR42", 42, true},
		{"existing run-issue format", "run-42-123", 0, false},
		{"PR without number", "PR", 0, false},
		{"PR with non-numeric suffix", "PR123abc", 0, false},
		{"plain number", "42", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			num, ok := (&portalRunsView{}).parseRunDirPR(tt.input)
			if ok != tt.wantMatch {
				t.Errorf("parseRunDirPR(%q) match = %v, want %v", tt.input, ok, tt.wantMatch)
			}
			if num != tt.wantNum {
				t.Errorf("parseRunDirPR(%q) num = %d, want %d", tt.input, num, tt.wantNum)
			}
		})
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
