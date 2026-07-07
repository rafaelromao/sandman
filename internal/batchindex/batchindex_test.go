package batchindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestLoad_ValidIndex(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{
				ID:        "abc123",
				Path:      filepath.Join(batchesDir, "abc123"),
				Kind:      KindIssue,
				Status:    StatusActive,
				CreatedAt: time.Now(),
				Issues:    []int{1213, 1214},
			},
		},
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(indexPath, data, 0644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.Version != IndexVersion {
		t.Errorf("Version = %d, want %d", loaded.Version, IndexVersion)
	}
	if len(loaded.Entries) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(loaded.Entries))
	}
	if loaded.Entries[0].ID != "abc123" {
		t.Errorf("Entries[0].ID = %q, want %q", loaded.Entries[0].ID, "abc123")
	}
}

func TestLoad_AbsentFile_ReturnsZeroIndex(t *testing.T) {
	repoRoot := t.TempDir()
	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")

	idx, err := Load(indexPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if idx.Version != IndexVersion {
		t.Errorf("Version = %d, want %d", idx.Version, IndexVersion)
	}
	if idx.Entries != nil {
		t.Errorf("Batches = %v, want nil", idx.Entries)
	}
}

func TestLoad_UnsupportedVersion(t *testing.T) {
	repoRoot := t.TempDir()
	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")

	if err := os.MkdirAll(filepath.Dir(indexPath), 0755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	data, _ := json.Marshal(map[string]any{"version": 999, "entries": []any{}})
	if err := os.WriteFile(indexPath, data, 0644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	_, err := Load(indexPath)
	if err == nil {
		t.Fatal("Load succeeded, want error for unsupported version")
	}
}

func TestSave_AtomicRename(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{
				ID:        "abc123",
				Path:      filepath.Join(batchesDir, "abc123"),
				Kind:      KindIssue,
				Status:    StatusActive,
				CreatedAt: time.Now(),
			},
		},
	}
	if err := idx.Save(indexPath); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if _, err := os.Stat(indexPath); err != nil {
		t.Errorf("index path does not exist: %v", err)
	}

	tmpPath := indexPath + ".tmp"
	if _, err := os.Stat(tmpPath); err == nil {
		t.Errorf("tmp file still exists after rename")
	}
}

func TestSave_KeepsBackup(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")

	idx1 := &Index{Version: IndexVersion, Entries: []Entry{{ID: "first", Path: "/first", Kind: KindIssue, Status: StatusActive}}}
	data1, _ := json.Marshal(idx1)
	if err := os.WriteFile(indexPath, data1, 0644); err != nil {
		t.Fatalf("write initial index: %v", err)
	}

	idx2 := &Index{Version: IndexVersion, Entries: []Entry{{ID: "second", Path: "/second", Kind: KindReview, Status: StatusActive}}}
	if err := idx2.Save(indexPath); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	bakPath := indexPath + ".bak"
	if _, err := os.Stat(bakPath); err != nil {
		t.Errorf("backup file does not exist: %v", err)
	}
}

func TestResolve_FindsByBatchID(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "abc123", Path: "/path/abc123", Kind: KindIssue, Status: StatusActive},
			{ID: "def456", Path: "/path/def456", Kind: KindReview, Status: StatusActive},
		},
	}

	batch := idx.Resolve("abc123")
	if batch == nil {
		t.Fatal("Resolve returned nil, want batch")
	}
	if batch.ID != "abc123" {
		t.Errorf("batch.ID = %q, want %q", batch.ID, "abc123")
	}
}

func TestResolve_NotFound(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "abc123", Path: "/path/abc123", Kind: KindIssue, Status: StatusActive},
		},
	}

	batch := idx.Resolve("nonexistent")
	if batch != nil {
		t.Errorf("batch = %v, want nil", batch)
	}
}

func TestProbeStatus_ENOENT_SetsUnavailable(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "abc123", Path: filepath.Join(batchesDir, "nonexistent"), Kind: KindIssue, Status: StatusActive},
		},
	}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(indexPath, data, 0644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if err := loaded.EnsureStatus(); err != nil {
		t.Fatalf("EnsureStatus failed: %v", err)
	}
	if loaded.Entries[0].Status != StatusUnavailable {
		t.Errorf("Status = %q, want %q", loaded.Entries[0].Status, StatusUnavailable)
	}
}

