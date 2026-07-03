package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// TestPortalRunsView_ResolveBatchEntryForRunID_BothMiss pins the
// slice-1 prefactor contract: when neither the index's fast path nor
// any on-disk per-row manifest resolves the runID, the helper returns
// a typed *portalBatchEntryNotFoundError the caller can errors.As-match
// to map to 404. The helper is added with no production caller in this
// slice; slice 2 wires it into the archive handler (the slice 2 free
// function `resolveBatchEntryForRunID` in portal.go is its current
// production caller, and stays untouched in this slice).
func TestPortalRunsView_ResolveBatchEntryForRunID_BothMiss(t *testing.T) {
	v := &portalRunsView{}
	idx := &batchindex.Index{Version: batchindex.IndexVersion}

	cases := []struct {
		name  string
		idx   *batchindex.Index
		runID string
	}{
		{name: "empty index and run id", idx: idx, runID: ""},
		{name: "nil index", idx: nil, runID: "abcd-260618113825-issue-42"},
		{name: "empty index with run id", idx: &batchindex.Index{Version: batchindex.IndexVersion}, runID: "abcd-260618113825-issue-42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, err := v.resolveBatchEntryForRunID(tc.idx, tc.runID)
			if entry != nil {
				t.Fatalf("expected nil entry on miss, got %#v", entry)
			}
			if err == nil {
				t.Fatalf("expected typed not-found error, got nil")
			}
			var notFound *portalBatchEntryNotFoundError
			if !errors.As(err, &notFound) {
				t.Fatalf("expected *portalBatchEntryNotFoundError, got %T (%v)", err, err)
			}
			if tc.runID != "" && !strings.Contains(notFound.Error(), tc.runID) {
				t.Errorf("expected not-found message to mention runID %q, got %q", tc.runID, notFound.Error())
			}
		})
	}
}

// TestPortalRunsView_ResolveBatchEntryForRunID_ExactMatch pins the
// slice-1 fast-path contract: when the run id matches an index entry's
// ID directly, the helper returns that entry without touching the
// filesystem. The returned entry must have Path populated so callers
// can use it for archive moves and log path resolution.
func TestPortalRunsView_ResolveBatchEntryForRunID_ExactMatch(t *testing.T) {
	runID := "abcd-260618113825-issue-42"
	batchDir := filepath.Join(t.TempDir(), runID)
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
		},
	}

	entry, err := (&portalRunsView{}).resolveBatchEntryForRunID(idx, runID)
	if err != nil {
		t.Fatalf("expected no error on exact match, got %v", err)
	}
	if entry == nil {
		t.Fatal("expected entry on exact match, got nil")
	}
	if entry.ID != runID {
		t.Errorf("expected entry ID %q, got %q", runID, entry.ID)
	}
	if entry.Path != batchDir {
		t.Errorf("expected entry Path %q, got %q", batchDir, entry.Path)
	}
}

// TestPortalRunsView_ResolveBatchEntryForRunID_FallbackByRunManifest
// pins the slice-1 fallback contract: when the fast path misses, the
// helper walks each entry's runs/<runID>/run.json, parses the per-row
// manifest's BatchID, and resolves that ID in the index. The returned
// entry must have Entry.ID == parsed BatchID (proving the parse+re-
// resolve happened), and Entry.Path must be the containing batch dir
// so downstream archive moves point at the right location.
func TestPortalRunsView_ResolveBatchEntryForRunID_FallbackByRunManifest(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchEntryID := "abcd-260618113825-42+1"
	perRowID := "abcd-260618113825-42"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
	if err := os.MkdirAll(filepath.Join(batchDir, "runs", perRowID), 0755); err != nil {
		t.Fatal(err)
	}
	perRowManifest := batchindex.RunManifest{
		RunID:     perRowID,
		BatchID:   batchEntryID,
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusSuccess,
	}
	if err := batchindex.WriteManifest(filepath.Join(batchDir, "runs", perRowID), perRowManifest); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
		},
	}

	entry, err := (&portalRunsView{}).resolveBatchEntryForRunID(idx, perRowID)
	if err != nil {
		t.Fatalf("expected no error on fallback resolve, got %v", err)
	}
	if entry == nil {
		t.Fatal("expected entry on fallback resolve, got nil")
	}
	if entry.ID != batchEntryID {
		t.Errorf("expected entry ID %q (parsed BatchID), got %q", batchEntryID, entry.ID)
	}
	if entry.Path != batchDir {
		t.Errorf("expected entry Path %q, got %q", batchDir, entry.Path)
	}
}

// TestPortalRunsView_ResolveBatchEntryForRunID_FallbackBatchIdNotInIndex
// pins the load-bearing contract: the fallback MUST parse the BatchID
// and re-resolve it in the index, not just trust whatever entry's
// directory the run.json happened to live under. A manifest whose
// BatchID is absent from the index surfaces the typed not-found error,
// so a future refactor that returns the manifest's containing entry
// unconditionally will fail this test.
func TestPortalRunsView_ResolveBatchEntryForRunID_FallbackBatchIdNotInIndex(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchEntryID := "abcd-260618113825-42+1"
	perRowID := "abcd-260618113825-42"
	staleBatchID := "abcd-260618113825-42+1-stale-evicted"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
	if err := os.MkdirAll(filepath.Join(batchDir, "runs", perRowID), 0755); err != nil {
		t.Fatal(err)
	}
	// Per-row manifest claims a batchId that is NOT in the index (the
	// index entry was evicted or never existed). The helper must NOT
	// return the containing entry; it must surface the typed not-found.
	perRowManifest := batchindex.RunManifest{
		RunID:     perRowID,
		BatchID:   staleBatchID,
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusSuccess,
	}
	if err := batchindex.WriteManifest(filepath.Join(batchDir, "runs", perRowID), perRowManifest); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
		},
	}

	entry, err := (&portalRunsView{}).resolveBatchEntryForRunID(idx, perRowID)
	if entry != nil {
		t.Fatalf("expected nil entry when parsed BatchID is not in index, got %#v", entry)
	}
	if err == nil {
		t.Fatalf("expected typed not-found error when parsed BatchID is not in index, got nil")
	}
	var notFound *portalBatchEntryNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected *portalBatchEntryNotFoundError, got %T (%v)", err, err)
	}
}
