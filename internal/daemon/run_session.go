package daemon

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// batchesIndexMu serialises read-modify-write cycles on the shared
// batches index (batches.json). Without this, concurrent Prepare calls
// (e.g. from the review daemon processing multiple PRs in parallel)
// race on Load → Add → Save, causing lost entries.
var batchesIndexMu sync.Mutex

// RunSession owns the on-disk artifacts and the batch-level control socket
// that must exist *before* the daemon emits a run.started event and before
// any AgentRun writes log output. Issue #1024 fixed a class of ghost rows
// in the event log: a daemon could be killed after emitting run.started
// but before creating .sandman/batches/<batch-id>/, leaving the portal unable to
// reconcile the run. RunSession collapses the three-step boot
// (MkdirAll → WriteManifest → ControlSocket.Start) into a single Prepare
// call so the ordering is structural, not procedural. A future refactor
// cannot reorder the steps without rewriting Prepare itself.
//
// Per-run artifacts (run.json, run.log, run.sock) are created by the
// orchestrator in the per-row execution path.
//
// The session's lifetime is: NewRunSession → Prepare → ... → Close.
// Close is idempotent and preserves the batch directory on disk.
type RunSession struct {
	baseDir string
	runID   string
	runDir  string

	broadcaster  *Broadcaster
	ctlSocket    *ControlSocket
	started      bool
	closeOnceRan bool
}

// ErrStep* are sentinel errors tagged on every Prepare failure so tests
// (and operators) can branch on which step regressed without parsing
// the wrapped message. Prepare wraps the original error with
// fmt.Errorf("...: %w", ErrStepX) so errors.Is(err, ErrStepX) is
// sufficient to identify the failing step.
var (
	ErrStepMkdir         = errors.New("daemon: RunSession.Prepare failed at MkdirAll")
	ErrStepManifest      = errors.New("daemon: RunSession.Prepare failed at WriteManifest")
	ErrStepBatchesIndex  = errors.New("daemon: RunSession.Prepare failed at BatchesIndex")
	ErrStepControlSocket = errors.New("daemon: RunSession.Prepare failed at ControlSocket.Start")
)

// NewRunSession returns a session bound to the batch directory that
// BatchDir(baseDir, batchID) computes. The directory is not created
// yet — Prepare does that as the first boot step.
func NewRunSession(baseDir, batchID string) *RunSession {
	return &RunSession{
		baseDir:     baseDir,
		runID:       batchID,
		runDir:      BatchDir(baseDir, batchID),
		broadcaster: NewBroadcaster(),
	}
}

// RunDir returns the batch directory this session will own. It is
// available before Prepare (it is just a path computation) so callers
// can wire it into batch.Request.RunDir without waiting for the boot
// to complete. RunDir is not safe for concurrent use; the session is
// expected to be constructed, queried, and torn down by a single
// goroutine.
//
// Deprecated: RunDir is an alias for BatchDir. New code should use
// BatchDir. This alias is kept for backward compatibility during the
// transition to per-batch-per-run layout where run artifacts live
// in <batchDir>/runs/<runID>/ within .sandman/batches/<batch-id>/.
func (s *RunSession) RunDir() string {
	return s.runDir
}

// Broadcaster returns the broadcaster the ControlSocket streams to.
// Callers wire req.OutputWriter = rs.Broadcaster() before the
// orchestrator emits any output.
func (s *RunSession) Broadcaster() *Broadcaster {
	return s.broadcaster
}

