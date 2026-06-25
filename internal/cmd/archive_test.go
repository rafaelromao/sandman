package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

func writeBatchIndexForArchive(t *testing.T, baseDir string, entries []batchindex.Entry) {
	t.Helper()
	idx := batchindex.Index{Version: batchindex.IndexVersion, Entries: entries}
	if err := idx.Save(filepath.Join(baseDir, ".sandman", "batches.json")); err != nil {
		t.Fatalf("save batches.json: %v", err)
	}
}

func loadBatchIndexForArchive(t *testing.T, baseDir string) *batchindex.Index {
	t.Helper()
	idx, err := batchindex.Load(filepath.Join(baseDir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("load batches index: %v", err)
	}
	return idx
}

func writeBatchDirForArchive(t *testing.T, batchDir string, runManifest batchindex.RunManifest) {
	t.Helper()
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatalf("mkdir batch dir: %v", err)
	}
	data, err := json.MarshalIndent(runManifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "run.json"), data, 0644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}
}

func TestArchiveRun_NoArgsReturnsUsageError(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when run has no id")
	}
	var target *UsageError
	if !errors.As(err, &target) {
		t.Fatalf("expected *UsageError, got %T: %v", err, err)
	}
}

func TestArchiveStale_NoArgsAcceptsNone(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var buf bytes.Buffer
	deps := newTestDeps()
	deps.EventLog = &fakeEventLog{}
	cmd := NewArchiveCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error from 'archive stale' with no args: %v", err)
	}
}

func TestArchiveBatch_NonexistentBatchReturnsError(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	writeBatchIndexForArchive(t, dir, nil)

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"batch", "missing-1"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for nonexistent batch, got nil")
	}

	archiveDir := filepath.Join(dir, ".sandman", "archive")
	if _, err := os.Stat(archiveDir); !os.IsNotExist(err) {
		t.Errorf("expected archive dir to NOT be created when source does not exist, got stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveDir, "missing-1")); !os.IsNotExist(err) {
		t.Errorf("expected no archive/missing-1, got stat err: %v", err)
	}
}

func TestArchiveRun_NonexistentRunReturnsError(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	writeBatchIndexForArchive(t, dir, nil)

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "missing-1"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for nonexistent run, got nil")
	}

	archiveDir := filepath.Join(dir, ".sandman", "archive")
	if _, err := os.Stat(archiveDir); !os.IsNotExist(err) {
		t.Errorf("expected archive dir to NOT be created when source does not exist, got stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveDir, "missing-1")); !os.IsNotExist(err) {
		t.Errorf("expected no archive/missing-1, got stat err: %v", err)
	}
}

func TestArchiveRun_LiveRunReturnsError(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "live-1")
	now := time.Now()
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "live-1",
		Kind:      batchindex.KindIssue,
		CreatedAt: now,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "live-1", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	ctlSocket := daemon.NewControlSocket(batchDir, daemon.NewBroadcaster())
	if err := ctlSocket.Start(); err != nil {
		t.Fatalf("start control socket: %v", err)
	}
	defer ctlSocket.Stop()

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "live-1"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for live run, got nil")
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected live batch dir to be preserved on rejection, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "live-1")); !os.IsNotExist(err) {
		t.Errorf("expected no archive/live-1 after rejection, got: %v", err)
	}
}