func TestProbeStatus_NonENOENT_LeavesStatus(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	realBatchDir := filepath.Join(batchesDir, "realbatch")
	if err := os.MkdirAll(realBatchDir, 0755); err != nil {
		t.Fatalf("create real batch dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "realbatch", Path: realBatchDir, Kind: KindIssue, Status: StatusActive},
			{ID: "missing", Path: filepath.Join(batchesDir, "missing"), Kind: KindIssue, Status: StatusArchived},
		},
	}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(indexPath, data, 0644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if err := loaded.EnsureStatus(); err != nil {
		t.Fatalf("EnsureStatus failed: %v", err)
	}

	for _, b := range loaded.Entries {
		if b.ID == "realbatch" && b.Status != StatusActive {
			t.Errorf("realbatch Status = %q, want %q", b.Status, StatusActive)
		}
		if b.ID == "missing" && b.Status != StatusUnavailable {
			t.Errorf("missing Status = %q, want %q", b.Status, StatusUnavailable)
		}
	}
}

func TestRunManifestStatus_JSONRoundTrip(t *testing.T) {
	manifest := RunManifest{
		RunID:     "run-1",
		BatchID:   "batch-1",
		Status:    RunManifestStatusSuccess,
		CreatedAt: time.Now().Truncate(time.Second),
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to raw map failed: %v", err)
	}
	if raw["status"] != "success" {
		t.Errorf(`JSON status = %q, want "success"`, raw["status"])
	}

	var decoded RunManifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.Status != RunManifestStatusSuccess {
		t.Errorf("decoded Status = %q, want %q", decoded.Status, RunManifestStatusSuccess)
	}
}

func TestRunManifest_JSONSchema(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	manifest := RunManifest{
		RunID:        "abc-123-issue-1213",
		BatchID:      "abc-123-1213+2",
		Issue:        1213,
		Branch:       "sandman/1213-fix",
		BaseBranch:   "main",
		WorktreePath: ".sandman/worktrees/sandman/1213-fix",
		Kind:         KindIssue,
		CreatedAt:    now,
		Status:       RunManifestStatusActive,
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded RunManifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.RunID != manifest.RunID {
		t.Errorf("RunID = %q, want %q", decoded.RunID, manifest.RunID)
	}
	if decoded.BatchID != manifest.BatchID {
		t.Errorf("BatchID = %q, want %q", decoded.BatchID, manifest.BatchID)
	}
	if decoded.Issue != manifest.Issue {
		t.Errorf("Issue = %d, want %d", decoded.Issue, manifest.Issue)
	}
	if decoded.Kind != manifest.Kind {
		t.Errorf("Kind = %q, want %q", decoded.Kind, manifest.Kind)
	}
}

func TestReviewState_JSONSchema(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	state := ReviewState{
		PR: 1217,
		SeenComments: []SeenComment{
			{CommentID: "12345", Status: "success", Timestamp: now},
		},
		Claims: map[string]Claim{
			"12345": {Holder: "pid123", Since: now},
		},
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded ReviewState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.PR != state.PR {
		t.Errorf("PR = %d, want %d", decoded.PR, state.PR)
	}
	if len(decoded.SeenComments) != 1 {
		t.Fatalf("SeenComments len = %d, want 1", len(decoded.SeenComments))
	}
	if decoded.SeenComments[0].CommentID != "12345" {
		t.Errorf("SeenComments[0].CommentID = %q, want %q", decoded.SeenComments[0].CommentID, "12345")
	}
	if decoded.Claims["12345"].Holder != "pid123" {
		t.Errorf("Claims[12345].Holder = %q, want %q", decoded.Claims["12345"].Holder, "pid123")
	}
}

func TestBatch_JSONSchema(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	batch := Entry{
		ID:         "abc123",
		Path:       ".sandman/batches/abc123",
		Kind:       KindIssue,
		Status:     StatusActive,
		CreatedAt:  now,
		Issues:     []int{1213, 1214},
		ArchivedAt: nil,
	}

	data, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Entry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.ID != batch.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, batch.ID)
	}
	if decoded.Kind != batch.Kind {
		t.Errorf("Kind = %q, want %q", decoded.Kind, batch.Kind)
	}
	if decoded.Status != batch.Status {
		t.Errorf("Status = %q, want %q", decoded.Status, batch.Status)
	}
	if len(decoded.Issues) != 2 {
		t.Errorf("Issues len = %d, want 2", len(decoded.Issues))
	}
}

func TestIndex_JSONSchema_PromptOnlyIssuesAreExplicitEmptyArray(t *testing.T) {
	idx := Index{
		Version: IndexVersion,
		Entries: []Entry{{
			ID:        "prompt-only-abc123",
			Path:      ".sandman/batches/prompt-only-abc123",
			Kind:      KindPromptOnly,
			Status:    StatusActive,
			CreatedAt: time.Now().Truncate(time.Second),
		}},
	}

	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	entries, ok := decoded["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %T (%v)", decoded["entries"], decoded["entries"])
	}
	entryMap, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("expected entry to decode as object, got %T (%v)", entries[0], entries[0])
	}
	rawIssues, ok := entryMap["issues"]
	if !ok {
		t.Fatalf("expected issues field to be present in %s", string(data))
	}
	issues, ok := rawIssues.([]any)
	if !ok {
		t.Fatalf("expected issues to decode as array, got %T (%v)", rawIssues, rawIssues)
	}
	if len(issues) != 0 {
		t.Fatalf("expected prompt-only issues to be empty, got %v", issues)
	}
}

