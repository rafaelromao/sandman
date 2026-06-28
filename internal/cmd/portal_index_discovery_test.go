package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestDiscoverPortalInstances_IndexFirstDiscovery(t *testing.T) {
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)

	// Create .sandman directory structure
	batchesDir := layout.BatchesDir
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create batch directories
	activeBatchID := "abcd-260622105532-issue-42"
	archivedBatchID := "efgh-260622105633-issue-99"
	unavailableBatchID := "ijkl-260622105734-issue-100"

	activeBatchPath := filepath.Join(batchesDir, activeBatchID)
	archivedBatchPath := filepath.Join(batchesDir, archivedBatchID)
	// unavailableBatchPath is intentionally NOT created

	if err := os.MkdirAll(activeBatchPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archivedBatchPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Create runs directories inside batches
	activeRunsPath := filepath.Join(activeBatchPath, "runs")
	archivedRunsPath := filepath.Join(archivedBatchPath, "runs")
	if err := os.MkdirAll(activeRunsPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archivedRunsPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Create run folders
	activeRun1Path := filepath.Join(activeRunsPath, "run-001")
	activeRun2Path := filepath.Join(activeRunsPath, "run-002")
	archivedRun1Path := filepath.Join(archivedRunsPath, "run-001")

	for _, p := range []string{activeRun1Path, activeRun2Path, archivedRun1Path} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
		// Create run.json
		runManifest := batchindex.RunManifest{
			RunID:        filepath.Base(p),
			BatchID:      filepath.Base(filepath.Dir(filepath.Dir(p))),
			Issue:        42,
			Branch:       "sandman/42-fix",
			BaseBranch:   "main",
			WorktreePath: filepath.Join(repoRoot, ".sandman", "worktrees", "sandman-42-fix"),
			Kind:         batchindex.KindIssue,
			CreatedAt:    time.Now(),
			Status:       batchindex.RunManifestStatusActive,
		}
		if err := batchindex.WriteManifest(p, runManifest); err != nil {
			t.Fatal(err)
		}
	}

	// Create index with active, archived, and unavailable entries
	indexPath := layout.BatchesIndexPath
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:        activeBatchID,
				Path:      activeBatchPath,
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now().Add(-time.Hour),
				Issues:    []int{42},
			},
			{
				ID:         archivedBatchID,
				Path:       archivedBatchPath,
				Kind:       batchindex.KindIssue,
				Status:     batchindex.StatusArchived,
				CreatedAt:  time.Now().Add(-2 * time.Hour),
				Issues:     []int{99},
				ArchivedAt: timePtr(time.Now().Add(-time.Hour)),
			},
			{
				ID:        unavailableBatchID,
				Path:      filepath.Join(batchesDir, unavailableBatchID),
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now().Add(-time.Hour),
				Issues:    []int{100},
			},
		},
	}
	if err := idx.Save(indexPath); err != nil {
		t.Fatal(err)
	}

	// Call discoverPortalInstances
	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		t.Fatalf("discoverPortalInstances: %v", err)
	}

	// No sockets were created, so no instances should be returned.
	// The key behaviors under test:
	// 1. Index is read (not file system walk)
	// 2. Unavailable entries are flipped on read via MarkUnavailable
	// 3. No error is returned for missing entries
	// 4. Instances are filtered by socket presence
	if len(instances) != 0 {
		t.Fatalf("expected 0 instances (no sockets created), got %d", len(instances))
	}

	// Verify the index was saved with unavailable entries marked
	loadedIdx, err := batchindex.Load(indexPath)
	if err != nil {
		t.Fatalf("load index: %v", err)
	}

	unavailableEntry := loadedIdx.Resolve(unavailableBatchID)
	if unavailableEntry == nil {
		t.Fatal("unavailable entry not found in index")
	}
	if unavailableEntry.Status != batchindex.StatusUnavailable {
		t.Fatalf("expected unavailable status %q, got %q", batchindex.StatusUnavailable, unavailableEntry.Status)
	}

	// Verify other entries are not affected
	activeEntry := loadedIdx.Resolve(activeBatchID)
	if activeEntry == nil {
		t.Fatal("active entry not found in index")
	}
	if activeEntry.Status != batchindex.StatusActive {
		t.Fatalf("expected active status %q, got %q", batchindex.StatusActive, activeEntry.Status)
	}

	archivedEntry := loadedIdx.Resolve(archivedBatchID)
	if archivedEntry == nil {
		t.Fatal("archived entry not found in index")
	}
	if archivedEntry.Status != batchindex.StatusArchived {
		t.Fatalf("expected archived status %q, got %q", batchindex.StatusArchived, archivedEntry.Status)
	}
}

