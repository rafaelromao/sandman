package paths

import (
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestNewLayout_DefaultAndCustomFields(t *testing.T) {
	repoRoot := t.TempDir()
	defaultLayout := NewLayout(&config.Config{WorktreeDir: ""}, repoRoot)
	if defaultLayout.RepoRoot != repoRoot {
		t.Errorf("RepoRoot = %q, want %q", defaultLayout.RepoRoot, repoRoot)
	}
	if got, want := defaultLayout.SandmanDir, filepath.Join(repoRoot, ".sandman"); got != want {
		t.Errorf("defaultLayout.SandmanDir = %q, want %q", got, want)
	}
	if got, want := defaultLayout.WorktreeDir, filepath.Join(repoRoot, ".sandman", "worktrees"); got != want {
		t.Errorf("defaultLayout.WorktreeDir = %q, want %q", got, want)
	}
	if got, want := defaultLayout.BatchesDir, filepath.Join(repoRoot, ".sandman", "batches"); got != want {
		t.Errorf("defaultLayout.BatchesDir = %q, want %q", got, want)
	}
	if got, want := defaultLayout.BatchesIndex, filepath.Join(repoRoot, ".sandman", "batches.json"); got != want {
		t.Errorf("defaultLayout.BatchesIndex = %q, want %q", got, want)
	}
	if got, want := defaultLayout.EventsLogPath, filepath.Join(repoRoot, ".sandman", "events.jsonl"); got != want {
		t.Errorf("defaultLayout.EventsLogPath = %q, want %q", got, want)
	}
	if got, want := defaultLayout.ArchiveDir, filepath.Join(repoRoot, ".sandman", "archive"); got != want {
		t.Errorf("defaultLayout.ArchiveDir = %q, want %q", got, want)
	}

	customLayout := NewLayout(&config.Config{WorktreeDir: "custom/wt"}, repoRoot)
	if got, want := customLayout.WorktreeDir, filepath.Join(repoRoot, "custom", "wt"); got != want {
		t.Errorf("customLayout.WorktreeDir = %q, want %q", got, want)
	}

	absLayout := NewLayout(&config.Config{WorktreeDir: "/abs/worktrees"}, repoRoot)
	if got, want := absLayout.WorktreeDir, "/abs/worktrees"; got != want {
		t.Errorf("absLayout.WorktreeDir = %q, want %q", got, want)
	}
}

func TestNewLayout_NilConfig(t *testing.T) {
	repoRoot := t.TempDir()
	l := NewLayout(nil, repoRoot)
	if got, want := l.WorktreeDir, filepath.Join(repoRoot, ".sandman", "worktrees"); got != want {
		t.Errorf("WorktreeDir with nil cfg = %q, want %q", got, want)
	}
	if got, want := l.BatchesDir, filepath.Join(repoRoot, ".sandman", "batches"); got != want {
		t.Errorf("BatchesDir with nil cfg = %q, want %q", got, want)
	}
	if got, want := l.BatchesIndex, filepath.Join(repoRoot, ".sandman", "batches.json"); got != want {
		t.Errorf("BatchesIndex with nil cfg = %q, want %q", got, want)
	}
}
