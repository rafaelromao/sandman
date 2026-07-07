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
	if got, want := defaultLayout.BatchesIndexPath, filepath.Join(repoRoot, ".sandman", "batches.json"); got != want {
		t.Errorf("defaultLayout.BatchesIndexPath = %q, want %q", got, want)
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
	if got, want := l.BatchesIndexPath, filepath.Join(repoRoot, ".sandman", "batches.json"); got != want {
		t.Errorf("BatchesIndexPath with nil cfg = %q, want %q", got, want)
	}
}

func TestLayout_PerBatchPaths(t *testing.T) {
	l := Layout{RepoRoot: "/r", BatchesDir: filepath.Join("/r", ".sandman", "batches")}
	const (
		batchID = "b1"
		runID   = "r1"
	)
	runDir := filepath.Join(l.BatchesDir, batchID, "runs", runID)
	batchDir := filepath.Join(l.BatchesDir, batchID)

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"RunLogPath", l.RunLogPath(batchID, runID), filepath.Join(runDir, "run.log")},
		{"RunSocketPath", l.RunSocketPath(batchID, runID), filepath.Join(runDir, "run.sock")},
		{"RunManifestPath", l.RunManifestPath(batchID, runID), filepath.Join(runDir, "run.json")},
		{"ReviewStatePath", l.ReviewStatePath(batchID, runID), filepath.Join(runDir, "review-state.json")},
		{"RunConfigSnapshotDir", l.RunConfigSnapshotDir(batchID, runID), filepath.Join(runDir, "config")},
		{"BatchManifestPath", l.BatchManifestPath(batchID), filepath.Join(batchDir, "batch.json")},
		{"BatchSocketPath", l.BatchSocketPath(batchID), filepath.Join(batchDir, "batch.sock")},
		{"BatchConfigSnapshotDir", l.BatchConfigSnapshotDir(batchID), filepath.Join(batchDir, "config")},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestLayout_AgentControlledPaths(t *testing.T) {
	l := Layout{
		RepoRoot:   "/r",
		SandmanDir: filepath.Join("/r", ".sandman"),
		StateDir:   filepath.Join("/r", ".sandman", "state"),
	}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ReviewsDir", l.ReviewsDir(), filepath.Join(l.SandmanDir, "reviews")},
		{"PromptVersionPath", l.PromptVersionPath(), filepath.Join(l.StateDir, ".prompt-version")},
		{"BadgeControlFilePath", l.BadgeControlFilePath(), filepath.Join(l.StateDir, ".built_with_sandman")},
		{"SelectedIssuesPath", l.SelectedIssuesPath(), filepath.Join(l.StateDir, "selected-issues.json")},
		{"PRHeadShaPath", l.PRHeadShaPath(42), filepath.Join(l.StateDir, "42.head_sha")},
		{"PRAddressedCommentsPath", l.PRAddressedCommentsPath(42), filepath.Join(l.StateDir, "42.addressed_comments")},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestLayout_ScaffoldAndReviewPaths(t *testing.T) {
	l := Layout{
		RepoRoot:   "/r",
		SandmanDir: filepath.Join("/r", ".sandman"),
	}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"DockerfilePath", l.DockerfilePath(), filepath.Join(l.SandmanDir, "Dockerfile")},
		{"ConfigPath", l.ConfigPath(), filepath.Join(l.SandmanDir, "config.yaml")},
		{"PromptPath", l.PromptPath(), filepath.Join(l.SandmanDir, "prompt.md")},
		{"AutoSelectionPromptPath", l.AutoSelectionPromptPath(), filepath.Join(l.SandmanDir, "auto-selection-prompt.md")},
		{"ReviewPromptPath", l.ReviewPromptPath(), filepath.Join(l.ReviewsDir(), "review-prompt.md")},
		{"QualityRulesPath", l.QualityRulesPath(), filepath.Join(l.ReviewsDir(), "quality-rules.md")},
		{"ReviewSocketPath", l.ReviewSocketPath(), filepath.Join(l.ReviewsDir(), "review.sock")},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestNewLayout_ResolvesStateDir(t *testing.T) {
	repoRoot := t.TempDir()
	l := NewLayout(nil, repoRoot)
	if got, want := l.StateDir, filepath.Join(repoRoot, ".sandman", "state"); got != want {
		t.Errorf("NewLayout StateDir = %q, want %q", got, want)
	}
}
