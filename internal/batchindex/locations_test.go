package batchindex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/paths"
)

func TestLocations_PersistedPathsAndArchiveAuthority(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	t.Chdir(t.TempDir())
	for _, kind := range []Kind{KindIssue, KindPromptOnly, KindReview} {
		t.Run(string(kind), func(t *testing.T) {
			b := Batch{ID: "public-" + string(kind), Path: filepath.Join(".sandman", "batches", "physical-"+string(kind)), Kind: kind, Status: StatusActive}
			live := b.Location(layout).Run("row")
			if err := os.MkdirAll(live.Dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := WriteManifest(live.Dir, RunManifest{RunID: "row", BatchID: "stale", Kind: kind}); err != nil {
				t.Fatal(err)
			}
			idx := &Index{Version: IndexVersion, Batches: []Batch{b}}
			for _, id := range []string{b.ID, filepath.Base(b.Path), "row"} {
				if owner := idx.ResolveRunBatch(layout, id); owner == nil || owner.ID != b.ID {
					t.Fatalf("owner for %q = %+v", id, owner)
				}
			}
			archive := layout.ArchiveBatch(b.ID).Run("row")
			if err := os.MkdirAll(archive.Dir, 0755); err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, archive.Dir)
			b.Runs = []RunRecord{{RunID: "row", Status: RunRecordStatusArchived, ArchivePath: rel}}
			if got := b.RunLocation(layout, "row").Dir; got != archive.Dir {
				t.Fatalf("reader = %q, want %q", got, archive.Dir)
			}
			if b.Location(layout).Run("row").Dir != live.Dir {
				t.Fatal("writer source changed")
			}
			b.Runs[0].ArchivePath = archive.Dir
			idx.Batches[0] = b
			if err := os.RemoveAll(live.Dir); err != nil {
				t.Fatal(err)
			}
			if err := idx.EnsureStatusWithLayout(root); err != nil {
				t.Fatal(err)
			}
			if idx.Batches[0].Status != StatusActive || idx.Batches[0].Runs[0].Status != RunRecordStatusArchived {
				t.Fatalf("valid paths marked unavailable: %+v", idx.Batches[0])
			}
			if idx.Batches[0].Path != b.Path || idx.Batches[0].Runs[0].ArchivePath != archive.Dir {
				t.Fatal("resolution rewrote persistence")
			}
		})
	}
}
