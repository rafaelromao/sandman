package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestClean_Stale_AloneAccepted(t *testing.T) {
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
		t.Fatalf("expected --stale alone to be accepted, got: %v", err)
	}
}

func TestClean_Stale_MutuallyExclusiveWithArchived(t *testing.T) {
	deps := newTestDeps(t)
	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale", "--archived"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --stale combined with --archived")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("expected error to mention --stale, got: %v", err)
	}
}

func TestClean_Stale_MutuallyExclusiveWithDryRun(t *testing.T) {
	deps := newTestDeps(t)
	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale", "--dry-run"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --stale combined with --dry-run")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("expected error to mention --stale, got: %v", err)
	}
}

func writeBatchManifest(t *testing.T, baseDir, runID string, issues []int, createdAt time.Time) {
	t.Helper()
	runDir := filepath.Join(baseDir, ".sandman", "batches", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	manifest := daemon.BatchManifest{Issues: issues, CreatedAt: createdAt}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func writeBatchIndex(t *testing.T, baseDir string, entries []batchindex.Batch) {
	t.Helper()
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: entries}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	batchesDir := filepath.Join(baseDir, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("mkdir batches dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatalf("write batches.json: %v", err)
	}
}

func writeRunManifest(t *testing.T, batchDir string, manifest batchindex.RunManifest) {
	t.Helper()
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatalf("mkdir batch dir: %v", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "run.json"), data, 0644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}
}

func TestClean_DryRun_ProducesNoIO(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-1")
	worktreeDir := filepath.Join(dir, ".sandman", "worktrees", "42-fix")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:        "batch-1",
		BatchID:      "batch-1",
		Issue:        42,
		Branch:       "42-fix",
		WorktreePath: worktreeDir,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-1", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all", "--dry-run"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreeDir); os.IsNotExist(err) {
		t.Errorf("worktree should NOT be removed by --dry-run")
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "batches", "batch-1")); os.IsNotExist(err) {
		t.Errorf("batch dir should NOT be removed by --dry-run")
	}
}

