package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestRunWait_ClaimAndFixedRecoveryWindow(t *testing.T) {
	root := t.TempDir()
	claim, err := ClaimRun(root, "row")
	if err != nil {
		t.Fatal(err)
	}
	if second, err := ClaimRun(root, "row"); !errors.Is(err, ErrRunOwned) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("concurrent claim=%v, want owned", err)
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	batchDir := filepath.Join(root, "batches", "batch")
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "ci:17:head", OperationDeadline: now.Add(3 * time.Minute)}
	if err := RenewRunWait(batchDir, record, now); err != nil {
		t.Fatal(err)
	}
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
	saved, err := ReadRunWait(batchDir, "row")
	if err != nil || !saved.LeaseExpiresAt.Equal(record.OperationDeadline) {
		t.Fatalf("lease extended operation: record=%#v error=%v", saved, err)
	}
	if !saved.RecoverableAt(now.Add(2*time.Minute)) || saved.RecoverableAt(now.Add(3*time.Minute)) {
		t.Fatal("recovery did not respect capped fixed deadline")
	}
	claim, err = ClaimRun(root, "row")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
}

func TestRunWait_LegacyQuotaEstimateCannotShortenExistingRecoveryGrace(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	batchDir := filepath.Join(root, "batches", "batch")
	path := filepath.Join(batchDir, "runs", "row", "wait.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: "42-fix", BaseBranch: "main", UsageLimitProbe: true, OperationID: "quota:old-estimate", OperationDeadline: now.Add(-time.Minute), LeaseExpiresAt: now.Add(4 * time.Minute)}
	if err := atomicfs.WriteAtomicJSON(path, record, 0o600); err != nil {
		t.Fatal(err)
	}
	states := events.ProjectRunStates([]events.Event{
		{Type: "run.started", RunID: "row", Issue: 42, Timestamp: now.Add(-2 * time.Minute), Payload: map[string]any{"batch_id": "batch", "branch": "42-fix"}},
		{Type: "run.await", RunID: "row", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"await_reason": "usage-limit", "usage_limit_waited_seconds": 600}},
	})
	claim, err := ClaimRun(root, "row")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	for _, at := range []time.Time{now, now.Add(2 * time.Minute)} {
		protected, suspended := waitRecoverySchedule(root, states[0], at)
		if !protected || !suspended {
			t.Fatalf("expired diagnostic estimate vetoed recoverable quota work at %v", at)
		}
		saved, err := ReadRunWait(batchDir, "row")
		if err != nil || !saved.OperationDeadline.IsZero() || saved.OperationID != "quota:row" || !saved.LeaseExpiresAt.Equal(record.LeaseExpiresAt) {
			t.Fatalf("quota discovery renewed grace or retained hard estimate: saved=%+v error=%v", saved, err)
		}
	}
	if protected, _ := waitRecoverySchedule(root, states[0], record.LeaseExpiresAt); protected {
		t.Fatal("quota discovery renewed expired ownerless grace")
	}
}

func TestInitialWaitRejectsForeignBatchOwnership(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "owner", Issue: 42, BaseBranch: "main", InitialAdmission: true, OperationID: "admission"}
	if err := RenewRunWait(filepath.Join(root, "batches", "owner"), record, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRunWait(filepath.Join(root, "batches", "foreign"), "row"); err == nil {
		t.Fatal("foreign batch self-authorized initial intent")
	}
	if _, err := TransferRunWait(filepath.Join(root, "batches", "foreign"), filepath.Join(root, "batches", "new"), "row", now); err == nil {
		t.Fatal("foreign batch transferred initial intent")
	}
}

func TestRunWait_BatchHandoffPreservesQuotaScheduleAndGrace(t *testing.T) {
	for _, initial := range []bool{true, false} {
		t.Run(map[bool]string{true: "initial", false: "started"}[initial], func(t *testing.T) {
			root := t.TempDir()
			claim, err := ClaimRun(root, "row")
			if err != nil {
				t.Fatal(err)
			}
			defer claim.Close()
			now := time.Now().UTC()
			oldDir, newDir := filepath.Join(root, "batches", "old"), filepath.Join(root, "batches", "new")
			record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42, Branch: "42-fix", BaseBranch: "main", InitialAdmission: initial, OperationID: "quota:fixed", OperationDeadline: now.Add(3 * time.Minute), NextPollAt: now.Add(2 * time.Minute), UsageLimitProbe: true}
			if err := RenewRunWait(oldDir, record, now); err != nil {
				t.Fatal(err)
			}
			if _, err := TransferRunWait(oldDir, newDir, "row", now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			moved, err := ReadRunWait(newDir, "row")
			if err != nil || moved.BatchID != "new" || moved.Ready || !moved.UsageLimitProbe || !moved.OperationDeadline.IsZero() || !moved.NextPollAt.Equal(record.NextPollAt) || moved.OperationID != "quota:row" || !moved.LeaseExpiresAt.Equal(now.Add(time.Minute+RunRecoveryGrace)) {
				t.Fatalf("handoff lost quota intent or retained a hard estimate: moved=%+v err=%v", moved, err)
			}
			if _, err := TransferRunWait(newDir, oldDir, "row", moved.LeaseExpiresAt); err == nil {
				t.Fatal("expired recovery grace received another window")
			}
		})
	}
}