func TestArchiveRun_DeadRunMovesDirectory(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "dead-1")
	now := time.Now()
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "dead-1",
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: now,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "dead-1", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	})

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "dead-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	archiveBatchDir := filepath.Join(dir, ".sandman", "archive", "dead-1")
	if _, err := os.Stat(archiveBatchDir); err != nil {
		t.Fatalf("expected archived batch dir to exist: %v", err)
	}
	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Errorf("expected source batch dir to be gone after archive, got: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(archiveBatchDir, "run.json"))
	if err != nil {
		t.Fatalf("read archived run.json: %v", err)
	}
	var manifest batchindex.RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if manifest.Issue != 42 {
		t.Errorf("archived run.json issue = %v, want 42", manifest.Issue)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive")); err != nil {
		t.Errorf("expected .sandman/archive/ to exist after archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "batches.json.bak")); err != nil {
		t.Errorf("expected batches.json.bak after archive save, got: %v", err)
	}

	idx := loadBatchIndexForArchive(t, dir)
	entry := idx.Resolve("dead-1")
	if entry == nil {
		t.Fatal("expected archived entry in batches index")
	}
	if entry.Status != batchindex.StatusArchived {
		t.Fatalf("archived entry status = %s, want %s", entry.Status, batchindex.StatusArchived)
	}
	wantPath := filepath.Join(".sandman", "archive", "dead-1")
	if entry.Path != wantPath {
		t.Fatalf("archived entry path = %q, want %q", entry.Path, wantPath)
	}
	if entry.ArchivedAt == nil {
		t.Fatal("expected archived entry archivedAt to be set")
	}
}

func TestArchiveRun_NoSocketsInArchive(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "socket-test")
	now := time.Now()
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "socket-test",
		Issue:     99,
		Kind:      batchindex.KindIssue,
		CreatedAt: now,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "socket-test", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{99}},
	})

	batchSockPath := filepath.Join(batchDir, "batch.sock")
	runSockPath := filepath.Join(batchDir, "run.sock")
	if err := os.WriteFile(batchSockPath, []byte(""), 0644); err != nil {
		t.Fatalf("create batch.sock: %v", err)
	}
	if err := os.WriteFile(runSockPath, []byte(""), 0644); err != nil {
		t.Fatalf("create run.sock: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "socket-test"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive command failed: %v", err)
	}

	archiveBatchDir := filepath.Join(dir, ".sandman", "archive", "socket-test")
	sockets, err := filepathGlobRecursive(archiveBatchDir, "*sock*")
	if err != nil {
		t.Fatalf("globbing archive for socket files: %v", err)
	}
	if len(sockets) > 0 {
		t.Errorf("archive contains socket files: %v", sockets)
	}
}

func filepathGlobRecursive(root, pattern string) ([]string, error) {
	var matches []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		matched, err := filepath.Match(pattern, filepath.Base(path))
		if err != nil {
			return err
		}
		if matched {
			matches = append(matches, path)
		}
		return nil
	})
	return matches, err
}

func TestArchiveBatch_LiveBatchReturnsError(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "live-1")
	now := time.Now()
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "live-1",
		Kind:      batchindex.KindIssue,
		CreatedAt: now,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "live-1", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	ctlSocket := daemon.NewControlSocket(batchDir, daemon.NewBroadcaster())
	if err := ctlSocket.Start(); err != nil {
		t.Fatalf("start control socket: %v", err)
	}
	defer ctlSocket.Stop()

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"batch", "live-1"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for live batch, got nil")
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected live batch dir to be preserved on rejection, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "live-1")); !os.IsNotExist(err) {
		t.Errorf("expected no archive/live-1 after rejection, got: %v", err)
	}
}

func TestArchiveBatch_DeadBatchMovesDirectory(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "dead-1")
	now := time.Now()
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "dead-1",
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: now,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "dead-1", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	})

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"batch", "dead-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	archiveBatchDir := filepath.Join(dir, ".sandman", "archive", "dead-1")
	if _, err := os.Stat(archiveBatchDir); err != nil {
		t.Fatalf("expected archived batch dir to exist: %v", err)
	}
	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Errorf("expected source batch dir to be gone after archive, got: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(archiveBatchDir, "run.json"))
	if err != nil {
		t.Fatalf("read archived run.json: %v", err)
	}
	var manifest batchindex.RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if manifest.Issue != 42 {
		t.Errorf("archived run.json issue = %v, want 42", manifest.Issue)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive")); err != nil {
		t.Errorf("expected .sandman/archive/ to exist after archive: %v", err)
	}

	idx := loadBatchIndexForArchive(t, dir)
	entry := idx.Resolve("dead-1")
	if entry == nil {
		t.Fatal("expected archived entry in batches index")
	}
	if entry.Status != batchindex.StatusArchived {
		t.Fatalf("archived entry status = %s, want %s", entry.Status, batchindex.StatusArchived)
	}
	wantPath := filepath.Join(".sandman", "archive", "dead-1")
	if entry.Path != wantPath {
		t.Fatalf("archived entry path = %q, want %q", entry.Path, wantPath)
	}
	if entry.ArchivedAt == nil {
		t.Fatal("expected archived entry archivedAt to be set")
	}
}