func TestClean_All_PreservesActiveEntries(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-active")
	batchArchived := filepath.Join(dir, ".sandman", "batches", "batch-archived")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "42-fix")
	worktreeArchived := filepath.Join(dir, ".sandman", "worktrees", "43-fix")

	if err := os.MkdirAll(worktreeActive, 0755); err != nil {
		t.Fatalf("create worktree active: %v", err)
	}
	if err := os.MkdirAll(worktreeArchived, 0755); err != nil {
		t.Fatalf("create worktree archived: %v", err)
	}

	writeRunManifest(t, batchActive, batchindex.RunManifest{
		RunID:        "batch-active",
		BatchID:      "batch-active",
		Issue:        42,
		Branch:       "42-fix",
		WorktreePath: worktreeActive,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	writeRunManifest(t, batchArchived, batchindex.RunManifest{
		RunID:        "batch-archived",
		BatchID:      "batch-archived",
		Issue:        43,
		Branch:       "43-fix",
		WorktreePath: worktreeArchived,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-active", Path: batchActive, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
		{ID: "batch-unavail", Path: "", Kind: batchindex.KindIssue, Status: batchindex.StatusUnavailable, CreatedAt: now},
		{ID: "batch-archived", Path: batchArchived, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
	})

	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = gr

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreeActive); os.IsNotExist(err) {
		t.Errorf("expected active worktree to be PRESERVED by --all, but it was removed")
	}
	if _, err := os.Stat(worktreeArchived); !os.IsNotExist(err) {
		t.Errorf("expected archived worktree to be removed")
	}
	if len(gr.removeWorktreeCalls) != 1 {
		t.Fatalf("expected 1 removeWorktree call (archived only), got %d", len(gr.removeWorktreeCalls))
	}
	if gr.removeWorktreeCalls[0] != worktreeArchived {
		t.Errorf("expected removeWorktree(%q), got %q", worktreeArchived, gr.removeWorktreeCalls[0])
	}

	var idx batchindex.Index
	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	json.Unmarshal(data, &idx)
	if len(idx.Batches) != 1 {
		t.Fatalf("expected 1 entry remaining (active), got %d", len(idx.Batches))
	}
	if idx.Batches[0].ID != "batch-active" {
		t.Errorf("expected remaining entry to be batch-active, got %s", idx.Batches[0].ID)
	}
}

// TestClean_All_RemovesCompletedActiveBatchWorktrees reproduces the gap
// where a batch whose runs are all terminal but whose batch-level
// Status is still `active` (because no one ever ran `archive batch` to
// promote it) is left untouched by `clean --all`. Per-row archiving
// (`archive run`, `archive older-than`, `archive stale`) marks each
// RunRecord archived but does NOT promote the batch-level Status; the
// batch dir and worktree persist indefinitely. `clean --all` is the
// operator escape hatch that should reclaim those resources.
func TestClean_All_RemovesCompletedActiveBatchWorktrees(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-completed")
	batchRunDir := filepath.Join(batchActive, "runs", "batch-completed")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "42-fix")

	if err := os.MkdirAll(batchRunDir, 0755); err != nil {
		t.Fatalf("create batch run dir: %v", err)
	}
	if err := os.MkdirAll(worktreeActive, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	writeRunManifest(t, batchRunDir, batchindex.RunManifest{
		RunID:        "batch-completed",
		BatchID:      "batch-completed",
		Issue:        42,
		Branch:       "42-fix",
		WorktreePath: worktreeActive,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusSuccess,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-completed", Path: batchActive, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = gr
	deps.RunActivityProbe = func(string) bool { return false }

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreeActive); !os.IsNotExist(err) {
		t.Errorf("expected worktree of completed-but-active batch to be removed by --all, but %s still exists", worktreeActive)
	}
	if len(gr.removeWorktreeCalls) != 1 {
		t.Fatalf("expected 1 removeWorktree call, got %d", len(gr.removeWorktreeCalls))
	}
	if gr.removeWorktreeCalls[0] != worktreeActive {
		t.Errorf("expected removeWorktree(%q), got %q", worktreeActive, gr.removeWorktreeCalls[0])
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	var idx batchindex.Index
	json.Unmarshal(data, &idx)
	if len(idx.Batches) != 0 {
		t.Errorf("expected batches index to be empty after --all, got %d entries", len(idx.Batches))
	}
}

// TestClean_All_ReclaimsBatchWhoseRowsWerePerRowArchived covers the
// all-rows-archived gap: when every row has been moved out of the live
// batch by per-row archive (`archive run`, `archive older-than`,
// `archive stale`), `runs/` is empty, the batch-level Status stays
// `active`, and the worktree would leak. `clean --all` must reconcile
// the archived RunRecords and reclaim the batch and its worktree from
// the archived run manifest.
func TestClean_All_ReclaimsBatchWhoseRowsWerePerRowArchived(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-archived-rows")
	archiveRunDir := filepath.Join(dir, ".sandman", "archive", "batch-archived-rows", "runs", "batch-archived-rows")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "42-fix")

	if err := os.MkdirAll(batchActive, 0755); err != nil {
		t.Fatalf("create batch dir: %v", err)
	}
	if err := os.MkdirAll(archiveRunDir, 0755); err != nil {
		t.Fatalf("create archive run dir: %v", err)
	}
	if err := os.MkdirAll(worktreeActive, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	writeRunManifest(t, archiveRunDir, batchindex.RunManifest{
		RunID:        "batch-archived-rows",
		BatchID:      "batch-archived-rows",
		Issue:        42,
		Branch:       "42-fix",
		WorktreePath: worktreeActive,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusSuccess,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{
			ID:        "batch-archived-rows",
			Path:      batchActive,
			Kind:      batchindex.KindIssue,
			Status:    batchindex.StatusActive,
			CreatedAt: now,
			Runs: []batchindex.RunRecord{{
				RunID:       "batch-archived-rows",
				Status:      batchindex.RunRecordStatusArchived,
				ArchivePath: filepath.Join(".sandman", "archive", "batch-archived-rows", "runs", "batch-archived-rows"),
			}},
		},
	})

	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = gr
	deps.RunActivityProbe = func(string) bool { return false }

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreeActive); !os.IsNotExist(err) {
		t.Errorf("expected worktree of all-rows-archived batch to be removed by --all, but %s still exists", worktreeActive)
	}
	if len(gr.removeWorktreeCalls) != 1 {
		t.Fatalf("expected 1 removeWorktree call, got %d", len(gr.removeWorktreeCalls))
	}
	if gr.removeWorktreeCalls[0] != worktreeActive {
		t.Errorf("expected removeWorktree(%q), got %q", worktreeActive, gr.removeWorktreeCalls[0])
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	var idx batchindex.Index
	json.Unmarshal(data, &idx)
	if len(idx.Batches) != 0 {
		t.Errorf("expected batches index to be empty after --all, got %d entries", len(idx.Batches))
	}
}

// TestClean_All_PreservesActiveBatchWithUnarchivedRow covers the
// fail-closed half of the all-rows-archived reconciliation: a batch
// whose runs dir is empty but whose RunRecords are NOT all archived
// must be preserved, because some row's state is unknown.
func TestClean_All_PreservesActiveBatchWithUnarchivedRow(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-mixed-rows")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "55-fix")

	if err := os.MkdirAll(batchActive, 0755); err != nil {
		t.Fatalf("create batch dir: %v", err)
	}
	if err := os.MkdirAll(worktreeActive, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{
			ID:        "batch-mixed-rows",
			Path:      batchActive,
			Kind:      batchindex.KindIssue,
			Status:    batchindex.StatusActive,
			CreatedAt: now,
			Runs: []batchindex.RunRecord{
				{RunID: "row-1", Status: batchindex.RunRecordStatusArchived, ArchivePath: filepath.Join(".sandman", "archive", "batch-mixed-rows", "runs", "row-1")},
				{RunID: "row-2", Status: batchindex.RunRecordStatusActive},
			},
		},
	})

	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = gr
	deps.RunActivityProbe = func(string) bool { return false }

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreeActive); os.IsNotExist(err) {
		t.Errorf("expected worktree of batch with unarchived row to be PRESERVED by --all, but it was removed")
	}
	if len(gr.removeWorktreeCalls) != 0 {
		t.Errorf("expected 0 removeWorktree calls for batch with unarchived row, got %d", len(gr.removeWorktreeCalls))
	}
}

// TestClean_All_PreservesBatchWithMissingArchivedRunManifest covers
// the other fail-closed case: an archived RunRecord whose archive
// directory exists but whose run.json is missing cannot identify its
// worktree or branch, so the batch must remain untouched.
func TestClean_All_PreservesBatchWithMissingArchivedRunManifest(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-missing-manifest")
	archiveRunDir := filepath.Join(dir, ".sandman", "archive", "batch-missing-manifest", "runs", "row-1")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "55-fix")

	for _, d := range []string{batchActive, archiveRunDir, worktreeActive} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("create dir %s: %v", d, err)
		}
	}

	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{
			ID:        "batch-missing-manifest",
			Path:      batchActive,
			Kind:      batchindex.KindIssue,
			Status:    batchindex.StatusActive,
			CreatedAt: now,
			Runs: []batchindex.RunRecord{{
				RunID:       "row-1",
				Status:      batchindex.RunRecordStatusArchived,
				ArchivePath: filepath.Join(".sandman", "archive", "batch-missing-manifest", "runs", "row-1"),
			}},
		},
	})

	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = gr
	deps.RunActivityProbe = func(string) bool { return false }

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(batchActive); err != nil {
		t.Errorf("expected batch with missing archived manifest to be preserved, got: %v", err)
	}
	if _, err := os.Stat(worktreeActive); os.IsNotExist(err) {
		t.Errorf("expected worktree of batch with missing archived manifest to be preserved, but it was removed")
	}
	if len(gr.removeWorktreeCalls) != 0 {
		t.Errorf("expected 0 removeWorktree calls for missing archived manifest, got %d", len(gr.removeWorktreeCalls))
	}
}

