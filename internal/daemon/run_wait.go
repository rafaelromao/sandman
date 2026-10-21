package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"golang.org/x/sys/unix"
)

const RunRecoveryGrace = 5 * time.Minute

var ErrRunOwned = errors.New("run already has a live owner")

type RunClaim struct{ file *os.File }

// ClaimRun fences scheduler ownership by RunID. The stable lock inode is never
// removed: owner death releases the advisory lock, not the durable intent.
func ClaimRun(sandmanDir, runID string) (*RunClaim, error) {
	if !safeWaitID(runID) {
		return nil, fmt.Errorf("invalid run claim identity")
	}
	dir := filepath.Join(sandmanDir, "state", "run-claims")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, runID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrRunOwned
		}
		return nil, err
	}
	return &RunClaim{file: file}, nil
}

func (c *RunClaim) Close() error {
	if c == nil || c.file == nil {
		return nil
	}
	err := c.file.Close()
	c.file = nil
	return err
}

// RunWait carries ownership/schedule evidence only. Lifecycle and terminality
// remain exclusively event-derived, including initial admission descriptors.
type RunWait struct {
	Protocol          string    `json:"protocol"`
	RunID             string    `json:"run_id"`
	BatchID           string    `json:"batch_id"`
	Issue             int       `json:"issue"`
	Branch            string    `json:"branch"`
	BaseBranch        string    `json:"base_branch"`
	PreviousRunID     string    `json:"previous_run_id,omitempty"`
	PreviousBatchID   string    `json:"previous_batch_id,omitempty"`
	InitialAdmission  bool      `json:"initial_admission"`
	AdmissionMode     int       `json:"admission_mode"`
	Dependencies      []int     `json:"dependencies,omitempty"`
	ReuseSession      bool      `json:"reuse_session,omitempty"`
	Ready             bool      `json:"ready"`
	UsageLimitProbe   bool      `json:"usage_limit_probe,omitempty"`
	OperationID       string    `json:"operation_id"`
	OperationDeadline time.Time `json:"operation_deadline,omitempty"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
	NextPollAt        time.Time `json:"next_poll_at,omitempty"`
	RecoveryEventAt   time.Time `json:"-"` // event-only compatibility provenance
}

// Quota is bounded by accounted polling, not a wall-clock exhaustion estimate.
// Normalize older snapshots without changing their existing recovery grace.
func normalizeQuotaWait(record RunWait) RunWait {
	if record.UsageLimitProbe {
		record.OperationID = "quota:" + record.RunID
		record.OperationDeadline = time.Time{}
	}
	return record
}

func (w RunWait) RecoverableAt(now time.Time) bool {
	return !w.LeaseExpiresAt.IsZero() && now.Before(w.LeaseExpiresAt) &&
		(w.OperationDeadline.IsZero() || now.Before(w.OperationDeadline))
}

func safeWaitID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id && !strings.ContainsAny(id, `/\`)
}

func waitPath(batchDir, runID string) string {
	return filepath.Join(paths.BatchLocation{Dir: batchDir}.Run(runID).Dir, "wait.json")
}

// initialWaitPath is the repository-wide sidecar for a Run that has not yet
// started; it never depends on where the Batch directory happens to live.
// Earlier releases derived it from the Batch directory's grandparent. For
// canonical and flat relocated Batches that is this same path. Sidecars that
// a nested relocation or a batchless legacy Run left elsewhere are not
// migrated; their Runs recover through the event-anchored grace instead.
func initialWaitPath(layout paths.Layout, runID string) (string, error) {
	if layout.StateDir == "" {
		return "", fmt.Errorf("waiting state directory is unknown")
	}
	return filepath.Join(layout.StateDir, "waiting", runID+".json"), nil
}

// BatchLocationForDir returns the public identity owning a frozen Batch
// directory, with the directory spelled as the owner records it. Indexed
// owners win; an unindexed canonical directory keeps its basename only when no
// indexed Batch claims that name. Paths are compared after resolving symlinks,
// so a repository reached through another spelling of the same directory
// (a symlinked checkout, /tmp vs /private/tmp) finds the same owner.
func BatchLocationForDir(layout paths.Layout, batchDir string) (paths.BatchLocation, error) {
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return paths.BatchLocation{}, err
	}
	target := resolveDir(batchDir)
	var owner *paths.BatchLocation
	for _, batch := range idx.Batches {
		location, ok := idx.LocateBatchIdentity(layout, batch.ID)
		if !ok || location.ID != batch.ID || !target.same(location.Dir) {
			continue
		}
		if owner != nil {
			return paths.BatchLocation{}, fmt.Errorf("batch location has more than one owner: %s", batchDir)
		}
		owner = &location
	}
	if owner != nil {
		return *owner, nil
	}
	legacy := layout.LocateBatch(filepath.Base(batchDir), "")
	if target.same(legacy.Dir) && ownsBatchLocation(idx, layout, legacy) {
		return legacy, nil
	}
	return paths.BatchLocation{}, fmt.Errorf("batch location has no unambiguous identity: %s", batchDir)
}

// resolvedDir is a directory spelling with its symlink-resolved form computed
// once, so matching it against many candidates resolves it only once.
type resolvedDir struct {
	clean, real string
}

func resolveDir(dir string) resolvedDir {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = ""
	}
	return resolvedDir{clean: filepath.Clean(dir), real: real}
}

// same reports whether other names this directory. Cleaned equality suffices;
// otherwise both must resolve through symlinks to the same path.
func (d resolvedDir) same(other string) bool {
	if d.clean == filepath.Clean(other) {
		return true
	}
	if d.real == "" {
		return false
	}
	real, err := filepath.EvalSymlinks(other)
	return err == nil && real == d.real
}

// ownsBatchLocation reports whether a public Batch ID, never another Batch's
// physical alias, owns the location's directory.
func ownsBatchLocation(idx *batchindex.Index, layout paths.Layout, batch paths.BatchLocation) bool {
	if !safeWaitID(batch.ID) || !idx.MatchesBatchLocation(layout, batch.ID, batch.Dir) {
		return false
	}
	owner := idx.ResolveBatchIdentity(batch.ID)
	return owner == nil || owner.ID == batch.ID
}

func validateRunWait(batchDir, expectedBatchID, runID string, record RunWait) error {
	identityMismatch := expectedBatchID != "" && record.BatchID != expectedBatchID
	legacyInitialMismatch := expectedBatchID == "" && record.InitialAdmission && record.BatchID != filepath.Base(batchDir)
	if !safeWaitID(runID) || !safeWaitID(record.BatchID) || identityMismatch || legacyInitialMismatch || record.Protocol != "run-wait/v1" || record.RunID != runID || record.Issue <= 0 || record.BaseBranch == "" || record.OperationID == "" || record.LeaseExpiresAt.IsZero() || record.AdmissionMode < 0 || record.AdmissionMode > 2 {
		return fmt.Errorf("waiting ownership identity/timing is invalid")
	}
	clean := filepath.Clean(record.Branch)
	if record.Branch != "" && (filepath.IsAbs(record.Branch) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))) {
		return fmt.Errorf("waiting branch escapes worktree ownership")
	}
	if !record.InitialAdmission && record.Branch == "" {
		return fmt.Errorf("waiting continuation branch is invalid")
	}
	for _, issue := range record.Dependencies {
		if issue <= 0 || issue == record.Issue {
			return fmt.Errorf("waiting dependency identity is invalid")
		}
	}
	return nil
}

// waitRecovery classifies only event-backed unfinished intent. A live exclusive
// claim prevents stale recovery; ownerless leases use their fixed grace. Legacy
// suspensions without a snapshot get one grace anchored by their event timestamp.
func waitRecovery(baseDir, batchDir string, run events.RunState, now time.Time) (protected, suspended bool) {
	if run.IsTerminal() {
		return false, false
	}
	suspended = run.IsAwaiting() || run.IsCapacityQueued() || run.Status() == "queued"
	if !suspended {
		return false, false
	}
	claim, err := ClaimRun(baseDir, run.RunID)
	if errors.Is(err, ErrRunOwned) {
		return true, true
	}
	if err != nil {
		return true, true
	}
	defer claim.Close()
	return waitRecoveryScheduleAt(layoutForBaseDir(baseDir), paths.BatchLocation{ID: run.BatchID(), Dir: batchDir}, run, now)
}

// waitRecoverySchedule requires the caller's RunID claim so a lease refresh
// cannot race the decision to terminalize ownerless waiting intent.
func waitRecoverySchedule(baseDir string, run events.RunState, now time.Time) (protected, suspended bool) {
	layout := layoutForBaseDir(baseDir)
	batch, err := ResolveBatchLocation(layout, run.BatchID())
	if err != nil {
		return false, true
	}
	return waitRecoveryScheduleAt(layout, batch, run, now)
}

func waitRecoveryScheduleAt(layout paths.Layout, batch paths.BatchLocation, run events.RunState, now time.Time) (protected, suspended bool) {
	if run.IsTerminal() {
		return false, false
	}
	if !run.IsAwaiting() && !run.IsCapacityQueued() && run.Status() != "queued" {
		return false, false
	}
	record, err := ReadRunWaitAt(layout, batch, run.RunID)
	if err == nil {
		return record.RecoverableAt(now), true
	}
	if !os.IsNotExist(err) {
		return false, true
	}
	at := run.Started.Timestamp
	if run.IsCapacityQueued() && run.CapacityQueuedEvent != nil {
		at = run.CapacityQueuedEvent.Timestamp
	} else if run.IsAwaiting() && run.AwaitEvent != nil {
		at = run.AwaitEvent.Timestamp
	}
	return !at.IsZero() && now.Before(at.Add(RunRecoveryGrace)), true
}

// ReadRunWaitAt reads a lease from a resolved physical batch location while
// validating the record against the location's public BatchID, never the
// physical directory name. An empty ID is reserved for legacy Runs recorded
// without a batch: their started records are read without an identity check
// (no writer produces them any more), while their initial admissions keep
// the basename check.
func ReadRunWaitAt(layout paths.Layout, batch paths.BatchLocation, runID string) (RunWait, error) {
	var record RunWait
	if !safeWaitID(runID) || (batch.ID != "" && !safeWaitID(batch.ID)) {
		return record, fmt.Errorf("invalid waiting run identity")
	}
	data, err := os.ReadFile(waitPath(batch.Dir, runID))
	if os.IsNotExist(err) {
		var initial string
		if initial, err = initialWaitPath(layout, runID); err == nil {
			data, err = os.ReadFile(initial)
		}
	}
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	if err := validateRunWait(batch.Dir, batch.ID, runID, record); err != nil {
		return record, err
	}
	return normalizeQuotaWait(record), nil
}

// RenewRunWait may renew a live owner's lease, but never changes a continuing
// operation's fixed deadline. A newly selected operation has a different ID.
// The record must carry the public identity of the Batch it is written to.
func RenewRunWait(layout paths.Layout, batch paths.BatchLocation, record RunWait, now time.Time) error {
	if record.BatchID != batch.ID {
		return fmt.Errorf("waiting ownership identity is invalid")
	}
	record = normalizeQuotaWait(record)
	if old, err := ReadRunWaitAt(layout, batch, record.RunID); err == nil && old.OperationID == record.OperationID && !old.OperationDeadline.Equal(record.OperationDeadline) {
		return fmt.Errorf("waiting operation deadline cannot be renewed")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return writeRunWait(layout, batch.Dir, record, now)
}

// Callers must hold ClaimRun(sandmanDir, runID) throughout TransferRunWait.
// Both locations must be owned by their public Batch IDs in the index.
// Revalidate the source before atomically transferring schedule ownership to
// another batch.
func TransferRunWait(layout paths.Layout, from, to paths.BatchLocation, runID string, now time.Time) (RunWait, error) {
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return RunWait{}, err
	}
	if !ownsBatchLocation(idx, layout, from) {
		return RunWait{}, fmt.Errorf("waiting ownership identity is invalid")
	}
	record, err := ReadRunWaitAt(layout, from, runID)
	if err != nil {
		return record, err
	}
	if !record.RecoverableAt(now) {
		return record, fmt.Errorf("waiting recovery grace expired")
	}
	if !ownsBatchLocation(idx, layout, to) {
		return record, fmt.Errorf("batch location has no unambiguous identity: %s", to.Dir)
	}
	if err := writeRunWait(layout, from.Dir, record, now); err != nil {
		return record, err
	}
	record.BatchID = to.ID
	return record, writeRunWait(layout, to.Dir, record, now)
}

func writeRunWait(layout paths.Layout, batchDir string, record RunWait, now time.Time) error {
	record = normalizeQuotaWait(record)
	record.LeaseExpiresAt = now.Add(RunRecoveryGrace)
	if !record.OperationDeadline.IsZero() && record.OperationDeadline.Before(record.LeaseExpiresAt) {
		record.LeaseExpiresAt = record.OperationDeadline
	}
	if err := validateRunWait(batchDir, record.BatchID, record.RunID, record); err != nil {
		return err
	}
	initial, err := initialWaitPath(layout, record.RunID)
	if err != nil {
		return err
	}
	path := waitPath(batchDir, record.RunID)
	if record.InitialAdmission {
		path = initial
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := atomicfs.WriteAtomicJSON(path, record, 0o600); err != nil {
		return err
	}
	if !record.InitialAdmission {
		// A started schedule supersedes its initial admission sidecar. The
		// exclusive owner prevents a concurrent initial-admission takeover.
		if err := os.Remove(initial); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
