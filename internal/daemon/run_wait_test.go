package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
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
	layout := layoutForBaseDir(root)
	batch := layout.LocateBatch("batch", "")
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "ci:17:head", OperationDeadline: now.Add(3 * time.Minute)}
	if err := RenewRunWait(layout, batch, record, now); err != nil {
		t.Fatal(err)
	}
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
	saved, err := ReadRunWaitAt(layout, batch, "row")
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
		saved, err := ReadRunWaitAt(layoutForBaseDir(root), paths.BatchLocation{ID: "batch", Dir: batchDir}, "row")
		if err != nil || !saved.OperationDeadline.IsZero() || saved.OperationID != "quota:row" || !saved.LeaseExpiresAt.Equal(record.LeaseExpiresAt) {
			t.Fatalf("quota discovery renewed grace or retained hard estimate: saved=%+v error=%v", saved, err)
		}
	}
	if protected, _ := waitRecoverySchedule(root, states[0], record.LeaseExpiresAt); protected {
		t.Fatal("quota discovery renewed expired ownerless grace")
	}
}

func TestRunWait_ReadResolvedLocationUsesPublicBatchIdentity(t *testing.T) {
	root := t.TempDir()
	batchDir := filepath.Join(root, "relocated", "physical-name")
	now := time.Now().UTC()
	path := filepath.Join(batchDir, "runs", "row", "wait.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "public-batch", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "ci:17:head", LeaseExpiresAt: now.Add(time.Minute)}
	if err := atomicfs.WriteAtomicJSON(path, record, 0o600); err != nil {
		t.Fatal(err)
	}

	layout := layoutForBaseDir(filepath.Join(root, ".sandman"))
	got, err := ReadRunWaitAt(layout, paths.BatchLocation{ID: "public-batch", Dir: batchDir}, "row")
	if err != nil || got.BatchID != record.BatchID {
		t.Fatalf("resolved lease = %+v, error=%v", got, err)
	}
	if _, err := ReadRunWaitAt(layout, paths.BatchLocation{ID: "physical-name", Dir: batchDir}, "row"); err == nil {
		t.Fatal("resolved lease accepted the physical directory name as identity")
	}
	if _, err := ReadRunWaitAt(layout, paths.BatchLocation{ID: "other-batch", Dir: batchDir}, "row"); err == nil {
		t.Fatal("resolved lease accepted a mismatched public batch identity")
	}
}

func TestInitialWaitRejectsForeignBatchOwnership(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	now := time.Now().UTC()
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "owner", Issue: 42, BaseBranch: "main", InitialAdmission: true, OperationID: "admission"}
	if err := RenewRunWait(layout, layout.LocateBatch("owner", ""), record, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRunWaitAt(layout, layout.LocateBatch("foreign", ""), "row"); err == nil {
		t.Fatal("foreign batch self-authorized initial intent")
	}
	if _, err := TransferRunWait(layout, layout.LocateBatch("foreign", ""), layout.LocateBatch("new", ""), "row", now); err == nil {
		t.Fatal("foreign batch transferred initial intent")
	}
}

func TestRunWait_ReadsLeaseAtIndexedRelocatedLocation(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	physical := filepath.Join(layout.SandmanDir, "relocated", "frozen-location")
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{{
		ID: "public-batch", Path: physical, Status: batchindex.StatusActive,
	}}}).Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "public-batch", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "ci:17:head", OperationDeadline: now.Add(time.Hour)}
	batch := paths.BatchLocation{ID: "public-batch", Dir: physical}
	if err := RenewRunWait(layout, batch, record, now); err != nil {
		t.Fatalf("write relocated lease: %v", err)
	}
	read, err := ReadRunWaitAt(layout, batch, record.RunID)
	if err != nil || read.BatchID != record.BatchID || !read.RecoverableAt(now) {
		t.Fatalf("read relocated lease = %+v, error=%v", read, err)
	}
}

func TestRunWait_TransferPreservesIndexedDestinationIdentity(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	destination := filepath.Join(layout.SandmanDir, "relocated", "frozen-destination")
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{{
		ID: "public-destination", Path: destination, Status: batchindex.StatusActive,
	}}}).Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "capacity"}
	if err := RenewRunWait(layout, layout.LocateBatch("old", ""), record, now); err != nil {
		t.Fatal(err)
	}
	moved, err := TransferRunWait(layout, layout.LocateBatch("old", ""), paths.BatchLocation{ID: "public-destination", Dir: destination}, record.RunID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("transfer relocated lease: %v", err)
	}
	if moved.BatchID != "public-destination" {
		t.Fatalf("transferred BatchID = %q, want public-destination", moved.BatchID)
	}
}

func TestRunWait_TransferUsesAuthoritativeLayoutForNestedRelocation(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	source := filepath.Join(layout.SandmanDir, "relocated", "deep", "source-dir")
	destination := filepath.Join(layout.SandmanDir, "relocated", "deeper", "destination-dir")
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: "public-source", Path: source, Status: batchindex.StatusActive},
		{ID: "public-destination", Path: destination, Status: batchindex.StatusActive},
	}}).Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "public-source", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "capacity"}
	from := paths.BatchLocation{ID: "public-source", Dir: source}
	to := paths.BatchLocation{ID: "public-destination", Dir: destination}
	if err := RenewRunWait(layout, from, record, now); err != nil {
		t.Fatal(err)
	}

	moved, err := TransferRunWait(layout, from, to, record.RunID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("transfer nested relocated lease: %v", err)
	}
	if moved.BatchID != "public-destination" {
		t.Fatalf("transferred BatchID = %q, want public-destination", moved.BatchID)
	}
	if _, err := ReadRunWaitAt(layout, to, record.RunID); err != nil {
		t.Fatalf("read transferred nested lease: %v", err)
	}
}