func TestClean_All_PreservesActiveBatchWithInvalidRunManifest(t *testing.T) {
	tests := []struct {
		name       string
		archived   bool
		manifestID string
		batchID    string
		branch     string
	}{
		{name: "mismatched live run id", manifestID: "other-row", branch: "42-fix"},
		{name: "missing live branch", manifestID: "row-1"},
		{name: "mismatched archived batch id", archived: true, manifestID: "row-1", batchID: "other-batch", branch: "42-fix"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := newRunDepsAuto(t, &fakeBatchRunner{})
			dir, err := os.Getwd()
			if err != nil {
				t.Fatalf("os.Getwd: %v", err)
			}

			batchID := "batch-invalid"
			if tt.archived {
				batchID += "-archived"
			} else {
				batchID += "-live"
			}
			batchDir := filepath.Join(dir, ".sandman", "batches", batchID)
			worktree := filepath.Join(dir, ".sandman", "worktrees", batchID)
			manifestDir := filepath.Join(batchDir, "runs", "row-1")
			var records []batchindex.RunRecord
			if tt.archived {
				manifestDir = filepath.Join(dir, ".sandman", "archive", batchID, "runs", "row-1")
				records = []batchindex.RunRecord{{
					RunID:       "row-1",
					Status:      batchindex.RunRecordStatusArchived,
					ArchivePath: filepath.Join(".sandman", "archive", batchID, "runs", "row-1"),
				}}
			}
			if err := os.MkdirAll(batchDir, 0755); err != nil {
				t.Fatalf("create batch dir: %v", err)
			}
			if err := os.MkdirAll(manifestDir, 0755); err != nil {
				t.Fatalf("create manifest dir: %v", err)
			}
			if err := os.MkdirAll(worktree, 0755); err != nil {
				t.Fatalf("create worktree: %v", err)
			}

			manifestBatchID := batchID
			if tt.batchID != "" {
				manifestBatchID = tt.batchID
			}
			writeRunManifest(t, manifestDir, batchindex.RunManifest{
				RunID:        tt.manifestID,
				BatchID:      manifestBatchID,
				Branch:       tt.branch,
				WorktreePath: worktree,
				Kind:         batchindex.KindIssue,
				Status:       batchindex.RunManifestStatusSuccess,
			})
			writeBatchIndex(t, dir, []batchindex.Batch{{
				ID:        batchID,
				Path:      batchDir,
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now(),
				Runs:      records,
			}})

			gr := &fakeGitRunner{}
			deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
			eventRunID := "row-1"
			if tt.archived {
				eventRunID = batchID
			}
			deps.EventLog = &fakeEventLog{events: []events.Event{{Type: "run.finished", RunID: eventRunID}}}
			deps.GitRunner = gr
			deps.RunActivityProbe = func(string) bool { return false }

			cmd := NewCleanCmd(deps)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"--all"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, err := os.Stat(batchDir); err != nil {
				t.Fatalf("expected invalid-manifest batch to be preserved, got: %v\noutput: %s", err, output.String())
			}
			if _, err := os.Stat(worktree); err != nil {
				t.Fatalf("expected invalid-manifest worktree to be preserved, got: %v", err)
			}
			if len(gr.removeWorktreeCalls) != 0 {
				t.Fatalf("expected no worktree cleanup for invalid manifest, got %v", gr.removeWorktreeCalls)
			}
		})
	}
}

