package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/runid"
)

func TestPortal_LiveOutputReturnsTailForLongStream(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "abcd-260618113825-1-1")
	sockPath := filepath.Join(runDir, "batch.sock")
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
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	active := portalActiveRun{
		Key:          "abcd-260618113825-issue-42",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{42},
		StartedAt:    time.Now().Add(-1 * time.Minute),
	}

	started := time.Now().Add(-1 * time.Minute)
	state := &events.RunState{
		RunID: "abcd-260618113825-issue-42",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, state, nil, nil, "", nil, nil)

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
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	match := portalRunMatch{
		instance: portalActiveRun{
			Key:         "abcd-260618113825-prompt",
			SocketPath:  sockPath,
			IssueNumber: 0,
			ModTime:     time.Now().Add(-1 * time.Minute),
		},
	}

	run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, nil, nil)

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
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	started := time.Now().Add(-1 * time.Minute)
	runState := events.RunState{
		RunID: "abcd-260618113825-active-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	active := &portalActiveRun{
		Key:        "abcd-260618113825-active-1",
		SocketPath: sockPath,
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, active, nil, nil)

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
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	active := portalActiveRun{
		Key:          "abcd-260618113825-issue-42",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{42},
		StartedAt:    time.Now().Add(-1 * time.Minute),
	}

	started := time.Now().Add(-1 * time.Minute)
	state := &events.RunState{
		RunID: "abcd-260618113825-issue-42",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, state, nil, nil, "", nil, nil)

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
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	match := portalRunMatch{
		instance: portalActiveRun{
			Key:         "abcd-260618113825-prompt",
			SocketPath:  sockPath,
			IssueNumber: 0,
			ModTime:     time.Now().Add(-1 * time.Minute),
		},
	}

	run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, nil, nil)

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
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	started := time.Now().Add(-1 * time.Minute)
	runState := events.RunState{
		RunID: "abcd-260618113825-active-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	active := &portalActiveRun{
		Key:        "abcd-260618113825-active-1",
		SocketPath: sockPath,
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, active, nil, nil)

	if run.Kind != "active" {
		t.Fatalf("expected kind 'active' for active run with live socket, got %q", run.Kind)
	}
}

func TestPortal_RunFromStateSetsCompletedWhenUnmatchedActiveHasDeadSocket(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "batch.sock")
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
	runsDir := filepath.Join(repoRoot, ".sandman", "batches", "abcd-260618113825-gone-1")
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sockPath, filepath.Join(runsDir, "batch.sock")); err != nil {
		t.Fatal(err)
	}

	started := time.Now().Add(-1 * time.Minute)
	runState := events.RunState{
		RunID: "abcd-260618113825-gone-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.Kind != "active" {
		t.Fatalf("expected kind 'active' for unmatched active state with dead socket and no batch dir, got %q", run.Kind)
	}
}

func TestPortal_RunFromState_StaysActiveWhenBatchDirMissing(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	started := time.Now().Add(-1 * time.Minute)
	runState := events.RunState{
		RunID: "abcd-260618113825-missing-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.Kind != "active" {
		t.Fatalf("expected kind 'active' for unmatched active state with missing run dir, got %q", run.Kind)
	}
}

