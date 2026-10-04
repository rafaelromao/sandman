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
