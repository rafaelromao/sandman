package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

func TestDiscoverPortalInstances_IndexFirstWithMissingSocket(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "batch-no-socket")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:     "batch-no-socket",
				Path:   batchDir,
				Kind:   batchindex.KindIssue,
				Status: batchindex.StatusActive,
			},
		},
		StatFn: os.Stat,
	}

	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		t.Fatalf("discoverPortalInstances: %v", err)
	}

	if len(instances) != 0 {
		t.Fatalf("expected 0 instances (no socket file), got %d", len(instances))
	}
}

func TestDiscoverPortalInstances_IndexOnlyReturnsIndexedBatches(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	indexedBatch := filepath.Join(repoRoot, ".sandman", "batches", "batch-indexed")
	if err := os.MkdirAll(indexedBatch, 0755); err != nil {
		t.Fatal(err)
	}

	onDiskOnly := filepath.Join(repoRoot, ".sandman", "batches", "batch-on-disk-only")
	if err := os.MkdirAll(onDiskOnly, 0755); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:     "batch-indexed",
				Path:   indexedBatch,
				Kind:   batchindex.KindIssue,
				Status: batchindex.StatusActive,
			},
		},
		StatFn: os.Stat,
	}

	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		t.Fatalf("discoverPortalInstances: %v", err)
	}

	if len(instances) != 0 {
		t.Fatalf("expected 0 (indexed batch has no socket, on-disk-only not in index), got %d", len(instances))
	}
}

func TestDiscoverPortalInstances_MissingFolderFlipsUnavailable(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchActiveDir := filepath.Join(repoRoot, ".sandman", "batches", "batch-active")
	if err := os.MkdirAll(batchActiveDir, 0755); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:     "batch-active",
				Path:   batchActiveDir,
				Kind:   batchindex.KindIssue,
				Status: batchindex.StatusActive,
			},
			{
				ID:     "batch-missing",
				Path:   filepath.Join(repoRoot, ".sandman", "batches", "batch-missing"),
				Kind:   batchindex.KindIssue,
				Status: batchindex.StatusActive,
			},
		},
		StatFn: os.Stat,
	}

	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	loadedIdx, err := batchindex.Load(idxPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := loadedIdx.EnsureStatus(); err != nil {
		t.Fatal(err)
	}

	byID := make(map[string]batchindex.Status)
	for _, e := range loadedIdx.Entries {
		byID[e.ID] = e.Status
	}

	if byID["batch-active"] != batchindex.StatusActive {
		t.Fatalf("expected batch-active to stay StatusActive, got %v", byID["batch-active"])
	}
	if byID["batch-missing"] != batchindex.StatusUnavailable {
		t.Fatalf("expected batch-missing to flip to StatusUnavailable, got %v", byID["batch-missing"])
	}
}

func TestDiscoverPortalInstances_PermissionErrorDoesNotFlip(t *testing.T) {
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:     "batch-forbidden",
				Path:   "/root/batch-forbidden",
				Kind:   batchindex.KindIssue,
				Status: batchindex.StatusActive,
			},
		},
		StatFn: func(path string) (os.FileInfo, error) {
			return nil, os.ErrPermission
		},
	}

	if err := idx.EnsureStatus(); err != nil {
		t.Fatal(err)
	}

	if idx.Entries[0].Status != batchindex.StatusActive {
		t.Fatalf("expected StatusActive (non-ENOENT error should not flip), got %v", idx.Entries[0].Status)
	}
}

func TestDiscoverPortalInstances_ArchivedStatusNotProbed(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	archivedPath := filepath.Join(repoRoot, ".sandman", "archive", "batch-archived")
	if err := os.MkdirAll(archivedPath, 0755); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:     "batch-archived",
				Path:   archivedPath,
				Kind:   batchindex.KindIssue,
				Status: batchindex.StatusArchived,
			},
		},
		StatFn: os.Stat,
	}

	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { t.Fatal("liveness probe should not be called for archived entries"); return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		t.Fatalf("discoverPortalInstances: %v", err)
	}

	if len(instances) != 0 {
		t.Fatalf("expected 0 instances (archived not probed), got %d", len(instances))
	}
}