func TestIndex_JSONSchema_PromptOnlyIssuesIgnoreStaleValues(t *testing.T) {
	idx := Index{
		Version: IndexVersion,
		Entries: []Entry{{
			ID:        "prompt-only-abc123",
			Path:      ".sandman/batches/prompt-only-abc123",
			Kind:      KindPromptOnly,
			Status:    StatusActive,
			CreatedAt: time.Now().Truncate(time.Second),
			Issues:    []int{1, 2, 3},
		}},
	}

	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	entries, ok := decoded["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %T (%v)", decoded["entries"], decoded["entries"])
	}
	entryMap, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("expected entry to decode as object, got %T (%v)", entries[0], entries[0])
	}
	rawIssues, ok := entryMap["issues"]
	if !ok {
		t.Fatalf("expected issues field to be present in %s", string(data))
	}
	issues, ok := rawIssues.([]any)
	if !ok {
		t.Fatalf("expected issues to decode as array, got %T (%v)", rawIssues, rawIssues)
	}
	if len(issues) != 0 {
		t.Fatalf("expected stale prompt-only issues to be ignored, got %v", issues)
	}
}

func TestAddEntry_New(t *testing.T) {
	idx := &Index{Version: IndexVersion}
	batch := Entry{ID: "abc123", Kind: KindIssue}
	idx.Add(batch)
	if len(idx.Entries) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0].ID != "abc123" {
		t.Errorf("Batches[0].ID = %q, want %q", idx.Entries[0].ID, "abc123")
	}
	if idx.Entries[0].Status != StatusActive {
		t.Errorf("Batches[0].Status = %q, want %q", idx.Entries[0].Status, StatusActive)
	}
}

func TestAddEntry_Existing(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "abc123", Kind: KindIssue, Status: StatusActive},
		},
	}
	newBatch := Entry{ID: "abc123", Kind: KindReview}
	idx.Add(newBatch)
	if len(idx.Entries) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0].Kind != KindReview {
		t.Errorf("Batches[0].Kind = %q, want %q", idx.Entries[0].Kind, KindReview)
	}
	if idx.Entries[0].Status != StatusActive {
		t.Errorf("Batches[0].Status = %q, want %q", idx.Entries[0].Status, StatusActive)
	}
}

func TestAddEntry_EmptyID_BackfilledFromPathBasename(t *testing.T) {
	idx := &Index{Version: IndexVersion}
	batch := Entry{
		ID:     "",
		Path:   "/tmp/sandman/.sandman/batches/abc123",
		Kind:   KindIssue,
		Status: StatusActive,
	}
	idx.Add(batch)

	if len(idx.Entries) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0].ID != "abc123" {
		t.Errorf("Batches[0].ID = %q, want %q (backfilled from path basename)", idx.Entries[0].ID, "abc123")
	}
}