func TestArchiveBatch_CollisionWithExistingArchiveDirReturnsError(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "dead-2")
	now := time.Now()
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "dead-2",
		Kind:      batchindex.KindIssue,
		CreatedAt: now,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "dead-2", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	existingArchive := filepath.Join(dir, ".sandman", "archive", "dead-2")
	if err := os.MkdirAll(existingArchive, 0755); err != nil {
		t.Fatalf("mkdir existing archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(existingArchive, "sentinel.txt"), []byte("preserved"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"batch", "dead-2"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when destination already exists, got nil")
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected source batch dir preserved on collision, got: %v", err)
	}
	sentinel, err := os.ReadFile(filepath.Join(existingArchive, "sentinel.txt"))
	if err != nil {
		t.Fatalf("expected existing archive sentinel preserved, got: %v", err)
	}
	if string(sentinel) != "preserved" {
		t.Errorf("expected existing archive sentinel untouched, got %q", string(sentinel))
	}
}

func TestArchiveRun_CollisionWithExistingArchiveDirReturnsError(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "dead-2")
	now := time.Now()
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "dead-2",
		Kind:      batchindex.KindIssue,
		CreatedAt: now,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "dead-2", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now},
	})

	existingArchive := filepath.Join(dir, ".sandman", "archive", "dead-2")
	if err := os.MkdirAll(existingArchive, 0755); err != nil {
		t.Fatalf("mkdir existing archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(existingArchive, "sentinel.txt"), []byte("preserved"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "dead-2"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when destination already exists, got nil")
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected source batch dir preserved on collision, got: %v", err)
	}
	sentinel, err := os.ReadFile(filepath.Join(existingArchive, "sentinel.txt"))
	if err != nil {
		t.Fatalf("expected existing archive sentinel preserved, got: %v", err)
	}
	if string(sentinel) != "preserved" {
		t.Errorf("expected existing archive sentinel untouched, got %q", string(sentinel))
	}
}

func TestArchiveOlderThan_NoRunsLeavesEmptyArchiveDir(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	writeBatchIndexForArchive(t, dir, nil)

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	archiveDir := filepath.Join(dir, ".sandman", "archive")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("expected archive dir to be created on first use: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty archive dir, got %d entries", len(entries))
	}
}

func TestArchiveOlderThan_ArchivesOldDeadBatch(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Round(time.Second)
	batchDir := filepath.Join(dir, ".sandman", "batches", "old-dead")
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "old-dead",
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: old,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "old-dead", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: old, Issues: []int{42}},
	})

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	archiveBatchDir := filepath.Join(dir, ".sandman", "archive", "old-dead")
	if _, err := os.Stat(archiveBatchDir); err != nil {
		t.Fatalf("expected archived batch dir to exist: %v", err)
	}
	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Errorf("expected source batch dir to be gone after archive, got: %v", err)
	}
}

func TestArchiveOlderThan_SkipsYoungDeadBatch(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	young := time.Now().Add(-5 * 24 * time.Hour).UTC().Round(time.Second)
	batchDir := filepath.Join(dir, ".sandman", "batches", "young-dead")
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "young-dead",
		Issue:     7,
		Kind:      batchindex.KindIssue,
		CreatedAt: young,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "young-dead", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: young, Issues: []int{7}},
	})

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected young batch dir to be preserved, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "young-dead")); !os.IsNotExist(err) {
		t.Errorf("expected no archive entry for young batch, got: %v", err)
	}
}

func TestArchiveOlderThan_SkipsLiveBatch(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	old := time.Now().Add(-100 * 24 * time.Hour).UTC().Round(time.Second)
	batchDir := filepath.Join(dir, ".sandman", "batches", "old-live")
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "old-live",
		Issue:     99,
		Kind:      batchindex.KindIssue,
		CreatedAt: old,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "old-live", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: old, Issues: []int{99}},
	})

	ctlSocket := daemon.NewControlSocket(batchDir, daemon.NewBroadcaster())
	if err := ctlSocket.Start(); err != nil {
		t.Fatalf("start control socket: %v", err)
	}
	defer ctlSocket.Stop()

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected live batch dir to be preserved regardless of age, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "old-live")); !os.IsNotExist(err) {
		t.Errorf("expected no archive entry for live batch, got: %v", err)
	}
}

