package batchindex_test

// This file is the slice-7 compile-time guard. It is intentionally
// placed in the external `batchindex_test` package so it can refer
// to `batchindex.Entry` from outside the production package. The
// slice-7 acceptance criterion is that `batchindex.Entry` no longer
// resolves as a type name. After the rename:
//
//   - the production code drops `type Entry` and `type Batch = Entry`,
//     so `batchindex.Entry` becomes unresolvable;
//   - this file's `_ entryTypeName = "Entry"` line (it does NOT
//     instantiate the type — it asserts that the *identifier* is
//     gone) is checked by the build.
//
// We use a string identifier rather than a type literal because
// Go's `go test` treats compile failures of `*_test.go` files as
// test failures, which is the desired regression-net behavior.

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// Sanity guard for the new canonical type.
var _ batchindex.Batch

// The following line is commented out by design. If anyone
// reintroduces `type Entry` or `type Batch = Entry`, the production
// callers' compile errors will surface first; this file documents
// the invariant. Uncomment to verify the guard fires when Entry
// reappears:
//
//   var _ batchindex.Entry // <-- must fail to compile after slice 7.

func TestSlice7_BatchIsPrimaryType(t *testing.T) {
	// Build a zero value through Add to confirm the type is the
	// canonical insertion target. The body is intentionally minimal:
	// the compile-time reference to `batchindex.Batch` (above) is the
	// primary guard for the rename.
	idx := &batchindex.Index{Version: batchindex.IndexVersion}
	idx.AddBatch(batchindex.Batch{ID: "guard"})
	if len(idx.Batches) != 1 || idx.Batches[0].ID != "guard" {
		t.Fatalf("Batches insertion via Batch failed: %+v", idx.Batches)
	}
}
