package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func writePerRowArchiveManifest(t *testing.T, runDir string, manifest batchindex.RunManifest) {
	t.Helper()
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), data, 0644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}
}

func createUnixSocketAt(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir socket parent: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		if rmErr := os.Remove(path); rmErr != nil {
			t.Fatalf("remove pre-existing socket %q: %v", path, rmErr)
		}
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("bind %q: %v", path, err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("listen %q: %v", path, err)
	}
	// Keep the listener alive for the rest of the test. t.Cleanup
	// (via t.TempDir) will reap the directory on test exit.
	t.Cleanup(func() { _ = unix.Close(fd) })
}

// TestArchiveRow_MovesRunFolder is the slice-2 tracer bullet: a
// terminal row under batches/<batchID>/runs/<runID>/ moves to
// archive/<batchID>/runs/<runID>/ and the resulting RunRecord carries
// the relative archive path. The move must be atomic from the caller's
// perspective: either the row is at the new path and absent from the
// live path, or an error is returned without partial state.
func TestArchiveRow_MovesRunFolder(t *testing.T) {
	repoRoot := t.TempDir()
	runID := "row-1"
	batchID := "batch-1"
	runDir := filepath.Join(repoRoot, ".sandman", "batches", batchID, "runs", runID)
	writePerRowArchiveManifest(t, runDir, batchindex.RunManifest{
		RunID:     runID,
		BatchID:   batchID,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusSuccess,
	})

	entry := &batchindex.Batch{
		ID:   batchID,
		Path: filepath.Join(repoRoot, ".sandman", "batches", batchID),
		Kind: batchindex.KindIssue,
	}

	rec, err := ArchiveRow(repoRoot, entry, runID)
	if err != nil {
		t.Fatalf("ArchiveRow: %v", err)
	}
	if rec.Status != batchindex.RunRecordStatusArchived {
		t.Errorf("rec.Status = %s, want %s", rec.Status, batchindex.RunRecordStatusArchived)
	}
	if rec.RunID != runID {
		t.Errorf("rec.RunID = %q, want %q", rec.RunID, runID)
	}
	wantPath := filepath.Join(".sandman", "archive", batchID, "runs", runID)
	if rec.ArchivePath != wantPath {
		t.Errorf("rec.ArchivePath = %q, want %q", rec.ArchivePath, wantPath)
	}

	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("live runDir still present after archive: stat err = %v", err)
	}
	archivedRunDir := filepath.Join(repoRoot, ".sandman", "archive", batchID, "runs", runID)
	info, err := os.Stat(archivedRunDir)
	if err != nil {
		t.Fatalf("archived run dir missing: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("archived target is not a directory, mode = %s", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(archivedRunDir, "run.json")); err != nil {
		t.Errorf("run.json not moved: %v", err)
	}
}

