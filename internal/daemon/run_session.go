package daemon

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// batchesIndexMu serialises read-modify-write cycles on the shared
// batches index (batches.json). Without this, concurrent Prepare calls
// (e.g. from the review daemon processing multiple PRs in parallel)
// race on Load → Add → Save, causing lost entries.
var batchesIndexMu sync.Mutex

// RunSession owns the on-disk artifacts and the per-run sockets that must
// exist *before* the daemon emits a run.started event and before any
// AgentRun writes log output. Issue #1024 fixed a class of ghost rows in
// the event log: a daemon could be killed after emitting run.started but
// before creating .sandman/runs/<id>/, leaving the portal unable to
// reconcile the run. RunSession collapses the four-step boot
// (MkdirAll → WriteManifest → ControlSocket.Start → CommandServer.Start)
// into a single Prepare call so the ordering is structural, not
// procedural. A future refactor cannot reorder the steps without
// rewriting Prepare itself.
//
// The session's lifetime is: NewRunSession → Prepare → ... → Close.
// Close is idempotent and removes the run directory, so the caller
// writes a single `defer rs.Close()` and never needs to track the
// individual artifact lifetimes.
type RunSession struct {
	baseDir string
	runID   string
	runDir  string

	broadcaster  *Broadcaster
	ctlSocket    *ControlSocket
	cmdServer    *CommandServer
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
	ErrStepCommandServer = errors.New("daemon: RunSession.Prepare failed at CommandServer.Start")
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
// BatchDir. This alias is kept for backward compatibility and will
// be removed when .sandman/runs/ is wiped in Slice 5.
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
// the ControlSocket (and CommandServer if commander is non-nil), and
// appends an entry to the batches index — in that fixed order. The steps
// are not reorderable: a future refactor that moves any step after
// Prepare's return value would break the boot invariant that issue #1024
// enforces.
//
// On failure, Prepare returns a wrapped sentinel error identifying
// the failing step. The session is left in a partially-constructed
// state — the caller must still call Close to release whatever
// resources were acquired. The orchestrator is never reached, so
// run.started is never emitted when Prepare fails.
func (s *RunSession) Prepare(manifest BatchManifest, commander IssueCommander) error {
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
	idx.Add(batchindex.Entry{
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

	// A typed-nil interface (e.g. `var c IssueCommander = (*T)(nil)`)
	// is non-nil but unusable; calling cmdServer.Start with such a
	// value would panic on the first abort request. reflect.IsNil
	// detects the typed-nil trap by introspecting the dynamic value.
	hasRealCommander := commander != nil && !reflect.ValueOf(commander).IsNil()
	if hasRealCommander {
		s.cmdServer = NewCommandServer(s.runDir, commander)
		if err := s.cmdServer.Start(); err != nil {
			return fmt.Errorf("%w: %v", ErrStepCommandServer, err)
		}
	}

	s.started = true
	return nil
}

// Close releases the session's resources. It is safe to call multiple
// times; only the first call performs teardown. The teardown order
// matches the boot order in reverse:
//
//  1. CommandServer.Stop (if started)
//  2. ControlSocket.Stop (which also closes the broadcaster)
//
// The batch directory is NOT removed — it persists on disk so the portal
// can still recover runs from it in Slice 3.
func (s *RunSession) Close() error {
	if s.closeOnceRan {
		return nil
	}
	s.closeOnceRan = true

	if s.cmdServer != nil {
		_ = s.cmdServer.Stop()
	}
	if s.ctlSocket != nil {
		_ = s.ctlSocket.Stop()
	}
	return nil
}
