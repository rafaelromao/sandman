package batchindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		Batches: []Batch{
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
	if len(loaded.Batches) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(loaded.Batches))
	}
	if loaded.Batches[0].ID != "abc123" {
		t.Errorf("Batch[0].ID = %q, want %q", loaded.Batches[0].ID, "abc123")
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
	if idx.Batches != nil {
		t.Errorf("Batches = %v, want nil", idx.Batches)
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
		Batches: []Batch{
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

	idx1 := &Index{Version: IndexVersion, Batches: []Batch{{ID: "first", Path: "/first", Kind: KindIssue, Status: StatusActive}}}
	data1, _ := json.Marshal(idx1)
	if err := os.WriteFile(indexPath, data1, 0644); err != nil {
		t.Fatalf("write initial index: %v", err)
	}

	idx2 := &Index{Version: IndexVersion, Batches: []Batch{{ID: "second", Path: "/second", Kind: KindReview, Status: StatusActive}}}
	if err := idx2.Save(indexPath); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	bakPath := indexPath + ".bak"
	if _, err := os.Stat(bakPath); err != nil {
		t.Errorf("backup file does not exist: %v", err)
	}
}

func TestResolveBatch_FindsByBatchID(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Batches: []Batch{
			{ID: "abc123", Path: "/path/abc123", Kind: KindIssue, Status: StatusActive},
			{ID: "def456", Path: "/path/def456", Kind: KindReview, Status: StatusActive},
		},
	}

	batch := idx.ResolveBatch("abc123")
	if batch == nil {
		t.Fatal("ResolveBatch returned nil, want batch")
	}
	if batch.ID != "abc123" {
		t.Errorf("batch.ID = %q, want %q", batch.ID, "abc123")
	}
}

func TestResolveBatch_NotFound(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Batches: []Batch{
			{ID: "abc123", Path: "/path/abc123", Kind: KindIssue, Status: StatusActive},
		},
	}

	batch := idx.ResolveBatch("nonexistent")
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
		Batches: []Batch{
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
	if loaded.Batches[0].Status != StatusUnavailable {
		t.Errorf("Status = %q, want %q", loaded.Batches[0].Status, StatusUnavailable)
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
		Batches: []Batch{
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

	for _, b := range loaded.Batches {
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
	batch := Batch{
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

	var decoded Batch
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
		Batches: []Batch{{
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
		Batches: []Batch{{
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

func TestAddBatch_New(t *testing.T) {
	idx := &Index{Version: IndexVersion}
	batch := Batch{ID: "abc123", Kind: KindIssue}
	idx.AddBatch(batch)
	if len(idx.Batches) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(idx.Batches))
	}
	if idx.Batches[0].ID != "abc123" {
		t.Errorf("Batches[0].ID = %q, want %q", idx.Batches[0].ID, "abc123")
	}
	if idx.Batches[0].Status != StatusActive {
		t.Errorf("Batches[0].Status = %q, want %q", idx.Batches[0].Status, StatusActive)
	}
}

func TestAddBatch_Existing(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Batches: []Batch{
			{ID: "abc123", Kind: KindIssue, Status: StatusActive},
		},
	}
	newBatch := Batch{ID: "abc123", Kind: KindReview}
	idx.AddBatch(newBatch)
	if len(idx.Batches) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(idx.Batches))
	}
	if idx.Batches[0].Kind != KindReview {
		t.Errorf("Batches[0].Kind = %q, want %q", idx.Batches[0].Kind, KindReview)
	}
	if idx.Batches[0].Status != StatusActive {
		t.Errorf("Batches[0].Status = %q, want %q", idx.Batches[0].Status, StatusActive)
	}
}

func TestAddBatch_EmptyID_BackfilledFromPathBasename(t *testing.T) {
	idx := &Index{Version: IndexVersion}
	batch := Batch{
		ID:     "",
		Path:   "/tmp/sandman/.sandman/batches/abc123",
		Kind:   KindIssue,
		Status: StatusActive,
	}
	idx.AddBatch(batch)

	if len(idx.Batches) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(idx.Batches))
	}
	if idx.Batches[0].ID != "abc123" {
		t.Errorf("Batches[0].ID = %q, want %q (backfilled from path basename)", idx.Batches[0].ID, "abc123")
	}
}

func TestAddBatch_EmptyID_NoPathBasename_FallsBackToPath(t *testing.T) {
	// When the path has no usable basename (rare — only happens for
	// an empty path), AddBatch must still produce an addressable batch to
	// avoid dropping it on the floor. The resulting ID is the literal
	// path or a stable placeholder.
	idx := &Index{Version: IndexVersion}
	idx.AddBatch(Batch{ID: "", Path: ".", Kind: KindIssue})

	if len(idx.Batches) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(idx.Batches))
	}
	if idx.Batches[0].ID == "" {
		t.Error("Batches[0].ID is still empty; AddBatch must not produce an unidentified batch")
	}
}

func TestAddBatch_TwoEmptyIDs_DoNotEvict(t *testing.T) {
	// Regression for issue #1464: a second empty-id AddBatch used to
	// overwrite the previous one in place (same "" key in the lookup),
	// silently evicting the prior batch from the index. After the fix
	// both batches must be addressable and persisted.
	idx := &Index{Version: IndexVersion}
	idx.AddBatch(Batch{
		ID:   "",
		Path: "/tmp/sandman/.sandman/batches/first-001",
		Kind: KindIssue,
	})
	idx.AddBatch(Batch{
		ID:   "",
		Path: "/tmp/sandman/.sandman/batches/second-002",
		Kind: KindIssue,
	})

	if len(idx.Batches) != 2 {
		t.Fatalf("Batches len = %d, want 2 (empty-id must not collide)", len(idx.Batches))
	}

	paths := make(map[string]bool, len(idx.Batches))
	for i, b := range idx.Batches {
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
		Batches: []Batch{
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
	if len(loaded.Batches) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(loaded.Batches))
	}
	if loaded.Batches[0].ID == "" {
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
	initialIdx := &Index{Version: IndexVersion, Batches: nil}
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
			idx.AddBatch(Batch{
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

	if len(loaded.Batches) == 0 {
		t.Fatalf("final index has 0 batches, want at least 1 (last-writer-wins is acceptable)")
	}

	for _, b := range loaded.Batches {
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
		Batches: []Batch{
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

	for _, b := range loaded.Batches {
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
		Batches: []Batch{
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
	if len(loaded.Batches) != 1 {
		t.Errorf("Batches len = %d, want 1 (recovered from .bak)", len(loaded.Batches))
	}
	if loaded.Batches[0].ID != "recovered-batch" {
		t.Errorf("Batch ID = %q, want %q", loaded.Batches[0].ID, "recovered-batch")
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
		Batches: []Batch{
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
	if len(loaded.Batches) != 1 {
		t.Errorf("Batches len = %d, want 1 (recovered from .bak)", len(loaded.Batches))
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
		Batches: []Batch{
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
	if len(loaded.Batches) != 1 {
		t.Errorf("Batches len = %d, want 1 (recovered from .bak after crash)", len(loaded.Batches))
	}
	if loaded.Batches[0].ID != "pre-crash" {
		t.Errorf("Batch ID = %q, want %q", loaded.Batches[0].ID, "pre-crash")
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
