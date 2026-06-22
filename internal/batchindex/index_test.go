package batchindex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_MissingFile_ReturnsEmptyIndex(t *testing.T) {
	tmp := t.TempDir()
	idx, err := Load(filepath.Join(tmp, "batches.json"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if idx.Version != 1 {
		t.Errorf("Version = %d, want 1", idx.Version)
	}
	if len(idx.Entries) != 0 {
		t.Errorf("Entries = %v, want empty", idx.Entries)
	}
}

func TestLoad_MalformedFile_ReturnsError(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "batches.json")
	if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Error("Load() error = nil, want error")
	}
}

func TestSave_CreatesAtomicWrite(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "batches.json")

	idx := &Index{Version: 1, Entries: []Entry{{ID: "test", Path: "/tmp/test", Kind: "issue", Status: "active"}}, path: path}
	if err := idx.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var loaded Index
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if loaded.Version != 1 {
		t.Errorf("loaded.Version = %d, want 1", loaded.Version)
	}
	if len(loaded.Entries) != 1 {
		t.Fatalf("len(loaded.Entries) = %d, want 1", len(loaded.Entries))
	}
	if loaded.Entries[0].ID != "test" {
		t.Errorf("loaded.Entries[0].ID = %q, want %q", loaded.Entries[0].ID, "test")
	}
}

func TestSave_CreatesBackup(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "batches.json")

	os.WriteFile(path, []byte("old content"), 0644)

	idx := &Index{Version: 1, Entries: []Entry{}, path: path}
	if err := idx.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("ReadFile(.bak) error = %v", err)
	}
	if string(bak) != "old content" {
		t.Errorf("bak content = %q, want %q", string(bak), "old content")
	}
}

func TestAdd_NewEntry_Appends(t *testing.T) {
	idx := &Index{Version: 1, Entries: []Entry{}, path: "/tmp/batches.json"}
	entry := Entry{ID: "batch1", Path: "/tmp/batches/batch1", Kind: "issue", Issues: []int{42}}
	idx.Add(entry)

	if len(idx.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0].Status != "active" {
		t.Errorf("Entries[0].Status = %q, want %q", idx.Entries[0].Status, "active")
	}
}

func TestAdd_ExistingEntry_Updates(t *testing.T) {
	createdAt := time.Now().Add(-1 * time.Hour)
	idx := &Index{
		Version: 1,
		Entries: []Entry{{ID: "batch1", Path: "/old/path", Kind: "issue", Status: "active", CreatedAt: createdAt}},
		path:    "/tmp/batches.json",
	}
	entry := Entry{ID: "batch1", Path: "/new/path", Kind: "review", Issues: []int{42, 43}}
	idx.Add(entry)

	if len(idx.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0].Path != "/new/path" {
		t.Errorf("Entries[0].Path = %q, want %q", idx.Entries[0].Path, "/new/path")
	}
	if !idx.Entries[0].CreatedAt.Equal(createdAt) {
		t.Errorf("Entries[0].CreatedAt = %v, want %v (preserved)", idx.Entries[0].CreatedAt, createdAt)
	}
}

func TestResolve_Found_ReturnsEntry(t *testing.T) {
	idx := &Index{Version: 1, Entries: []Entry{{ID: "batch1", Path: "/tmp/batches/batch1"}}, path: "/tmp/batches.json"}
	entry := idx.Resolve("batch1")
	if entry == nil {
		t.Fatal("Resolve() returned nil")
	}
	if entry.ID != "batch1" {
		t.Errorf("entry.ID = %q, want %q", entry.ID, "batch1")
	}
}

func TestResolve_NotFound_ReturnsNilNil(t *testing.T) {
	idx := &Index{Version: 1, Entries: []Entry{}, path: "/tmp/batches.json"}
	entry := idx.Resolve("nonexistent")
	if entry != nil {
		t.Errorf("Resolve() = %v, want nil", entry)
	}
}