func TestArchiveOlderThan_MixedBatchArchivesOnlyEligible(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	oldTs := time.Now().Add(-40 * 24 * time.Hour).UTC().Round(time.Second)
	youngTs := time.Now().Add(-5 * 24 * time.Hour).UTC().Round(time.Second)

	oldDeadDir := filepath.Join(dir, ".sandman", "batches", "old-dead")
	oldLiveDir := filepath.Join(dir, ".sandman", "batches", "old-live")
	youngDeadDir := filepath.Join(dir, ".sandman", "batches", "young-dead")
	youngLiveDir := filepath.Join(dir, ".sandman", "batches", "young-live")

	for _, d := range []string{oldDeadDir, oldLiveDir, youngDeadDir, youngLiveDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	writeBatchDirForArchive(t, oldDeadDir, batchindex.RunManifest{BatchID: "old-dead", Issue: 1, Kind: batchindex.KindIssue, CreatedAt: oldTs, Status: batchindex.StatusActive})
	writeBatchDirForArchive(t, oldLiveDir, batchindex.RunManifest{BatchID: "old-live", Issue: 2, Kind: batchindex.KindIssue, CreatedAt: oldTs, Status: batchindex.StatusActive})
	writeBatchDirForArchive(t, youngDeadDir, batchindex.RunManifest{BatchID: "young-dead", Issue: 3, Kind: batchindex.KindIssue, CreatedAt: youngTs, Status: batchindex.StatusActive})
	writeBatchDirForArchive(t, youngLiveDir, batchindex.RunManifest{BatchID: "young-live", Issue: 4, Kind: batchindex.KindIssue, CreatedAt: youngTs, Status: batchindex.StatusActive})

	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "old-dead", Path: oldDeadDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: oldTs, Issues: []int{1}},
		{ID: "old-live", Path: oldLiveDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: oldTs, Issues: []int{2}},
		{ID: "young-dead", Path: youngDeadDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: youngTs, Issues: []int{3}},
		{ID: "young-live", Path: youngLiveDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: youngTs, Issues: []int{4}},
	})

	oldLiveSock := daemon.NewControlSocket(oldLiveDir, daemon.NewBroadcaster())
	if err := oldLiveSock.Start(); err != nil {
		t.Fatalf("start old-live control socket: %v", err)
	}
	defer oldLiveSock.Stop()

	youngLiveSock := daemon.NewControlSocket(youngLiveDir, daemon.NewBroadcaster())
	if err := youngLiveSock.Start(); err != nil {
		t.Fatalf("start young-live control socket: %v", err)
	}
	defer youngLiveSock.Stop()

	existingArchive := filepath.Join(dir, ".sandman", "archive", "old-dead")
	if err := os.MkdirAll(existingArchive, 0755); err != nil {
		t.Fatalf("mkdir existing archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(existingArchive, "sentinel.txt"), []byte("preserved"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "old-dead")); err != nil {
		t.Errorf("expected existing archive entry preserved on collision, got: %v", err)
	}
	sentinel, err := os.ReadFile(filepath.Join(existingArchive, "sentinel.txt"))
	if err != nil {
		t.Fatalf("expected sentinel preserved, got: %v", err)
	}
	if string(sentinel) != "preserved" {
		t.Errorf("expected existing archive sentinel untouched, got %q", string(sentinel))
	}

	if _, err := os.Stat(oldDeadDir); err != nil {
		t.Errorf("expected old-dead source preserved on collision, got: %v", err)
	}
	if _, err := os.Stat(oldLiveDir); err != nil {
		t.Errorf("expected old-live preserved, got: %v", err)
	}
	if _, err := os.Stat(youngDeadDir); err != nil {
		t.Errorf("expected young-dead preserved, got: %v", err)
	}
	if _, err := os.Stat(youngLiveDir); err != nil {
		t.Errorf("expected young-live preserved, got: %v", err)
	}

	for _, id := range []string{"old-live", "young-dead", "young-live"} {
		if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", id)); !os.IsNotExist(err) {
			t.Errorf("expected no archive entry for %s, got: %v", id, err)
		}
	}

	if !strings.Contains(buf.String(), "skip") {
		t.Errorf("expected output to mention skip on collision, got: %q", buf.String())
	}
}