func TestAddEntry_EmptyID_NoPathBasename_FallsBackToPath(t *testing.T) {
	// When the path has no usable basename (rare — only happens for
	// an empty path), Add must still produce an addressable batch to
	// avoid dropping it on the floor. The resulting ID is the literal
	// path or a stable placeholder.
	idx := &Index{Version: IndexVersion}
	idx.Add(Entry{ID: "", Path: ".", Kind: KindIssue})

	if len(idx.Entries) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0].ID == "" {
		t.Error("Batches[0].ID is still empty; Add must not produce an unidentified batch")
	}
}

func TestAddEntry_TwoEmptyIDs_DoNotEvict(t *testing.T) {
	// Regression for issue #1464: a second empty-id Add used to
	// overwrite the previous one in place (same "" key in the lookup),
	// silently evicting the prior batch from the index. After the fix
	// both batches must be addressable and persisted.
	idx := &Index{Version: IndexVersion}
	idx.Add(Entry{
		ID:   "",
		Path: "/tmp/sandman/.sandman/batches/first-001",
		Kind: KindIssue,
	})
	idx.Add(Entry{
		ID:   "",
		Path: "/tmp/sandman/.sandman/batches/second-002",
		Kind: KindIssue,
	})

	if len(idx.Entries) != 2 {
		t.Fatalf("Batches len = %d, want 2 (empty-id must not collide)", len(idx.Entries))
	}

	paths := make(map[string]bool, len(idx.Entries))
	for i, b := range idx.Entries {
		if b.ID == "" {
			t.Errorf("Batches[%d].ID is empty; expected backfill from path basename", i)
		}
		if paths[b.Path] {
			t.Errorf("Batches[%d].Path %q duplicated in index", i, b.Path)
		}
		paths[b.Path] = true
	}
}

func TestSave_RoundTrip_EmptyIDIsBackfilled(t *testing.T) {
	repoRoot := t.TempDir()
	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0755); err != nil {
		t.Fatalf("create dir: %v", err)
	}

	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{
				ID:        "",
				Path:      filepath.Join(repoRoot, ".sandman", "batches", "abc123"),
				Kind:      KindIssue,
				Status:    StatusActive,
				CreatedAt: time.Now(),
				Issues:    []int{42},
			},
		},
	}
	if err := idx.Save(indexPath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(loaded.Entries))
	}
	if loaded.Entries[0].ID == "" {
		t.Fatalf("Batches[0].ID is empty after round-trip; expected backfill from path basename")
	}
}

func TestWriteReadManifest(t *testing.T) {
	runDir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	manifest := RunManifest{
		RunID:      "abc-123-issue-1213",
		BatchID:    "abc-123-1213+2",
		Issue:      1213,
		Branch:     "sandman/1213-fix",
		BaseBranch: "main",
		Kind:       KindIssue,
		CreatedAt:  now,
		Status:     "active",
	}

	if err := WriteManifest(runDir, manifest); err != nil {
		t.Fatalf("WriteManifest failed: %v", err)
	}

	read, err := ReadManifest(runDir)
	if err != nil {
		t.Fatalf("ReadManifest failed: %v", err)
	}
	if read.RunID != manifest.RunID {
		t.Errorf("RunID = %q, want %q", read.RunID, manifest.RunID)
	}
	if read.BatchID != manifest.BatchID {
		t.Errorf("BatchID = %q, want %q", read.BatchID, manifest.BatchID)
	}
}

func TestSave_ConcurrentWriters(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	initialIdx := &Index{Version: IndexVersion, Entries: nil}
	initialData, _ := json.Marshal(initialIdx)
	if err := os.WriteFile(indexPath, initialData, 0644); err != nil {
		t.Fatalf("write initial index: %v", err)
	}

	const numWriters = 20
	var wg sync.WaitGroup
	wg.Add(numWriters)

	for i := 0; i < numWriters; i++ {
		i := i
		go func() {
			defer wg.Done()
			idx, err := Load(indexPath)
			if err != nil {
				t.Errorf("goroutine %d: Load failed: %v", i, err)
				return
			}
			idx.Add(Entry{
				ID:        fmt.Sprintf("batch-%d", i),
				Path:      filepath.Join(batchesDir, fmt.Sprintf("batch-%d", i)),
				Kind:      KindIssue,
				Status:    StatusActive,
				CreatedAt: time.Now(),
			})
			if err := idx.Save(indexPath); err != nil {
				t.Errorf("goroutine %d: Save failed: %v", i, err)
			}
		}()
	}

	wg.Wait()

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("final Load failed: %v", err)
	}

	if len(loaded.Entries) == 0 {
		t.Fatalf("final index has 0 batches, want at least 1 (last-writer-wins is acceptable)")
	}

	for _, b := range loaded.Entries {
		if b.ID == "" {
			t.Errorf("batch has empty ID")
		}
	}

	tmpFiles, err := filepath.Glob(indexPath + ".tmp*")
	if err != nil {
		t.Fatalf("glob tmp files: %v", err)
	}
	if len(tmpFiles) != 0 {
		t.Errorf("temp files still exist after all writers done: %v", tmpFiles)
	}
}