// TestArchiveRow_StripsSocketsFromMovedFolder covers slice 2: every
// socket file inside the moved folder must be removed before the move
// returns, so the archive never ships live daemon sockets.
func TestArchiveRow_StripsSocketsFromMovedFolder(t *testing.T) {
	repoRoot := testenv.MkdirShort(t, "sm-arow-")
	runID := "r1"
	batchID := "b1"
	runDir := filepath.Join(repoRoot, ".sandman", "batches", batchID, "runs", runID)
	writePerRowArchiveManifest(t, runDir, batchindex.RunManifest{
		RunID:   runID,
		BatchID: batchID,
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusSuccess,
	})
	createUnixSocketAt(t, filepath.Join(runDir, "run.sock"))
	createUnixSocketAt(t, filepath.Join(runDir, "nested", "deep.sock"))

	entry := &batchindex.Batch{
		ID:   batchID,
		Path: filepath.Join(repoRoot, ".sandman", "batches", batchID),
		Kind: batchindex.KindIssue,
	}

	if _, err := ArchiveRow(repoRoot, entry, runID); err != nil {
		t.Fatalf("ArchiveRow: %v", err)
	}

	archivedRunDir := filepath.Join(repoRoot, ".sandman", "archive", batchID, "runs", runID)
	var sockets []string
	err := filepath.Walk(archivedRunDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSocket != 0 {
			sockets = append(sockets, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk archive: %v", err)
	}
	if len(sockets) > 0 {
		t.Errorf("archive contains socket files: %v", sockets)
	}
}

// TestArchiveRow_RefusesNonTerminalRow covers slice 2: a row whose
// run.json Status is still active must not be archived; ArchiveRow
// must return a typed error and leave both the live folder and
// events.jsonl untouched.
func TestArchiveRow_RefusesNonTerminalRow(t *testing.T) {
	repoRoot := t.TempDir()
	runID := "row-active"
	batchID := "batch-active"
	runDir := filepath.Join(repoRoot, ".sandman", "batches", batchID, "runs", runID)
	writePerRowArchiveManifest(t, runDir, batchindex.RunManifest{
		RunID:   runID,
		BatchID: batchID,
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusActive,
	})

	entry := &batchindex.Batch{
		ID:   batchID,
		Path: filepath.Join(repoRoot, ".sandman", "batches", batchID),
		Kind: batchindex.KindIssue,
	}

	_, err := ArchiveRow(repoRoot, entry, runID)
	if err == nil {
		t.Fatal("ArchiveRow on active row must return an error, got nil")
	}
	var nonTerminal *NonTerminalRowError
	if !errors.As(err, &nonTerminal) {
		t.Errorf("expected *NonTerminalRowError, got %T: %v", err, err)
	}
	if nonTerminal != nil && nonTerminal.RunID != runID {
		t.Errorf("NonTerminalRowError.RunID = %q, want %q", nonTerminal.RunID, runID)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("live runDir must remain on rejection, got: %v", err)
	}
}

// TestArchiveRow_DoesNotTouchWorktreesOrEvents covers slice 2: the
// per-row archive is a pure index + filesystem move. It must not
// touch .sandman/worktrees/ and must not edit events.jsonl.
func TestArchiveRow_DoesNotTouchWorktreesOrEvents(t *testing.T) {
	repoRoot := t.TempDir()
	runID := "row-clean"
	batchID := "batch-clean"
	runDir := filepath.Join(repoRoot, ".sandman", "batches", batchID, "runs", runID)
	writePerRowArchiveManifest(t, runDir, batchindex.RunManifest{
		RunID:   runID,
		BatchID: batchID,
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusSuccess,
	})

	worktreesDir := filepath.Join(repoRoot, ".sandman", "worktrees", "branch-x")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("mkdir worktrees: %v", err)
	}
	eventsPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0755); err != nil {
		t.Fatalf("mkdir events parent: %v", err)
	}
	if err := os.WriteFile(eventsPath, []byte(`{"type":"run.started"}`+"\n"), 0644); err != nil {
		t.Fatalf("write events: %v", err)
	}

	entry := &batchindex.Batch{
		ID:   batchID,
		Path: filepath.Join(repoRoot, ".sandman", "batches", batchID),
		Kind: batchindex.KindIssue,
	}

	eventsInfoBefore, _ := os.Stat(eventsPath)
	if _, err := ArchiveRow(repoRoot, entry, runID); err != nil {
		t.Fatalf("ArchiveRow: %v", err)
	}
	eventsInfoAfter, _ := os.Stat(eventsPath)
	if eventsInfoBefore != nil && eventsInfoAfter != nil && eventsInfoBefore.ModTime() != eventsInfoAfter.ModTime() {
		t.Errorf("events.jsonl mtime changed: before=%v after=%v", eventsInfoBefore.ModTime(), eventsInfoAfter.ModTime())
	}
	if _, err := os.Stat(worktreesDir); err != nil {
		t.Errorf("worktrees dir disappeared after per-row archive: %v", err)
	}
}