// TestClean_All_RemovesAllWorktreesInMultiRunBatch covers the
// multi-run gap: a batch whose rows each own a distinct worktree and
// branch must have every pair removed, not just the first. Orphaning
// sibling worktrees/branches would leak their sandbox state.
func TestClean_All_RemovesAllWorktreesInMultiRunBatch(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-multi-run")
	run1Dir := filepath.Join(batchActive, "runs", "run-1")
	run2Dir := filepath.Join(batchActive, "runs", "run-2")
	worktree1 := filepath.Join(dir, ".sandman", "worktrees", "42-fix")
	worktree2 := filepath.Join(dir, ".sandman", "worktrees", "43-fix")

	for _, d := range []string{run1Dir, run2Dir, worktree1, worktree2} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("create dir %s: %v", d, err)
		}
	}

	writeRunManifest(t, run1Dir, batchindex.RunManifest{
		RunID:        "run-1",
		BatchID:      "batch-multi-run",
		Issue:        42,
		Branch:       "42-fix",
		WorktreePath: worktree1,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusSuccess,
	})
	writeRunManifest(t, run2Dir, batchindex.RunManifest{
		RunID:        "run-2",
		BatchID:      "batch-multi-run",
		Issue:        43,
		Branch:       "43-fix",
		WorktreePath: worktree2,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusFailure,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-multi-run", Path: batchActive, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = gr
	deps.RunActivityProbe = func(string) bool { return false }

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gr.removeWorktreeCalls) != 2 {
		t.Fatalf("expected 2 removeWorktree calls, got %d: %v", len(gr.removeWorktreeCalls), gr.removeWorktreeCalls)
	}
	for _, wt := range []string{worktree1, worktree2} {
		found := false
		for _, call := range gr.removeWorktreeCalls {
			if call == wt {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected removeWorktree(%q) to be called, calls=%v", wt, gr.removeWorktreeCalls)
		}
	}
	if len(gr.deleteBranchCalls) != 2 {
		t.Errorf("expected 2 deleteBranch calls, got %d: %v", len(gr.deleteBranchCalls), gr.deleteBranchCalls)
	}
	if _, err := os.Stat(worktree1); !os.IsNotExist(err) {
		t.Errorf("expected worktree1 %s to be removed, but it exists", worktree1)
	}
	if _, err := os.Stat(worktree2); !os.IsNotExist(err) {
		t.Errorf("expected worktree2 %s to be removed, but it exists", worktree2)
	}
}

// TestClean_All_PreservesActiveBatchWithLiveDaemon complements the
// "active entries preserved" guarantee by exercising the new
// eligible-active path: a batch whose runs are terminal but whose
// daemon is still alive must NOT be reclaimed. This protects against
// over-aggressive reclamation that would race against a daemon that
// is restarting between socket probes.
func TestClean_All_PreservesActiveBatchWithLiveDaemon(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-live")
	batchRunDir := filepath.Join(batchActive, "runs", "batch-live")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "99-fix")

	if err := os.MkdirAll(batchRunDir, 0755); err != nil {
		t.Fatalf("create batch run dir: %v", err)
	}
	if err := os.MkdirAll(worktreeActive, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	writeRunManifest(t, batchRunDir, batchindex.RunManifest{
		RunID:        "batch-live",
		BatchID:      "batch-live",
		Issue:        99,
		Branch:       "99-fix",
		WorktreePath: worktreeActive,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusSuccess,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-live", Path: batchActive, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = gr
	deps.RunActivityProbe = func(string) bool { return true }

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreeActive); os.IsNotExist(err) {
		t.Errorf("expected worktree of live-daemon batch to be PRESERVED by --all, but it was removed")
	}
	if len(gr.removeWorktreeCalls) != 0 {
		t.Errorf("expected 0 removeWorktree calls for live daemon, got %d", len(gr.removeWorktreeCalls))
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	var idx batchindex.Index
	json.Unmarshal(data, &idx)
	if len(idx.Batches) != 1 || idx.Batches[0].ID != "batch-live" {
		t.Errorf("expected batches index to keep batch-live, got %d entries: %+v", len(idx.Batches), idx.Batches)
	}
}

// TestClean_All_PreservesActiveBatchWithNonTerminalRun covers the
// run-side terminal check: a batch with a live daemon gone but a
// run still in active status must NOT be reclaimed, because that run
// could be mid-flight or queued.
func TestClean_All_PreservesActiveBatchWithNonTerminalRun(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-inflight")
	batchRunDir := filepath.Join(batchActive, "runs", "batch-inflight")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "77-fix")

	if err := os.MkdirAll(batchRunDir, 0755); err != nil {
		t.Fatalf("create batch run dir: %v", err)
	}
	if err := os.MkdirAll(worktreeActive, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	writeRunManifest(t, batchRunDir, batchindex.RunManifest{
		RunID:        "batch-inflight",
		BatchID:      "batch-inflight",
		Issue:        77,
		Branch:       "77-fix",
		WorktreePath: worktreeActive,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-inflight", Path: batchActive, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = gr
	deps.RunActivityProbe = func(string) bool { return false }

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreeActive); os.IsNotExist(err) {
		t.Errorf("expected worktree of in-flight batch to be PRESERVED by --all, but it was removed")
	}
	if len(gr.removeWorktreeCalls) != 0 {
		t.Errorf("expected 0 removeWorktree calls for in-flight run, got %d", len(gr.removeWorktreeCalls))
	}
}

func TestClean_All_RunsEveryPass(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-dead-1", []int{42}, createdAt)

	batchArchived := filepath.Join(dir, ".sandman", "batches", "batch-archived")
	worktreeArchived := filepath.Join(dir, ".sandman", "worktrees", "43-fix")
	if err := os.MkdirAll(worktreeArchived, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	writeRunManifest(t, batchArchived, batchindex.RunManifest{
		RunID:        "batch-archived",
		BatchID:      "batch-archived",
		Issue:        43,
		Branch:       "43-fix",
		WorktreePath: worktreeArchived,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-archived", Path: batchArchived, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
	})

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "42-fix"}},
	}}
	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = log
	deps.GitRunner = gr
	fakeTC := &fakeTempCleaner{
		scanTempDirsReturn: []string{"/tmp/sandman-smoke-prewarm-allpass"},
	}
	deps.TempCleaner = fakeTC

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fakeTC.scanTempDirsCalled {
		t.Errorf("expected temp sweep to run as part of --all")
	}

	if len(gr.removeWorktreeCalls) != 1 {
		t.Errorf("expected archived worktree to be removed by --all, got %d removeWorktree calls", len(gr.removeWorktreeCalls))
	}

	var aborted int
	for _, e := range log.logged {
		if e.Type == "run.aborted" {
			aborted++
		}
	}
	if aborted != 1 {
		t.Errorf("expected 1 run.aborted event from stale pass, got %d", aborted)
	}

	if !strings.Contains(buf.String(), "Recovered 1 stale runs") {
		t.Errorf("expected stale summary in --all output, got: %s", buf.String())
	}
}

func TestClean_All_DryRun_PrintsAllPasses(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-dead-1", []int{42}, createdAt)

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-active")
	batchArchived := filepath.Join(dir, ".sandman", "batches", "batch-archived")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "42-fix")
	worktreeArchived := filepath.Join(dir, ".sandman", "worktrees", "43-fix")
	if err := os.MkdirAll(worktreeActive, 0755); err != nil {
		t.Fatalf("create active worktree: %v", err)
	}
	if err := os.MkdirAll(worktreeArchived, 0755); err != nil {
		t.Fatalf("create archived worktree: %v", err)
	}
	writeRunManifest(t, batchActive, batchindex.RunManifest{
		RunID:        "batch-active",
		BatchID:      "batch-active",
		Issue:        42,
		Branch:       "42-fix",
		WorktreePath: worktreeActive,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	writeRunManifest(t, batchArchived, batchindex.RunManifest{
		RunID:        "batch-archived",
		BatchID:      "batch-archived",
		Issue:        43,
		Branch:       "43-fix",
		WorktreePath: worktreeArchived,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-active", Path: batchActive, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
		{ID: "batch-archived", Path: batchArchived, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
	})

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "42-fix"}},
	}}
	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = log
	deps.GitRunner = gr
	fakeTC := &fakeTempCleaner{
		scanTempDirsReturn: []string{"/tmp/sandman-smoke-prewarm-dryrun-all"},
	}
	deps.TempCleaner = fakeTC

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Recovered 1 stale runs") {
		t.Errorf("expected stale summary in dry-run output, got: %s", out)
	}
	if !strings.Contains(out, "Would remove") {
		t.Errorf("expected 'Would remove' phrasing for archived pass in dry-run output, got: %s", out)
	}
	if !strings.Contains(out, "batch-archived") {
		t.Errorf("expected batch-archived mentioned in dry-run report, got: %s", out)
	}
	if !strings.Contains(out, "temp director") {
		t.Errorf("expected temp sweep section in dry-run report, got: %s", out)
	}

	if fakeTC.removeTempDirCalled {
		t.Errorf("expected RemoveTempDir NOT to be called in dry-run mode")
	}
	if len(gr.removeWorktreeCalls) != 0 {
		t.Errorf("expected no removeWorktree calls in dry-run mode, got %d", len(gr.removeWorktreeCalls))
	}
	if _, err := os.Stat(worktreeActive); os.IsNotExist(err) {
		t.Errorf("active worktree should NOT be removed by --dry-run")
	}
	if _, err := os.Stat(worktreeArchived); os.IsNotExist(err) {
		t.Errorf("archived worktree should NOT be removed by --dry-run")
	}

	var idx batchindex.Index
	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	json.Unmarshal(data, &idx)
	if len(idx.Batches) != 2 {
		t.Errorf("expected batches index to be unchanged by --dry-run, got %d entries", len(idx.Batches))
	}
}