func TestProbeStatus_StatFnPermissionError_LeavesStatus(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	realBatchDir := filepath.Join(batchesDir, "realbatch")
	if err := os.MkdirAll(realBatchDir, 0755); err != nil {
		t.Fatalf("create real batch dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "realbatch", Path: realBatchDir, Kind: KindIssue, Status: StatusActive},
		},
		StatFn: func(path string) (os.FileInfo, error) {
			return nil, os.ErrPermission
		},
	}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(indexPath, data, 0644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if err := loaded.EnsureStatus(); err != nil {
		t.Fatalf("EnsureStatus failed: %v", err)
	}

	for _, b := range loaded.Entries {
		if b.ID == "realbatch" && b.Status != StatusActive {
			t.Errorf("realbatch Status = %q, want %q (non-ENOENT error should not flip status)", b.Status, StatusActive)
		}
	}
}

func TestLoad_BakFallback_CorruptMain(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	bakPath := indexPath + ".bak"

	goodIdx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "recovered-batch", Path: "/recovered", Kind: KindIssue, Status: StatusActive},
		},
	}
	goodData, _ := json.Marshal(goodIdx)
	if err := os.WriteFile(bakPath, goodData, 0644); err != nil {
		t.Fatalf("write bak: %v", err)
	}

	if err := os.WriteFile(indexPath, []byte("not valid json{{{"), 0644); err != nil {
		t.Fatalf("write corrupt main: %v", err)
	}

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(loaded.Entries) != 1 {
		t.Errorf("Batches len = %d, want 1 (recovered from .bak)", len(loaded.Entries))
	}
	if loaded.Entries[0].ID != "recovered-batch" {
		t.Errorf("Batch ID = %q, want %q", loaded.Entries[0].ID, "recovered-batch")
	}
}

func TestLoad_BakFallback_MissingMain_ValidBak(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	bakPath := indexPath + ".bak"

	goodIdx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "bak-only-batch", Path: "/bakonly", Kind: KindIssue, Status: StatusActive},
		},
	}
	goodData, _ := json.Marshal(goodIdx)
	if err := os.WriteFile(bakPath, goodData, 0644); err != nil {
		t.Fatalf("write bak: %v", err)
	}

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("Load with missing main but valid bak failed: %v", err)
	}
	if len(loaded.Entries) != 1 {
		t.Errorf("Batches len = %d, want 1 (recovered from .bak)", len(loaded.Entries))
	}
}

func TestLoad_CrashRecovery(t *testing.T) {
	repoRoot := t.TempDir()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatalf("create batches dir: %v", err)
	}

	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	bakPath := indexPath + ".bak"

	preCrashIdx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "pre-crash", Path: "/precrash", Kind: KindIssue, Status: StatusActive},
		},
	}
	preCrashData, _ := json.Marshal(preCrashIdx)
	if err := os.WriteFile(bakPath, preCrashData, 0644); err != nil {
		t.Fatalf("write bak: %v", err)
	}

	if err := os.WriteFile(indexPath, []byte("garbage"), 0644); err != nil {
		t.Fatalf("write corrupt main: %v", err)
	}

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("Load recovered from bak failed: %v", err)
	}
	if len(loaded.Entries) != 1 {
		t.Errorf("Batches len = %d, want 1 (recovered from .bak after crash)", len(loaded.Entries))
	}
	if loaded.Entries[0].ID != "pre-crash" {
		t.Errorf("Batch ID = %q, want %q", loaded.Entries[0].ID, "pre-crash")
	}
}