func TestDiscoverPortalInstances_UnavailableStatusNotProbed(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:     "batch-unavailable",
				Path:   filepath.Join(repoRoot, ".sandman", "batches", "batch-nonexistent"),
				Kind:   batchindex.KindIssue,
				Status: batchindex.StatusUnavailable,
			},
		},
		StatFn: os.Stat,
	}

	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool {
		t.Fatal("liveness probe should not be called for unavailable entries")
		return false
	}
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		t.Fatalf("discoverPortalInstances: %v", err)
	}

	if len(instances) != 0 {
		t.Fatalf("expected 0 instances (unavailable not probed), got %d", len(instances))
	}
}

func TestBatchIndexEnsureStatus_AllScenarios(t *testing.T) {
	t.Run("active ENOENT flips", func(t *testing.T) {
		idx := &batchindex.Index{
			Version: batchindex.IndexVersion,
			Entries: []batchindex.Entry{{ID: "x", Path: "/nonexistent", Status: batchindex.StatusActive}},
			StatFn:  os.Stat,
		}
		if err := idx.EnsureStatus(); err != nil {
			t.Fatal(err)
		}
		if idx.Entries[0].Status != batchindex.StatusUnavailable {
			t.Fatalf("got %v", idx.Entries[0].Status)
		}
	})

	t.Run("archived ENOENT flips", func(t *testing.T) {
		idx := &batchindex.Index{
			Version: batchindex.IndexVersion,
			Entries: []batchindex.Entry{{ID: "x", Path: "/nonexistent", Status: batchindex.StatusArchived}},
			StatFn:  os.Stat,
		}
		if err := idx.EnsureStatus(); err != nil {
			t.Fatal(err)
		}
		if idx.Entries[0].Status != batchindex.StatusUnavailable {
			t.Fatalf("got %v", idx.Entries[0].Status)
		}
	})

	t.Run("unavailable ENOENT stays unavailable", func(t *testing.T) {
		idx := &batchindex.Index{
			Version: batchindex.IndexVersion,
			Entries: []batchindex.Entry{{ID: "x", Path: "/nonexistent", Status: batchindex.StatusUnavailable}},
			StatFn:  os.Stat,
		}
		if err := idx.EnsureStatus(); err != nil {
			t.Fatal(err)
		}
		if idx.Entries[0].Status != batchindex.StatusUnavailable {
			t.Fatalf("got %v", idx.Entries[0].Status)
		}
	})

	t.Run("non-ENOENT error does not flip", func(t *testing.T) {
		idx := &batchindex.Index{
			Version: batchindex.IndexVersion,
			Entries: []batchindex.Entry{{ID: "x", Path: "/forbidden", Status: batchindex.StatusActive}},
			StatFn: func(path string) (os.FileInfo, error) {
				return nil, os.ErrPermission
			},
		}
		if err := idx.EnsureStatus(); err != nil {
			t.Fatal(err)
		}
		if idx.Entries[0].Status != batchindex.StatusActive {
			t.Fatalf("got %v", idx.Entries[0].Status)
		}
	})
}

func TestPortalLogPathForRun_ResolvesRunLogInRunFolder(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "batch-42")
	runDir := filepath.Join(batchDir, "runs", "run-abc")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(runDir, "run.log")
	if err := os.WriteFile(logPath, []byte("test log"), 0644); err != nil {
		t.Fatal(err)
	}

	view := &portalRunsView{}
	result := view.portalLogPathForRun(repoRoot, 42, "", "run-abc", false, 0, batchDir)

	expected := filepath.Join(batchDir, "runs", "run-abc", "run.log")
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}

	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if string(data) != "test log" {
		t.Fatalf("expected 'test log', got %q", string(data))
	}
}

func TestPortalLogDownload_PermittedLogPathFormat(t *testing.T) {
	cases := []struct {
		path     string
		expected bool
	}{
		{"batches/batch-1/runs/run-1/run.log", true},
		{"archive/batch-1/runs/run-1/run.log", true},
		{"batches/my-batch/runs/my-run/run.log", true},
		{"batches/batch-1/runs/run-1/run.log.extra", false},
		{"batches/batch-1/run.log", false},
		{"batches/batch-1/runs/run.log", false},
		{"logs/run.log", false},
		{"batches/../etc/passwd", false},
	}

	for _, c := range cases {
		if permittedLogPath.MatchString(c.path) != c.expected {
			t.Errorf("MatchString(%q) = %v, want %v", c.path, !c.expected, c.expected)
		}
	}
}