func TestClean_Archived_RemovesArchivedAndUnavailableEntries(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-active")
	batchArchived := filepath.Join(dir, ".sandman", "batches", "batch-archived")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "42-fix")
	worktreeArchived := filepath.Join(dir, ".sandman", "worktrees", "43-fix")

	if err := os.MkdirAll(worktreeActive, 0755); err != nil {
		t.Fatalf("create worktree active: %v", err)
	}
	if err := os.MkdirAll(worktreeArchived, 0755); err != nil {
		t.Fatalf("create worktree archived: %v", err)
	}

	writeRunManifest(t, batchActive, batchindex.RunManifest{
		RunID:        "batch-active",
		BatchID:      "batch-active",
		Issue:        42,
		Branch:       "42-fix",
		WorktreePath: worktreeActive,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	writeRunManifest(t, batchArchived, batchindex.RunManifest{
		RunID:        "batch-archived",
		BatchID:      "batch-archived",
		Issue:        43,
		Branch:       "43-fix",
		WorktreePath: worktreeArchived,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-active", Path: batchActive, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
		{ID: "batch-unavail", Path: "", Kind: batchindex.KindIssue, Status: batchindex.StatusUnavailable, CreatedAt: now},
		{ID: "batch-archived", Path: batchArchived, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
	})

	gr := &fakeGitRunner{}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = gr

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--archived"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(worktreeActive); os.IsNotExist(err) {
		t.Errorf("expected active worktree to be preserved")
	}
	if _, err := os.Stat(worktreeArchived); !os.IsNotExist(err) {
		t.Errorf("expected archived worktree to be removed")
	}

	var idx batchindex.Index
	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	json.Unmarshal(data, &idx)
	if len(idx.Batches) != 1 {
		t.Errorf("expected 1 entry remaining (active), got %d", len(idx.Batches))
	}
	if idx.Batches[0].ID != "batch-active" {
		t.Errorf("expected remaining entry to be batch-active, got %s", idx.Batches[0].ID)
	}
}

func TestClean_Unavailable_ReapedByBothCleanAndArchived(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-unavail", Path: "", Kind: batchindex.KindIssue, Status: batchindex.StatusUnavailable, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var idx batchindex.Index
	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	json.Unmarshal(data, &idx)
	if len(idx.Batches) != 0 {
		t.Errorf("expected unavailable entry to be removed, got %d entries", len(idx.Batches))
	}
}

func TestClean_Stale_NoIndexChange(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-1")
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:   "batch-1",
		BatchID: "batch-1",
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-1", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var idx batchindex.Index
	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	json.Unmarshal(data, &idx)
	if len(idx.Batches) != 1 {
		t.Errorf("expected index to be unchanged (1 entry), got %d entries", len(idx.Batches))
	}
}

func TestRecoverStaleRuns_DeadBatchUnterminated_EmitsAborted(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-dead-1", []int{42, 43}, createdAt)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "42-fix"}},
		{Type: "run.started", RunID: "run-43", Issue: 43, Timestamp: started, Payload: map[string]any{"branch": "43-fix"}},
	}}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = log
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 2 {
		t.Fatalf("expected 2 run.aborted events, got %d: %+v", got, log.logged)
	}
	for _, e := range log.logged {
		if e.Type != "run.aborted" {
			t.Errorf("expected type run.aborted, got %q", e.Type)
		}
		recovered, ok := e.Payload["recovered"].(bool)
		if !ok || !recovered {
			t.Errorf("expected payload.recovered=true, got %v", e.Payload)
		}
		if e.IssueRef == nil || (*e.IssueRef != 42 && *e.IssueRef != 43) {
			t.Errorf("expected IssueRef to point to 42 or 43, got %v", e.IssueRef)
		}
	}
	if !strings.Contains(buf.String(), "Recovered 2 stale runs as aborted across 1 dead directories.") {
		t.Errorf("expected summary, got: %s", buf.String())
	}
}

