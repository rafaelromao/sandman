package batchindex_test

// This file is the slice-7 compile-time guard. It is placed in the
// external `batchindex_test` package so it can refer to
// `batchindex.Entry` from outside the production package.
//
// The slice-7 acceptance criterion is that `batchindex.Entry` no
// longer resolves as a type name. The regression net is the
// `var _ batchindex.Batch` line below — any caller that still tries
// to use `batchindex.Entry` will fail to build the package that
// imports it (e.g. `internal/cmd/portal_runs_view.go`'s
// `*batchindex.Entry` return type). That build break IS the
// regression net; this test file just pins the positive invariant
// (Batch is the canonical type).
//
// To verify the negative invariant manually, uncomment the
// `var _ batchindex.Entry` line below and run `go test
// ./internal/batchindex/`. The build must fail with
// "undefined: batchindex.Entry". Re-comment after the manual
// verification — the test file must compile in normal operation.

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// Live guard for the new canonical type — this resolves today and
// must keep resolving. If `type Batch` is ever removed, this line
// breaks the build, which `go test` reports as a test failure.
var _ batchindex.Batch

// Negative-invocation probe — intentionally commented out so the
// test file compiles in the steady state. Uncomment to verify the
// guard fires when `type Entry` is reintroduced.
//
// var _ batchindex.Entry // <-- must fail to compile after slice 7.

func TestBatchIndex_BatchIsPrimaryType(t *testing.T) {
	// Build a zero value through Add to confirm the type is the
	// canonical insertion target.
	idx := &batchindex.Index{Version: batchindex.IndexVersion}
	idx.AddBatch(batchindex.Batch{ID: "guard"})
	if len(idx.Batches) != 1 || idx.Batches[0].ID != "guard" {
		t.Fatalf("Batches insertion via Batch failed: %+v", idx.Batches)
	}
}
