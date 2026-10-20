package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
)

// Whole-batch archive resolves operator input through the location module:
// a physical alias selects the indexed Batch, and every artifact decision
// (archive target, index key, event membership) uses its public identity.
func TestArchiveBatch_PhysicalAliasUsesPublicIdentity(t *testing.T) {
	root := t.TempDir()
	batchDir := filepath.Join(root, ".sandman", "batches", "physical")
	writeRunDirForArchive(t, batchDir, "done", batchindex.RunManifest{BatchID: "public", Status: batchindex.RunManifestStatusSuccess})
	writeBatchIndexForArchive(t, root, []batchindex.Batch{{
		ID: "public", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive,
	}})
	deps := terminalArchiveDeps(t, root, "done")
	deps.RepoRoot = root

	cmd := NewArchiveCmd(deps)
	cmd.SetArgs([]string{"batch", "physical"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive by physical alias: %v", err)
	}

	archived := filepath.Join(root, ".sandman", "archive", "public")
	if _, err := os.Stat(filepath.Join(archived, "runs", "done", "run.json")); err != nil {
		t.Fatalf("batch not archived at public identity: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".sandman", "archive", "physical")); !os.IsNotExist(err) {
		t.Fatalf("alias-named archive created: %v", err)
	}
	idx := loadBatchIndexForArchive(t, root)
	entry := idx.ResolveBatch("public")
	if entry == nil || entry.Status != batchindex.StatusArchived || paths.NewLayout(nil, root).ResolvePath(entry.Path) != archived {
		t.Fatalf("public index entry not archived: %+v", entry)
	}
	if len(idx.Batches) != 1 {
		t.Fatalf("archive added index entries: %+v", idx.Batches)
	}
}

// Orphan cleanup prunes the index by the removed frozen location, never by
// matching a directory basename against public Batch IDs.
func TestCleanupOrphanedBatches_PrunesAliasedEntryByLocation(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, root)
	aliased := filepath.Join(layout.BatchesDir, "physical")
	if err := os.MkdirAll(aliased, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aliased, "batch.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(layout.SandmanDir, "moved", "physical-home")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBatchIndex(t, root, []batchindex.Batch{
		{ID: "public", Path: aliased, Status: batchindex.StatusActive},
		{ID: "physical", Path: elsewhere, Status: batchindex.StatusActive},
	})

	removed, err := cleanupOrphanedBatches(layout, &fakeEventLog{}, func(string) bool { return false }, &fakeCleanupRemover{})
	if err != nil || len(removed) != 1 || removed[0] != aliased {
		t.Fatalf("orphan cleanup removed=%v err=%v", removed, err)
	}
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry := idx.ResolveBatch("public"); entry != nil {
		t.Fatalf("index kept entry for removed location: %+v", entry)
	}
	if entry := idx.ResolveBatch("physical"); entry == nil {
		t.Fatalf("index pruned entry whose ID only matches the removed basename: %+v", idx.Batches)
	}
}