func TestRunWait_TransferRejectsUnreadableIndex(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	if err := os.MkdirAll(layout.SandmanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.BatchesIndexPath, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	source := layout.LocateBatch("old", "")
	record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42,
		Branch: "42-fix", BaseBranch: "main", OperationID: "capacity"}
	if err := RenewRunWait(layout, source, record, now); err != nil {
		t.Fatal(err)
	}
	before, err := ReadRunWaitAt(layout, source, record.RunID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := TransferRunWait(layout, source, layout.LocateBatch("new", ""), record.RunID, now); err == nil {
		t.Fatal("transfer accepted an unreadable batches index")
	}
	unchanged, err := ReadRunWaitAt(layout, source, record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged.LeaseExpiresAt.Equal(before.LeaseExpiresAt) {
		t.Fatalf("failed transfer renewed source lease: %v", unchanged.LeaseExpiresAt)
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
			layout := paths.NewLayout(nil, root)
			oldDir, newDir := layout.LocateBatch("old", ""), layout.LocateBatch("new", "")
			record := RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "old", Issue: 42, Branch: "42-fix", BaseBranch: "main", InitialAdmission: initial, OperationID: "quota:fixed", OperationDeadline: now.Add(3 * time.Minute), NextPollAt: now.Add(2 * time.Minute), UsageLimitProbe: true}
			if err := RenewRunWait(layout, oldDir, record, now); err != nil {
				t.Fatal(err)
			}
			if _, err := TransferRunWait(layout, oldDir, newDir, "row", now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			moved, err := ReadRunWaitAt(layout, newDir, "row")
			if err != nil || moved.BatchID != "new" || moved.Ready || !moved.UsageLimitProbe || !moved.OperationDeadline.IsZero() || !moved.NextPollAt.Equal(record.NextPollAt) || moved.OperationID != "quota:row" || !moved.LeaseExpiresAt.Equal(now.Add(time.Minute+RunRecoveryGrace)) {
				t.Fatalf("handoff lost quota intent or retained a hard estimate: moved=%+v err=%v", moved, err)
			}
			if _, err := TransferRunWait(layout, newDir, oldDir, "row", moved.LeaseExpiresAt); err == nil {
				t.Fatal("expired recovery grace received another window")
			}
		})
	}
}

func TestRunWait_RenewalClassifiesInvalidOwnershipApartFromIOFailures(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	initial := func() RunWait {
		return RunWait{Protocol: "run-wait/v1", RunID: "row", BatchID: "batch", Issue: 42, Branch: "42-fix", BaseBranch: "main", InitialAdmission: true, OperationID: "admission"}
	}
	setup := func(t *testing.T) (paths.Layout, paths.BatchLocation) {
		layout := layoutForBaseDir(t.TempDir())
		batch := layout.LocateBatch("batch", "")
		if err := RenewRunWait(layout, batch, initial(), now); err != nil {
			t.Fatal(err)
		}
		return layout, batch
	}
	invalid := map[string]func(*testing.T, paths.Layout, paths.BatchLocation) error{
		"corrupt record": func(t *testing.T, layout paths.Layout, batch paths.BatchLocation) error {
			if err := os.WriteFile(filepath.Join(layout.StateDir, "waiting", "row.json"), []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
			return RenewRunWait(layout, batch, initial(), now)
		},
		"foreign batch": func(t *testing.T, layout paths.Layout, batch paths.BatchLocation) error {
			record := initial()
			record.BatchID = "other"
			return RenewRunWait(layout, batch, record, now)
		},
		"changed deadline": func(t *testing.T, layout paths.Layout, batch paths.BatchLocation) error {
			record := initial()
			record.OperationDeadline = now.Add(time.Hour)
			return RenewRunWait(layout, batch, record, now)
		},
		"invalid record": func(t *testing.T, layout paths.Layout, batch paths.BatchLocation) error {
			record := initial()
			record.BaseBranch = ""
			return RenewRunWait(layout, batch, record, now)
		},
	}
	for name, renew := range invalid {
		t.Run(name, func(t *testing.T) {
			layout, batch := setup(t)
			if err := renew(t, layout, batch); !errors.Is(err, ErrRunWaitInvalid) {
				t.Fatalf("renewal error = %v, want ErrRunWaitInvalid", err)
			}
		})
	}
	t.Run("unwritable state", func(t *testing.T) {
		layout, batch := setup(t)
		// A regular file in place of the directory fails the write with
		// ENOTDIR even for root, unlike a permission change.
		waiting := filepath.Join(layout.StateDir, "waiting")
		if err := os.RemoveAll(waiting); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(waiting, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := RenewRunWait(layout, batch, initial(), now)
		if err == nil || errors.Is(err, ErrRunWaitInvalid) {
			t.Fatalf("renewal error = %v, want a retryable I/O failure", err)
		}
	})
}
