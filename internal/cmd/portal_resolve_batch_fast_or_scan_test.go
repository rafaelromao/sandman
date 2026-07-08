package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// TestResolveBatchFromRunIDFastOrScan_FastPathMatchesBatchID pins the
// fast-path behavior of portal.go's resolveBatchFromRunIDFastOrScan:
// when the supplied runID equals an indexed Batch.ID the helper
// returns that batch without touching the filesystem.
func TestResolveBatchFromRunIDFastOrScan_FastPathMatchesBatchID(t *testing.T) {
	baseDir := t.TempDir()
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{ID: "abc123", Path: filepath.Join(baseDir, "abc123"), Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now()},
		},
	}

	batch := resolveBatchFromRunIDFastOrScan(idx, "abc123")
	if batch == nil {
		t.Fatal("expected a batch for public BatchId; got nil")
	}
	if batch.ID != "abc123" {
		t.Errorf("batch.ID = %q, want %q", batch.ID, "abc123")
	}
}

// TestResolveBatchFromRunIDFastOrScan_FallbackStatOnly pins the
// divergent fallback behavior of portal.go's helper: when the
// supplied runID is NOT an indexed Batch.ID, the helper stat-checks
// each batch's runs/<runID>/run.json path and returns the first
// batch whose file exists. Unlike the parse-then-resolve variant in
// portal_runs_view.go, this helper does NOT parse run.json and does
// NOT trust the per-row manifest's BatchID field — see the
// `TestResolveBatchFromRunIDFastOrScan_FallbackIgnoresManifestBatchID`
// subtest below. The two resolvers are deliberately distinct
// contracts preserved by the slice 7 renaming.
func TestResolveBatchFromRunIDFastOrScan_FallbackStatOnly(t *testing.T) {
	baseDir := t.TempDir()
	batchDir := filepath.Join(baseDir, "batches", "260618113825-abc-42+2")
	// Create the per-row folder AND its run.json so the stat-only
	// fallback finds the file. The body of run.json is intentionally
	// arbitrary; the stat-only path never parses it.
	runDir := filepath.Join(batchDir, "runs", "260618113825-abc-43")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), []byte(`{"runID":"260618113825-abc-43","batchId":"260618113825-abc-42+2","kind":"issue","status":"success"}`), 0644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{ID: "260618113825-abc-42+2", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now()},
		},
	}

	batch := resolveBatchFromRunIDFastOrScan(idx, "260618113825-abc-43")
	if batch == nil {
		t.Fatal("expected the batch that owns the per-row run folder; got nil")
	}
	if batch.ID != "260618113825-abc-42+2" {
		t.Errorf("batch.ID = %q, want %q", batch.ID, "260618113825-abc-42+2")
	}
}

// TestResolveBatchFromRunIDFastOrScan_FallbackIgnoresManifestBatchID
// pins the stat-only fallback: a stale per-row manifest whose
// BatchID points somewhere else in the index MUST NOT steer the
// stat-only fallback toward the wrong batch. The parse-then-resolve
// variant in portal_runs_view.go has the opposite contract and is
// covered by its own tests.
func TestResolveBatchFromRunIDFastOrScan_FallbackIgnoresManifestBatchID(t *testing.T) {
	baseDir := t.TempDir()
	hostBatchDir := filepath.Join(baseDir, "batches", "host-42+1")
	hostRunDir := filepath.Join(hostBatchDir, "runs", "host-43")
	if err := os.MkdirAll(hostRunDir, 0755); err != nil {
		t.Fatalf("mkdir host run dir: %v", err)
	}
	// Write a per-row manifest whose BatchID points at a *different* batch.
	// The stat-only fallback must ignore this and return the host batch
	// (the one whose file stat-exists), not the manifest-named batch.
	staleManifest := batchindex.RunManifest{
		RunID:   "host-43",
		BatchID: "wrong-42",
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusSuccess,
	}
	if err := os.MkdirAll(hostRunDir, 0755); err != nil {
		t.Fatalf("re-mkdir host run dir: %v", err)
	}
	if err := batchindex.WriteManifest(hostRunDir, staleManifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{ID: "wrong-42", Path: filepath.Join(baseDir, "batches", "wrong-42"), Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now()},
			{ID: "host-42+1", Path: hostBatchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now()},
		},
	}

	batch := resolveBatchFromRunIDFastOrScan(idx, "host-43")
	if batch == nil {
		t.Fatal("expected the batch owning the run folder; got nil")
	}
	if batch.ID != "host-42+1" {
		t.Errorf("stat-only fallback chose batch.ID = %q, want %q (must ignore manifest.BatchID)", batch.ID, "host-42+1")
	}
}

// TestResolveBatchFromRunIDFastOrScan_MissReturnsNil pins that a
// fully unknown runID returns (nil, no-error) — the package-level
// helper signals miss without an error value, unlike the method
// resolver in portal_runs_view.go which returns typed
// *portalBatchNotFoundError. This guards the API split between the
// two helpers.
func TestResolveBatchFromRunIDFastOrScan_MissReturnsNil(t *testing.T) {
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{ID: "abc123", Path: "/nonexistent", Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now()},
		},
	}

	if got := resolveBatchFromRunIDFastOrScan(idx, "never-seen"); got != nil {
		t.Errorf("expected nil for unknown runID; got %+v", got)
	}
	if got := resolveBatchFromRunIDFastOrScan(idx, ""); got != nil {
		t.Errorf("expected nil for empty runID; got %+v", got)
	}
	if got := resolveBatchFromRunIDFastOrScan(nil, "abc123"); got != nil {
		t.Errorf("expected nil for nil idx; got %+v", got)
	}
}
