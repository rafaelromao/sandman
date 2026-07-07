// Package atomicfs owns the three atomic-write idioms used across Sandman:
//
//   - WriteAtomic / WriteAtomicJSON: rename-style atomicity, where the
//     destination is replaced via os.CreateTemp in the same directory
//     followed by os.Rename. A crash between write and rename leaves the
//     previous file intact; readers either see the previous-good bytes or
//     the new bytes, never a torn mix. This is the pattern already used by
//     batches.json, run.json, and config.yaml.
//
//   - OpenAppend: O_APPEND-style atomicity, where the file is opened with
//     O_APPEND|O_CREATE|O_WRONLY. The kernel positions every write at the
//     current EOF atomically, which is what makes multi-process writers
//     (the events.jsonl log, run.log) safe against byte-level interleaving.
//
// IMPORTANT: O_APPEND-style files must NOT be migrated to rename-style
// atomicity. An in-flight O_APPEND file descriptor holds a reference to
// the inode at the moment it was opened. If the on-disk name is later
// replaced via os.Rename, that FD continues to write to the original
// inode — a "ghost" file no reader can see by name. The existing
// JSONLLogger.ensureOpen comment makes the same point:
//
//	O_APPEND is mandatory: multiple sandman daemons (and the portal)
//	can write to the same repo-scoped events.jsonl. Without O_APPEND,
//	write(2) goes at the FD's current position, which is independent
//	per process, so two processes' writes interleave at the byte level
//	and tear every line longer than a single pipe-sized write. O_APPEND
//	makes the kernel position every write at the current EOF atomically,
//	which is exactly the guarantee a JSONL log needs.
//
// Callers must pick the helper that matches the file's concurrency
// model. events.jsonl and run.log use OpenAppend; batches.json, run.json,
// and config.yaml use WriteAtomic / WriteAtomicJSON. The two idioms are
// not interchangeable.
//
// WriteAtomic / WriteAtomicJSON take an explicit perm and apply it via
// os.Chmod before the rename. The pre-existing in-tree writers
// (batchindex.Index.Save, batchindex.WriteManifest, config.Save) do not
// chmod and rely on the umask-driven 0600 default of os.CreateTemp;
// once those call sites migrate onto WriteAtomic, each one will pass
// the perm it wants rather than inherit the umask.
package atomicfs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic writes data to path via a unique temp file in the same
// directory followed by os.Rename. The temp file is named
// "<base>.tmp.<random>" so concurrent writers do not collide on a fixed
// name. On any error path the temp file is removed, leaving the
// destination's previous contents (if any) intact.
//
// The temp file inherits the requested perm; the rename preserves the
// perm across the swap.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir for %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, base+".tmp.")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()

	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod temp for %s: %w", path, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp to %s: %w", path, err)
	}
	renamed = true
	return nil
}

// WriteAtomicJSON marshals v with json.MarshalIndent ("", "  ") and
// writes the result via WriteAtomic. The two-space indent mirrors the
// existing JSON writers in batchindex, config, and the run manifest.
func WriteAtomicJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal for %s: %w", path, err)
	}
	if err := WriteAtomic(path, data, perm); err != nil {
		return err
	}
	return nil
}

// OpenAppend opens path for append-only writes with O_APPEND|O_CREATE|
// O_WRONLY and the given perm. The returned *os.File must be closed by
// the caller. See the package doc for why this helper exists alongside
// WriteAtomic: O_APPEND provides byte-level atomicity for multi-process
// writers, which rename-style atomicity cannot.
func OpenAppend(path string, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return nil, fmt.Errorf("open append %s: %w", path, err)
	}
	return f, nil
}