func TestArchiveOlderThan_NonIntegerDaysReturnsError(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	writeBatchIndexForArchive(t, dir, nil)

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "abc"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-integer days, got nil")
	}
	if !strings.Contains(err.Error(), "non-negative integer") {
		t.Errorf("expected error to mention 'non-negative integer', got: %v", err)
	}
}

func TestArchiveOlderThan_NegativeDaysReturnsError(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	writeBatchIndexForArchive(t, dir, nil)

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "--", "-5"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for negative days, got nil")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("expected error to mention 'negative', got: %v", err)
	}
}

func TestArchiveOlderThan_MissingArgReturnsError(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	writeBatchIndexForArchive(t, dir, nil)

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when days arg missing, got nil")
	}
}

func TestArchiveOlderThan_ZeroDaysArchivesAllDead(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	oneSecAgo := time.Now().UTC().Add(-1 * time.Second)
	batchDir := filepath.Join(dir, ".sandman", "batches", "just-now")
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "just-now",
		Issue:     1,
		Kind:      batchindex.KindIssue,
		CreatedAt: oneSecAgo,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "just-now", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: oneSecAgo, Issues: []int{1}},
	})

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "0"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "just-now")); err != nil {
		t.Errorf("expected 0-day cutoff to archive every dead batch, got: %v", err)
	}
}

func TestArchiveHelpListsStaleSubcommand(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "stale") {
		t.Errorf("expected `archive --help` output to list `stale` subcommand, got:\n%s", buf.String())
	}
}

func TestArchiveStale_CollisionWithExistingArchivePreservesBoth(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	batchDir := filepath.Join(dir, ".sandman", "batches", "dead-collision")
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "dead-collision",
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: createdAt,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "dead-collision", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: createdAt, Issues: []int{42}},
	})

	if err := os.WriteFile(filepath.Join(batchDir, "source.txt"), []byte("source"), 0644); err != nil {
		t.Fatalf("write source sentinel: %v", err)
	}

	existingArchive := filepath.Join(dir, ".sandman", "archive", "dead-collision")
	if err := os.MkdirAll(existingArchive, 0755); err != nil {
		t.Fatalf("mkdir existing archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(existingArchive, "preserved.txt"), []byte("preserved"), 0644); err != nil {
		t.Fatalf("write preserved sentinel: %v", err)
	}

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: createdAt.Add(5 * time.Minute)},
		{Type: "run.finished", RunID: "run-42", Issue: 42, Timestamp: createdAt.Add(10 * time.Minute), Payload: map[string]any{"status": "success"}},
	}}
	deps := newTestDeps()
	deps.EventLog = log

	var buf bytes.Buffer
	cmd := NewArchiveCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected source batch dir preserved on collision, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(existingArchive, "preserved.txt")); err != nil {
		t.Errorf("expected existing archive sentinel preserved, got: %v", err)
	}
	if !strings.Contains(buf.String(), "skip") {
		t.Errorf("expected output to mention skip on collision, got: %q", buf.String())
	}
}

func TestArchiveStale_MixedStatusDeadBatchEmitsAbortedAndArchives(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	batchDir := filepath.Join(dir, ".sandman", "batches", "dead-mixed")
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "dead-mixed",
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: createdAt,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "dead-mixed", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: createdAt, Issues: []int{42, 43}},
	})

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-43", Issue: 43, Timestamp: createdAt.Add(5 * time.Minute)},
		{Type: "run.finished", RunID: "run-43", Issue: 43, Timestamp: createdAt.Add(10 * time.Minute), Payload: map[string]any{"status": "success"}},
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: createdAt.Add(20 * time.Minute)},
	}}
	deps := newTestDeps()
	deps.EventLog = log

	var buf bytes.Buffer
	cmd := NewArchiveCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Errorf("expected source batch dir to be gone, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "dead-mixed")); err != nil {
		t.Errorf("expected archived batch dir to exist, got: %v", err)
	}

	var abortedFor42 bool
	for _, e := range log.logged {
		if e.Type == "run.aborted" && e.Issue == 42 {
			abortedFor42 = true
			if e.RunID != "run-42" {
				t.Errorf("expected run.aborted RunID=run-42, got %q", e.RunID)
			}
			if v, _ := e.Payload["recovered"].(bool); !v {
				t.Errorf("expected payload.recovered=true, got %v", e.Payload)
			}
		}
		if e.Type == "run.aborted" && e.Issue == 43 {
			t.Errorf("expected no run.aborted for already-terminated issue 43, got: %+v", e)
		}
	}
	if !abortedFor42 {
		t.Errorf("expected run.aborted event for unterminated issue 42, got events: %+v", log.logged)
	}

	if !strings.Contains(buf.String(), "Recovered 1 stale runs and archived 1 dead batches.") {
		t.Errorf("expected summary, got: %q", buf.String())
	}
}

