package daemon

import (
	"github.com/rafaelromao/sandman/internal/atomicfs"
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
	if err := RenewRunWait(layout, layout.LocateBatch("old", ""), record, now); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(layout.BatchDir("old"), "runs", "row", "wait.json")
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, destination := range []paths.BatchLocation{
		{ID: "shadowed", Dir: layout.BatchDir("shadowed")},
		// A physical alias is not the frozen Batch's public identity.
		{ID: "frozen", Dir: filepath.Join(layout.SandmanDir, "relocated", "frozen")},
	} {
		if moved, err := TransferRunWait(layout, layout.LocateBatch("old", ""), destination, "row", now.Add(time.Minute)); err == nil {
			t.Fatalf("transfer attributed %+v to the wrong identity: %+v", destination, moved)
		}
		if _, err := os.Stat(filepath.Join(destination.Dir, "runs", "row", "wait.json")); !os.IsNotExist(err) {
			t.Fatalf("transfer wrote misattributed wait: %v", err)
		}
	}
	if after, err := os.ReadFile(sourcePath); err != nil || string(after) != string(before) {
		t.Fatalf("refused transfer changed the source wait: %v", err)
	}
	if source, err := ReadRunWaitAt(layout, layout.LocateBatch("old", ""), "row"); err != nil || !source.RecoverableAt(now) {
		t.Fatalf("refused transfer stranded the source wait: %+v %v", source, err)
	}
}

// Unindexed canonical legacy destinations keep their directory name as their
// identity when no indexed Batch claims that name.
func TestTransferRunWait_AcceptsUnindexedCanonicalLegacyDestination(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: "public", Path: filepath.Join(layout.SandmanDir, "relocated", "frozen"), Status: batchindex.StatusActive},
	}}).Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "capacity"}
	if err := RenewRunWait(layout, layout.LocateBatch("old", ""), record, now); err != nil {
		t.Fatal(err)
	}
	moved, err := TransferRunWait(layout, layout.LocateBatch("old", ""), layout.LocateBatch("legacy", ""), "row", now.Add(time.Minute))
	if err != nil || moved.BatchID != "legacy" {
		t.Fatalf("legacy transfer = %+v, %v", moved, err)
	}
	if _, err := ReadRunWaitAt(layout, layout.LocateBatch("legacy", ""), "row"); err != nil {
		t.Fatalf("legacy destination wait unreadable: %v", err)
	}
}

// The initial-admission sidecar is repository state: a relocated frozen Batch
// directory must not move it beside the Batch's grandparent.
func TestInitialWaitSidecarLivesInLayoutStateDir(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	batch := paths.BatchLocation{ID: "public", Dir: filepath.Join(layout.SandmanDir, "relocated", "deep", "frozen")}
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batch.ID, Path: batch.Dir, Status: batchindex.StatusActive},
	}}).Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: batch.ID, Issue: 42,
		BaseBranch: "main", InitialAdmission: true, OperationID: "admission"}
	if err := RenewRunWait(layout, batch, record, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(layout.StateDir, "waiting", "row.json")); err != nil {
		t.Fatalf("initial sidecar missing from layout state dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layout.SandmanDir, "relocated", "state")); !os.IsNotExist(err) {
		t.Fatalf("initial sidecar derived from the batch dir's grandparent: %v", err)
	}
	if got, err := ReadRunWaitAt(layout, batch, "row"); err != nil || !got.InitialAdmission || got.BatchID != batch.ID {
		t.Fatalf("initial sidecar read = %+v, %v", got, err)
	}
}

// Writers cannot stamp a record with an identity other than its location's.
func TestRenewRunWait_RejectsRecordForAnotherBatch(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "frozen", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "capacity"}
	batch := paths.BatchLocation{ID: "public", Dir: layout.BatchDir("frozen")}
	if err := RenewRunWait(layout, batch, record, time.Now().UTC()); err == nil {
		t.Fatal("renewed a record under another Batch's identity")
	}
	if _, err := os.Stat(filepath.Join(batch.Dir, "runs", "row", "wait.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected renewal wrote a wait record: %v", err)
	}
}

