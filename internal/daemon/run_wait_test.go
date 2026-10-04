package daemon

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
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

func TestRunWait_BatchHandoffPreservesOperationAndSchedule(t *testing.T) {
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
			if err != nil || moved.BatchID != "new" || moved.Ready || !moved.UsageLimitProbe || !moved.OperationDeadline.Equal(record.OperationDeadline) || !moved.NextPollAt.Equal(record.NextPollAt) || moved.OperationID != record.OperationID || !moved.LeaseExpiresAt.Equal(record.OperationDeadline) {
				t.Fatalf("handoff changed fixed intent: moved=%+v err=%v", moved, err)
			}
			if _, err := TransferRunWait(newDir, oldDir, "row", record.OperationDeadline); err == nil {
				t.Fatal("expired operation received another recovery window")
			}
		})
	}
}
