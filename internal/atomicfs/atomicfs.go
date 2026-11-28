// Package atomicfs owns the atomic-write idioms used across Sandman:
//
//   - WriteAtomic / WriteAtomicJSON: rename-style atomicity, where the
//     destination is replaced via os.CreateTemp in the same directory
//     followed by os.Rename. A crash between write and rename leaves the
//     previous file intact; readers either see the previous-good bytes or
//     the new bytes, never a torn mix. This is the pattern already used by
//     batches.json, run.json, and config.yaml.
//
//   - WriteAtomicIn / WriteAtomicNew: the same idioms confined to an os.Root,
//     replacing a file by rename or publishing a new one by hard link, never
//     outside the root.
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
	"crypto/rand"
	"encoding/hex"
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

// link publishes WriteAtomicNew's temp file; tests replace it.
var link = (*os.Root).Link

// WriteAtomicNew writes data to a new file at name inside root without ever
// replacing an existing one, and never outside root: a symlink that leads out
// of it fails the write. The data goes to a unique temp file in the same
// directory, which is then hard-linked to name, so the file appears complete
// or not at all. It fails with an error wrapping os.ErrExist when name already
// exists, and with the link error, leaving no file at name, where the file
// system cannot hard-link.
func WriteAtomicNew(root *os.Root, name string, data []byte, perm os.FileMode) error {
	return writeInRoot(root, name, data, perm, func(tmpName string) error {
		if err := link(root, tmpName, name); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		return nil
	})
}

// WriteAtomicIn writes data to name inside root, replacing any file there, and
// never outside root: a symlink that leads out of it fails the write. The data
// goes to a unique temp file in the same directory, which is then renamed over
// name, so readers see the previous file or the new one, never a torn mix.
func WriteAtomicIn(root *os.Root, name string, data []byte, perm os.FileMode) error {
	return writeInRoot(root, name, data, perm, func(tmpName string) error {
		if err := root.Rename(tmpName, name); err != nil {
			return fmt.Errorf("replace %s: %w", name, err)
		}
		return nil
	})
}

// writeInRoot writes data to a unique temp file beside name inside root and
// hands its name to publish. The temp file is removed whatever publish does.
func writeInRoot(root *os.Root, name string, data []byte, perm os.FileMode, publish func(tmpName string) error) error {
	if err := root.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return fmt.Errorf("create dir for %s: %w", name, err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("name temp for %s: %w", name, err)
	}
	tmpName := name + ".tmp." + hex.EncodeToString(suffix[:])
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", name, err)
	}
	defer func() { _ = root.Remove(tmpName) }()
	_, err = tmp.Write(data)
	if err == nil {
		// The umask does not narrow the requested mode.
		err = tmp.Chmod(perm)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write temp for %s: %w", name, err)
	}
	return publish(tmpName)
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