func TestWriteReadReviewState(t *testing.T) {
	runDir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	state := ReviewState{
		PR: 1217,
		SeenComments: []SeenComment{
			{CommentID: "12345", Status: "success", Timestamp: now},
		},
		Claims: map[string]Claim{
			"12345": {Holder: "pid123", Since: now},
		},
	}

	if err := WriteReviewState(runDir, state); err != nil {
		t.Fatalf("WriteReviewState failed: %v", err)
	}

	read, err := ReadReviewState(runDir)
	if err != nil {
		t.Fatalf("ReadReviewState failed: %v", err)
	}
	if read.PR != state.PR {
		t.Errorf("PR = %d, want %d", read.PR, state.PR)
	}
	if len(read.SeenComments) != 1 {
		t.Fatalf("SeenComments len = %d, want 1", len(read.SeenComments))
	}
}

// TestWriteReviewState_AtomicRenameNoLeftoverTmp asserts that
// WriteReviewState writes review-state.json through a unique temp file
// (os.CreateTemp + os.Rename). A pre-existing destination is fully
// replaced with the new state, and no .tmp* siblings remain in the run
// directory after the call returns.
func TestWriteReviewState_AtomicRenameNoLeftoverTmp(t *testing.T) {
	t.Run("happy path replaces stale destination atomically", func(t *testing.T) {
		runDir := t.TempDir()
		statePath := filepath.Join(runDir, "review-state.json")

		prevData, _ := json.Marshal(ReviewState{
			PR:           9999,
			SeenComments: []SeenComment{},
			Claims:       map[string]Claim{},
		})
		if err := os.WriteFile(statePath, prevData, 0644); err != nil {
			t.Fatalf("write stale state: %v", err)
		}

		now := time.Now().Truncate(time.Second)
		state := ReviewState{
			PR: 1217,
			SeenComments: []SeenComment{
				{CommentID: "12345", Status: "success", Timestamp: now},
			},
			Claims: map[string]Claim{
				"12345": {Holder: "pid123", Since: now},
			},
		}

		if err := WriteReviewState(runDir, state); err != nil {
			t.Fatalf("WriteReviewState: %v", err)
		}

		read, err := ReadReviewState(runDir)
		if err != nil {
			t.Fatalf("ReadReviewState: %v", err)
		}
		if read.PR != state.PR {
			t.Errorf("PR after rewrite: got %d, want %d (torn mix would decode to stale value)", read.PR, state.PR)
		}
		if len(read.SeenComments) != 1 || read.SeenComments[0].CommentID != "12345" {
			t.Errorf("SeenComments after rewrite: got %+v, want one entry with id 12345", read.SeenComments)
		}

		matches, err := filepath.Glob(filepath.Join(runDir, "review-state.json.tmp*"))
		if err != nil {
			t.Fatalf("glob tmp files: %v", err)
		}
		if len(matches) != 0 {
			t.Errorf("temp files still exist after WriteReviewState: %v", matches)
		}
	})

	t.Run("rename failure leaves previous file intact and no leftover tmp", func(t *testing.T) {
		runDir := t.TempDir()
		statePath := filepath.Join(runDir, "review-state.json")
		prev := ReviewState{
			PR:           9999,
			SeenComments: []SeenComment{},
			Claims:       map[string]Claim{},
		}
		prevData, _ := json.Marshal(prev)
		if err := os.WriteFile(statePath, prevData, 0644); err != nil {
			t.Fatalf("write prev: %v", err)
		}

		// Force the rename to fail: the destination's parent must be a
		// regular file, so os.CreateTemp succeeds (the dir still exists)
		// but os.Rename across it cannot land inside a non-directory.
		blocker := filepath.Join(runDir, "blocker")
		if err := os.WriteFile(blocker, []byte("not a dir"), 0644); err != nil {
			t.Fatalf("write blocker: %v", err)
		}
		badRunDir := filepath.Join(blocker, "fake-rundir")

		err := WriteReviewState(badRunDir, ReviewState{PR: 1217})
		if err == nil {
			t.Fatal("expected WriteReviewState to fail when destination parent is a regular file")
		}

		got, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("read previous file: %v", err)
		}
		if string(got) != string(prevData) {
			t.Errorf("previous file mutated on failed WriteReviewState\n got: %q\nwant: %q", got, prevData)
		}

		// The temp file (if any was created) must be cleaned up. We allow
		// for the case where CreateTemp failed first (in which case no
		// .tmp* exists at all) — both outcomes satisfy the contract.
		matches, err := filepath.Glob(filepath.Join(runDir, "review-state.json.tmp*"))
		if err != nil {
			t.Fatalf("glob tmp files: %v", err)
		}
		if len(matches) != 0 {
			t.Errorf("temp files leaked into runDir after failed WriteReviewState: %v", matches)
		}
	})
}

