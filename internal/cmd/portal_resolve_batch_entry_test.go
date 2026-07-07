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

// Both miss returns the typed not-found error.
func TestPortalRunsView_ResolveBatchFromRowID_BothMiss(t *testing.T) {
	idx := &batchindex.Index{Version: batchindex.IndexVersion}

	cases := []struct {
		name  string
		idx   *batchindex.Index
		runID string
	}{
		{name: "empty index and run id", idx: idx, runID: ""},
		{name: "nil index", idx: nil, runID: "abcd-260618113825-42"},
		{name: "empty index with run id", idx: &batchindex.Index{Version: batchindex.IndexVersion}, runID: "abcd-260618113825-42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, err := (&portalRunsView{}).resolveBatchFromRowID(tc.idx, tc.runID)
			if entry != nil {
				t.Fatalf("expected nil entry on miss, got %#v", entry)
			}
			if err == nil {
				t.Fatalf("expected typed not-found error, got nil")
			}
			var notFound *portalBatchNotFoundError
			if !errors.As(err, &notFound) {
				t.Fatalf("expected *portalBatchNotFoundError, got %T (%v)", err, err)
			}
			if tc.runID != "" && !strings.Contains(notFound.Error(), tc.runID) {
				t.Errorf("expected not-found message to mention runID %q, got %q", tc.runID, notFound.Error())
			}
		})
	}
}

// Exact match returns the index entry with Path populated.
func TestPortalRunsView_ResolveBatchFromRowID_ExactMatch(t *testing.T) {
	runID := "abcd-260618113825-42"
	batchDir := filepath.Join(t.TempDir(), runID)
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
		},
	}

	entry, err := (&portalRunsView{}).resolveBatchFromRowID(idx, runID)
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

// Fallback walks each entry's runs/<runID>/run.json, parses BatchID, and re-resolves it.
func TestPortalRunsView_ResolveBatchFromRowID_FallbackByRunManifest(t *testing.T) {
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
		Batches: []batchindex.Batch{
			{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
		},
	}

	entry, err := (&portalRunsView{}).resolveBatchFromRowID(idx, perRowID)
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

// Parsed BatchID not in the index surfaces the typed not-found error.
func TestPortalRunsView_ResolveBatchFromRowID_FallbackBatchIdNotInIndex(t *testing.T) {
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
		Batches: []batchindex.Batch{
			{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
		},
	}

	entry, err := (&portalRunsView{}).resolveBatchFromRowID(idx, perRowID)
	if entry != nil {
		t.Fatalf("expected nil entry when parsed BatchID is not in index, got %#v", entry)
	}
	if err == nil {
		t.Fatalf("expected typed not-found error when parsed BatchID is not in index, got nil")
	}
	var notFound *portalBatchNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected *portalBatchNotFoundError, got %T (%v)", err, err)
	}
}
