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
	cmd.SetArgs([]string{"--all", "--dry-run"})

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
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Removed 1 batch entries:") {
		t.Errorf("expected 'Removed 1 batch entries:' header, got: %s", out)
	}
	if !strings.Contains(out, "batch-default-real") {
		t.Errorf("expected per-line to include batch ID, got: %s", out)
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
	if !strings.Contains(out, "Removed 1 batch entries:") {
		t.Errorf("expected 'Removed 1 batch entries:' header, got: %s", out)
	}
	if !strings.Contains(out, "batch-archived-real") {
		t.Errorf("expected per-line to include batch ID, got: %s", out)
	}
	if !strings.Contains(out, "path:") {
		t.Errorf("expected per-line to include path, got: %s", out)
	}
}

func TestClean_Archived_PrintsRemovedReport(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-ac-report")
	worktreeDir := filepath.Join(dir, ".sandman", "worktrees", "sandman", "ac-archived")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:        "batch-ac-report",
		BatchID:      "batch-ac-report",
		Issue:        2061,
		Branch:       "sandman/2061-ac",
		WorktreePath: worktreeDir,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-ac-report", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
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
	if !strings.Contains(out, "Removed 1 batch entries:") {
		t.Errorf("expected 'Removed 1 batch entries:' header, got: %s", out)
	}
	if strings.Contains(out, "Removed 1 batch entries.") && !strings.Contains(out, "Removed 1 batch entries:") {
		t.Errorf("expected header-style 'Removed N batch entries:' (colon, not period), got: %s", out)
	}
	if !strings.Contains(out, "batch-ac-report") {
		t.Errorf("expected per-line to include batch ID, got: %s", out)
	}
	if !strings.Contains(out, "[issue]") {
		t.Errorf("expected per-line to include kind tag '[issue]', got: %s", out)
	}
	if !strings.Contains(out, "path: "+batchDir) {
		t.Errorf("expected per-line to include 'path: <batchDir>', got: %s", out)
	}
	if !strings.Contains(out, "worktree: "+worktreeDir) {
		t.Errorf("expected per-line to include 'worktree: <worktreeDir>', got: %s", out)
	}
}

func TestClean_Archived_NoWorktree_OmitsWorktreeField(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-ac-no-wt")
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:   "batch-ac-no-wt",
		BatchID: "batch-ac-no-wt",
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-ac-no-wt", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
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
	if !strings.Contains(out, "Removed 1 batch entries:") {
		t.Errorf("expected 'Removed 1 batch entries:' header, got: %s", out)
	}
	if strings.Contains(out, "worktree:") {
		t.Errorf("expected NO 'worktree:' field when manifest has no worktree, got: %s", out)
	}
}

func TestClean_Orphaned_PrintsRemovedReport(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	orphanDir := filepath.Join(dir, ".sandman", "batches", "orphan-ac")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "batch.json"), []byte(`{"createdAt":"2026-07-02T00:00:00Z","batchId":"orphan-ac"}`), 0o644); err != nil {
		t.Fatalf("write orphan manifest: %v", err)
	}
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "orphan-ac", Path: orphanDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
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
	if !strings.Contains(out, "orphan-ac") {
		t.Errorf("expected per-line to mention orphan dir name, got: %s", out)
	}
	if !strings.Contains(out, "  - ") {
		t.Errorf("expected per-line to use '  - ' prefix, got: %s", out)
	}
}

func TestClean_Orphaned_NoOrphans_PrintsNothingToRemove(t *testing.T) {
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
	if !strings.Contains(out, "Nothing to remove.") {
		t.Errorf("expected 'Nothing to remove.' when no orphans found, got: %s", out)
	}
	if strings.Contains(out, "No orphaned batch directories found.") {
		t.Errorf("legacy 'No orphaned batch directories found.' should no longer appear, got: %s", out)
	}
	if strings.Contains(out, "Removed 0 orphaned") {
		t.Errorf("zero-orphan case should NOT print 'Removed 0 orphaned …' header, got: %s", out)
	}
}