func TestRecoverStaleRuns_LiveBatch_NoEventEmitted(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	runDir := filepath.Join(dir, ".sandman", "batches", "run-live-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	manifest := daemon.BatchManifest{Issues: []int{42}, CreatedAt: createdAt}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	ctlSocket := daemon.NewControlSocket(runDir, daemon.NewBroadcaster())
	if err := ctlSocket.Start(); err != nil {
		t.Fatalf("start control socket: %v", err)
	}
	defer ctlSocket.Stop()

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "42-fix"}},
	}}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = log
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 0 {
		t.Errorf("expected 0 logged events for live batch, got %d: %+v", got, log.logged)
	}
	if !strings.Contains(buf.String(), "Recovered 0 stale runs") {
		t.Errorf("expected summary to report 0 recovered, got: %s", buf.String())
	}
}

func TestRecoverStaleRuns_RunStartedBeforeManifestCreatedAt_RecoveredAsOrphan(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(-1 * time.Hour)
	writeBatchManifest(t, dir, "run-old", []int{42}, createdAt)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "42-fix"}},
	}}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = log
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 1 {
		t.Errorf("expected 1 logged event for orphaned run, got %d: %+v", got, log.logged)
	}
	if got := log.logged[0].Type; got != "run.aborted" {
		t.Errorf("expected run.aborted, got %s", got)
	}
}

