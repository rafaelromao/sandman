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
			batch, err := (&portalRunsView{}).resolveBatchFromRowID(tc.idx, tc.runID)
			if batch != nil {
				t.Fatalf("expected nil batch on miss, got %#v", batch)
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

// Exact match returns the index batch with Path populated.
func TestPortalRunsView_ResolveBatchFromRowID_ExactMatch(t *testing.T) {
	runID := "abcd-260618113825-42"
	batchDir := filepath.Join(t.TempDir(), runID)
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
		},
	}

	batch, err := (&portalRunsView{}).resolveBatchFromRowID(idx, runID)
	if err != nil {
		t.Fatalf("expected no error on exact match, got %v", err)
	}
	if batch == nil {
		t.Fatal("expected batch on exact match, got nil")
	}
	if batch.ID != runID {
		t.Errorf("expected batch ID %q, got %q", runID, batch.ID)
	}
	if batch.Path != batchDir {
		t.Errorf("expected batch Path %q, got %q", batchDir, batch.Path)
	}
}

// Fallback walks each batch's runs/<runID>/run.json, parses BatchID, and re-resolves it.
func TestPortalRunsView_ResolveBatchFromRowID_FallbackByRunManifest(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	publicBatchID := "abcd-260618113825-42+1"
	perRowID := "abcd-260618113825-42"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", publicBatchID)
	if err := os.MkdirAll(filepath.Join(batchDir, "runs", perRowID), 0755); err != nil {
		t.Fatal(err)
	}
	perRowManifest := batchindex.RunManifest{
		RunID:     perRowID,
		BatchID:   publicBatchID,
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
			{ID: publicBatchID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
		},
	}

	batch, err := (&portalRunsView{}).resolveBatchFromRowID(idx, perRowID)
	if err != nil {
		t.Fatalf("expected no error on fallback resolve, got %v", err)
	}
	if batch == nil {
		t.Fatal("expected batch on fallback resolve, got nil")
	}
	if batch.ID != publicBatchID {
		t.Errorf("expected batch ID %q (parsed BatchID), got %q", publicBatchID, batch.ID)
	}
	if batch.Path != batchDir {
		t.Errorf("expected batch Path %q, got %q", batchDir, batch.Path)
	}
}

// Parsed BatchID not in the index surfaces the typed not-found error.
func TestPortalRunsView_ResolveBatchFromRowID_FallbackBatchIdNotInIndex(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	publicBatchID := "abcd-260618113825-42+1"
	perRowID := "abcd-260618113825-42"
	staleBatchID := "abcd-260618113825-42+1-stale-evicted"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", publicBatchID)
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
			{ID: publicBatchID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
		},
	}

	batch, err := (&portalRunsView{}).resolveBatchFromRowID(idx, perRowID)
	if batch != nil {
		t.Fatalf("expected nil batch when parsed BatchID is not in index, got %#v", batch)
	}
	if err == nil {
		t.Fatalf("expected typed not-found error when parsed BatchID is not in index, got nil")
	}
	var notFound *portalBatchNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected *portalBatchNotFoundError, got %T (%v)", err, err)
	}
}