func TestClean_All_MutuallyExclusiveWithArchived(t *testing.T) {
	deps := newTestDeps(t)
	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all", "--archived"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --all combined with --archived")
	}
	if !strings.Contains(err.Error(), "all") {
		t.Errorf("expected error to mention --all, got: %v", err)
	}
}

func TestClean_All_MutuallyExclusiveWithOtherModes(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"--all --archived", []string{"--all", "--archived"}},
		{"--all --stale", []string{"--all", "--stale"}},
		{"--all --orphaned", []string{"--all", "--orphaned"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := newTestDeps(t)
			var buf bytes.Buffer
			cmd := NewCleanCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tc.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected error for %v, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), "--all") {
				t.Errorf("expected error to mention --all, got: %v", err)
			}
		})
	}
}

func TestClean_All_DryRun_NotMutuallyExclusive(t *testing.T) {
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
	cmd.SetArgs([]string{"--all", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("--all --dry-run should be accepted, got: %v", err)
	}
}

func TestClean_Archived_TempsAndImages_AppearInSameReportBlock(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-ac-tmp")
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:   "batch-ac-tmp",
		BatchID: "batch-ac-tmp",
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-ac-tmp", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}
	deps.TempCleaner = &fakeTempCleaner{
		resolveRuntimeReturn: "docker",
		scanTempDirsReturn:   []string{"/tmp/sandman-test-1", "/tmp/sandman-test-2"},
		listContainerReturn:  []string{"sandman-smoke-1", "sandman-smoke-2", "sandman-smoke-3"},
	}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--archived"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Removed 1 batch entries:") {
		t.Errorf("expected batch entries header, got: %s", out)
	}
	if !strings.Contains(out, "Removed 2 temp director(ies):") {
		t.Errorf("expected temp director(ies) header in same report block, got: %s", out)
	}
	if !strings.Contains(out, "Removed 3 container image(s):") {
		t.Errorf("expected container image(s) header in same report block, got: %s", out)
	}
	if strings.Contains(out, "Removed 1 batch entries, ") || strings.Contains(out, "Removed 2 temp director(y/ies), ") {
		t.Errorf("legacy 'Removed X, Y.' summary line should NOT appear, got: %s", out)
	}

	batchIdx := strings.Index(out, "Removed 1 batch entries:")
	tempIdx := strings.Index(out, "Removed 2 temp director(ies):")
	imgIdx := strings.Index(out, "Removed 3 container image(s):")
	if batchIdx < 0 || tempIdx < 0 || imgIdx < 0 {
		t.Fatalf("missing expected headers in: %s", out)
	}
	if !(batchIdx < tempIdx && tempIdx < imgIdx) {
		t.Errorf("expected order batch entries < temp dirs < container images in same report block, got: %s", out)
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
	if strings.Contains(out, "No batches to clean.") {
		t.Errorf("stale body should NOT print 'No batches to clean.' (helper handles nil actions gracefully), got: %s", out)
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
	cmd.SetArgs([]string{"--all", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Nothing to remove.") {
		t.Errorf("expected 'Nothing to remove.' message, got: %s", out)
	}
	if strings.Contains(out, "No batches to clean.") {
		t.Errorf("legacy 'No batches to clean.' should no longer appear, got: %s", out)
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
	if !strings.Contains(out, "Nothing to remove.") {
		t.Errorf("expected 'Nothing to remove.' message, got: %s", out)
	}
	if strings.Contains(out, "No orphaned batch directories found.") {
		t.Errorf("legacy 'No orphaned batch directories found.' should no longer appear, got: %s", out)
	}
	if strings.Contains(out, "Removed 0 orphaned") {
		t.Errorf("zero-orphan case should NOT print 'Removed 0 orphaned …' header, got: %s", out)
	}
}