func TestRecoverStaleRuns_AlreadyTerminated_NoEventEmitted(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-finished", []int{42}, createdAt)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "42-fix"}},
		{Type: "run.finished", RunID: "run-42", Issue: 42, Timestamp: started.Add(time.Hour), Payload: map[string]any{"status": "success"}},
	}}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = log
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 0 {
		t.Errorf("expected 0 logged events for terminated run, got %d: %+v", got, log.logged)
	}
}

func TestRecoverStaleRuns_ContinuedResetsStartedTimestamp(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	firstStart := createdAt.Add(-2 * time.Hour)
	continuedAt := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-cont-1", []int{42}, createdAt)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: firstStart, Payload: map[string]any{"branch": "42-fix"}},
		{Type: "run.continued", RunID: "run-42", Issue: 42, Timestamp: continuedAt, Payload: map[string]any{"branch": "42-fix"}},
	}}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = log
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 1 {
		t.Fatalf("expected 1 logged event for continued run inside window, got %d: %+v", got, log.logged)
	}
	if log.logged[0].Type != "run.aborted" {
		t.Errorf("expected type run.aborted, got %q", log.logged[0].Type)
	}
}

func TestRecoverStaleRuns_MultipleDeadBatches(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdA := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	createdB := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	writeBatchManifest(t, dir, "run-a", []int{1}, createdA)
	writeBatchManifest(t, dir, "run-b", []int{2}, createdB)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-1", Issue: 1, Timestamp: createdA.Add(time.Minute)},
		{Type: "run.started", RunID: "run-2", Issue: 2, Timestamp: createdB.Add(time.Minute)},
	}}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = log
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(log.logged); got != 2 {
		t.Fatalf("expected 2 logged events across two dead batches, got %d", got)
	}
	if !strings.Contains(buf.String(), "Recovered 2 stale runs as aborted across 2 dead directories.") {
		t.Errorf("expected summary to count 2 dirs, got: %s", buf.String())
	}
}

