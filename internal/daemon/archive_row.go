package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// NonTerminalRowError is returned by ArchiveRow when the targeted
// row's run.json Status is still active. It carries the offending
// run id so the HTTP handler can surface a 409 with the row's identity.
type NonTerminalRowError struct {
	RunID string
}

func (e *NonTerminalRowError) Error() string {
	return fmt.Sprintf("run %q is not in a terminal status", e.RunID)
}

// AlreadyArchivedError is returned by ArchiveRow when the per-row
// destination folder already exists. It carries the existing
// ArchivePath so the HTTP handler can echo it in the 409 response
// body and the CLI can surface it for operator inspection.
type AlreadyArchivedError struct {
	ArchivePath string
}

func (e *AlreadyArchivedError) Error() string {
	return fmt.Sprintf("run already archived at %q", e.ArchivePath)
}

// StripSockets walks dir and removes every file whose mode carries
// ModeSocket. It is exported so the per-row archive primitive in this
// package and any external callers (CLI subcommands) can share one
// implementation. The function returns the first non-ENOENT error
// encountered while removing; missing files are skipped silently.
func StripSockets(dir string) error {
	var lastErr error
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || info.Mode()&os.ModeSocket == 0 {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			lastErr = rmErr
		}
		return nil
	})
	return lastErr
}

// ArchiveRow moves runs/<runID>/ from the batch's live directory to
// .sandman/archive/<batchID>/runs/<runID>/, strips sockets from the
// moved folder, and returns the resulting RunRecord. The targeted
// row must have a terminal run.json Status (success / failure /
// aborted / blocked); an active row returns a *NonTerminalRowError.
//
// ArchiveRow is the single seam both the CLI subcommand and the HTTP
// archive endpoint dispatch through. It does not touch
// .sandman/worktrees/, does not edit events.jsonl, and does not dial
// the per-run socket (the row is terminal by contract). When the
// destination already exists, ArchiveRow returns *AlreadyArchivedError
// with the existing ArchivePath populated, so callers can surface it
// in error bodies without re-walking the filesystem.
func ArchiveRow(repoRoot string, entry *batchindex.Entry, runID string) (batchindex.RunRecord, error) {
	if entry == nil {
		return batchindex.RunRecord{}, errors.New("nil entry")
	}
	if runID == "" {
		return batchindex.RunRecord{}, errors.New("empty run id")
	}

	liveRunDir := filepath.Join(repoRoot, ".sandman", "batches", entry.ID, "runs", runID)
	liveManifest := filepath.Join(liveRunDir, "run.json")
	data, err := os.ReadFile(liveManifest)
	if err != nil {
		return batchindex.RunRecord{}, fmt.Errorf("read run manifest for %q: %w", runID, err)
	}
	var manifest batchindex.RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return batchindex.RunRecord{}, fmt.Errorf("decode run manifest for %q: %w", runID, err)
	}
	if !isTerminalRunStatus(manifest.Status) {
		return batchindex.RunRecord{}, &NonTerminalRowError{RunID: runID}
	}

	relArchive := filepath.Join(".sandman", "archive", entry.ID, "runs", runID)
	archiveRunDir := filepath.Join(repoRoot, relArchive)
	if info, statErr := os.Stat(archiveRunDir); statErr == nil {
		if info.IsDir() {
			return batchindex.RunRecord{}, &AlreadyArchivedError{ArchivePath: relArchive}
		}
		return batchindex.RunRecord{}, fmt.Errorf("archive target %q exists and is not a directory", archiveRunDir)
	} else if !os.IsNotExist(statErr) {
		return batchindex.RunRecord{}, fmt.Errorf("stat archive target %q: %w", archiveRunDir, statErr)
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "archive", entry.ID, "runs"), 0755); err != nil {
		return batchindex.RunRecord{}, fmt.Errorf("create archive parent: %w", err)
	}

	if err := os.Rename(liveRunDir, archiveRunDir); err != nil {
		return batchindex.RunRecord{}, fmt.Errorf("move run dir to %q: %w", archiveRunDir, err)
	}

	if err := StripSockets(archiveRunDir); err != nil {
		return batchindex.RunRecord{}, fmt.Errorf("strip sockets from %q: %w", archiveRunDir, err)
	}

	return batchindex.RunRecord{
		RunID:       runID,
		Status:      batchindex.RunRecordStatusArchived,
		ArchivePath: relArchive,
	}, nil
}

func isTerminalRunStatus(s batchindex.RunManifestStatus) bool {
	switch s {
	case batchindex.RunManifestStatusSuccess,
		batchindex.RunManifestStatusFailure,
		batchindex.RunManifestStatusAborted,
		batchindex.RunManifestStatusBlocked:
		return true
	}
	return false
}