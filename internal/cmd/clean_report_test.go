package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
)

func TestCleanReport_DefaultDryRun_PrintsWouldRemoveBatchEntries(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-default-dry")
	worktreeDir := filepath.Join(dir, ".sandman", "worktrees", "sandman", "11-default-dry")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:        "batch-default-dry",
		BatchID:      "batch-default-dry",
		Issue:        11,
		Branch:       "sandman/11-default-dry",
		WorktreePath: worktreeDir,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-default-dry", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Would remove 1 batch entries:") {
		t.Errorf("expected 'Would remove 1 batch entries:' header, got: %s", out)
	}
	if !strings.Contains(out, "batch-default-dry") {
		t.Errorf("expected per-line to include batch ID, got: %s", out)
	}
	if !strings.Contains(out, "path:") {
		t.Errorf("expected per-line to include path, got: %s", out)
	}
}

func TestCleanReport_DefaultRealRun_PrintsRemovedBatchEntries(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-default-real")
	worktreeDir := filepath.Join(dir, ".sandman", "worktrees", "sandman", "12-default-real")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:        "batch-default-real",
		BatchID:      "batch-default-real",
		Issue:        12,
		Branch:       "sandman/12-default-real",
		WorktreePath: worktreeDir,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-default-real", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Removed 1 batch entries.") {
		t.Errorf("expected 'Removed 1 batch entries.' line, got: %s", out)
	}
}

func TestCleanReport_ArchivedDryRun_PrintsWouldRemoveBatchEntries(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-archived-dry")
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:   "batch-archived-dry",
		BatchID: "batch-archived-dry",
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-archived-dry", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--archived", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Would remove 1 batch entries:") {
		t.Errorf("expected 'Would remove 1 batch entries:' header, got: %s", out)
	}
	if !strings.Contains(out, "batch-archived-dry") {
		t.Errorf("expected per-line to include batch ID, got: %s", out)
	}
}

func TestCleanReport_ArchivedRealRun_PrintsPerLineRemoved(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-archived-real")
	worktreeDir := filepath.Join(dir, ".sandman", "worktrees", "sandman", "13-archived-real")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:        "batch-archived-real",
		BatchID:      "batch-archived-real",
		Issue:        13,
		Branch:       "sandman/13-archived-real",
		WorktreePath: worktreeDir,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-archived-real", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--archived"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Removed 1 batch entries.") {
		t.Errorf("expected 'Removed 1 batch entries.' line, got: %s", out)
	}
	if !strings.Contains(out, "batch-archived-real") {
		t.Errorf("expected per-line to include batch ID, got: %s", out)
	}
	if !strings.Contains(out, "path:") {
		t.Errorf("expected per-line to include path, got: %s", out)
	}
}

func TestCleanReport_OrphanedDryRun_PrintsWouldRemoveOrphans(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	orphanDir := filepath.Join(dir, ".sandman", "batches", "orphan-dry")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "batch.json"), []byte(`{"createdAt":"2026-07-02T00:00:00Z","batchId":"orphan-dry"}`), 0o644); err != nil {
		t.Fatalf("write orphan manifest: %v", err)
	}
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "orphan-dry", Path: orphanDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--orphaned", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Would remove 1 orphaned batch director(ies):") {
		t.Errorf("expected 'Would remove 1 orphaned batch director(ies):' header, got: %s", out)
	}
	if !strings.Contains(out, "orphan-dry") {
		t.Errorf("expected per-line to mention orphan dir, got: %s", out)
	}
}

func TestCleanReport_OrphanedRealRun_PrintsRemovedOrphans(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	orphanDir := filepath.Join(dir, ".sandman", "batches", "orphan-real")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "batch.json"), []byte(`{"createdAt":"2026-07-02T00:00:00Z","batchId":"orphan-real"}`), 0o644); err != nil {
		t.Fatalf("write orphan manifest: %v", err)
	}
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "orphan-real", Path: orphanDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--orphaned"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Removed 1 orphaned batch director(ies):") {
		t.Errorf("expected 'Removed 1 orphaned batch director(ies):' header, got: %s", out)
	}
	if !strings.Contains(out, "orphan-real") {
		t.Errorf("expected per-line to mention orphan dir, got: %s", out)
	}
}

func TestCleanReport_StaleBody_HasNoBatchEntriesHeader(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "Would remove") {
		t.Errorf("stale body should NOT print 'Would remove' header, got: %s", out)
	}
	if strings.Contains(out, "Removed ") && strings.Contains(out, "batch entries") {
		t.Errorf("stale body should NOT print 'Removed N batch entries.' line, got: %s", out)
	}
}

func TestCleanReport_NoBatches_PrintsNoBatchesMessage(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	writeBatchIndex(t, dir, []batchindex.Batch{})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "No batches to clean.") {
		t.Errorf("expected 'No batches to clean.' message, got: %s", out)
	}
}

func TestCleanReport_OrphanedEmpty_PrintsNoOrphansMessage(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	writeBatchIndex(t, dir, []batchindex.Batch{})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--orphaned"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "No orphaned batch directories found.") {
		t.Errorf("expected 'No orphaned batch directories found.' message, got: %s", out)
	}
}