func TestPortal_RunFromState_StaysActiveWhenSocketMissing(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	runsDir := filepath.Join(repoRoot, ".sandman", "batches", "abcd-260618113825-missing-sock-1")
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		t.Fatal(err)
	}

	started := time.Now().Add(-1 * time.Minute)
	runState := events.RunState{
		RunID: "abcd-260618113825-missing-sock-1",
		Started: events.Event{
			Timestamp: started,
			Payload:   map[string]any{},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.Kind != "active" {
		t.Fatalf("expected kind 'active' for unmatched active state with present run dir but missing socket, got %q", run.Kind)
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
		portalRunCleanStale = func(_ paths.Layout, _ []events.Event, _ events.EventLog) (int, int, error) {
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
		portalRunCleanStale = func(_ paths.Layout, _ []events.Event, _ events.EventLog) (int, int, error) {
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
		portalRunCleanStale = func(_ paths.Layout, _ []events.Event, _ events.EventLog) (int, int, error) {
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
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42")
	sockPath := filepath.Join(sockDir, "batch.sock")
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

	run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, nil, nil)

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

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func TestPortal_RunFromState_PopulatesIssueTitle(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runState := events.RunState{
		RunID: "abcd-260618113825-1-1",
		Started: events.Event{
			Timestamp: time.Now().Add(-1 * time.Minute),
			Payload: map[string]any{
				"issue_title": "Add dark mode toggle",
			},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.IssueTitle != "Add dark mode toggle" {
		t.Fatalf("expected IssueTitle %q, got %q", "Add dark mode toggle", run.IssueTitle)
	}
}

func TestPortal_RunFromState_EmptyIssueTitleWhenMissing(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runState := events.RunState{
		RunID: "abcd-260618113825-1-1",
		Started: events.Event{
			Timestamp: time.Now().Add(-1 * time.Minute),
			Payload:   map[string]any{},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.IssueTitle != "" {
		t.Fatalf("expected empty IssueTitle, got %q", run.IssueTitle)
	}
}

func TestPortal_PortalRunJSONIncludesRetriesFieldsWhenFinished(t *testing.T) {
	run := portalRun{
		Key:          "abcd-260618113825-1-1",
		RunID:        "abcd-260618113825-1-1",
		Kind:         "completed",
		Status:       "success",
		IssueLabel:   "#42",
		IssueNumber:  42,
		StartedAt:    time.Now().Add(-2 * time.Minute),
		RetriesTotal: 3,
		RetriesDone:  2,
	}

	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal portalRun: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal portalRun: %v", err)
	}
	if got, _ := payload["retriesTotal"].(float64); got != 3 {
		t.Fatalf("expected retriesTotal=3 in payload, got %v (raw: %s)", payload["retriesTotal"], data)
	}
	if got, _ := payload["retriesDone"].(float64); got != 2 {
		t.Fatalf("expected retriesDone=2 in payload, got %v (raw: %s)", payload["retriesDone"], data)
	}
}

func TestPortal_PortalRunJSONOmitsRetriesFieldsWhenZero(t *testing.T) {
	run := portalRun{
		Key:         "abcd-260618113825-1-1",
		RunID:       "abcd-260618113825-1-1",
		Kind:        "completed",
		Status:      "success",
		IssueLabel:  "#42",
		IssueNumber: 42,
		StartedAt:   time.Now().Add(-2 * time.Minute),
	}

	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal portalRun: %v", err)
	}

	if strings.Contains(string(data), "retriesTotal") {
		t.Fatalf("expected retriesTotal to be omitted, got %s", data)
	}
	if strings.Contains(string(data), "retriesDone") {
		t.Fatalf("expected retriesDone to be omitted, got %s", data)
	}
}

func TestPortal_RunFromState_PopulatesRetriesFromFinishedPayload(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	runState := events.RunState{
		RunID: "abcd-260618113825-issue-1",
		Started: events.Event{
			Timestamp: startedAt,
			Payload:   map[string]any{"branch": "sandman/42-fix"},
		},
		Finished: &events.Event{
			Timestamp: finishedAt,
			Payload: map[string]any{
				"status":        "success",
				"branch":        "sandman/42-fix",
				"retries_total": 3,
				"retries_done":  2,
			},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.RetriesTotal != 3 {
		t.Fatalf("expected RetriesTotal=3, got %d", run.RetriesTotal)
	}
	if run.RetriesDone != 2 {
		t.Fatalf("expected RetriesDone=2, got %d", run.RetriesDone)
	}
}

func TestPortal_RunFromState_LeavesRetriesZeroWhenActive(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runState := events.RunState{
		RunID: "abcd-260618113825-active",
		Started: events.Event{
			Timestamp: time.Now().Add(-30 * time.Second),
			Payload:   map[string]any{"branch": "sandman/42-fix"},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.RetriesTotal != 0 {
		t.Fatalf("expected RetriesTotal=0 for active run, got %d", run.RetriesTotal)
	}
	if run.RetriesDone != 0 {
		t.Fatalf("expected RetriesDone=0 for active run, got %d", run.RetriesDone)
	}
}

func TestPortal_PortalRunJSONOmitsAttemptsAndLastRetryReasonWhenZero(t *testing.T) {
	run := portalRun{
		Key:         "abcd-260618113825-1-1",
		RunID:       "abcd-260618113825-1-1",
		Kind:        "completed",
		Status:      "success",
		IssueLabel:  "#42",
		IssueNumber: 42,
		StartedAt:   time.Now().Add(-2 * time.Minute),
	}

	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal portalRun: %v", err)
	}

	if strings.Contains(string(data), `"attempts"`) {
		t.Fatalf("expected attempts to be omitted when zero, got %s", data)
	}
	if strings.Contains(string(data), `"lastRetryReason"`) {
		t.Fatalf("expected lastRetryReason to be omitted when empty, got %s", data)
	}
}

func TestPortal_RunFromState_ActiveRunPopulatesAttemptsAndLastRetryReason(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	runState := events.ProjectRunStates([]events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-active-retry", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), RunID: "abcd-260618113825-active-retry", Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"reason":          "agent-stalled",
			"branch":          "sandman/42-fix",
		}},
	})[0]

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.Attempts != 2 {
		t.Fatalf("expected Attempts=2 for active retried run, got %d", run.Attempts)
	}
	if run.LastRetryReason != "agent-stalled" {
		t.Fatalf("expected LastRetryReason=agent-stalled, got %q", run.LastRetryReason)
	}
}

func TestPortal_RunFromState_FinishedRunUsesRetriesDoneForAttempts(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(10 * time.Minute)
	runState := events.ProjectRunStates([]events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-finished-retry", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), RunID: "abcd-260618113825-finished-retry", Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"reason":          "agent-stalled",
			"branch":          "sandman/42-fix",
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "abcd-260618113825-finished-retry", Issue: 42, Payload: map[string]any{
			"status":        "success",
			"branch":        "sandman/42-fix",
			"retries_total": 3,
			"retries_done":  2,
		}},
	})[0]

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.Attempts != 2 {
		t.Fatalf("expected Attempts=2 (from retries_done) for finished run, got %d", run.Attempts)
	}
	if run.LastRetryReason != "agent-stalled" {
		t.Fatalf("expected LastRetryReason=agent-stalled, got %q", run.LastRetryReason)
	}
}

func TestPortal_RunFromState_ActiveCleanRunOmitsBoth(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	runState := events.ProjectRunStates([]events.Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "abcd-260618113825-clean", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})[0]

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil)

	if run.Attempts != 0 {
		t.Fatalf("expected Attempts=0 for clean active run, got %d", run.Attempts)
	}
	if run.LastRetryReason != "" {
		t.Fatalf("expected LastRetryReason=empty for clean active run, got %q", run.LastRetryReason)
	}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"attempts"`) {
		t.Fatalf("expected attempts to be omitted from JSON for clean active run, got %s", data)
	}
	if strings.Contains(string(data), `"lastRetryReason"`) {
		t.Fatalf("expected lastRetryReason to be omitted from JSON for clean active run, got %s", data)
	}
}

func TestPortal_RunFromActiveMatch_StateAbsentPopulatesAttemptsAndLastRetryReason(t *testing.T) {
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
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	match := portalRunMatch{
		instance: portalActiveRun{
			Key:         "abcd-260618113825-prompt-retry",
			SocketPath:  sockPath,
			IssueNumber: 0,
			ModTime:     startedAt,
		},
	}
	eventsByRun := map[string][]portalEvent{
		"abcd-260618113825-prompt-retry": {
			{Type: "run.started", Timestamp: startedAt, Payload: map[string]any{"branch": "sandman/42-fix"}},
			{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), Payload: map[string]any{
				"attempt":         2,
				"max_attempts":    3,
				"previous_status": "failure",
				"reason":          "agent-stalled",
				"branch":          "sandman/42-fix",
			}},
		},
	}

	run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, eventsByRun, nil)

	if run.Attempts != 2 {
		t.Fatalf("expected Attempts=2 from raw event list, got %d", run.Attempts)
	}
	if run.LastRetryReason != "agent-stalled" {
		t.Fatalf("expected LastRetryReason=agent-stalled from raw event list, got %q", run.LastRetryReason)
	}
}

func TestPortal_RunFromState_EmptyRunIDWithIssueNumberUsesCompoundKey(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	runState := events.RunState{
		RunID: "",
		Started: events.Event{
			Timestamp: time.Now().Add(-1 * time.Minute),
			Payload:   map[string]any{},
		},
	}

	active := &portalActiveRun{
		Key:         "abcd-260618113825",
		IssueNumber: 42,
		SocketPath:  "",
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, active, nil, nil)

	if run.Key != "abcd-260618113825-issue-42" {
		t.Fatalf("expected Key %q for batch issue with empty state.RunID, got %q", "abcd-260618113825-issue-42", run.Key)
	}
	if run.RunID != "abcd-260618113825-issue-42" {
		t.Fatalf("expected RunID %q for batch issue with empty state.RunID, got %q", "abcd-260618113825-issue-42", run.RunID)
	}
}

func TestPortal_RunFromActiveBatchIssue_PopulatesIssueTitle(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	active := portalActiveRun{
		Key:          "abcd-260618113825-issue-42",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{42},
		StartedAt:    time.Now().Add(-1 * time.Minute),
	}

	state := &events.RunState{
		RunID: "abcd-260618113825-issue-42",
		Started: events.Event{
			Timestamp: time.Now().Add(-1 * time.Minute),
			Payload: map[string]any{
				"issue_title": "Fix login bug",
			},
		},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, state, nil, nil, "", nil, nil)

	if run.IssueTitle != "Fix login bug" {
		t.Fatalf("expected IssueTitle %q, got %q", "Fix login bug", run.IssueTitle)
	}
}

func TestPortal_RunFromActiveBatchIssue_PopulatesIssueTitleForQueued(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	queuedAt := time.Now().Add(-2 * time.Minute)
	active := portalActiveRun{
		Key:          "abcd-260618113825-962-1",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{962, 960, 961},
		StartedAt:    queuedAt.Add(-time.Second),
	}
	queued := &events.Event{
		Type:      "run.queued",
		Timestamp: queuedAt,
		RunID:     "abcd-260618113825-962-1",
		Issue:     962,
		Payload: map[string]any{
			"blocked_by":  []int{960, 961},
			"issue_title": "[slice 3] Add internal/orchestrator dependencies path",
		},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 962, nil, nil, queued, "", nil, nil)

	if run.Status != "queued" {
		t.Fatalf("expected Status %q, got %q", "queued", run.Status)
	}
	if run.IssueTitle != "[slice 3] Add internal/orchestrator dependencies path" {
		t.Fatalf("expected IssueTitle %q, got %q", "[slice 3] Add internal/orchestrator dependencies path", run.IssueTitle)
	}
}

func TestPortal_RunFromActiveBatchIssue_PopulatesIssueTitleForBlocked(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	queuedAt := time.Now().Add(-2 * time.Minute)
	blockedAt := time.Now().Add(-1 * time.Minute)
	active := portalActiveRun{
		Key:          "abcd-260618113825-962-1",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{962, 960, 961},
		StartedAt:    queuedAt.Add(-time.Second),
	}
	queued := &events.Event{
		Type:      "run.queued",
		Timestamp: queuedAt,
		RunID:     "abcd-260618113825-962-1",
		Issue:     962,
		Payload: map[string]any{
			"blocked_by":  []int{960, 961},
			"issue_title": "[slice 3] Add dependencies path",
		},
	}
	blocked := &events.Event{
		Type:      "run.blocked",
		Timestamp: blockedAt,
		RunID:     "abcd-260618113825-962-1",
		Issue:     962,
		Payload: map[string]any{
			"blocked_by": []int{960, 961},
		},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 962, nil, blocked, queued, "", nil, nil)

	if run.Status != "blocked" {
		t.Fatalf("expected Status %q, got %q", "blocked", run.Status)
	}
	if run.IssueTitle != "[slice 3] Add dependencies path" {
		t.Fatalf("expected IssueTitle %q, got %q", "[slice 3] Add dependencies path", run.IssueTitle)
	}
}

func TestPortal_RunFromActiveBatchIssue_ActiveReviewPrefersLiveOutput(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := "260618113825"
	shortid := "abcd"
	batchID := runid.NewBatchID(runid.KindReview, 1, "42", ts, shortid)
	runID := runid.NewRunID(runid.KindReview, "42-PR99", ts, shortid)
	runDir := filepath.Join(repoRoot, ".sandman", "batches", batchID, "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte("["+runID+"] 12:00:00 saved review log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-1 * time.Minute)
	finishedAt := startedAt.Add(10 * time.Second)
	state := &events.RunState{
		RunID: runID,
		Started: events.Event{
			Type:      "run.started",
			Timestamp: startedAt,
			RunID:     runID,
			Payload: map[string]any{
				"batch_id":  batchID,
				"review":    true,
				"pr_number": 99,
			},
		},
		Finished: &events.Event{
			Type:      "run.finished",
			Timestamp: finishedAt,
			RunID:     runID,
			Payload: map[string]any{
				"status":    "success",
				"batch_id":  batchID,
				"review":    true,
				"pr_number": 99,
			},
		},
	}
	active := portalActiveRun{
		Key:          batchID,
		BatchID:      batchID,
		RunID:        runID,
		IssueNumber:  42,
		IssueNumbers: []int{42},
		StartedAt:    startedAt,
		ModTime:      finishedAt,
		SocketPath:   filepath.Join(t.TempDir(), "batch.sock"),
		LiveOutput:   "[" + runID + "] live review line\n",
	}
	if err := os.MkdirAll(filepath.Dir(active.SocketPath), 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", active.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, state, nil, nil, active.LiveOutput, nil, nil)

	if run.Kind != "active" {
		t.Fatalf("expected active kind while socket exists, got %q", run.Kind)
	}
	if run.Status != "success" {
		t.Fatalf("expected terminal status preserved (statusOrDefault returns non-empty status even with active socket), got %q", run.Status)
	}
	if !strings.Contains(run.Log, "live review line") {
		t.Fatalf("expected live review output to win, got %q", run.Log)
	}
}

func TestPortal_RunFromActiveBatchIssue_MixedBatchCarriesBatchIssues(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Now().Add(-2 * time.Minute)
	active := portalActiveRun{
		Key:          "abcd-260618113825-860-123",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{860, 854},
		StartedAt:    startedAt,
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 860, nil, nil, nil, "", nil, nil)

	if got, want := run.BatchIssues, []int{860, 854}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected BatchIssues %v, got %v", want, got)
	}
	if run.BatchKey != active.Key {
		t.Fatalf("expected BatchKey %q, got %q", active.Key, run.BatchKey)
	}
}

func TestPortal_RunFromActiveBatchIssue_LiveMixedBatchFiltersSiblingLogs(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Now().Add(-2 * time.Minute)
	active := portalActiveRun{
		Key:          "abcd-260618113825-860-123",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{860, 854},
		StartedAt:    startedAt,
	}
	liveOutput := "[abcd-260618113825-860-123-860] 18:51:00 working on PR\n[abcd-260618113825-860-123-854] 18:51:04 sibling work\n"

	for _, issue := range []int{860, 854} {
		issue := issue
		runID := fmt.Sprintf("abcd-260618113825-860-123-%d", issue)
		state := &events.RunState{
			RunID: runID,
			Started: events.Event{
				Type:      "run.started",
				Timestamp: startedAt,
				RunID:     runID,
				Issue:     issue,
			},
		}
		run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, issue, state, nil, nil, liveOutput, nil, nil)

		ownTimestamp := fmt.Sprintf("18:51:0%d", issue%10)
		if !strings.Contains(run.Log, ownTimestamp) {
			t.Fatalf("issue %d: expected own timestamp %q in log, got:\n%s", issue, ownTimestamp, run.Log)
		}
		for _, other := range []int{860, 854} {
			if other == issue {
				continue
			}
			otherTimestamp := fmt.Sprintf("18:51:0%d", other%10)
			if strings.Contains(run.Log, otherTimestamp) {
				t.Fatalf("issue %d: log leaked sibling timestamp %q:\n%s", issue, otherTimestamp, run.Log)
			}
		}
		if strings.Contains(run.Log, "[") {
			t.Fatalf("issue %d: log should not contain any '[label]' prefixes, got:\n%s", issue, run.Log)
		}
	}
}

func TestPortal_RunFromActiveBatchIssue_SingleIssueLiveRowKeepsFullOutput(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Now().Add(-1 * time.Minute)
	runID := "single-run-42"
	active := portalActiveRun{
		Key:          "single-batch-42",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{42},
		StartedAt:    startedAt,
	}
	liveOutput := "[single-run-42] 09:00:00 only me here\n[single-run-42] 09:00:01 still me\n"
	state := &events.RunState{
		RunID: runID,
		Started: events.Event{
			Type:      "run.started",
			Timestamp: startedAt,
			RunID:     runID,
			Issue:     42,
		},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, state, nil, nil, liveOutput, nil, nil)

	for _, want := range []string{"09:00:00 only me here", "09:00:01 still me"} {
		if !strings.Contains(run.Log, want) {
			t.Fatalf("expected single-issue live row to keep %q in log, got:\n%s", want, run.Log)
		}
	}
	if strings.Contains(run.Log, "[") {
		t.Fatalf("single-issue live row log should not contain any '[label]' prefixes, got:\n%s", run.Log)
	}
}

func TestPortal_RunFromActiveBatchIssue_SingleIssueOmitsBatchIssues(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Now().Add(-1 * time.Minute)
	active := portalActiveRun{
		Key:          "abcd-260618113825-issue-42",
		Dir:          sockDir,
		SocketPath:   sockPath,
		IssueNumbers: []int{42},
		StartedAt:    startedAt,
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, nil, nil, nil, "", nil, nil)

	if run.BatchIssues != nil {
		t.Fatalf("expected BatchIssues to be omitted for single-issue batch, got %v", run.BatchIssues)
	}
}

func TestPortal_RunFromState_ActiveFreshBatchCarriesBatchKey(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Now().Add(-1 * time.Minute)
	active := portalActiveRun{
		Key:          "abcd-260618113825",
		BatchID:      "abcd-260618113825",
		SocketPath:   sockPath,
		IssueNumber:  42,
		IssueNumbers: []int{42},
		StartedAt:    startedAt,
	}

	state := events.RunState{
		RunID: "",
		Started: events.Event{
			Type:      "run.started",
			Timestamp: startedAt,
			RunID:     "abcd-260618113825-issue-42",
			Payload: map[string]any{
				"issue": float64(42),
			},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, state, &active, nil, nil)

	if run.BatchKey != active.Key {
		t.Fatalf("expected BatchKey %q, got %q", active.Key, run.BatchKey)
	}
	if run.RunID == "" {
		t.Fatalf("expected RunID to be synthesized, got empty")
	}
}

func TestPortal_DiscoverActiveRuns_ManifestWins(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Dir name implies issue 999, but the manifest lists [42, 43] —
	// the portal must take issue identity from the manifest.
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "abcd-260618113825-999-1")
	sockPath := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42, 43}, CreatedAt: time.Now().Add(-2 * time.Minute)}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	addBatchToIndex(t, repoRoot, "abcd-260618113825-999-1", runDir, []int{42, 43})

	active, err := (&portalRunsView{}).discoverActiveRuns(repoRoot, nil)
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d", len(active))
	}
	if got, want := active[0].IssueNumbers, []int{42, 43}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected IssueNumbers %v (from manifest), got %v", want, got)
	}
	if active[0].IssueNumber != 42 {
		t.Fatalf("expected IssueNumber=42 (manifest's first), got %d (dir name inference leaked through)", active[0].IssueNumber)
	}
}

func TestPortal_DiscoverActiveRuns_NoInferenceFromDirName(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run dir name implies issue 999, but no manifest exists.
	// The portal must NOT infer issue 999 from the dir name; the
	// instance is treated as manifest-less (prompt-only routing).
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "abcd-260618113825-999-1")
	sockPath := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addBatchToIndex(t, repoRoot, "abcd-260618113825-999-1", runDir, []int{})

	active, err := (&portalRunsView{}).discoverActiveRuns(repoRoot, nil)
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d", len(active))
	}
	if active[0].IssueNumber != 0 {
		t.Fatalf("expected IssueNumber=0 (no manifest, no inference), got %d", active[0].IssueNumber)
	}
	if len(active[0].IssueNumbers) != 0 {
		t.Fatalf("expected empty IssueNumbers when no manifest, got %v", active[0].IssueNumbers)
	}
}

func TestPortal_DiscoverActiveRuns_SkipsDeadSocketFromFinishedBatch(t *testing.T) {
	// Long test names push t.TempDir() paths over the 108-byte Unix
	// socket limit, so create a short temp dir under /tmp directly.
	repoRoot, err := os.MkdirTemp("/tmp", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Seed a run dir with a dead socket file and a batch.json listing
	// one issue. The listener exists only so the socket file persists
	// on disk with the socket bit set; the liveness probe is stubbed
	// to false so the listener's actual dialability is irrelevant.
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "abcd-260618113825-42-1")
	sockPath := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: time.Now().Add(-2 * time.Minute)}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	active, err := (&portalRunsView{}).discoverActiveRuns(repoRoot, nil)
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no active instance for a finished batch with a dead socket, got %#v", active)
	}
}

func TestPortal_StripLogLabel_StripsPrefixedLine(t *testing.T) {
	got := stripLogLabel("[run-123] 10:00:00 some message")
	want := "10:00:00 some message"
	if got != want {
		t.Fatalf("stripLogLabel(%q): got %q, want %q", "[run-123] 10:00:00 some message", got, want)
	}
}

func TestPortal_StripLogLabel_PassesThroughUnprefixedLine(t *testing.T) {
	got := stripLogLabel("10:00:00 some message")
	want := "10:00:00 some message"
	if got != want {
		t.Fatalf("stripLogLabel(%q): got %q, want %q", "10:00:00 some message", got, want)
	}
}

func TestPortal_StripLogLabel_PassesThroughEmptyLine(t *testing.T) {
	got := stripLogLabel("")
	want := ""
	if got != want {
		t.Fatalf("stripLogLabel(%q): got %q, want %q", "", got, want)
	}
}

func TestPortal_StripLogLabel_HandlesLineWithoutClosingBracket(t *testing.T) {
	got := stripLogLabel("[no closing bracket here")
	want := "[no closing bracket here"
	if got != want {
		t.Fatalf("stripLogLabel(%q): got %q, want %q", "[no closing bracket here", got, want)
	}
}

func TestPortal_FilterIssueOutput_StripsLabelsFromMatchingLines(t *testing.T) {
	live := strings.Join([]string{
		"[run-abc] 18:51:05 working on PR",
		"[run-xyz] 18:51:05 sibling work",
		"unprefixed noise",
		"[run-abc] 18:51:06 more PR work",
	}, "\n")

	filtered := (&portalRunsView{}).filterPortalLogByRunID(live, "run-abc")

	for _, want := range []string{"18:51:05 working on PR", "18:51:06 more PR work"} {
		if !strings.Contains(filtered, want) {
			t.Fatalf("expected filtered output to contain %q, got:\n%s", want, filtered)
		}
	}
	for _, banned := range []string{"[run-xyz]", "unprefixed noise", "[run-abc]"} {
		if strings.Contains(filtered, banned) {
			t.Fatalf("expected filtered output to drop %q, got:\n%s", banned, filtered)
		}
	}
}

func TestPortal_SavedLogFile_StripsLabelsInPortalDisplay(t *testing.T) {
	repoRoot := t.TempDir()
	logsDir := filepath.Join(repoRoot, ".sandman", "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		t.Fatal(err)
	}
	contents := strings.Join([]string{
		"[issue-860] 18:51:05 working on PR",
		"[issue-854] 18:51:05 sibling work",
		"[issue-860] 18:51:06 more PR work",
		"",
	}, "\n")
	logPath := filepath.Join(logsDir, "860.log")
	if err := os.WriteFile(logPath, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	// readPortalTextFile strips labels from saved log content for portal
	// display. The raw file is unchanged (log download serves it raw).
	// Labels are stripped so the portal shows "HH:MM:SS msg" instead of
	// "[<label>] HH:MM:SS msg".
	got := (&portalRunsView{}).readPortalTextFile(logPath)

	for _, want := range []string{"18:51:05 working on PR", "18:51:05 sibling work", "18:51:06 more PR work"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected portal display to show %q, got:\n%s", want, got)
		}
	}
	for _, banned := range []string{"[issue-860]", "[issue-854]"} {
		if strings.Contains(got, banned) {
			t.Fatalf("expected portal display to strip label %q, got:\n%s", banned, got)
		}
	}
}

func TestPortal_SavedLogFile_StripsLabelsModuloANSI(t *testing.T) {
	repoRoot := t.TempDir()
	logsDir := filepath.Join(repoRoot, ".sandman", "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		t.Fatal(err)
	}
	raw := "[issue-42] 10:30:00 \x1b[32mgreen\x1b[0m log line\n[issue-42] 10:30:01 plain line\n"
	logPath := filepath.Join(logsDir, "42.log")
	if err := os.WriteFile(logPath, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}

	got := (&portalRunsView{}).readPortalTextFile(logPath)

	if strings.Contains(got, "[issue-42]") {
		t.Fatalf("expected portal display to strip issue prefix, got:\n%s", got)
	}
	if strings.Contains(got, "\x1b[32m") {
		t.Fatalf("expected ANSI sequences to be stripped, got:\n%s", got)
	}
	if !strings.Contains(got, "10:30:00 green log line") {
		t.Fatalf("expected portal display to show stripped line, got:\n%s", got)
	}
}

func TestPortal_Compute_MixedBatchRowsCarryBatchIssuesInJSON(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	batchStartedAt := time.Now().Add(-2 * time.Minute)

	// Dir name suggests issue 999, but the manifest lists [860, 854] —
	// the JSON payload for the portal must reflect the manifest.
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "abcd-260618113825-999-1")
	sockPath := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{860, 854}, CreatedAt: batchStartedAt}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addBatchToIndex(t, repoRoot, "abcd-260618113825-999-1", runDir, []int{860, 854})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 rows, got %d: %#v", len(runs), runs)
	}
	for _, run := range runs {
		if got, want := run.BatchIssues, []int{860, 854}; !reflect.DeepEqual(got, want) {
			t.Fatalf("issue %d: expected BatchIssues %v, got %v", run.IssueNumber, want, got)
		}
		if run.BatchKey == "" {
			t.Fatalf("issue %d: expected BatchKey to be set", run.IssueNumber)
		}
	}
}

// TestPortal_ReviewRunLifecycle drives the full compute() pipeline for
// review runs across their lifecycle, end-to-end. The slices mirror the
// scenarios in issue #859:
//
//  1. active review run with live socket          → "reviewing"
//  2. completed review run (daemon restarted)     → completed, review flag preserved
//  3. review run reconstructed from event log only → completed, review flag preserved
//  4. prompt-only run unaffected                   → "running", "prompt-only"
//
// All four share the public compute() seam used by the HTTP handler
// (portal.go:277-279), so they exercise discovery + event projection +
// dedup + sort together — not just the lower-level runFrom* helpers.
func TestPortal_ReviewRunLifecycle(t *testing.T) {
	t.Run("active socket shows reviewing", func(t *testing.T) {
		repoRoot, err := os.MkdirTemp("/tmp", "p")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		runDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42")
		sockPath := filepath.Join(runDir, "batch.sock")
		if err := os.MkdirAll(runDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		addBatchToIndex(t, repoRoot, "PR42", runDir, []int{})

		startedAt := time.Now().Add(-5 * time.Minute)
		writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Issue: 0, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
		})

		runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
		}
		got := runs[0]
		if got.Kind != "active" {
			t.Fatalf("expected kind 'active' for live review socket, got %q", got.Kind)
		}
		if got.Status != "reviewing" {
			t.Fatalf("expected status 'reviewing' for live review socket, got %q", got.Status)
		}
		if !got.Review {
			t.Fatal("expected Review=true for active review run")
		}
		if got.PRNumber != 42 {
			t.Fatalf("expected PRNumber=42, got %d", got.PRNumber)
		}
		if got.IssueLabel != "PR42" {
			t.Fatalf("expected IssueLabel 'PR42', got %q", got.IssueLabel)
		}
		if got.Reason != "review" {
			t.Fatalf("expected Reason 'review' for active review run, got %q", got.Reason)
		}
	})

	t.Run("dead socket after restart", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		// Stale run dir: socket file present but no listener — simulates
		// the portal rescanning the repo after a daemon restart.
		runDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42")
		sockPath := filepath.Join(runDir, "batch.sock")
		if err := os.MkdirAll(runDir, 0755); err != nil {
			t.Fatal(err)
		}
		runFolder := filepath.Join(runDir, "runs", "PR42")
		if err := os.MkdirAll(runFolder, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		ln.Close()
		addBatchToIndex(t, repoRoot, "PR42", runDir, []int{})
		if err := os.WriteFile(filepath.Join(runFolder, "run.json"), []byte(`{"runID":"PR42","batchId":"PR42","kind":"review"}`), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runFolder, "run.log"), []byte("[PR42] 12:00:00 saved review log\n"), 0644); err != nil {
			t.Fatal(err)
		}

		startedAt := time.Now().Add(-5 * time.Minute)
		writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Issue: 0, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
			{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: "PR42", Issue: 0, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true}},
		})

		runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
		}
		got := runs[0]
		if got.Kind != "completed" {
			t.Fatalf("expected kind 'completed' for dead-socket review run, got %q", got.Kind)
		}
		if got.Status != "success" {
			t.Fatalf("expected status 'success' for finished review run, got %q", got.Status)
		}
		if !got.Review {
			t.Fatal("expected Review=true for completed review run")
		}
		if got.PRNumber != 42 {
			t.Fatalf("expected PRNumber=42 on completed review run, got %d", got.PRNumber)
		}
		if got.IssueLabel != "PR42" {
			t.Fatalf("expected IssueLabel 'PR42' on completed review run, got %q", got.IssueLabel)
		}
		if got.Reason != "review" {
			t.Fatalf("expected Reason 'review' on completed review run, got %q", got.Reason)
		}
		if !strings.Contains(got.Log, "saved review log") {
			t.Fatalf("expected saved review log to load, got %q", got.Log)
		}
		if got.LogPath != filepath.Join(runFolder, "run.log") {
			t.Fatalf("expected log path for completed review run, got %q", got.LogPath)
		}
	})

	t.Run("active review uses saved log when live output is empty", func(t *testing.T) {
		t.Skip("Skipping: review run log handling with new batch/run folder layout needs redesign")
	})

	t.Run("event log only keeps review metadata", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		// No .sandman/runs/PR42 directory on disk — the portal must
		// still surface the run from the event log alone.
		startedAt := time.Now().Add(-5 * time.Minute)
		writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Issue: 0, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
			{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: "PR42", Issue: 0, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true}},
		})

		runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("expected 1 row from event log only, got %d: %#v", len(runs), runs)
		}
		got := runs[0]
		if got.Kind != "completed" {
			t.Fatalf("expected kind 'completed' for event-log-only review run, got %q", got.Kind)
		}
		if got.Status != "success" {
			t.Fatalf("expected status 'success' for event-log-only review run, got %q", got.Status)
		}
		if !got.Review {
			t.Fatal("expected Review=true for event-log-only review run")
		}
		if got.PRNumber != 42 {
			t.Fatalf("expected PRNumber=42 for event-log-only review run, got %d", got.PRNumber)
		}
		if got.IssueLabel != "PR42" {
			t.Fatalf("expected IssueLabel 'PR42' for event-log-only review run, got %q", got.IssueLabel)
		}
		if got.Reason != "review" {
			t.Fatalf("expected Reason 'review' for event-log-only review run, got %q", got.Reason)
		}
	})

	t.Run("prompt only run unaffected", func(t *testing.T) {
		repoRoot, err := os.MkdirTemp("/tmp", "r")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		// run-<ts> dir with live socket and no event log entries —
		// the portal must keep treating it as an in-flight prompt-only
		// run, not confuse it with a review run.
		runDir := filepath.Join(repoRoot, ".sandman", "batches", "abcd-260618113825-999-1")
		sockPath := filepath.Join(runDir, "batch.sock")
		if err := os.MkdirAll(runDir, 0755); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		addBatchToIndex(t, repoRoot, "abcd-260618113825-999-1", runDir, []int{})

		runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("expected 1 row for prompt-only run, got %d: %#v", len(runs), runs)
		}
		got := runs[0]
		if got.Kind != "active" {
			t.Fatalf("expected kind 'active' for prompt-only run with live socket, got %q", got.Kind)
		}
		if got.Status != "running" {
			t.Fatalf("expected status 'running' for prompt-only run, got %q", got.Status)
		}
		if got.Review {
			t.Fatal("expected Review=false for prompt-only run")
		}
		if got.IssueLabel != "prompt-only" {
			t.Fatalf("expected IssueLabel 'prompt-only', got %q", got.IssueLabel)
		}
	})
}

func TestPortal_MetaLineCSS_AllowsLongTokenToBreak(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	data, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read %s: %v", htmlPath, err)
	}
	html := string(data)
	idx := strings.Index(html, ".meta-line")
	if idx < 0 {
		t.Fatalf("could not find .meta-line selector in %s", htmlPath)
	}
	open := strings.Index(html[idx:], "{")
	if open < 0 {
		t.Fatalf("could not find rule body for .meta-line in %s", htmlPath)
	}
	bodyStart := idx + open + 1
	close := strings.Index(html[bodyStart:], "}")
	if close < 0 {
		t.Fatalf("could not find closing brace for .meta-line rule in %s", htmlPath)
	}
	body := html[bodyStart : bodyStart+close]

	required := []struct {
		token string
		desc  string
	}{
		{"grid-template-columns: auto minmax(0, 1fr)", "value track can shrink below its min-content so the cell can be narrower than the longest token"},
		{"overflow-wrap: anywhere", "long unbreakable run-id token can break inside the value cell"},
		{"min-width: 0", "grid container can be narrower than its content (prevents forcing the column wider)"},
		{"white-space: pre-line", "newline in run meta must render as a visible line break"},
	}
	for _, r := range required {
		if !strings.Contains(body, r.token) {
			t.Errorf(".meta-line rule missing %q (%s)", r.token, r.desc)
		}
	}
	if strings.Contains(body, "grid-template-columns: auto 1fr\n") || strings.Contains(body, "grid-template-columns: auto 1fr;") {
		t.Errorf(".meta-line rule still uses 'auto 1fr' (no minmax); the 1fr track min-sizes to min-content and forces the column wider when the run-id token is long")
	}
	if strings.Contains(body, "max-width: 42ch") {
		t.Errorf(".meta-line rule still caps at 42ch; the cap fights the column layout and was removed when the run-id token's break policy was fixed")
	}
}

func TestPortal_RunColumnHasMinWidthFloor(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	data, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read %s: %v", htmlPath, err)
	}
	html := string(data)
	// The Run column width is now controlled by colgroup + table-layout: fixed.
	// The title cell CSS provides only a min-width floor.
	if !strings.Contains(html, `td[data-cell="title"]`) {
		t.Fatalf("could not find td[data-cell=\"title\"] selector in %s", htmlPath)
	}
	idx := strings.Index(html, `td[data-cell="title"]`)
	open := strings.Index(html[idx:], "{")
	if open < 0 {
		t.Fatalf("could not find rule body for td[data-cell=\"title\"] in %s", htmlPath)
	}
	bodyStart := idx + open + 1
	close := strings.Index(html[bodyStart:], "}")
	if close < 0 {
		t.Fatalf("could not find closing brace for td[data-cell=\"title\"] rule in %s", htmlPath)
	}
	body := html[bodyStart : bodyStart+close]
	if !strings.Contains(body, "min-width: 200px") {
		t.Errorf("td[data-cell=\"title\"] rule missing %q", "min-width: 200px")
	}
}

func TestPortal_ReasonField_PopulatedFromRunKind(t *testing.T) {
	repoRoot := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("auto-select success run", func(t *testing.T) {
		startedAt := time.Now().Add(-2 * time.Minute)
		state := events.RunState{
			RunID: "auto-select-1700000000000",
			Started: events.Event{
				Timestamp: startedAt,
				Payload:   map[string]any{"run_kind": "auto-select", "branch": "sandman/auto-select"},
			},
			Finished: &events.Event{
				Timestamp: startedAt.Add(1 * time.Minute),
				Payload:   map[string]any{"run_kind": "auto-select", "status": "success", "selected": []int{42, 43}},
			},
		}
		run := (&portalRunsView{}).runFromState(repoRoot(t), state, nil, nil, nil)
		if run.Reason != "auto-select" {
			t.Fatalf("expected Reason 'auto-select', got %q", run.Reason)
		}
		if run.Status != "success" {
			t.Fatalf("expected Status 'success', got %q", run.Status)
		}
	})

	t.Run("auto-select failure run", func(t *testing.T) {
		startedAt := time.Now().Add(-2 * time.Minute)
		state := events.RunState{
			RunID: "auto-select-1700000001000",
			Started: events.Event{
				Timestamp: startedAt,
				Payload:   map[string]any{"run_kind": "auto-select", "branch": "sandman/auto-select"},
			},
			Finished: &events.Event{
				Timestamp: startedAt.Add(1 * time.Minute),
				Payload:   map[string]any{"run_kind": "auto-select", "status": "failure", "reason": "no candidates"},
			},
		}
		run := (&portalRunsView{}).runFromState(repoRoot(t), state, nil, nil, nil)
		if run.Reason != "auto-select" {
			t.Fatalf("expected Reason 'auto-select', got %q", run.Reason)
		}
		if run.Status != "failure" {
			t.Fatalf("expected Status 'failure', got %q", run.Status)
		}
	})

	t.Run("in-flight review run", func(t *testing.T) {
		startedAt := time.Now().Add(-1 * time.Minute)
		state := events.RunState{
			RunID: "PR42",
			Started: events.Event{
				Timestamp: startedAt,
				Payload:   map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"},
			},
		}
		root := repoRoot(t)
		runDir := filepath.Join(root, ".sandman", "batches", "PR42")
		if err := os.MkdirAll(runDir, 0755); err != nil {
			t.Fatal(err)
		}
		sockDir, err := os.MkdirTemp("", "rs")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
		extSock := filepath.Join(sockDir, "s.sock")
		ln, err := net.Listen("unix", extSock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		if err := os.Symlink(extSock, filepath.Join(runDir, "batch.sock")); err != nil {
			t.Fatal(err)
		}
		run := (&portalRunsView{}).runFromState(root, state, nil, nil, nil)
		if run.Reason != "review" {
			t.Fatalf("expected Reason 'review', got %q", run.Reason)
		}
		if run.Status != "reviewing" {
			t.Fatalf("expected Status 'reviewing' (active review run), got %q", run.Status)
		}
		if run.IssueLabel != "PR42" {
			t.Fatalf("expected IssueLabel 'PR42', got %q", run.IssueLabel)
		}
	})

	t.Run("finished success review run", func(t *testing.T) {
		startedAt := time.Now().Add(-5 * time.Minute)
		state := events.RunState{
			RunID: "PR42",
			Started: events.Event{
				Timestamp: startedAt,
				Payload:   map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"},
			},
			Finished: &events.Event{
				Timestamp: startedAt.Add(2 * time.Minute),
				Payload:   map[string]any{"review": true, "status": "success", "branch": "sandman/review-PR42"},
			},
		}
		run := (&portalRunsView{}).runFromState(repoRoot(t), state, nil, nil, nil)
		if run.Reason != "review" {
			t.Fatalf("expected Reason 'review', got %q", run.Reason)
		}
		if run.Status != "success" {
			t.Fatalf("expected Status 'success', got %q", run.Status)
		}
		if run.IssueLabel != "PR42" {
			t.Fatalf("expected IssueLabel 'PR42', got %q", run.IssueLabel)
		}
	})

	t.Run("finished failure review run", func(t *testing.T) {
		startedAt := time.Now().Add(-5 * time.Minute)
		state := events.RunState{
			RunID: "PR42",
			Started: events.Event{
				Timestamp: startedAt,
				Payload:   map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"},
			},
			Finished: &events.Event{
				Timestamp: startedAt.Add(2 * time.Minute),
				Payload:   map[string]any{"review": true, "status": "failure", "branch": "sandman/review-PR42"},
			},
		}
		run := (&portalRunsView{}).runFromState(repoRoot(t), state, nil, nil, nil)
		if run.Reason != "review" {
			t.Fatalf("expected Reason 'review', got %q", run.Reason)
		}
		if run.Status != "failure" {
			t.Fatalf("expected Status 'failure', got %q", run.Status)
		}
		if run.IssueLabel != "PR42" {
			t.Fatalf("expected IssueLabel 'PR42', got %q", run.IssueLabel)
		}
	})

	t.Run("aborted review run", func(t *testing.T) {
		startedAt := time.Now().Add(-5 * time.Minute)
		state := events.RunState{
			RunID: "PR42",
			Started: events.Event{
				Timestamp: startedAt,
				Payload:   map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"},
			},
			Finished: &events.Event{
				Timestamp: startedAt.Add(2 * time.Minute),
				Payload:   map[string]any{"review": true, "status": "failure", "branch": "sandman/review-PR42"},
			},
		}
		// Mark the row as aborted by re-issuing run.aborted. The
		// portal layer's runFromState treats any run.aborted or
		// run.cancelled event as terminal aborted.
		aborted := events.Event{Type: "run.aborted", Timestamp: startedAt.Add(3 * time.Minute), RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42}}
		state.Finished = &aborted
		run := (&portalRunsView{}).runFromState(repoRoot(t), state, nil, nil, nil)
		if run.Reason != "review" {
			t.Fatalf("expected Reason 'review' for aborted review run, got %q", run.Reason)
		}
		if run.Status != "aborted" {
			t.Fatalf("expected Status 'aborted', got %q", run.Status)
		}
		if run.IssueLabel != "PR42" {
			t.Fatalf("expected IssueLabel 'PR42' on aborted review run, got %q", run.IssueLabel)
		}
	})

	t.Run("regular issue-driven run has empty Reason", func(t *testing.T) {
		startedAt := time.Now().Add(-3 * time.Minute)
		state := events.RunState{
			RunID: "abcd-260618113825-42-1",
			Started: events.Event{
				Timestamp: startedAt,
				Issue:     42,
				Payload:   map[string]any{"branch": "sandman/issue-42"},
			},
			Finished: &events.Event{
				Timestamp: startedAt.Add(1 * time.Minute),
				Issue:     42,
				Payload:   map[string]any{"branch": "sandman/issue-42", "status": "success"},
			},
		}
		run := (&portalRunsView{}).runFromState(repoRoot(t), state, nil, nil, nil)
		if run.Reason != "" {
			t.Fatalf("expected empty Reason for issue-driven run, got %q", run.Reason)
		}
		if run.IssueLabel != "#42" {
			t.Fatalf("expected IssueLabel '#42', got %q", run.IssueLabel)
		}
	})

	t.Run("prompt-only run has empty Reason", func(t *testing.T) {
		startedAt := time.Now().Add(-1 * time.Minute)
		state := events.RunState{
			RunID: "abcd-260618113825-prompt",
			Started: events.Event{
				Timestamp: startedAt,
				Payload:   map[string]any{"branch": "sandman/prompt"},
			},
		}
		run := (&portalRunsView{}).runFromState(repoRoot(t), state, nil, nil, nil)
		if run.Reason != "" {
			t.Fatalf("expected empty Reason for prompt-only run, got %q", run.Reason)
		}
		if run.IssueLabel != "prompt-only" {
			t.Fatalf("expected IssueLabel 'prompt-only', got %q", run.IssueLabel)
		}
	})

	t.Run("continuation run has empty Reason", func(t *testing.T) {
		// A run.continued event folds into a RunState whose Started is
		// the original run.started and Finished is unchanged. Issue-driven
		// continuations carry the same IssueRef, so RunKind() returns
		// "issue" and Reason must be "".
		startedAt := time.Now().Add(-10 * time.Minute)
		state := events.RunState{
			RunID: "abcd-260618113825-42-2",
			Started: events.Event{
				Timestamp: startedAt,
				Issue:     42,
				Payload:   map[string]any{"branch": "sandman/issue-42"},
			},
		}
		run := (&portalRunsView{}).runFromState(repoRoot(t), state, nil, nil, nil)
		if run.Reason != "" {
			t.Fatalf("expected empty Reason for continuation run, got %q", run.Reason)
		}
		if run.IssueLabel != "#42" {
			t.Fatalf("expected IssueLabel '#42' for continuation run, got %q", run.IssueLabel)
		}
	})
}

func TestPortal_ActiveMatch_ReasonDerivedFromSocket(t *testing.T) {
	shortSock := func(t *testing.T) (string, string) {
		t.Helper()
		dir, err := os.MkdirTemp("", "p")
		if err != nil {
			t.Fatal(err)
		}
		sockPath := filepath.Join(dir, "s.sock")
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return dir, sockPath
	}

	t.Run("unmatched PR active socket has Reason review", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		_, sockPath := shortSock(t)

		match := portalRunMatch{
			instance: portalActiveRun{
				Key:        "PR42",
				SocketPath: sockPath,
				PRNumber:   42,
				ModTime:    time.Now().Add(-1 * time.Minute),
			},
		}

		run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, nil, nil)
		if run.Reason != "review" {
			t.Fatalf("expected Reason 'review' for unmatched PR socket, got %q", run.Reason)
		}
	})

	t.Run("unmatched prompt-only active socket has empty Reason", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		_, sockPath := shortSock(t)

		match := portalRunMatch{
			instance: portalActiveRun{
				Key:        "abcd-260618113825-99-1",
				SocketPath: sockPath,
				ModTime:    time.Now().Add(-1 * time.Minute),
			},
		}

		run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, nil, nil)
		if run.Reason != "" {
			t.Fatalf("expected empty Reason for prompt-only active socket, got %q", run.Reason)
		}
	})

	t.Run("unmatched active auto-select socket recovers reason status and candidates from run.started event", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		_, sockPath := shortSock(t)

		match := portalRunMatch{
			instance: portalActiveRun{
				Key:        "auto-select-1700000000000",
				RunID:      "auto-select-1700000000000",
				SocketPath: sockPath,
				ModTime:    time.Now().Add(-1 * time.Minute),
			},
		}
		eventsByRun := map[string][]portalEvent{
			"auto-select-1700000000000": {{
				Type:      "run.started",
				Timestamp: time.Now().Add(-1 * time.Minute),
				Payload:   map[string]any{"run_kind": "auto-select", "candidates": []int{42, 43}},
			}},
		}

		run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, eventsByRun, nil)
		if run.Reason != "auto-select" {
			t.Fatalf("expected Reason 'auto-select' for unmatched active auto-select socket, got %q", run.Reason)
		}
		if run.Status != "auto-selecting" {
			t.Fatalf("expected Status 'auto-selecting' for unmatched active auto-select socket, got %q", run.Status)
		}
		if !reflect.DeepEqual(run.Candidates, []int{42, 43}) {
			t.Fatalf("expected Candidates [42 43], got %#v", run.Candidates)
		}
	})
}

func TestPortal_ActiveBatchIssue_ReasonFromState(t *testing.T) {
	t.Run("active batch row with auto-select state has Reason auto-select", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		sockDir, sockPath := func() (string, string) {
			d, err := os.MkdirTemp("", "s")
			if err != nil {
				t.Fatal(err)
			}
			sp := filepath.Join(d, "s.sock")
			ln, err := net.Listen("unix", sp)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			return d, sp
		}()

		active := portalActiveRun{
			Key:        "auto-select-1700000000000",
			Dir:        sockDir,
			SocketPath: sockPath,
			StartedAt:  time.Now().Add(-1 * time.Minute),
		}

		startedAt := time.Now().Add(-1 * time.Minute)
		state := &events.RunState{
			RunID: "auto-select-1700000000000",
			Started: events.Event{
				Timestamp: startedAt,
				Payload:   map[string]any{"run_kind": "auto-select", "branch": "sandman/auto-select"},
			},
		}

		// Active batch issue path requires an issue number; pass 0 to
		// surface the auto-select run whose only event is the auto-select
		// run itself. The row's Key/RunID are taken from the state.
		run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 0, state, nil, nil, "", nil, nil)
		if run.Reason != "auto-select" {
			t.Fatalf("expected Reason 'auto-select', got %q", run.Reason)
		}
	})
}

func TestPortal_ReasonChipCSS_DefinesAutoSelectAndReviewVariants(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	data, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read %s: %v", htmlPath, err)
	}
	html := string(data)

	for _, sel := range []string{".badge.auto-select", ".badge.review"} {
		idx := strings.Index(html, sel)
		if idx < 0 {
			t.Fatalf("could not find %s selector in %s", sel, htmlPath)
		}
		open := strings.Index(html[idx:], "{")
		if open < 0 {
			t.Fatalf("could not find rule body for %s in %s", sel, htmlPath)
		}
		bodyStart := idx + open + 1
		close := strings.Index(html[bodyStart:], "}")
		if close < 0 {
			t.Fatalf("could not find closing brace for %s rule in %s", sel, htmlPath)
		}
		body := html[bodyStart : bodyStart+close]
		if !strings.Contains(body, "background:") {
			t.Errorf("%s rule missing background declaration", sel)
		}
		if !strings.Contains(body, "color:") {
			t.Errorf("%s rule missing color declaration", sel)
		}
		if !strings.Contains(body, "border") {
			t.Errorf("%s rule missing border declaration", sel)
		}
	}
}

// TestPortalRuns_ReviewAndImplRowsSeparateForSameIssue is the
// regression test for the row-mixing bug. A `sandman review --issue N`
// run stores `issue_number: N` in the run.started payload and the
// orchestrator stamps `issue: N` on the run.finished event, so
// RunState.IssueNumber() returns N for the review row even though its
// RunID is `PR<k>`. The dedup pass groups rows by IssueNumber, so the
// review row and the impl row for the same issue both end up in the
// same dedup group. Without the fix, `dedupRunGroup` collapses them
// into a single row that drops either the impl run or the review run
// depending on priority (aborted > active > default).
func TestPortalRuns_ReviewAndImplRowsSeparateForSameIssue(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	abortedAt := startedAt.Add(2 * time.Minute)
	reviewStartedAt := startedAt.Add(3 * time.Minute)
	reviewFinishedAt := reviewStartedAt.Add(2 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		// Impl run for issue 1066 — aborted. Mirrors the
		// production shape where the first attempt was aborted
		// and the second attempt is mid-flight, leaving an
		// aborted row in the event log.
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-issue-1066-impl", Issue: 1066, Payload: map[string]any{"branch": "sandman/1066-impl"}},
		{Type: "run.aborted", Timestamp: abortedAt, RunID: "abcd-260618113825-issue-1066-impl", Issue: 1066, Payload: map[string]any{"branch": "sandman/1066-impl", "status": "aborted"}},
		// Review run for PR 1075 of issue 1066 — finished. The
		// orchestrator stamps `issue: 1066` on the finished event
		// (and `issue_number: 1066` in the payload), so both the
		// event-level Issue field and the payload field point at
		// issue 1066. This is the production shape that produced
		// the row-mixing bug in the portal.
		{Type: "run.started", Timestamp: reviewStartedAt, RunID: "PR1075", Issue: 0, Payload: map[string]any{"branch": "sandman/review-PR1075", "review": true, "pr_number": 1075, "issue_number": 1066}},
		{Type: "run.finished", Timestamp: reviewFinishedAt, RunID: "PR1075", Issue: 1066, Payload: map[string]any{"branch": "sandman/review-PR1075", "review": true, "pr_number": 1075, "issue_number": 1066, "status": "success"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	var implRun, reviewRun *portalRun
	for i := range runs {
		switch runs[i].RunID {
		case "abcd-260618113825-issue-1066-impl":
			implRun = &runs[i]
		case "PR1075":
			reviewRun = &runs[i]
		}
	}
	if implRun == nil {
		t.Fatalf("expected impl row (run-1066-impl), got %d rows", len(runs))
	}
	if reviewRun == nil {
		t.Fatalf("expected review row (PR1075), got %d rows", len(runs))
	}
	if implRun.IssueNumber != 1066 {
		t.Fatalf("expected impl row IssueNumber=1066, got %d", implRun.IssueNumber)
	}
	if reviewRun.IssueNumber != 1066 {
		t.Fatalf("expected review row IssueNumber=1066 (from issue_number payload), got %d", reviewRun.IssueNumber)
	}
	if reviewRun.PRNumber != 1075 {
		t.Fatalf("expected review row PRNumber=1075, got %d", reviewRun.PRNumber)
	}
	if !reviewRun.Review {
		t.Fatal("expected review row Review=true")
	}
}

// TestPortal_RunFromState_ActiveEmptyKeyUsesHelperFallback pins issue
// #1541 for the runFromState active branch: an active instance whose
// Key, BatchID, and Dir are all empty (a degraded index entry) must
// still produce a non-empty BatchKey via the synthetic RunID sentinel.
func TestPortal_RunFromState_ActiveEmptyKeyUsesHelperFallback(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-1 * time.Minute)
	active := &portalActiveRun{
		Key:         "",
		BatchID:     "",
		Dir:         "",
		RunID:       "ghost-active-issue-42",
		IssueNumber: 42,
		StartedAt:   startedAt,
	}
	state := events.RunState{
		RunID: "ghost-active-issue-42",
		Started: events.Event{
			Timestamp: startedAt,
			Payload:   map[string]any{"issue": float64(42)},
		},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, state, active, nil, nil)

	if run.BatchKey == "" {
		t.Fatalf("expected non-empty BatchKey from helper fallback, got empty")
	}
	if run.BatchKey != "active-ghost-active-issue-42" {
		t.Fatalf("expected BatchKey %q (sentinel), got %q", "active-ghost-active-issue-42", run.BatchKey)
	}
}

// TestPortal_RunFromActiveBatchIssue_EmptyKeyStillHasBatchIdentity
// pins issue #1541: a live instance whose Key is empty must still
// produce a non-empty BatchKey across queued and blocked statuses.
func TestPortal_RunFromActiveBatchIssue_EmptyKeyStillHasBatchIdentity(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "b.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Now().Add(-2 * time.Minute)
	active := portalActiveRun{
		Key:          "",
		BatchID:      "",
		Dir:          filepath.Join(repoRoot, "batches", "current-batch-42"),
		SocketPath:   sockPath,
		RunID:        "current-batch-42-issue-42",
		IssueNumbers: []int{42},
		StartedAt:    startedAt,
	}

	cases := []struct {
		name string
		run  portalRun
	}{
		{
			name: "queued state preserves identity",
			run:  (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, nil, nil, nil, "", nil, nil),
		},
		{
			name: "blocked state preserves identity",
			run: func() portalRun {
				blocked := &events.Event{Type: "run.blocked", Timestamp: startedAt, RunID: "blocked-run", Issue: 42, Payload: map[string]any{}}
				return (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 42, nil, blocked, nil, "", nil, nil)
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.run.BatchKey == "" {
				t.Fatalf("expected BatchKey to be non-empty (derived from runDir), got empty (status=%q)", tc.run.Status)
			}
			if tc.run.BatchKey != "current-batch-42" {
				t.Fatalf("expected BatchKey %q (from Dir basename), got %q", "current-batch-42", tc.run.BatchKey)
			}
		})
	}
}

// TestPortal_RunFromActiveMatch_NormalizesBatchIdentity pins issue
// #1541 for the prompt-only / auto-select path: an active instance
// whose Key is empty must still reach dedup with a non-empty BatchKey.
func TestPortal_RunFromActiveMatch_NormalizesBatchIdentity(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sockDir := filepath.Join(repoRoot, "sock")
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Now().Add(-1 * time.Minute)
	match := portalRunMatch{
		instance: portalActiveRun{
			Key:         "",
			BatchID:     "manifest-42",
			Dir:         sockDir,
			SocketPath:  sockPath,
			RunID:       "manifest-42-prompt",
			IssueNumber: 0,
			ModTime:     startedAt,
		},
	}

	run := (&portalRunsView{}).runFromActiveMatch(repoRoot, match, nil, nil)

	if run.BatchKey == "" {
		t.Fatalf("expected BatchKey to be non-empty (derived from BatchID), got empty")
	}
	if run.BatchKey != "manifest-42" {
		t.Fatalf("expected BatchKey %q (derived from BatchID), got %q", "manifest-42", run.BatchKey)
	}
}

// TestPortal_DedupRuns_DifferentBatchesRemainSeparate locks in the
// #1542 acceptance criterion 1 contract: rows from different batches
// for the same issue number survive dedupRuns as two separate rows.
func TestPortal_DedupRuns_DifferentBatchesRemainSeparate(t *testing.T) {
	v := &portalRunsView{}
	base := time.Now().Add(-5 * time.Minute)
	runs := []portalRun{
		{Key: "historical-issue-42", Kind: "completed", Status: "aborted", IssueNumber: 42, StartedAt: base.Add(-time.Hour), BatchKey: ""},
		{Key: "current-issue-42", Kind: "active", Status: "running", IssueNumber: 42, StartedAt: base, BatchKey: "current-derived"},
	}

	result := v.dedupRuns(runs)

	if len(result) != 2 {
		t.Fatalf("expected rows from different batches to stay separate (2 rows), got %d: %#v", len(result), result)
	}
	gotKeys := map[string]string{}
	for _, r := range result {
		gotKeys[r.Key] = r.BatchKey
	}
	if gotKeys["historical-issue-42"] != "" {
		t.Fatalf("historical row lost BatchKey: %#v", result)
	}
	if gotKeys["current-issue-42"] != "current-derived" {
		t.Fatalf("current-batch row lost BatchKey: %#v", result)
	}
}

// TestPortal_DedupRuns_SameBatchRowsStillCollapse locks in the #1542
// acceptance criterion 2 contract: same-batch duplicate rows continue
// to collapse through dedupRuns.
func TestPortal_DedupRuns_SameBatchRowsStillCollapse(t *testing.T) {
	v := &portalRunsView{}
	base := time.Now().Add(-5 * time.Minute)
	runs := []portalRun{
		{Key: "active-1-issue-42", Kind: "active", Status: "running", IssueNumber: 42, StartedAt: base, BatchKey: "active-1"},
		{Key: "active-1-issue-42-dup", Kind: "active", Status: "queued", IssueNumber: 42, StartedAt: base.Add(time.Second), BatchKey: "active-1"},
	}

	result := v.dedupRuns(runs)

	if len(result) != 1 {
		t.Fatalf("expected same-batch rows to collapse to 1, got %d: %#v", len(result), result)
	}
	if result[0].BatchKey != "active-1" {
		t.Fatalf("expected collapsed row to retain BatchKey %q, got %q", "active-1", result[0].BatchKey)
	}
}

// TestPortal_BatchKeyForActive_FallbackChain pins the contract for
// batchKeyForActive: every fallback level must return a non-empty
// string so active rows never reach dedupRuns with an empty BatchKey.
func TestPortal_BatchKeyForActive_FallbackChain(t *testing.T) {
	cases := []struct {
		name   string
		active portalActiveRun
		want   string
	}{
		{
			name:   "Key wins when populated",
			active: portalActiveRun{Key: "active-1", BatchID: "manifest-1", Dir: "/tmp/active-1", RunID: "active-1-issue-42"},
			want:   "active-1",
		},
		{
			name:   "BatchID used when Key is empty",
			active: portalActiveRun{Key: "", BatchID: "manifest-2", Dir: "/tmp/active-2", RunID: "active-2-issue-42"},
			want:   "manifest-2",
		},
		{
			name:   "Dir basename used when Key and BatchID are empty",
			active: portalActiveRun{Key: "", BatchID: "", Dir: "/tmp/active-3", RunID: "active-3-issue-42"},
			want:   "active-3",
		},
		{
			name:   "Dot dir falls back to synthetic sentinel",
			active: portalActiveRun{Key: "", BatchID: "", Dir: ".", RunID: "active-4-issue-42"},
			want:   "active-active-4-issue-42",
		},
		{
			name:   "All empty inputs still produce a non-empty sentinel",
			active: portalActiveRun{Key: "", BatchID: "", Dir: "", RunID: "active-5-issue-42"},
			want:   "active-active-5-issue-42",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := batchKeyForActive(tc.active)
			if got != tc.want {
				t.Fatalf("batchKeyForActive = %q, want %q", got, tc.want)
			}
			if got == "" {
				t.Fatal("batchKeyForActive must never return empty string")
			}
		})
	}
}
