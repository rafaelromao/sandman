package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
)

func TestArchiveBatch_NonexistentBatchReturnsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

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
	dir := t.TempDir()
	t.Chdir(dir)

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
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "live-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	cmdServer := daemon.NewCommandServer(runDir, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("start command server: %v", err)
	}
	defer cmdServer.Stop()

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "live-1"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for live run, got nil")
	}

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("expected live run dir to be preserved on rejection, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "live-1")); !os.IsNotExist(err) {
		t.Errorf("expected no archive/live-1 after rejection, got: %v", err)
	}
}

func TestArchiveRun_DeadRunMovesDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "dead-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), []byte(`{"issues":[42]}`), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "dead-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	archiveRunDir := filepath.Join(dir, ".sandman", "archive", "dead-1")
	if _, err := os.Stat(archiveRunDir); err != nil {
		t.Fatalf("expected archived run dir to exist: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("expected source run dir to be gone after archive, got: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(archiveRunDir, "batch.json"))
	if err != nil {
		t.Fatalf("read archived batch.json: %v", err)
	}
	if string(data) != `{"issues":[42]}` {
		t.Errorf("archived batch.json content = %q, want %q", string(data), `{"issues":[42]}`)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive")); err != nil {
		t.Errorf("expected .sandman/archive/ to exist after archive: %v", err)
	}
}

func TestArchiveBatch_LiveBatchReturnsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "live-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	cmdServer := daemon.NewCommandServer(runDir, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("start command server: %v", err)
	}
	defer cmdServer.Stop()

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"batch", "live-1"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for live batch, got nil")
	}

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("expected live run dir to be preserved on rejection, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "live-1")); !os.IsNotExist(err) {
		t.Errorf("expected no archive/live-1 after rejection, got: %v", err)
	}
}

func TestArchiveBatch_DeadBatchMovesDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "dead-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), []byte(`{"issues":[42]}`), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"batch", "dead-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	archiveRunDir := filepath.Join(dir, ".sandman", "archive", "dead-1")
	if _, err := os.Stat(archiveRunDir); err != nil {
		t.Fatalf("expected archived run dir to exist: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("expected source run dir to be gone after archive, got: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(archiveRunDir, "batch.json"))
	if err != nil {
		t.Fatalf("read archived batch.json: %v", err)
	}
	if string(data) != `{"issues":[42]}` {
		t.Errorf("archived batch.json content = %q, want %q", string(data), `{"issues":[42]}`)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive")); err != nil {
		t.Errorf("expected .sandman/archive/ to exist after archive: %v", err)
	}
}

func TestArchiveBatch_CollisionWithExistingArchiveDirReturnsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "dead-2")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), []byte("source"), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

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

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("expected source run dir preserved on collision, got: %v", err)
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
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "dead-2")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), []byte("source"), 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

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

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("expected source run dir preserved on collision, got: %v", err)
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
	dir := t.TempDir()
	t.Chdir(dir)

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

func TestArchiveOlderThan_ArchivesOldDeadRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Round(time.Second)
	runDir := filepath.Join(dir, ".sandman", "runs", "old-dead")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	manifest := daemon.BatchManifest{Issues: []int{42}, CreatedAt: old}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), data, 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	archiveRunDir := filepath.Join(dir, ".sandman", "archive", "old-dead")
	if _, err := os.Stat(archiveRunDir); err != nil {
		t.Fatalf("expected archived run dir to exist: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("expected source run dir to be gone after archive, got: %v", err)
	}
	archived, err := os.ReadFile(filepath.Join(archiveRunDir, "batch.json"))
	if err != nil {
		t.Fatalf("read archived batch.json: %v", err)
	}
	if string(archived) != string(data) {
		t.Errorf("archived batch.json content = %q, want %q", string(archived), string(data))
	}
}

func TestArchiveOlderThan_SkipsYoungDeadRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	young := time.Now().Add(-5 * 24 * time.Hour).UTC().Round(time.Second)
	runDir := filepath.Join(dir, ".sandman", "runs", "young-dead")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	manifest := daemon.BatchManifest{Issues: []int{7}, CreatedAt: young}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), data, 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("expected young run dir to be preserved, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "young-dead")); !os.IsNotExist(err) {
		t.Errorf("expected no archive entry for young run, got: %v", err)
	}
}

