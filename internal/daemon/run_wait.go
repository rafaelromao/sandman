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
	"github.com/rafaelromao/sandman/internal/events"
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
	return filepath.Join(batchDir, "runs", runID, "wait.json")
}

func initialWaitPath(batchDir, runID string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(batchDir)), "state", "waiting", runID+".json")
}

func validateRunWait(batchDir, runID string, record RunWait) error {
	if !safeWaitID(runID) || !safeWaitID(record.BatchID) || record.Protocol != "run-wait/v1" || record.RunID != runID || record.BatchID != filepath.Base(batchDir) || record.Issue <= 0 || record.BaseBranch == "" || record.OperationID == "" || record.LeaseExpiresAt.IsZero() || record.AdmissionMode < 0 || record.AdmissionMode > 2 {
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
func waitRecovery(baseDir string, run events.RunState, now time.Time) (protected, suspended bool) {
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
	return waitRecoverySchedule(baseDir, run, now)
}

// waitRecoverySchedule requires the caller's RunID claim so a lease refresh
// cannot race the decision to terminalize ownerless waiting intent.
func waitRecoverySchedule(baseDir string, run events.RunState, now time.Time) (protected, suspended bool) {
	if run.IsTerminal() {
		return false, false
	}
	if !run.IsAwaiting() && !run.IsCapacityQueued() && run.Status() != "queued" {
		return false, false
	}
	batchID := run.BatchID()
	record, err := ReadRunWait(filepath.Join(baseDir, "batches", batchID), run.RunID)
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

func ReadRunWait(batchDir, runID string) (RunWait, error) {
	var record RunWait
	if !safeWaitID(runID) {
		return record, fmt.Errorf("invalid waiting run identity")
	}
	data, err := os.ReadFile(waitPath(batchDir, runID))
	if os.IsNotExist(err) {
		data, err = os.ReadFile(initialWaitPath(batchDir, runID))
	}
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	if err := validateRunWait(batchDir, runID, record); err != nil {
		return record, err
	}
	return normalizeQuotaWait(record), nil
}

// RenewRunWait may renew a live owner's lease, but never changes a continuing
// operation's fixed deadline. A newly selected operation has a different ID.
func RenewRunWait(batchDir string, record RunWait, now time.Time) error {
	record = normalizeQuotaWait(record)
	if old, err := ReadRunWait(batchDir, record.RunID); err == nil && old.OperationID == record.OperationID && !old.OperationDeadline.Equal(record.OperationDeadline) {
		return fmt.Errorf("waiting operation deadline cannot be renewed")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return writeRunWait(batchDir, record, now)
}

// Callers must hold ClaimRun(sandmanDir, runID) throughout TransferRunWait.
// Revalidate the
// source before atomically transferring schedule ownership to another batch.
func TransferRunWait(oldBatchDir, newBatchDir, runID string, now time.Time) (RunWait, error) {
	record, err := ReadRunWait(oldBatchDir, runID)
	if err != nil {
		return record, err
	}
	if !record.RecoverableAt(now) {
		return record, fmt.Errorf("waiting recovery grace expired")
	}
	if err := writeRunWait(oldBatchDir, record, now); err != nil {
		return record, err
	}
	record.BatchID = filepath.Base(newBatchDir)
	return record, writeRunWait(newBatchDir, record, now)
}

func writeRunWait(batchDir string, record RunWait, now time.Time) error {
	record = normalizeQuotaWait(record)
	record.LeaseExpiresAt = now.Add(RunRecoveryGrace)
	if !record.OperationDeadline.IsZero() && record.OperationDeadline.Before(record.LeaseExpiresAt) {
		record.LeaseExpiresAt = record.OperationDeadline
	}
	if err := validateRunWait(batchDir, record.RunID, record); err != nil {
		return err
	}
	path := waitPath(batchDir, record.RunID)
	if record.InitialAdmission {
		path = initialWaitPath(batchDir, record.RunID)
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
		if err := os.Remove(initialWaitPath(batchDir, record.RunID)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