func TestArchiveStale_AllTerminatedDeadBatchIsArchived(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	batchDir := filepath.Join(dir, ".sandman", "batches", "dead-done")
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "dead-done",
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: createdAt,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "dead-done", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: createdAt, Issues: []int{42}},
	})

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: createdAt.Add(5 * time.Minute)},
		{Type: "run.finished", RunID: "run-42", Issue: 42, Timestamp: createdAt.Add(10 * time.Minute), Payload: map[string]any{"status": "success"}},
	}}
	deps := newTestDeps()
	deps.EventLog = log

	var buf bytes.Buffer
	cmd := NewArchiveCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Errorf("expected source batch dir to be gone, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "dead-done")); err != nil {
		t.Errorf("expected archived batch dir to exist, got: %v", err)
	}
	for _, e := range log.logged {
		if e.Type == "run.aborted" {
			t.Errorf("expected no run.aborted event for already-terminated run, got: %+v", e)
		}
	}
	if !strings.Contains(buf.String(), "Recovered 0 stale runs and archived 1 dead batches.") {
		t.Errorf("expected summary, got: %q", buf.String())
	}
}

func TestArchiveStale_LiveBatchIsNoop(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "live-1")
	createdAt := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	writeBatchDirForArchive(t, batchDir, batchindex.RunManifest{
		BatchID:   "live-1",
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: createdAt,
		Status:    batchindex.StatusActive,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "live-1", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: createdAt, Issues: []int{42}},
	})

	ctlSocket := daemon.NewControlSocket(batchDir, daemon.NewBroadcaster())
	if err := ctlSocket.Start(); err != nil {
		t.Fatalf("start control socket: %v", err)
	}
	defer ctlSocket.Stop()

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: createdAt.Add(5 * time.Minute)},
	}}
	deps := newTestDeps()
	deps.EventLog = log

	var buf bytes.Buffer
	cmd := NewArchiveCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected live batch dir to be preserved, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "live-1")); !os.IsNotExist(err) {
		t.Errorf("expected no archive entry for live batch, got: %v", err)
	}
	if len(log.logged) != 0 {
		t.Errorf("expected no events logged for live batch, got %d: %+v", len(log.logged), log.logged)
	}
	if !strings.Contains(buf.String(), "Recovered 0 stale runs and archived 0 dead batches.") {
		t.Errorf("expected summary, got: %q", buf.String())
	}
}

func TestArchiveOlderThan_YoungMtimeKeepsUnmanifestedBatch(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "no-manifest-young")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatalf("mkdir batch dir: %v", err)
	}

	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "no-manifest-young", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now()},
	})

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Errorf("expected recent batch preserved, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "no-manifest-young")); !os.IsNotExist(err) {
		t.Errorf("expected no archive entry for young batch, got: %v", err)
	}
}

func TestArchiveOlderThan_ArchivesUnmanifestedBatchByDirMtime(t *testing.T) {
	dir := newSandmanDir(t)
	t.Chdir(dir)

	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Round(time.Second)
	batchDir := filepath.Join(dir, ".sandman", "batches", "no-manifest-old")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatalf("mkdir batch dir: %v", err)
	}
	if err := os.Chtimes(batchDir, old, old); err != nil {
		t.Fatalf("chtimes batch dir: %v", err)
	}

	writeBatchIndexForArchive(t, dir, []batchindex.Entry{
		{ID: "no-manifest-old", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{8}},
	})

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "no-manifest-old")); err != nil {
		t.Fatalf("expected old unmanifested batch to be archived by dir mtime: %v", err)
	}
	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Errorf("expected source batch dir to be gone after archive, got: %v", err)
	}
}
