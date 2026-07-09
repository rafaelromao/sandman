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
	worktreeDir := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	writeRunManifest(t, batchDir, batchindex.RunManifest{
		RunID:        "batch-1",
		BatchID:      "batch-1",
		Issue:        42,
		Branch:       "sandman/42-fix",
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
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix")
	worktreeArchived := filepath.Join(dir, ".sandman", "worktrees", "sandman", "43-fix")

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
		Branch:       "sandman/42-fix",
		WorktreePath: worktreeActive,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	writeRunManifest(t, batchArchived, batchindex.RunManifest{
		RunID:        "batch-archived",
		BatchID:      "batch-archived",
		Issue:        43,
		Branch:       "sandman/43-fix",
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
	worktreeArchived := filepath.Join(dir, ".sandman", "worktrees", "sandman", "43-fix")
	if err := os.MkdirAll(worktreeArchived, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	writeRunManifest(t, batchArchived, batchindex.RunManifest{
		RunID:        "batch-archived",
		BatchID:      "batch-archived",
		Issue:        43,
		Branch:       "sandman/43-fix",
		WorktreePath: worktreeArchived,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	now := time.Now()
	writeBatchIndex(t, dir, []batchindex.Batch{
		{ID: "batch-archived", Path: batchArchived, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived, CreatedAt: now},
	})

	log := &fakeEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
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

func TestClean_Archived_RemovesArchivedAndUnavailableEntries(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	batchActive := filepath.Join(dir, ".sandman", "batches", "batch-active")
	batchArchived := filepath.Join(dir, ".sandman", "batches", "batch-archived")
	worktreeActive := filepath.Join(dir, ".sandman", "worktrees", "sandman", "42-fix")
	worktreeArchived := filepath.Join(dir, ".sandman", "worktrees", "sandman", "43-fix")

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
		Branch:       "sandman/42-fix",
		WorktreePath: worktreeActive,
		Kind:         batchindex.KindIssue,
		Status:       batchindex.RunManifestStatusActive,
	})
	writeRunManifest(t, batchArchived, batchindex.RunManifest{
		RunID:        "batch-archived",
		BatchID:      "batch-archived",
		Issue:        43,
		Branch:       "sandman/43-fix",
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
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.started", RunID: "run-43", Issue: 43, Timestamp: started, Payload: map[string]any{"branch": "sandman/43-fix"}},
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
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
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
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
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
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
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
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: firstStart, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.continued", RunID: "run-42", Issue: 42, Timestamp: continuedAt, Payload: map[string]any{"branch": "sandman/42-fix"}},
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
		{Type: "run.started", RunID: "run-42", Issue: 42, Timestamp: started, Payload: map[string]any{"branch": "sandman/42-fix"}},
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