// The index records frozen paths under the git toplevel while callers may
// name the same repository through a symlink (or /tmp vs /private/tmp). The
// owner is the same directory, so identity must not depend on the spelling,
// and the returned location keeps the index's frozen spelling.
func TestBatchLocationForDir_MatchesThroughSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realLayout, linkLayout := paths.NewLayout(nil, real), paths.NewLayout(nil, link)
	frozen := filepath.Join(realLayout.BatchesDir, "frozen")
	legacy := realLayout.BatchDir("legacy")
	for _, dir := range []string{frozen, legacy} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: "public", Path: frozen, Status: batchindex.StatusActive},
	}}).Save(realLayout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	got, err := BatchLocationForDir(linkLayout, filepath.Join(linkLayout.BatchesDir, "frozen"))
	if err != nil || got.ID != "public" || got.Dir != frozen {
		t.Fatalf("symlinked indexed location = %+v, %v; want public at %s", got, err, frozen)
	}
	got, err = BatchLocationForDir(linkLayout, filepath.Join(link, ".sandman", "batches", "legacy"))
	if err != nil || got.ID != "legacy" {
		t.Fatalf("symlinked legacy location = %+v, %v", got, err)
	}
}

// Two indexed Batches resolving to one physical directory have no single
// owner; recovery must fail closed rather than pick one.
func TestBatchLocationForDir_RefusesTwoOwnersOfOneDirectory(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realLayout := paths.NewLayout(nil, real)
	frozen := filepath.Join(realLayout.BatchesDir, "frozen")
	if err := os.MkdirAll(frozen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: "first", Path: frozen, Status: batchindex.StatusActive},
		{ID: "second", Path: filepath.Join(link, ".sandman", "batches", "frozen"), Status: batchindex.StatusActive},
	}}).Save(realLayout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	if got, err := BatchLocationForDir(realLayout, frozen); err == nil {
		t.Fatalf("two owners resolved to %+v", got)
	}
}

// Pins the legacy batchless read: a Run recorded without a batch_id resolves
// to BatchesDir with an empty identity. Its started record stays readable so
// in-flight legacy waits keep their grace, while an initial admission must
// still name the directory it lives in.
func TestReadRunWaitAt_LegacyBatchlessLocation(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	batch, err := ResolveBatchLocation(layout, "")
	if err != nil || batch.ID != "" || batch.Dir != layout.BatchesDir {
		t.Fatalf("batchless location = %+v, %v", batch, err)
	}
	now := time.Now().UTC()
	write := func(path string, record RunWait) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := atomicfs.WriteAtomicJSON(path, record, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	started := RunWait{Protocol: "run-wait/v1", RunID: "started", BatchID: "legacy-batch", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "capacity", LeaseExpiresAt: now.Add(time.Minute)}
	write(filepath.Join(layout.BatchesDir, "runs", "started", "wait.json"), started)
	if got, err := ReadRunWaitAt(layout, batch, "started"); err != nil || got.BatchID != "legacy-batch" {
		t.Fatalf("legacy batchless started read = %+v, %v", got, err)
	}

	initial := RunWait{Protocol: "run-wait/v1", RunID: "initial", BatchID: "foreign", Issue: 42,
		BaseBranch: "main", InitialAdmission: true, OperationID: "admission", LeaseExpiresAt: now.Add(time.Minute)}
	write(filepath.Join(layout.StateDir, "waiting", "initial.json"), initial)
	if got, err := ReadRunWaitAt(layout, batch, "initial"); err == nil {
		t.Fatalf("batchless read accepted a foreign initial admission: %+v", got)
	}
	initial.BatchID = filepath.Base(layout.BatchesDir)
	write(filepath.Join(layout.StateDir, "waiting", "initial.json"), initial)
	if _, err := ReadRunWaitAt(layout, batch, "initial"); err != nil {
		t.Fatalf("batchless read rejected an initial admission named for its directory: %v", err)
	}
}