// TestArchiveRow_AlreadyArchivedReturnsError covers slice 2 idempotence:
// archiving a row whose destination already exists must return an
// AlreadyArchivedError carrying the existing archive path, leaving the
// live folder untouched.
func TestArchiveRow_AlreadyArchivedReturnsError(t *testing.T) {
	repoRoot := t.TempDir()
	runID := "row-dup"
	batchID := "batch-dup"
	runDir := filepath.Join(repoRoot, ".sandman", "batches", batchID, "runs", runID)
	writePerRowArchiveManifest(t, runDir, batchindex.RunManifest{
		RunID:   runID,
		BatchID: batchID,
		Kind:    batchindex.KindIssue,
		Status:  batchindex.RunManifestStatusSuccess,
	})

	existingArchiveDir := filepath.Join(repoRoot, ".sandman", "archive", batchID, "runs", runID)
	if err := os.MkdirAll(existingArchiveDir, 0755); err != nil {
		t.Fatalf("mkdir existing archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(existingArchiveDir, "sentinel.txt"), []byte("keep"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	entry := &batchindex.Batch{
		ID:   batchID,
		Path: filepath.Join(repoRoot, ".sandman", "batches", batchID),
		Kind: batchindex.KindIssue,
	}

	_, err := ArchiveRow(repoRoot, entry, runID)
	if err == nil {
		t.Fatal("ArchiveRow on already-archived row must return an error")
	}
	var alreadyArchived *AlreadyArchivedError
	if !errors.As(err, &alreadyArchived) {
		t.Fatalf("expected *AlreadyArchivedError, got %T: %v", err, err)
	}
	want := filepath.Join(".sandman", "archive", batchID, "runs", runID)
	if alreadyArchived.ArchivePath != want {
		t.Errorf("AlreadyArchivedError.ArchivePath = %q, want %q", alreadyArchived.ArchivePath, want)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("live runDir must remain on collision, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(existingArchiveDir, "sentinel.txt")); err != nil {
		t.Errorf("existing archive sentinel must be preserved: %v", err)
	}
}

// dialUnixSock is a tiny helper used to confirm a socket is or is
// not connectable. Kept here so the test file is self-contained.
func dialUnixSock(path string) bool {
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// TestArchiveRow_LeavesSiblingRowsAlive covers slice 6: archiving one
// row of a multi-run batch must leave the sibling row's folder, log
// file, and socket in place.
func TestArchiveRow_LeavesSiblingRowsAlive(t *testing.T) {
	repoRoot := testenv.MkdirShort(t, "sm-arow-")
	batchID := "bM"
	row1 := "r1"
	row2 := "r2"

	row1Dir := filepath.Join(repoRoot, ".sandman", "batches", batchID, "runs", row1)
	row2Dir := filepath.Join(repoRoot, ".sandman", "batches", batchID, "runs", row2)
	writePerRowArchiveManifest(t, row1Dir, batchindex.RunManifest{
		RunID: row1, BatchID: batchID, Kind: batchindex.KindIssue, Status: batchindex.RunManifestStatusSuccess,
	})
	writePerRowArchiveManifest(t, row2Dir, batchindex.RunManifest{
		RunID: row2, BatchID: batchID, Kind: batchindex.KindIssue, Status: batchindex.RunManifestStatusSuccess,
	})
	if err := os.WriteFile(filepath.Join(row2Dir, "run.log"), []byte("still live\n"), 0644); err != nil {
		t.Fatalf("write sibling log: %v", err)
	}
	createUnixSocketAt(t, filepath.Join(row2Dir, "run.sock"))

	entry := &batchindex.Batch{
		ID:   batchID,
		Path: filepath.Join(repoRoot, ".sandman", "batches", batchID),
		Kind: batchindex.KindIssue,
	}

	if _, err := ArchiveRow(repoRoot, entry, row1); err != nil {
		t.Fatalf("ArchiveRow: %v", err)
	}

	if _, err := os.Stat(row2Dir); err != nil {
		t.Errorf("sibling row2 must survive: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(row2Dir, "run.log"))
	if err != nil {
		t.Errorf("sibling log must survive: %v", err)
	}
	if !strings.Contains(string(log), "still live") {
		t.Errorf("sibling log content changed: %q", string(log))
	}
	if !dialUnixSock(filepath.Join(row2Dir, "run.sock")) {
		t.Errorf("sibling socket must remain connectable after per-row archive")
	}
}