// Prepare creates the batch directory, writes the batch manifest, starts
// the ControlSocket, and appends an entry to the batches index — in that
// fixed order. The steps are not reorderable: a future refactor that
// moves any step after Prepare's return value would break the boot
// invariant that issue #1024 enforces.
//
// On failure, Prepare returns a wrapped sentinel error identifying
// the failing step. The session is left in a partially-constructed
// state — the caller must still call Close to release whatever
// resources were acquired. The orchestrator is never reached, so
// run.started is never emitted when Prepare fails.
//
// Per-run artifacts (run.json, run.log, run.sock) are created by the
// orchestrator in the per-row execution path, not by Prepare.
//
// # Contract: manifest.BatchId equals the PUBLIC BatchId
//
// Per issue #1917 (slice 1 of #1916), the batches index entry id and
// the batch.json.batchId field MUST equal the public BatchId (the
// batch folder basename). This supersedes the earlier ADR-0036
// contract (manifest.BatchId == per-row RunID).
//
// The public BatchId rules (per #1917, refined for review by #1919 slice 3):
//
//   - Issue single:    "<ts>-<sid>-<num>"                    (no +N suffix)
//   - Issue multi:     "<ts>-<sid>-<firstIssue>+<additionalCount>"
//   - Review (orphan):  "<ts>-<sid>-PR<pr>"
//   - Review (linked):  "<ts>-<sid>-<linkedIssue>-PR<pr>"
//   - Auto-select:     "<ts>-<sid>-auto-<N>"
//   - Prompt-only:     "<ts>-<sid>-prompt[-<userid>]"
//
// The per-row RunID is no longer stamped into batch.json. It lives in
// run.json.RunID and the per-run folder name `<batch>/runs/<runID>/`.
// Per-row archive/abort resolution must map per-row RunID → public
// BatchId before consulting the index (e.g. by walking the batch
// folder for `runs/<runID>/run.json` or by scanning the entry's path
// for the per-run folder).
//
// Tests in `internal/daemon/run_session_test.go` (TestRunSession_IdxAddOnlyCalledFromPrepare)
// pin the invariant that `idx.AddBatch` is only called from `Prepare`,
// which is preserved across the #1917 contract change.
func (s *RunSession) Prepare(manifest BatchManifest) error {
	s.runDir = s.RunDir()

	if err := os.MkdirAll(s.runDir, 0o700); err != nil {
		return fmt.Errorf("%w: %v", ErrStepMkdir, err)
	}

	if err := WriteManifest(s.runDir, manifest); err != nil {
		return fmt.Errorf("%w: %v", ErrStepManifest, err)
	}

	batchesIndexMu.Lock()
	idx, err := batchindex.Load(BatchesIndexPath(s.baseDir))
	if err != nil {
		batchesIndexMu.Unlock()
		return fmt.Errorf("%w: %v", ErrStepBatchesIndex, err)
	}

	pr := 0
	if manifest.PR != nil {
		pr = *manifest.PR
	}
	idx.AddBatch(batchindex.Batch{
		ID:        manifest.BatchId,
		Path:      s.runDir,
		Kind:      batchindex.Kind(manifest.RunKind),
		Status:    batchindex.StatusActive,
		CreatedAt: manifest.CreatedAt,
		Issues:    manifest.Issues,
		PR:        pr,
	})
	if err := idx.Save(BatchesIndexPath(s.baseDir)); err != nil {
		batchesIndexMu.Unlock()
		return fmt.Errorf("%w: %v", ErrStepBatchesIndex, err)
	}
	batchesIndexMu.Unlock()

	s.ctlSocket = NewControlSocket(s.runDir, s.broadcaster)
	if err := s.ctlSocket.Start(); err != nil {
		return fmt.Errorf("%w: %v", ErrStepControlSocket, err)
	}

	s.started = true
	return nil
}

// Close releases the session's resources. It is safe to call multiple
// times; only the first call performs teardown. The teardown order
// matches the boot order in reverse:
//
//  1. ControlSocket.Stop (which also closes the broadcaster)
//
// The batch directory is NOT removed — it persists on disk so the portal
// can still recover runs from it.
func (s *RunSession) Close() error {
	if s.closeOnceRan {
		return nil
	}
	s.closeOnceRan = true

	if s.ctlSocket != nil {
		_ = s.ctlSocket.Stop()
	}
	return nil
}
