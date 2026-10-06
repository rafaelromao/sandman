package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
)

// NonTerminalRowError is returned by ArchiveRow when the targeted
// row's event-derived lifecycle is nonterminal or unknown. It carries the offending
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
// row must have a terminal event projection; an active or unknown row returns
// a *NonTerminalRowError. The manifest is artifact metadata, never authority.
//
// ArchiveRow is the single seam both the CLI subcommand and the HTTP
// archive endpoint dispatch through. It does not touch
// .sandman/worktrees/, does not edit events.jsonl, and does not dial
// the per-run socket (the row is terminal by contract). When the
// destination already exists, ArchiveRow returns *AlreadyArchivedError
// with the existing ArchivePath populated, so callers can surface it
// in error bodies without re-walking the filesystem.
func ArchiveRow(repoRoot string, batch *batchindex.Batch, runID string, log events.EventLog) (batchindex.RunRecord, error) {
	if batch == nil {
		return batchindex.RunRecord{}, errors.New("nil batch")
	}
	if runID == "" {
		return batchindex.RunRecord{}, errors.New("empty run id")
	}

	states, err := events.ReadRunStates(log)
	if err != nil {
		return batchindex.RunRecord{}, err
	}
	if !states[runID].IsTerminal() {
		return batchindex.RunRecord{}, &NonTerminalRowError{RunID: runID}
	}
	liveRunDir := filepath.Join(batch.Path, "runs", runID)

	relArchive := filepath.Join(".sandman", "archive", batch.ID, "runs", runID)
	archiveRunDir := filepath.Join(repoRoot, relArchive)
	if info, statErr := os.Stat(archiveRunDir); statErr == nil {
		if info.IsDir() {
			return batchindex.RunRecord{}, &AlreadyArchivedError{ArchivePath: relArchive}
		}
		return batchindex.RunRecord{}, fmt.Errorf("archive target %q exists and is not a directory", archiveRunDir)
	} else if !os.IsNotExist(statErr) {
		return batchindex.RunRecord{}, fmt.Errorf("stat archive target %q: %w", archiveRunDir, statErr)
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "archive", batch.ID, "runs"), 0755); err != nil {
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
