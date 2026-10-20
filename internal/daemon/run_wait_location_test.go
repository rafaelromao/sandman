package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/paths"
)

// Waiting recovery never guesses a canonical directory for an ambiguous
// physical alias: unfinished intent must not be read from a foreign location.
func TestResolveBatchDir_RefusesAmbiguousAlias(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: "ambiguous-a", Path: filepath.Join(layout.SandmanDir, "a", "same"), Status: batchindex.StatusActive},
		{ID: "ambiguous-b", Path: filepath.Join(layout.SandmanDir, "b", "same"), Status: batchindex.StatusActive},
	}}).Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	if dir, err := ResolveBatchDir(layout, "same"); err == nil {
		t.Fatalf("ambiguous alias resolved to %q", dir)
	}
}

// A canonical unindexed directory whose name is another Batch's public ID has
// no unambiguous identity; transferring ownership there would stamp the
// other Batch's ID on the record and strand the continuation.
func TestTransferRunWait_RefusesCanonicalDirShadowingPublicID(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: "shadowed", Path: filepath.Join(layout.SandmanDir, "relocated", "frozen"), Status: batchindex.StatusActive},
	}}).Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "capacity"}
	if err := RenewRunWait(layout.BatchDir("old"), record, now); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(layout.BatchDir("old"), "runs", "row", "wait.json")
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	destination := layout.BatchDir("shadowed")
	if moved, err := TransferRunWait(layout, layout.BatchDir("old"), destination, "row", now.Add(time.Minute)); err == nil {
		t.Fatalf("transfer attributed canonical dir to another batch: %+v", moved)
	}
	if _, err := os.Stat(filepath.Join(destination, "runs", "row", "wait.json")); !os.IsNotExist(err) {
		t.Fatalf("transfer wrote misattributed wait: %v", err)
	}
	if after, err := os.ReadFile(sourcePath); err != nil || string(after) != string(before) {
		t.Fatalf("refused transfer changed the source wait: %v", err)
	}
	if source, err := ReadRunWaitAt(layout.BatchDir("old"), "old", "row"); err != nil || !source.RecoverableAt(now) {
		t.Fatalf("refused transfer stranded the source wait: %+v %v", source, err)
	}
}