func TestEnsureStatus_ENOENT_SetsUnavailable(t *testing.T) {
	idx := &Index{
		Version: 1,
		Entries: []Entry{
			{ID: "batch1", Path: "/nonexistent/batch1", Status: "active"},
		},
		path: "/tmp/batches.json",
		StatFn: func(path string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		},
	}
	if err := idx.EnsureStatus(); err != nil {
		t.Fatalf("EnsureStatus() error = %v", err)
	}
	if idx.Entries[0].Status != "unavailable" {
		t.Errorf("Entries[0].Status = %q, want %q", idx.Entries[0].Status, "unavailable")
	}
}

func TestEnsureStatus_OtherError_LeavesStatusUnchanged(t *testing.T) {
	idx := &Index{
		Version: 1,
		Entries: []Entry{
			{ID: "batch1", Path: "/some/path", Status: "active"},
		},
		path: "/tmp/batches.json",
		StatFn: func(path string) (os.FileInfo, error) {
			return nil, os.ErrPermission
		},
	}
	if err := idx.EnsureStatus(); err != nil {
		t.Fatalf("EnsureStatus() error = %v", err)
	}
	if idx.Entries[0].Status != "active" {
		t.Errorf("Entries[0].Status = %q, want %q", idx.Entries[0].Status, "active")
	}
}

func TestEnsureStatus_AlreadyUnavailable_Skips(t *testing.T) {
	calls := 0
	idx := &Index{
		Version: 1,
		Entries: []Entry{
			{ID: "batch1", Path: "/nonexistent/batch1", Status: "unavailable"},
		},
		path: "/tmp/batches.json",
		StatFn: func(path string) (os.FileInfo, error) {
			calls++
			return nil, os.ErrNotExist
		},
	}
	if err := idx.EnsureStatus(); err != nil {
		t.Fatalf("EnsureStatus() error = %v", err)
	}
	if calls != 0 {
		t.Errorf("StatFn called %d times, want 0", calls)
	}
}

func TestArchiveBatch_ActiveEntry_TransitionsToArchived(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "batches.json")
	now := time.Now()

	idx := &Index{
		Version: 1,
		Entries: []Entry{
			{ID: "batch1", Path: "/tmp/batches/batch1", Status: "active", CreatedAt: time.Now()},
		},
		path: path,
	}
	if err := idx.ArchiveBatch("batch1", now); err != nil {
		t.Fatalf("ArchiveBatch() error = %v", err)
	}

	if idx.Entries[0].Status != "archived" {
		t.Errorf("Entries[0].Status = %q, want %q", idx.Entries[0].Status, "archived")
	}
	if idx.Entries[0].ArchivedAt == nil || !idx.Entries[0].ArchivedAt.Equal(now) {
		t.Errorf("Entries[0].ArchivedAt = %v, want %v", idx.Entries[0].ArchivedAt, now)
	}
}

func TestArchiveBatch_NotFound_ReturnsError(t *testing.T) {
	idx := &Index{Version: 1, Entries: []Entry{}, path: "/tmp/batches.json"}
	err := idx.ArchiveBatch("nonexistent", time.Now())
	if err != ErrNotFound {
		t.Errorf("ArchiveBatch() error = %v, want %v", err, ErrNotFound)
	}
}

func TestRemoveBatch_Found_RemovesEntry(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "batches.json")

	idx := &Index{
		Version: 1,
		Entries: []Entry{
			{ID: "batch1", Path: "/tmp/batches/batch1", Status: "active"},
			{ID: "batch2", Path: "/tmp/batches/batch2", Status: "active"},
		},
		path: path,
	}
	if err := idx.RemoveBatch("batch1"); err != nil {
		t.Fatalf("RemoveBatch() error = %v", err)
	}

	if len(idx.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0].ID != "batch2" {
		t.Errorf("Entries[0].ID = %q, want %q", idx.Entries[0].ID, "batch2")
	}
}

func TestRemoveBatch_NotFound_ReturnsError(t *testing.T) {
	idx := &Index{Version: 1, Entries: []Entry{}, path: "/tmp/batches.json"}
	err := idx.RemoveBatch("nonexistent")
	if err != ErrNotFound {
		t.Errorf("RemoveBatch() error = %v, want %v", err, ErrNotFound)
	}
}
