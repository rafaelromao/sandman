package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortal_LastOutputAt pins the staleness data source. The portal's
// core unmet job is distinguishing an active run that is producing output
// from one that has gone quiet. The saved run log (.sandman/logs/<N>.log)
// is opened with O_APPEND during AgentRun.Execute, so its mtime is the
// cheapest accurate "last output" signal — and, unlike event timestamps,
// it does not flag a healthy but quiet agent as stale.
func TestPortal_LastOutputAt(t *testing.T) {
	staleMtime := time.Date(2025, 6, 17, 12, 0, 0, 0, time.UTC)
	startedAt := time.Date(2025, 6, 17, 11, 55, 0, 0, time.UTC)

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "42.log")
	if err := os.WriteFile(logPath, []byte("[issue-42] 12:00:00 hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logPath, staleMtime, staleMtime); err != nil {
		t.Fatal(err)
	}

	v := &portalRunsView{}

	t.Run("uses saved log mtime when present", func(t *testing.T) {
		run := portalRun{Kind: "active", LogPath: logPath, StartedAt: startedAt}
		if got := v.lastOutputAt(run); !got.Equal(staleMtime) {
			t.Fatalf("lastOutputAt=%v, want log mtime %v", got, staleMtime)
		}
	})

	t.Run("falls back to started at when no log file yet", func(t *testing.T) {
		run := portalRun{Kind: "active", LogPath: filepath.Join(tmp, "missing.log"), StartedAt: startedAt}
		if got := v.lastOutputAt(run); !got.Equal(startedAt) {
			t.Fatalf("lastOutputAt=%v, want startedAt %v", got, startedAt)
		}
	})

	t.Run("falls back to started at when log path empty", func(t *testing.T) {
		run := portalRun{Kind: "active", StartedAt: startedAt}
		if got := v.lastOutputAt(run); !got.Equal(startedAt) {
			t.Fatalf("lastOutputAt=%v, want startedAt %v", got, startedAt)
		}
	})

	t.Run("zero when neither is set", func(t *testing.T) {
		run := portalRun{Kind: "active"}
		if got := v.lastOutputAt(run); !got.IsZero() {
			t.Fatalf("lastOutputAt=%v, want zero", got)
		}
	})

	t.Run("ignores a directory at the log path", func(t *testing.T) {
		dirPath := filepath.Join(tmp, "is-a-dir")
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			t.Fatal(err)
		}
		run := portalRun{Kind: "active", LogPath: dirPath, StartedAt: startedAt}
		if got := v.lastOutputAt(run); !got.Equal(startedAt) {
			t.Fatalf("lastOutputAt=%v, want startedAt fallback %v (dir must not be treated as a log)", got, startedAt)
		}
	})
}

// TestPortal_Compute_LeavesLastOutputAtNilForCompletedRows ensures the
// staleness field is omitted for terminal rows so the JSON contract only
// carries it for runs that can actually be stale.
func TestPortal_Compute_LeavesLastOutputAtNilForCompletedRows(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-issue-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "abcd-260618113825-issue-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(runs))
	}
	if runs[0].Kind != "completed" {
		t.Fatalf("expected completed kind, got %q", runs[0].Kind)
	}
	if runs[0].LastOutputAt != nil {
		t.Fatalf("expected LastOutputAt nil for completed row, got %v", *runs[0].LastOutputAt)
	}
}