// TestRunRecord_JSONRoundTrip is the slice-1 tracer bullet. It pins
// the per-row JSON wire shape: a RunRecord must serialise to
// {"runId": ..., "status": "active|archived", "archivePath": ""} (the
// archivePath field is omitted when empty so the persisted batches.json
// stays compact for the common active-row case). Decoding back must
// produce the same struct.
func TestRunRecord_JSONRoundTrip(t *testing.T) {
	t.Run("active row omits archivePath", func(t *testing.T) {
		rec := RunRecord{RunID: "abcd-260618113825-42", Status: RunRecordStatusActive}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := decoded["runId"]; got != "abcd-260618113825-42" {
			t.Errorf("runId = %v, want %q", got, "abcd-260618113825-42")
		}
		if got := decoded["status"]; got != "active" {
			t.Errorf("status = %v, want %q", got, "active")
		}
		if _, present := decoded["archivePath"]; present {
			t.Errorf("archivePath must be omitted on active row, got %v", decoded["archivePath"])
		}

		var back RunRecord
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("unmarshal back: %v", err)
		}
		if back != rec {
			t.Errorf("round-trip mismatch: got %+v, want %+v", back, rec)
		}
	})

	t.Run("archived row includes archivePath", func(t *testing.T) {
		rec := RunRecord{
			RunID:       "abcd-260618113825-42",
			Status:      RunRecordStatusArchived,
			ArchivePath: "archive/abcd-260618113825-42/runs/abcd-260618113825-42",
		}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := decoded["archivePath"]; got != "archive/abcd-260618113825-42/runs/abcd-260618113825-42" {
			t.Errorf("archivePath = %v, want archived path", got)
		}

		var back RunRecord
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("unmarshal back: %v", err)
		}
		if back != rec {
			t.Errorf("round-trip mismatch: got %+v, want %+v", back, rec)
		}
	})
}

// TestIndex_AddRun_PersistsRecord covers slice 1: AddRun records a row
// against the named entry, dedupes by RunID, and the result survives
// a Save/Load round-trip so subsequent code can rely on the persisted
// record to drive lazy recovery.
func TestIndex_AddRun_PersistsRecord(t *testing.T) {
	repoRoot := t.TempDir()
	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "batch-1", Path: filepath.Join(repoRoot, ".sandman", "batches", "batch-1"), Kind: KindIssue, Status: StatusActive, CreatedAt: time.Now().Truncate(time.Second)},
		},
	}
	idx.AddRun("batch-1", RunRecord{RunID: "row-1", Status: RunRecordStatusActive})
	idx.AddRun("batch-1", RunRecord{RunID: "row-2", Status: RunRecordStatusActive})

	if got := len(idx.Entries[0].Runs); got != 2 {
		t.Fatalf("Runs len = %d, want 2", got)
	}

	if err := idx.Save(indexPath); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load(indexPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	entry := loaded.Resolve("batch-1")
	if entry == nil {
		t.Fatal("Resolve(batch-1) returned nil after round-trip")
	}
	if got := len(entry.Runs); got != 2 {
		t.Fatalf("Runs len after round-trip = %d, want 2", got)
	}
	runIDs := []string{entry.Runs[0].RunID, entry.Runs[1].RunID}
	if !reflect.DeepEqual(runIDs, []string{"row-1", "row-2"}) {
		t.Errorf("Runs round-trip order = %v, want [row-1 row-2]", runIDs)
	}
}

