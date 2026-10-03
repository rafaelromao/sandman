package batchindex

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/rafaelromao/sandman/internal/paths"
)

// Layout anchors resolution at the repository containing this loaded index.
// In-memory indices with absolute paths need no root; relative test fixtures
// can instead pass an explicit Layout to the location APIs.
func (idx *Index) Layout() paths.Layout {
	root := "."
	if idx != nil && idx.indexPath != "" {
		root = filepath.Dir(filepath.Dir(idx.indexPath))
	}
	return paths.NewLayout(nil, root)
}

func (b Batch) Location(layout paths.Layout) paths.BatchLocation {
	return layout.LocateBatch(b.ID, b.Path)
}

// RunLocation is the reader location. Archive writers deliberately use
// Location(layout).Run(runID) instead, so an archived copy is never moved.
func (b Batch) RunLocation(layout paths.Layout, runID string) paths.RunLocation {
	for _, rec := range b.Runs {
		if rec.RunID == runID && rec.ArchivePath != "" {
			return paths.RunLocation{ID: runID, Dir: layout.ResolvePath(rec.ArchivePath)}
		}
	}
	return b.Location(layout).Run(runID)
}

// ResolveBatchIdentity accepts public IDs and unambiguous physical basenames.
// Exact public identity takes precedence over aliases of other batches.
// This does not interpret row IDs and is suitable for event batch_id evidence.
func (idx *Index) ResolveBatchIdentity(id string) *Batch {
	if idx == nil || id == "" {
		return nil
	}
	if b := idx.ResolveBatch(id); b != nil {
		return b
	}
	var found *Batch
	for i := range idx.Batches {
		b := &idx.Batches[i]
		if b.Path != "" && filepath.Base(b.Path) == id {
			if found != nil {
				return nil
			}
			found = b
		}
	}
	return found
}

// MatchesBatchLocation interprets event batch identity without row aliases.
// Unknown identities may refer to a canonical unindexed legacy directory;
// ambiguous indexed aliases never use that fallback.
func (idx *Index) MatchesBatchLocation(layout paths.Layout, id, dir string) bool {
	if owner := idx.ResolveBatchIdentity(id); owner != nil {
		return filepath.Clean(owner.Location(layout).Dir) == filepath.Clean(dir)
	}
	for _, b := range idx.Batches {
		if b.Path != "" && filepath.Base(b.Path) == id {
			return false
		}
	}
	return filepath.Clean(layout.BatchDir(id)) == filepath.Clean(dir)
}

// ResolveRunBatch resolves row-action IDs through the index and physical
// evidence, never by parsing a RunID or trusting a manifest's BatchID field.
func (idx *Index) ResolveRunBatch(layout paths.Layout, id string) *Batch {
	if idx == nil || id == "" {
		return nil
	}
	if b := idx.ResolveBatchIdentity(id); b != nil {
		return b
	}
	for i := range idx.Batches {
		for _, rec := range idx.Batches[i].Runs {
			if rec.RunID == id {
				return &idx.Batches[i]
			}
		}
	}
	for i := range idx.Batches {
		b := &idx.Batches[i]
		if _, err := os.Stat(b.RunLocation(layout, id).ManifestPath()); err == nil {
			return b
		}
	}
	return nil
}

// DiscoverBatchLocations combines persisted live locations with canonical
// unindexed directories. Archived index locations are excluded, including when
// their recorded path still lies under batches/. Results are deduplicated by
// normalized directory and sorted, independently of index order.
func DiscoverBatchLocations(layout paths.Layout) ([]paths.BatchLocation, error) {
	idx, err := Load(layout.BatchesIndexPath)
	if err != nil {
		return nil, err
	}
	return idx.BatchLocations(layout)
}

// BatchLocations discovers against this index snapshot, allowing recovery to
// use identical ownership evidence across its stale and orphan passes.
func (idx *Index) BatchLocations(layout paths.Layout) ([]paths.BatchLocation, error) {
	locations := make(map[string]paths.BatchLocation)
	indexed := make(map[string]bool)
	key := func(dir string) string {
		abs, err := filepath.Abs(dir)
		if err == nil {
			return abs
		}
		return filepath.Clean(dir)
	}
	for _, b := range idx.Batches {
		loc := b.Location(layout)
		k := key(loc.Dir)
		indexed[k] = true
		if b.Status != StatusArchived {
			locations[k] = loc
		}
	}
	entries, err := os.ReadDir(layout.BatchesDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read batches dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		loc := layout.LocateBatch(entry.Name(), "")
		k := key(loc.Dir)
		if !indexed[k] {
			locations[k] = loc
		}
	}
	result := make([]paths.BatchLocation, 0, len(locations))
	for _, loc := range locations {
		result = append(result, loc)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Dir < result[j].Dir })
	return result, nil
}