func TestDiscoverPortalInstances_NoSaveWhenClean(t *testing.T) {
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)

	batchesDir := layout.BatchesDir
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatal(err)
	}

	activeBatchID := "aaaa-260622105532-issue-1"
	activeBatchPath := filepath.Join(batchesDir, activeBatchID)
	if err := os.MkdirAll(activeBatchPath, 0755); err != nil {
		t.Fatal(err)
	}

	indexPath := layout.BatchesIndexPath
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:        activeBatchID,
				Path:      activeBatchPath,
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now().Add(-time.Hour),
				Issues:    []int{1},
			},
		},
	}
	if err := idx.Save(indexPath); err != nil {
		t.Fatal(err)
	}

	// Record file info before discover
	infoBefore, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	modTimeBefore := infoBefore.ModTime()

	// Call discoverPortalInstances — no entries need flipping, so no save
	if _, err := discoverPortalInstances(repoRoot); err != nil {
		t.Fatalf("discoverPortalInstances: %v", err)
	}

	// Verify file was NOT rewritten (no dirty entries → no save)
	infoAfter, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !infoAfter.ModTime().Equal(modTimeBefore) {
		t.Fatalf("index file was rewritten even though no entries needed flipping")
	}
}

func TestMarkUnavailable_OnlyFlipsENOENT(t *testing.T) {
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)

	batchesDir := layout.BatchesDir
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatal(err)
	}

	existingID := "bbbb-260622105532-issue-1"
	missingID := "cccc-260622105532-issue-2"
	existingPath := filepath.Join(batchesDir, existingID)
	if err := os.MkdirAll(existingPath, 0755); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:     existingID,
				Path:   existingPath,
				Status: batchindex.StatusActive,
			},
			{
				ID:     missingID,
				Path:   filepath.Join(batchesDir, missingID),
				Status: batchindex.StatusActive,
			},
		},
	}

	dirty := idx.MarkUnavailable()
	if !dirty {
		t.Fatal("expected dirty=true when an entry is flipped")
	}

	existing := idx.Resolve(existingID)
	if existing.Status != batchindex.StatusActive {
		t.Fatalf("existing entry should remain active, got %q", existing.Status)
	}

	missing := idx.Resolve(missingID)
	if missing.Status != batchindex.StatusUnavailable {
		t.Fatalf("missing entry should be unavailable, got %q", missing.Status)
	}
}

func TestMarkUnavailable_NoSaveWhenNothingChanges(t *testing.T) {
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)

	batchesDir := layout.BatchesDir
	if err := os.MkdirAll(batchesDir, 0755); err != nil {
		t.Fatal(err)
	}

	alreadyUnavailableID := "dddd-260622105532-issue-1"
	alreadyUnavailablePath := filepath.Join(batchesDir, alreadyUnavailableID)
	if err := os.MkdirAll(alreadyUnavailablePath, 0755); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:     alreadyUnavailableID,
				Path:   alreadyUnavailablePath,
				Status: batchindex.StatusUnavailable,
			},
		},
	}

	dirty := idx.MarkUnavailable()
	if dirty {
		t.Fatal("expected dirty=false when no entries need flipping")
	}
}

func timePtr(t time.Time) *time.Time {
	return &t
}
