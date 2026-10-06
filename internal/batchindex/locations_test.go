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

func TestLocations_DiscoveryAndIdentityAliases(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	batches := []Batch{
		{ID: "public", Path: layout.BatchDir("physical"), Status: StatusActive},
		{ID: "physical", Path: filepath.Join(layout.SandmanDir, "moved", "other"), Status: StatusActive},
		{ID: "archived", Path: layout.BatchDir("archived"), Status: StatusArchived},
		{ID: "ambiguous-a", Path: filepath.Join(layout.SandmanDir, "a", "same"), Status: StatusActive},
		{ID: "ambiguous-b", Path: filepath.Join(layout.SandmanDir, "b", "same"), Status: StatusActive},
	}
	for _, b := range batches {
		if err := os.MkdirAll(b.Path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(layout.BatchDir("legacy"), 0755); err != nil {
		t.Fatal(err)
	}
	idx := &Index{Version: IndexVersion, Batches: batches}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	if b := idx.ResolveBatchIdentity("physical"); b == nil || b.ID != "physical" {
		t.Fatalf("public ID precedence = %+v", b)
	}
	if b := idx.ResolveBatchIdentity("same"); b != nil {
		t.Fatalf("ambiguous alias resolved to %+v", b)
	}
	idx.Batches[3].Runs = []RunRecord{{RunID: "same", Status: RunRecordStatusActive}}
	if b := idx.ResolveRunBatch(layout, "same"); b != nil {
		t.Fatalf("row lookup bypassed ambiguous alias: %+v", b)
	}
	locations, err := DiscoverBatchLocations(layout)
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 5 {
		t.Fatalf("locations = %+v, want 4 indexed live + 1 legacy", locations)
	}
	seen := map[string]bool{}
	for i, loc := range locations {
		if seen[loc.Dir] || loc.ID == "archived" {
			t.Fatalf("duplicate/archive in discovery: %+v", locations)
		}
		if i > 0 && locations[i-1].Dir > loc.Dir {
			t.Fatal("discovery not sorted")
		}
		seen[loc.Dir] = true
	}
}