func TestArchiveOlderThan_SkipsLiveRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	old := time.Now().Add(-100 * 24 * time.Hour).UTC().Round(time.Second)
	runDir := filepath.Join(dir, ".sandman", "runs", "old-live")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	manifest := daemon.BatchManifest{Issues: []int{99}, CreatedAt: old}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), data, 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	cmdServer := daemon.NewCommandServer(runDir, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("start command server: %v", err)
	}
	defer cmdServer.Stop()

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("expected live run dir to be preserved regardless of age, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "old-live")); !os.IsNotExist(err) {
		t.Errorf("expected no archive entry for live run, got: %v", err)
	}
}

func TestArchiveOlderThan_MixedBatchArchivesOnlyEligible(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	oldTs := time.Now().Add(-40 * 24 * time.Hour).UTC().Round(time.Second)
	youngTs := time.Now().Add(-5 * 24 * time.Hour).UTC().Round(time.Second)

	oldDeadDir := filepath.Join(dir, ".sandman", "runs", "old-dead")
	oldLiveDir := filepath.Join(dir, ".sandman", "runs", "old-live")
	youngDeadDir := filepath.Join(dir, ".sandman", "runs", "young-dead")
	youngLiveDir := filepath.Join(dir, ".sandman", "runs", "young-live")

	for _, d := range []string{oldDeadDir, oldLiveDir, youngDeadDir, youngLiveDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	writeManifest := func(path string, created time.Time, issues []int) {
		data, err := json.Marshal(daemon.BatchManifest{Issues: issues, CreatedAt: created})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "batch.json"), data, 0644); err != nil {
			t.Fatalf("write batch.json: %v", err)
		}
	}
	writeManifest(oldDeadDir, oldTs, []int{1})
	writeManifest(oldLiveDir, oldTs, []int{2})
	writeManifest(youngDeadDir, youngTs, []int{3})
	writeManifest(youngLiveDir, youngTs, []int{4})

	cmdServer := daemon.NewCommandServer(oldLiveDir, nil)
	if err := cmdServer.Start(); err != nil {
		t.Fatalf("start live command server: %v", err)
	}
	defer cmdServer.Stop()

	otherLive := daemon.NewCommandServer(youngLiveDir, nil)
	if err := otherLive.Start(); err != nil {
		t.Fatalf("start young-live command server: %v", err)
	}
	defer otherLive.Stop()

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
		t.Errorf("expected old-dead run source preserved on collision, got: %v", err)
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
	dir := t.TempDir()
	t.Chdir(dir)

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
	dir := t.TempDir()
	t.Chdir(dir)

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
	dir := t.TempDir()
	t.Chdir(dir)

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
	dir := t.TempDir()
	t.Chdir(dir)

	oneSecAgo := time.Now().UTC().Add(-1 * time.Second)
	runDir := filepath.Join(dir, ".sandman", "runs", "just-now")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	data, err := json.Marshal(daemon.BatchManifest{Issues: []int{1}, CreatedAt: oneSecAgo})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "batch.json"), data, 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "0"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "just-now")); err != nil {
		t.Errorf("expected 0-day cutoff to archive every dead run, got: %v", err)
	}
}

func TestArchiveOlderThan_FallsBackToDirectoryMtimeWhenManifestMissing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "no-manifest")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	oldMtime := time.Now().Add(-45 * 24 * time.Hour).UTC()
	if err := os.Chtimes(runDir, oldMtime, oldMtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "no-manifest")); err != nil {
		t.Errorf("expected run archived by mtime fallback, got: %v", err)
	}
}

func TestArchiveOlderThan_YoungMtimeKeepsUnmanifestedRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runDir := filepath.Join(dir, ".sandman", "runs", "no-manifest-young")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}

	var buf bytes.Buffer
	cmd := NewArchiveCmd(newTestDeps())
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"older-than", "30"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("expected recent-mtime run preserved, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sandman", "archive", "no-manifest-young")); !os.IsNotExist(err) {
		t.Errorf("expected no archive entry for young-mtime run, got: %v", err)
	}
}
