package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestValidateOwnedPath_RejectsUnsafeCandidates(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "owned")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(inside, "escape")); err != nil {
		t.Fatal(err)
	}

	for name, candidate := range map[string]string{
		"empty":       "",
		"root":        string(filepath.Separator),
		"relative":    filepath.Join("owned", "batch"),
		"traversal":   filepath.Join(root, "owned", "..", "..", "outside"),
		"symlink":     filepath.Join(inside, "escape", "batch"),
		"trustedroot": inside,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateOwnedPath(candidate, inside); err == nil {
				t.Fatalf("validateOwnedPath(%q) accepted unsafe path", candidate)
			}
		})
	}

	if err := validateOwnedPath(filepath.Join(inside, "missing", "batch"), inside); err != nil {
		t.Fatalf("absent owned path rejected: %v", err)
	}
}

func TestExecuteClean_RetainsFailedAndRemovesSuccessfulEntries(t *testing.T) {
	repo := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, repo)
	good := filepath.Join(layout.BatchesDir, "good")
	bad := filepath.Join(layout.BatchesDir, "bad")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBatchIndex(t, repo, []batchindex.Batch{
		{ID: "good", Path: good, Status: batchindex.StatusArchived},
		{ID: "bad", Path: bad, Status: batchindex.StatusArchived},
	})

	oldRemove := removeCleanPath
	removeCleanPath = func(path string) error {
		if path == bad {
			return errors.New("permission denied")
		}
		return os.RemoveAll(path)
	}
	t.Cleanup(func() { removeCleanPath = oldRemove })

	actions := collectCleanActions(&batchindex.Index{Batches: []batchindex.Batch{
		{ID: "good", Path: good, Status: batchindex.StatusArchived},
		{ID: "bad", Path: bad, Status: batchindex.StatusArchived},
	}}, batchindex.StatusArchived)
	// These synthetic actions deliberately bypass manifest planning; the executor
	// still validates and retains the same index entries on failure.
	for i := range actions {
		actions[i].Err = nil
	}
	outcomes, err := executeClean(actions, &fakeGitRunner{}, layout)
	if err == nil || len(outcomes) != 2 {
		t.Fatalf("expected one action error and two outcomes, outcomes=%v err=%v", outcomes, err)
	}
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Resolve("good") != nil || idx.Resolve("bad") == nil {
		t.Fatalf("expected good removed and bad retained: %+v", idx.Batches)
	}
}