func TestRecoverStaleRuns_JSONRoundTripPreservesIssue(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)
	writeBatchManifest(t, dir, "run-rt-1", []int{42}, createdAt)

	logFile := filepath.Join(dir, ".sandman", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(logFile), 0755); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	logger := &events.JSONLLogger{Path: logFile}
	initial := []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "42-fix"}},
	}
	for _, e := range initial {
		if err := logger.Log(e); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	readBack, err := logger.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = logger
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	persisted, err := logger.Read()
	if err != nil {
		t.Fatalf("read events after recover: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("expected 2 persisted events (start + recovered abort), got %d", len(persisted))
	}
	var last events.Event
	for _, e := range persisted {
		last = e
	}
	if last.Type != "run.aborted" {
		t.Errorf("expected last persisted event to be run.aborted, got %q", last.Type)
	}
	if last.IssueRef == nil || *last.IssueRef != 42 {
		t.Errorf("expected IssueRef=42 in persisted run.aborted, got %v", last.IssueRef)
	}
	if recovered, _ := last.Payload["recovered"].(bool); !recovered {
		t.Errorf("expected payload.recovered=true in persisted run.aborted, got %v", last.Payload)
	}

	_ = readBack
}

func TestClean_DryRunArchived_PrintsIntendedDeletions(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchDir := filepath.Join(dir, ".sandman", "batches", "batch-archived")
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:   "batch-archived",
		BatchID: "batch-archived",
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-archived", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dry-run", "--archived"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(batchDir); os.IsNotExist(err) {
		t.Errorf("batch dir should NOT be removed by --dry-run --archived")
	}
	if !strings.Contains(buf.String(), "batch-archived") {
		t.Errorf("expected dry-run output to mention batch-archived, got: %s", buf.String())
	}
}

func TestClean_Orphaned_RemovesOrphanDirAndPrunesIndex(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	orphanDir := filepath.Join(dir, ".sandman", "batches", "orphan-x")
	liveDir := filepath.Join(dir, ".sandman", "batches", "live-y")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "batch.json"), []byte(`{"createdAt":"2026-07-02T00:00:00Z","batchId":"orphan-x"}`), 0o644); err != nil {
		t.Fatalf("write orphan manifest: %v", err)
	}
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatalf("mkdir live: %v", err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "batch.json"), []byte(`{"createdAt":"2026-07-02T00:00:00Z","batchId":"live-y"}`), 0o644); err != nil {
		t.Fatalf("write live manifest: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(liveDir, "runs", "live-run-y"), 0o755); err != nil {
		t.Fatalf("mkdir live run: %v", err)
	}
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "orphan-x", Path: orphanDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
		{ID: "live-y", Path: liveDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "live-run-y", Timestamp: now},
	}}
	deps.GitRunner = &fakeGitRunner{}

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--orphaned"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan dir should be removed, got err=%v", err)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Errorf("live dir should NOT be removed, got err=%v", err)
	}

	var idx batchindex.Index
	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("read index: %v", err)
	}
	if len(idx.Batches) != 1 || idx.Batches[0].ID != "live-y" {
		t.Errorf("expected index to keep only live-y, got %#v", idx.Batches)
	}
}

func TestClean_Orphaned_DryRun_NoIOAndKeepsIndex(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	orphanDir := filepath.Join(dir, ".sandman", "batches", "orphan-x")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "batch.json"), []byte(`{"createdAt":"2026-07-02T00:00:00Z","batchId":"orphan-x"}`), 0o644); err != nil {
		t.Fatalf("write orphan manifest: %v", err)
	}
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "orphan-x", Path: orphanDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
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

	if _, err := os.Stat(orphanDir); err != nil {
		t.Errorf("orphan dir should NOT be removed by --dry-run, got err=%v", err)
	}
	if !strings.Contains(buf.String(), "orphan-x") {
		t.Errorf("expected dry-run output to mention orphan-x, got: %s", buf.String())
	}

	var idx batchindex.Index
	data, _ := os.ReadFile(filepath.Join(dir, ".sandman", "batches.json"))
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("read index: %v", err)
	}
	if len(idx.Batches) != 1 {
		t.Errorf("expected index to be unchanged (1 entry), got %d", len(idx.Batches))
	}
}

func TestClean_Orphaned_MutuallyExclusiveWithStale(t *testing.T) {
	deps := newTestDeps(t)
	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--orphaned", "--stale"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --orphaned combined with --stale")
	}
	if !strings.Contains(err.Error(), "orphaned") {
		t.Errorf("expected error to mention --orphaned, got: %v", err)
	}
}

func TestClean_Orphaned_MutuallyExclusiveWithArchived(t *testing.T) {
	deps := newTestDeps(t)
	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--orphaned", "--archived"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --orphaned combined with --archived")
	}
	if !strings.Contains(err.Error(), "orphaned") {
		t.Errorf("expected error to mention --orphaned, got: %v", err)
	}
}
