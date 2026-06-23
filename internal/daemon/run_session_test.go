package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// mustParseTime parses an RFC3339 timestamp or fails the test.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return parsed
}

// stubCommander is a minimal IssueCommander used by RunSession tests.
type stubCommander struct {
	abortErr error
}

func (s *stubCommander) AbortIssue(issueNumber int) error { return s.abortErr }

// TestRunSession_Prepare_CreatesRunDirManifestAndSockets is the unit-level
// companion to the integration test in internal/cmd. It exercises the
// RunSession boot in isolation: Prepare must produce the run directory,
// the batch manifest, and batch.sock (control) — in that order.
// Per-run artifacts (run.json, run.sock) are created by the orchestrator
// in the per-row execution path, not by Prepare. Close must clean up
// afterwards.
func TestRunSession_Prepare_CreatesRunDirManifestAndSockets(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "smn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	rs := NewRunSession(dir, "tr1")

	manifest := BatchManifest{Issues: []int{42}, CreatedAt: mustParseTime(t, "2024-01-01T00:00:00Z")}

	if err := rs.Prepare(manifest, &stubCommander{}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	if rs.RunDir() == "" {
		t.Fatal("RunDir() must be non-empty after Prepare")
	}
	wantRunDir := filepath.Join(dir, "batches", "tr1")
	if rs.RunDir() != wantRunDir {
		t.Errorf("RunDir = %q, want %q", rs.RunDir(), wantRunDir)
	}

	// Run directory exists.
	runDirInfo, err := os.Stat(rs.RunDir())
	if err != nil {
		t.Fatalf("stat run dir: %v", err)
	}
	if !runDirInfo.IsDir() {
		t.Fatalf("run dir %s is not a directory", rs.RunDir())
	}

	// Batch manifest exists and decodes back to the same payload.
	manifestData, err := os.ReadFile(filepath.Join(rs.RunDir(), "batch.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	got, err := ReadManifest(rs.RunDir())
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if len(got.Issues) != 1 || got.Issues[0] != 42 {
		t.Errorf("manifest issues = %v, want [42]", got.Issues)
	}
	if !strings.Contains(string(manifestData), `"issues":[42]`) {
		t.Errorf("manifest payload missing issues: %s", manifestData)
	}

	// batch.sock (control socket) exists and is live at batch root.
	ctlSock := filepath.Join(rs.RunDir(), "batch.sock")
	if conn, err := net.Dial("unix", ctlSock); err != nil {
		t.Fatalf("dial batch.sock: %v", err)
	} else {
		conn.Close()
	}

	// Per-run artifacts are NOT created by Prepare — they are created
	// by the orchestrator in the per-row execution path. So batch-level
	// run.sock must NOT exist, and <batch>/runs/ directory must NOT exist.
	runSock := filepath.Join(rs.RunDir(), "run.sock")
	if _, err := os.Stat(runSock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run.sock must NOT exist at batch level after Prepare (per-run socket created by orchestrator): stat err = %v", err)
	}
	runsDir := filepath.Join(rs.RunDir(), "runs")
	if _, err := os.Stat(runsDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("runs/ directory must NOT exist after Prepare (per-run folders created by orchestrator): stat err = %v", err)
	}
}

// TestRunSession_Prepare_SkipsCommandServerWhenCommanderNil covers the
// review-daemon path: when the caller passes a nil commander, the boot
// must skip the run.sock step cleanly. batch.sock and batch.json must
// still be present.
func TestRunSession_Prepare_SkipsCommandServerWhenCommanderNil(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "smn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	rs := NewRunSession(dir, "rev1")
	t.Cleanup(func() { _ = rs.Close() })

	manifest := BatchManifest{Issues: []int{}, CreatedAt: mustParseTime(t, "2024-01-01T00:00:00Z")}
	if err := rs.Prepare(manifest, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if _, err := net.Dial("unix", filepath.Join(rs.RunDir(), "batch.sock")); err != nil {
		t.Fatalf("batch.sock must be live when commander is nil: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rs.RunDir(), "run.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run.sock must NOT exist when commander is nil, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rs.RunDir(), "batch.json")); err != nil {
		t.Errorf("batch.json must exist when commander is nil: %v", err)
	}
}

// TestRunSession_Prepare_PropagatesControlSocketError asserts that when
// ControlSocket.Start fails, Prepare returns a wrapped ErrStepControlSocket
// error. We force the failure by making the runDir a non-empty
// directory whose only entry is itself — a loop. ControlSocket.Start
// then calls Chmod on the run dir, but a non-empty directory whose
// only entry is the runDir itself is a pathological state we
// simulate by creating a child directory that consumes the inode: the
// batch.sock path can then be created (it's a file, not a directory),
// but net.Listen("unix", "<dir>/batch.sock") refuses to bind because
// the batch.sock path is a non-empty directory.
//
// The cleanest portable trick is to pre-create the batch.sock path as
// a non-empty directory. MkdirAll(<dir>) is happy, but
// net.Listen("unix", <dir>/batch.sock) where <dir>/batch.sock is a
// non-empty directory fails with EADDRINUSE / "address already in use".
func TestRunSession_Prepare_PropagatesControlSocketError(t *testing.T) {
	dir := t.TempDir()
	rs := NewRunSession(dir, "failing-run-1")
	t.Cleanup(func() { _ = rs.Close() })

	// Pre-create the batch.sock path as a non-empty directory. MkdirAll
	// on the run dir is happy; the manifest write is happy; but
	// net.Listen on a path that is an existing non-empty directory
	// fails.
	runDir := filepath.Join(dir, "batches", "failing-run-1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	batchSockPath := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(batchSockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchSockPath, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	manifest := BatchManifest{Issues: []int{1}, CreatedAt: mustParseTime(t, "2024-01-01T00:00:00Z")}
	err := rs.Prepare(manifest, nil)
	if err == nil {
		t.Fatal("Prepare must fail when batch.sock cannot be bound")
	}
	if !errors.Is(err, ErrStepControlSocket) {
		t.Errorf("Prepare error = %v, want wrap of ErrStepControlSocket", err)
	}
}

// TestRunSession_Close_StopsListenersButKeepsDirectory asserts idempotent
// teardown: calling Close stops the sockets, preserves the directory,
// and is safe to call a second time.
func TestRunSession_Close_StopsListenersButKeepsDirectory(t *testing.T) {
	dir := t.TempDir()
	rs := NewRunSession(dir, "closing-run-1")

	manifest := BatchManifest{Issues: []int{42}, CreatedAt: mustParseTime(t, "2024-01-01T00:00:00Z")}
	if err := rs.Prepare(manifest, &stubCommander{}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	runDir := rs.RunDir()

	if err := rs.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("batch dir must still exist after Close, stat err = %v", err)
	}
	if _, err := net.Dial("unix", filepath.Join(runDir, "run.sock")); err == nil {
		t.Errorf("run.sock must be gone after Close")
	}

	// Idempotent: a second Close is a no-op and does not error.
	if err := rs.Close(); err != nil {
		t.Errorf("second Close must be a no-op, got %v", err)
	}
}

// TestRunSession_Prepare_PropagatesMkdirError asserts that when the
// batch directory cannot be created (parent path is a non-directory),
// Prepare returns a wrapped ErrStepMkdir error and the batch dir is
// NOT created.
func TestRunSession_Prepare_PropagatesMkdirError(t *testing.T) {
	dir := t.TempDir()
	// Pre-create .sandman/batches as a regular file so MkdirAll
	// returns "not a directory" for any batchDir underneath.
	if err := os.MkdirAll(filepath.Join(dir, "sandman-stub"), 0o700); err != nil {
		t.Fatal(err)
	}
	batchesPath := filepath.Join(dir, "batches")
	if err := os.WriteFile(batchesPath, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(batchesPath) })

	rs := NewRunSession(dir, "mkdir-fail-run")
	t.Cleanup(func() { _ = rs.Close() })

	manifest := BatchManifest{Issues: []int{1}, CreatedAt: mustParseTime(t, "2024-01-01T00:00:00Z")}
	err := rs.Prepare(manifest, nil)
	if err == nil {
		t.Fatal("Prepare must fail when MkdirAll cannot create batchDir")
	}
	if !errors.Is(err, ErrStepMkdir) {
		t.Errorf("Prepare error = %v, want wrap of ErrStepMkdir", err)
	}
}

// nilCommander is a concrete IssueCommander whose pointer is nil.
// The only safe way to detect this is via reflect.ValueOf().IsNil();
// a plain `commander != nil` returns true for a typed-nil interface.
type nilCommander struct{}

func (*nilCommander) AbortIssue(int) error { return nil }

// TestRunSession_Prepare_AppendsToBatchesIndex asserts that Prepare
// appends an entry to batches.json with the expected id, kind, status,
// issues, and pr fields.
func TestRunSession_Prepare_AppendsToBatchesIndex(t *testing.T) {
	dir := t.TempDir()
	rs := NewRunSession(dir, "index-test-run-1")
	t.Cleanup(func() { _ = rs.Close() })

	prNum := 42
	manifest := BatchManifest{
		BatchId:   "index-test-run-1",
		RunKind:   "review",
		Issues:    []int{10, 20},
		PR:        &prNum,
		CreatedAt: mustParseTime(t, "2024-06-01T00:00:00Z"),
	}
	if err := rs.Prepare(manifest, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	idx, err := batchindex.Load(BatchesIndexPath(dir))
	if err != nil {
		t.Fatalf("Load batches index: %v", err)
	}

	entry := idx.Resolve("index-test-run-1")
	if entry == nil {
		t.Fatal("batches index must contain entry for index-test-run-1")
	}
	if entry.Kind != batchindex.KindReview {
		t.Errorf("entry.Kind = %v, want %v", entry.Kind, batchindex.KindReview)
	}
	if entry.Status != batchindex.StatusActive {
		t.Errorf("entry.Status = %v, want %v", entry.Status, batchindex.StatusActive)
	}
	if len(entry.Issues) != 2 || entry.Issues[0] != 10 || entry.Issues[1] != 20 {
		t.Errorf("entry.Issues = %v, want [10, 20]", entry.Issues)
	}
	if entry.PR != 42 {
		t.Errorf("entry.PR = %v, want 42", entry.PR)
	}
}

// TestRunSession_Prepare_TypedNilCommanderIsTreatedAsNil guards the
// reflect-based nil check in Prepare. A typed-nil IssueCommander
// (e.g. `var c IssueCommander = (*nilCommander)(nil)`) must NOT
// trigger the run.sock step, because calling its method would panic.
func TestRunSession_Prepare_TypedNilCommanderIsTreatedAsNil(t *testing.T) {
	dir := t.TempDir()
	rs := NewRunSession(dir, "typed-nil-run-1")
	t.Cleanup(func() { _ = rs.Close() })

	var commander IssueCommander = (*nilCommander)(nil)
	manifest := BatchManifest{Issues: []int{1}, CreatedAt: mustParseTime(t, "2024-01-01T00:00:00Z")}
	if err := rs.Prepare(manifest, commander); err != nil {
		t.Fatalf("Prepare must succeed for typed-nil commander: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rs.RunDir(), "run.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run.sock must NOT exist for typed-nil commander, stat err = %v", err)
	}
}
