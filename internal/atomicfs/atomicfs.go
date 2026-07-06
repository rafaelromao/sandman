// Package atomicfs owns the three atomic-write idioms used across Sandman's
// on-disk state.
//
// # Two distinct flavours of "atomic"
//
// Sandman state files fall into two camps, and the helpers below mirror
// that split deliberately:
//
//   - Whole-file replacement (config.yaml, batches.json, run.json, ...).
//     These use WriteAtomic / WriteAtomicJSON: write a unique temp file
//     in the destination directory via os.CreateTemp, fsync-by-Rename it
//     into place, and remove the temp on every error path. A crash
//     between write and rename leaves the previous file untouched, so
//     readers see either the old content or the new content, never a
//     torn mix.
//
//   - Append-only logs (events.jsonl, run.log, ...). These use
//     OpenAppend, which opens the file with O_APPEND|O_CREATE|O_WRONLY.
//     O_APPEND is mandatory for multi-process writers: it makes the
//     kernel position every write(2) at the current EOF atomically,
//     which is the byte-level guarantee a JSONL log needs.
//
// # DO NOT migrate O_APPEND files to rename-style atomicity
//
// The "ghost inode" risk: an in-flight O_APPEND FD opened against the
// pre-rename path would keep writing to the original inode, which no
// reader can see by name after os.Rename swaps the directory entry.
// Reads from the renamed path would skip those bytes entirely, so
// JSONL/event log files MUST stay on OpenAppend. This caveat mirrors
// the one already enforced on JSONLLogger.ensureOpen in internal/events.
package atomicfs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic writes data to path via a unique temp file in the same
// directory followed by os.Rename. On any error path the temp file is
// removed. A crash between write and rename leaves the previous file
// intact. The temp file uses the pattern "<base>.tmp.*" so leftover
// scratch from a partial run is identifiable.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	tmpFile, err := os.CreateTemp(dir, base+".tmp.")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	renamed = true
	return nil
}

// WriteAtomicJSON is WriteAtomic with json.MarshalIndent(v, "", "  ")
// applied first. The two-space indent matches the existing JSON writers
// in internal/batchindex and internal/events.
func WriteAtomicJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	return WriteAtomic(path, data, perm)
}

// OpenAppend opens path for append-only writes with the given perm,
// creating it if it does not exist. Multi-process writers rely on
// O_APPEND for byte-level atomicity; do not migrate JSONL/event-log
// writers to rename-style atomicity. See the package doc comment for
// the "ghost inode" caveat.
func OpenAppend(path string, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return nil, fmt.Errorf("open append: %w", err)
	}
	return f, nil
}
