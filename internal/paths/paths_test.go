package paths

import (
	"path/filepath"
	"strings"
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
	if got, want := defaultLayout.LogDir, filepath.Join(repoRoot, ".sandman", "logs"); got != want {
		t.Errorf("defaultLayout.LogDir = %q, want %q", got, want)
	}
	if got, want := defaultLayout.EventsLogPath, filepath.Join(repoRoot, ".sandman", "events.jsonl"); got != want {
		t.Errorf("defaultLayout.EventsLogPath = %q, want %q", got, want)
	}
	if got, want := defaultLayout.ArchiveDir, filepath.Join(repoRoot, ".sandman", "archive"); got != want {
		t.Errorf("defaultLayout.ArchiveDir = %q, want %q", got, want)
	}
	if got, want := defaultLayout.RunsDir, filepath.Join(repoRoot, ".sandman", "runs"); got != want {
		t.Errorf("defaultLayout.RunsDir = %q, want %q", got, want)
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
}

func TestSafeLogFilename_Slashes(t *testing.T) {
	l := NewLayout(&config.Config{}, t.TempDir())
	if got, want := l.SafeLogFilename("sandman/42-foo bar"), "sandman-42-foo-bar"; got != want {
		t.Errorf("SafeLogFilename = %q, want %q", got, want)
	}
}

func TestSafeLogFilename_Empty(t *testing.T) {
	l := NewLayout(&config.Config{}, t.TempDir())
	if got, want := l.SafeLogFilename(""), "prompt-only"; got != want {
		t.Errorf("SafeLogFilename(\"\") = %q, want %q", got, want)
	}
}

func TestSafeLogFilename_AllSeparators(t *testing.T) {
	l := NewLayout(&config.Config{}, t.TempDir())
	for _, in := range []string{"sandman/42", "sandman 42", "sandman" + string(filepath.Separator) + "42"} {
		got := l.SafeLogFilename(in)
		if strings.ContainsAny(got, "/ "+string(filepath.Separator)) {
			t.Errorf("SafeLogFilename(%q) = %q, must not contain /, space, or path separator", in, got)
		}
		if got == "" {
			t.Errorf("SafeLogFilename(%q) returned empty", in)
		}
	}
}
