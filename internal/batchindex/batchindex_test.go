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
		t.Fatalf("Entries len = %d, want 1", len(loaded.Entries))
	}
	if loaded.Entries[0].ID != "abc123" {
		t.Errorf("Entry[0].ID = %q, want %q", loaded.Entries[0].ID, "abc123")
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
		t.Errorf("Entries = %v, want nil", idx.Entries)
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

func TestResolve_FindsEntry(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "abc123", Path: "/path/abc123", Kind: KindIssue, Status: StatusActive},
			{ID: "def456", Path: "/path/def456", Kind: KindReview, Status: StatusActive},
		},
	}

	entry := idx.Resolve("abc123")
	if entry == nil {
		t.Fatal("Resolve returned nil, want entry")
	}
	if entry.ID != "abc123" {
		t.Errorf("entry.ID = %q, want %q", entry.ID, "abc123")
	}
}

func TestResolve_NotFound(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "abc123", Path: "/path/abc123", Kind: KindIssue, Status: StatusActive},
		},
	}

	entry := idx.Resolve("nonexistent")
	if entry != nil {
		t.Errorf("entry = %v, want nil", entry)
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

	for _, e := range loaded.Entries {
		if e.ID == "realbatch" && e.Status != StatusActive {
			t.Errorf("realbatch Status = %q, want %q", e.Status, StatusActive)
		}
		if e.ID == "missing" && e.Status != StatusUnavailable {
			t.Errorf("missing Status = %q, want %q", e.Status, StatusUnavailable)
		}
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
		Status:       "active",
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

func TestEntry_JSONSchema(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	entry := Entry{
		ID:         "abc123",
		Path:       ".sandman/batches/abc123",
		Kind:       KindIssue,
		Status:     StatusActive,
		CreatedAt:  now,
		Issues:     []int{1213, 1214},
		ArchivedAt: nil,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Entry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.ID != entry.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, entry.ID)
	}
	if decoded.Kind != entry.Kind {
		t.Errorf("Kind = %q, want %q", decoded.Kind, entry.Kind)
	}
	if decoded.Status != entry.Status {
		t.Errorf("Status = %q, want %q", decoded.Status, entry.Status)
	}
	if len(decoded.Issues) != 2 {
		t.Errorf("Issues len = %d, want 2", len(decoded.Issues))
	}
}

func TestEntry_JSONSchema_PromptOnlyIssuesAreExplicitEmptyArray(t *testing.T) {
	entry := Entry{
		ID:        "prompt-only-abc123",
		Path:      ".sandman/batches/prompt-only-abc123",
		Kind:      KindPromptOnly,
		Status:    StatusActive,
		CreatedAt: time.Now().Truncate(time.Second),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	rawIssues, ok := decoded["issues"]
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

func TestEntry_JSONSchema_PromptOnlyIssuesIgnoreStaleValues(t *testing.T) {
	entry := Entry{
		ID:        "prompt-only-abc123",
		Path:      ".sandman/batches/prompt-only-abc123",
		Kind:      KindPromptOnly,
		Status:    StatusActive,
		CreatedAt: time.Now().Truncate(time.Second),
		Issues:    []int{1, 2, 3},
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	rawIssues, ok := decoded["issues"]
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
	entry := Entry{ID: "abc123", Kind: KindIssue}
	idx.Add(entry)
	if len(idx.Entries) != 1 {
		t.Fatalf("Entries len = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0].ID != "abc123" {
		t.Errorf("Entries[0].ID = %q, want %q", idx.Entries[0].ID, "abc123")
	}
	if idx.Entries[0].Status != StatusActive {
		t.Errorf("Entries[0].Status = %q, want %q", idx.Entries[0].Status, StatusActive)
	}
}

func TestAddEntry_Existing(t *testing.T) {
	idx := &Index{
		Version: IndexVersion,
		Entries: []Entry{
			{ID: "abc123", Kind: KindIssue, Status: StatusActive},
		},
	}
	newEntry := Entry{ID: "abc123", Kind: KindReview}
	idx.Add(newEntry)
	if len(idx.Entries) != 1 {
		t.Fatalf("Entries len = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0].Kind != KindReview {
		t.Errorf("Entries[0].Kind = %q, want %q", idx.Entries[0].Kind, KindReview)
	}
	if idx.Entries[0].Status != StatusActive {
		t.Errorf("Entries[0].Status = %q, want %q", idx.Entries[0].Status, StatusActive)
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
				ID:        fmt.Sprintf("entry-%d", i),
				Path:      filepath.Join(batchesDir, fmt.Sprintf("entry-%d", i)),
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
		t.Fatalf("final index has 0 entries, want at least 1 (last-writer-wins is acceptable)")
	}

	for _, e := range loaded.Entries {
		if e.ID == "" {
			t.Errorf("entry has empty ID")
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

	for _, e := range loaded.Entries {
		if e.ID == "realbatch" && e.Status != StatusActive {
			t.Errorf("realbatch Status = %q, want %q (non-ENOENT error should not flip status)", e.Status, StatusActive)
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
			{ID: "recovered-entry", Path: "/recovered", Kind: KindIssue, Status: StatusActive},
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
		t.Errorf("Entries len = %d, want 1 (recovered from .bak)", len(loaded.Entries))
	}
	if loaded.Entries[0].ID != "recovered-entry" {
		t.Errorf("Entry ID = %q, want %q", loaded.Entries[0].ID, "recovered-entry")
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
			{ID: "bak-only-entry", Path: "/bakonly", Kind: KindIssue, Status: StatusActive},
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
		t.Errorf("Entries len = %d, want 1 (recovered from .bak)", len(loaded.Entries))
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
		t.Errorf("Entries len = %d, want 1 (recovered from .bak after crash)", len(loaded.Entries))
	}
	if loaded.Entries[0].ID != "pre-crash" {
		t.Errorf("Entry ID = %q, want %q", loaded.Entries[0].ID, "pre-crash")
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
