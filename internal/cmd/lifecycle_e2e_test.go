//go:build e2e

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
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestLifecycle_CleanStale_RecoversAndRemoves(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioLifecycleCommands) {
		t.Skip("set SANDMAN_E2E_GATES=lifecycle_commands (or all) to run lifecycle_commands e2e tests")
	}

	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	createdAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	started := createdAt.Add(5 * time.Minute)

	writeBatchManifest(t, dir, "run-dead-1", []int{42}, createdAt)
	writeBatchManifest(t, dir, "run-dead-2", []int{43}, createdAt)

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "42-fix"}},
		{Type: "run.started", RunID: "run-43", Issue: 43, Timestamp: started, Payload: map[string]any{"branch": "43-fix"}},
	}}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = log
	deps.GitRunner = &fakeGitRunner{}
	deps.RunActivityProbe = func(runPath string) bool { return false }

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
	}
	if !strings.Contains(buf.String(), "Recovered 2 stale runs") {
		t.Errorf("expected summary, got: %s", buf.String())
	}

	for _, batchDir := range []string{"run-dead-1", "run-dead-2"} {
		path := filepath.Join(dir, ".sandman", "batches", batchDir)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected batch dir %s to still exist after --stale, got: %v", batchDir, err)
		}
	}
}

func TestLifecycle_CleanOrphaned_RemovesOrphanDirs(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioLifecycleCommands) {
		t.Skip("set SANDMAN_E2E_GATES=lifecycle_commands (or all) to run lifecycle_commands e2e tests")
	}

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
	deps.RunActivityProbe = func(runPath string) bool { return false }
	deps.CleanupRemover = &osCleanupRemover{}

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

	if !strings.Contains(buf.String(), "orphan-x") {
		t.Errorf("expected output to mention orphan-x, got: %s", buf.String())
	}
}

func TestLifecycle_Status_ReportsCorrectState(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioLifecycleCommands) {
		t.Skip("set SANDMAN_E2E_GATES=lifecycle_commands (or all) to run lifecycle_commands e2e tests")
	}

	now := time.Now()

	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.started", Timestamp: now.Add(-5 * time.Minute), RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "42-fix"}},
			{Type: "run.started", Timestamp: now.Add(-10 * time.Minute), RunID: "run-43", Issue: 43, Payload: map[string]any{"branch": "43-fix"}},
			{Type: "run.finished", Timestamp: now.Add(-5 * time.Minute), RunID: "run-43", Issue: 43, Payload: map[string]any{"status": "success"}},
			{Type: "run.blocked", Timestamp: now.Add(-5 * time.Minute), RunID: "run-44", Issue: 44, Payload: map[string]any{"blocked_by": []int{42}}},
		},
	}

	var buf bytes.Buffer
	cmd := NewStatusCmd(log)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "#42") {
		t.Errorf("expected active run #42 to be shown, got:\n%s", out)
	}
	if strings.Contains(out, "#43") {
		t.Errorf("expected finished run #43 to NOT be shown, got:\n%s", out)
	}
	if strings.Contains(out, "#44") {
		t.Errorf("expected blocked run #44 to NOT be shown, got:\n%s", out)
	}
	if strings.Contains(out, "No active runs") {
		t.Errorf("expected 'No active runs' not to appear when there are active runs, got:\n%s", out)
	}
}

func TestLifecycle_Status_NoActiveRuns(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioLifecycleCommands) {
		t.Skip("set SANDMAN_E2E_GATES=lifecycle_commands (or all) to run lifecycle_commands e2e tests")
	}

	log := &fakeEventLog{
		events: []events.Event{
			{Type: "run.finished", Timestamp: time.Now().Add(-5 * time.Minute), RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "success"}},
		},
	}

	var buf bytes.Buffer
	cmd := NewStatusCmd(log)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "No active runs") {
		t.Errorf("expected 'No active runs' message, got:\n%s", out)
	}
	if strings.Contains(out, "#42") {
		t.Errorf("expected finished run #42 not to appear, got:\n%s", out)
	}
}

func TestLifecycle_ArchiveRun_ArchivesTerminalRun(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioLifecycleCommands) {
		t.Skip("set SANDMAN_E2E_GATES=lifecycle_commands (or all) to run lifecycle_commands e2e tests")
	}

	dir := newSandmanDir(t)
	t.Chdir(dir)

	batchDir := filepath.Join(dir, ".sandman", "batches", "done-1")
	now := time.Now()
	writeRunDirForArchive(t, batchDir, "done-1", batchindex.RunManifest{
		BatchID:   "done-1",
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: now,
		Status:    batchindex.RunManifestStatusSuccess,
	})
	writeBatchIndexForArchive(t, dir, []batchindex.Batch{
		{ID: "done-1", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: now, Issues: []int{42}},
	})

	var buf bytes.Buffer
	deps := newTestDeps(t)
	deps.EventLog = &fakeEventLog{}
	cmd := NewArchiveCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "done-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	archiveRunDir := filepath.Join(dir, ".sandman", "archive", "done-1", "runs", "done-1")
	if _, err := os.Stat(archiveRunDir); err != nil {
		t.Fatalf("expected archived run dir %q to exist: %v", archiveRunDir, err)
	}
	liveRunDir := filepath.Join(batchDir, "runs", "done-1")
	if _, err := os.Stat(liveRunDir); !os.IsNotExist(err) {
		t.Errorf("expected live run dir to be gone after archive, got: %v", err)
	}

	idx := loadBatchIndexForArchive(t, dir)
	rec := idx.RunRecordFor("done-1", "done-1")
	if rec == nil {
		t.Fatal("expected per-row RunRecord after archive")
	}
	if rec.Status != batchindex.RunRecordStatusArchived {
		t.Errorf("record status = %s, want %s", rec.Status, batchindex.RunRecordStatusArchived)
	}
}
