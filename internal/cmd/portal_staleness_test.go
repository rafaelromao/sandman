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

// TestPortal_Compute_PopulatesLastOutputAtForActiveRuns is the end-to-end
// wiring test: compute() must set LastOutputAt on active rows (from the
// saved log mtime, falling back to StartedAt) and must leave it nil for
// completed rows so the /api/runs contract only carries staleness where
// it is meaningful.
func TestPortal_Compute_PopulatesLastOutputAtForActiveRuns(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-5 * time.Minute)
	runDir := filepath.Join(repoRoot, ".sandman", "runs", "PR42")
	sockPath := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, sockPath)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
	})

	view := &portalRunsView{}
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")

	// First pass: active run with no saved log yet → LastOutputAt == StartedAt.
	first, err := view.compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(first) != 1 || first[0].Kind != "active" {
		t.Fatalf("expected one active row, got %#v", first)
	}
	if first[0].LastOutputAt == nil {
		t.Fatal("expected LastOutputAt to be set for active run with no log file")
	}
	if got := *first[0].LastOutputAt; !got.Equal(startedAt) {
		t.Fatalf("LastOutputAt=%v, want startedAt fallback %v", got, startedAt)
	}

	// Write a saved log file at the portal's computed LogPath, backdate it,
	// and recompute → LastOutputAt must track the file mtime.
	savedLog := first[0].LogPath
	if savedLog == "" {
		t.Fatal("expected active run to carry a LogPath")
	}
	outputMtime := startedAt.Add(2 * time.Minute)
	if err := os.MkdirAll(filepath.Dir(savedLog), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(savedLog, []byte("[issue-42] fresh output\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(savedLog, outputMtime, outputMtime); err != nil {
		t.Fatal(err)
	}

	second, err := view.compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(second) != 1 || second[0].Kind != "active" {
		t.Fatalf("expected one active row, got %#v", second)
	}
	if second[0].LastOutputAt == nil || !second[0].LastOutputAt.Equal(outputMtime) {
		t.Fatalf("LastOutputAt=%v, want saved log mtime %v", second[0].LastOutputAt, outputMtime)
	}
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