// TestIndex_MarkRunArchived_UpdatesRecord covers slice 1: MarkRunArchived
// flips the targeted row's record from active to archived, populates
// ArchivePath, and is a no-op when no row matches.
func TestIndex_MarkRunArchived_UpdatesRecord(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{
				ID:   "batch-1",
				Path: "/tmp/.sandman/batches/batch-1",
				Kind: KindIssue,
				Runs: []RunRecord{{RunID: "row-1", Status: RunRecordStatusActive}, {RunID: "row-2", Status: RunRecordStatusActive}},
			},
		},
	}

	if err := idx.MarkRunArchived("batch-1", "row-1", "archive/batch-1/runs/row-1"); err != nil {
		t.Fatalf("MarkRunArchived: %v", err)
	}

	if idx.Entries[0].Runs[0].Status != RunRecordStatusArchived {
		t.Errorf("row-1 status = %s, want %s", idx.Entries[0].Runs[0].Status, RunRecordStatusArchived)
	}
	if idx.Entries[0].Runs[0].ArchivePath != "archive/batch-1/runs/row-1" {
		t.Errorf("row-1 archivePath = %q, want archive path", idx.Entries[0].Runs[0].ArchivePath)
	}
	if idx.Entries[0].Runs[1].Status != RunRecordStatusActive {
		t.Errorf("row-2 status = %s, want %s (sibling must stay active)", idx.Entries[0].Runs[1].Status, RunRecordStatusActive)
	}

	if err := idx.MarkRunArchived("batch-1", "missing-row", "archive/missing"); err == nil {
		t.Errorf("MarkRunArchived on missing row must return an error, got nil")
	}
}

// TestIndex_ReconcileRuns_ArchivedMissingLive verifies slice 7: when
// the index already records a per-row ArchivePath but the live
// runs/<runID>/ folder is missing, ReconcileRuns leaves the record as
// archived (no change), preserving the post-archive on-disk view.
func TestIndex_ReconcileRuns_ArchivedMissingLive(t *testing.T) {
	repoRoot := t.TempDir()
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "batch-1")
	archiveDir := filepath.Join(repoRoot, ".sandman", "archive", "batch-1", "runs", "row-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatalf("mkdir batch: %v", err)
	}
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}

	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{
				ID:   "batch-1",
				Path: batchDir,
				Kind: KindIssue,
				Runs: []RunRecord{{RunID: "row-1", Status: RunRecordStatusArchived, ArchivePath: ".sandman/archive/batch-1/runs/row-1"}},
			},
		},
		StatFn: os.Stat,
	}

	idx.ReconcileRuns(repoRoot)

	entry := idx.Resolve("batch-1")
	if entry == nil {
		t.Fatal("Resolve(batch-1) returned nil")
	}
	if got := entry.Runs[0].Status; got != RunRecordStatusArchived {
		t.Errorf("row-1 status after reconcile = %s, want archived", got)
	}
	if got := entry.Runs[0].ArchivePath; got != ".sandman/archive/batch-1/runs/row-1" {
		t.Errorf("row-1 archivePath after reconcile = %q, want archived path", got)
	}
}

// TestIndex_ReconcileRuns_ArchivedMissingLiveAndArchive verifies slice
// 7: when the index records an ArchivePath but neither the live nor
// the archive folder exists on disk (a torn state from a crash), the
// row's record flips to unavailable with ArchivePath cleared.
func TestIndex_ReconcileRuns_ArchivedMissingLiveAndArchive(t *testing.T) {
	repoRoot := t.TempDir()
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "batch-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatalf("mkdir batch: %v", err)
	}

	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{
				ID:   "batch-1",
				Path: batchDir,
				Kind: KindIssue,
				Runs: []RunRecord{{RunID: "row-1", Status: RunRecordStatusArchived, ArchivePath: ".sandman/archive/batch-1/runs/row-1"}},
			},
		},
		StatFn: os.Stat,
	}

	idx.ReconcileRuns(repoRoot)

	entry := idx.Resolve("batch-1")
	if entry == nil {
		t.Fatal("Resolve(batch-1) returned nil")
	}
	if got := entry.Runs[0].Status; got != RunRecordStatusUnavailable {
		t.Errorf("row-1 status after reconcile (no live, no archive) = %s, want unavailable", got)
	}
	if got := entry.Runs[0].ArchivePath; got != "" {
		t.Errorf("row-1 archivePath after reconcile (no live, no archive) = %q, want cleared", got)
	}
}
