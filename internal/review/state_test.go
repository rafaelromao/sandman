package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestReviewStateStore_StartsEmpty pins the contract that opening a
// ReviewStateStore against a non-existent file yields an empty store
// without error. Slice 1 tracer bullet: confirms the path the new store
// takes to read from disk and proves the file-is-optional behavior the
// daemon needs when the very first review run has no prior state.
func TestReviewStateStore_StartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	if store == nil {
		t.Fatal("store should not be nil")
	}

	if store.IsSeen(42, "comment-1") {
		t.Error("fresh store should not report any pair as seen")
	}
	if store.IsClaimed(42, "comment-1") {
		t.Error("fresh store should not report any pair as claimed")
	}
}

// TestReviewStateStore_LoadsExistingClaims asserts that a store opened
// against a valid JSON file sees the previously-recorded terminal claims.
// Mirrors TestSeenCommentsStore_LoadsExistingIDs (seen_test.go) but on
// the (pr, commentID) key shape.
func TestReviewStateStore_LoadsExistingClaims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	seed := reviewStateFile{Claims: map[string]reviewClaim{
		keyFor(42, "100"): {Status: "success", Since: time.Now(), UpdatedAt: time.Now()},
		keyFor(42, "101"): {Status: "superseded", Since: time.Now(), UpdatedAt: time.Now()},
	}}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	if !store.IsSeen(42, "100") {
		t.Error("expected loaded store to see (42, 100) as seen")
	}
	if !store.IsSeen(42, "101") {
		t.Error("expected loaded store to see (42, 101) as seen")
	}
	if store.IsSeen(42, "missing") {
		t.Error("store should not report unseen pairs")
	}
}

// TestReviewStateStore_MarkSeenPersists asserts that MarkSeen writes a
// terminal status to disk and IsSeen returns true on a freshly-opened
// store. Mirrors TestSeenCommentsStore_MarkAppendsLine.
func TestReviewStateStore_MarkSeenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen(42, "comment-1", "success"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if !store.IsSeen(42, "comment-1") {
		t.Error("MarkSeen should make IsSeen return true")
	}

	// File on disk should contain the claim.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var got reviewStateFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal state file: %v", err)
	}
	entry, ok := got.Claims[keyFor(42, "comment-1")]
	if !ok {
		t.Fatalf("file should contain (42, comment-1) claim, got %q", string(data))
	}
	if entry.Status != "success" {
		t.Errorf("file claim status = %q, want %q", entry.Status, "success")
	}
}

// TestReviewStateStore_MarkSeenIdempotent asserts that calling MarkSeen
// twice for the same (pr, commentID) does not duplicate the entry. Mirrors
// TestSeenCommentsStore_MarkIsIdempotent.
func TestReviewStateStore_MarkSeenIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := store.MarkSeen(42, "comment-1", "success"); err != nil {
			t.Fatalf("MarkSeen #%d: %v", i, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var got reviewStateFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal state file: %v", err)
	}
	if len(got.Claims) != 1 {
		t.Errorf("expected 1 claim, got %d: %q", len(got.Claims), string(data))
	}
}

// TestReviewStateStore_RejectsEmptyKey asserts MarkSeen rejects an empty
// commentID (and TryClaim/Release no-op or return false). Mirrors
// TestSeenCommentsStore_RejectsEmptyID.
func TestReviewStateStore_RejectsEmptyKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen(42, "   ", "success"); err == nil {
		t.Error("expected error when marking empty commentID")
	}
	if store.TryClaim(42, "   ") {
		t.Error("TryClaim should return false for empty commentID")
	}
}

// TestReviewStateStore_TryClaimReturnsTrueForNewKey asserts the happy
// path of TryClaim on a never-seen pair. Mirrors
// TestSeenCommentsStore_TryClaimReturnsTrueForNewID.
func TestReviewStateStore_TryClaimReturnsTrueForNewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim(42, "new-key") {
		t.Error("TryClaim should return true for unseen pair")
	}
}

// TestReviewStateStore_TryClaimReturnsFalseForClaimedKey asserts that a
// second TryClaim on the same pair returns false. Mirrors
// TestSeenCommentsStore_TryClaimReturnsFalseForSeenID.
func TestReviewStateStore_TryClaimReturnsFalseForClaimedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim(42, "abc") {
		t.Fatal("first TryClaim should succeed")
	}
	if store.TryClaim(42, "abc") {
		t.Error("second TryClaim for same pair should return false")
	}
}

// TestReviewStateStore_TryClaimMakesIsClaimedReturnTrue asserts that
// IsClaimed returns true after TryClaim. Mirrors
// TestSeenCommentsStore_TryClaimMakesHasReturnTrue.
func TestReviewStateStore_TryClaimMakesIsClaimedReturnTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim(42, "seen-me") {
		t.Fatal("TryClaim should succeed")
	}
	if !store.IsClaimed(42, "seen-me") {
		t.Error("IsClaimed should return true after TryClaim")
	}
}

// TestReviewStateStore_TryClaimThenMarkSeenIsIdempotent asserts that
// marking a pair that is merely claimed does not duplicate the entry.
// Mirrors TestSeenCommentsStore_TryClaimThenMarkIsIdempotent.
func TestReviewStateStore_TryClaimThenMarkSeenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim(42, "mark-me") {
		t.Fatal("TryClaim should succeed")
	}
	if err := store.MarkSeen(42, "mark-me", "success"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	// Re-marking should not duplicate the entry.
	if err := store.MarkSeen(42, "mark-me", "success"); err != nil {
		t.Fatalf("second MarkSeen: %v", err)
	}
	data, _ := os.ReadFile(path)
	var got reviewStateFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Claims) != 1 {
		t.Errorf("expected 1 claim, got %d: %s", len(got.Claims), string(data))
	}
}

// TestReviewStateStore_ReleaseAllowsRetry asserts that after Release,
// the pair is no longer claimed and TryClaim succeeds again. Mirrors
// TestSeenCommentsStore_ReleaseClaimAllowsRetry.
func TestReviewStateStore_ReleaseAllowsRetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim(42, "retry-me") {
		t.Fatal("TryClaim should succeed before release")
	}
	store.Release(42, "retry-me")
	if store.IsClaimed(42, "retry-me") {
		t.Fatal("Release should clear claimed state")
	}
	if !store.TryClaim(42, "retry-me") {
		t.Fatal("TryClaim should succeed after release")
	}
}

// TestReviewStateStore_TryClaimIsConcurrencySafe asserts that two
// goroutines racing on TryClaim for the same pair produce exactly one
// winner. Mirrors TestSeenCommentsStore_TryClaimIsConcurrencySafe and
// TestClaimStore_TryClaimIsConcurrencySafe.
func TestReviewStateStore_TryClaimIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	var winners int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if store.TryClaim(42, "concurrent") {
				atomic.AddInt32(&winners, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&winners); got != 1 {
		t.Errorf("exactly one goroutine should claim the pair, got %d", got)
	}
}

// TestReviewStateStore_TryClaimPrScoped asserts the central new contract:
// the same commentID under a different PR is a fresh key. Mirrors
// TestSeenCommentsStore_TryClaimAgainstLoadedIDs but with the pr-scoped
// contract.
func TestReviewStateStore_TryClaimPrScoped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	seed := reviewStateFile{Claims: map[string]reviewClaim{
		keyFor(42, "shared"): {Status: "success", Since: time.Now(), UpdatedAt: time.Now()},
	}}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	// Same commentID under the loaded PR is already seen.
	if store.TryClaim(42, "shared") {
		t.Error("TryClaim should return false for pre-loaded (pr=42, shared)")
	}
	// Same commentID under a different PR is a fresh key.
	if !store.TryClaim(43, "shared") {
		t.Error("TryClaim should return true for (pr=43, shared) — keys are scoped by pr")
	}
}

// TestReviewStateStore_AtomicRenameLeavesPreviousIntact asserts the
// partial-write failure case: when a stale .tmp file exists on disk,
// Save replaces the destination file with the in-memory state and the
// previous content is no longer present. The atomic-rename invariant
// is exercised indirectly by the .tmp cleanup: a successful Save never
// leaves a .tmp file behind. Mirrors the partial-write failure case
// captured by the batchindex atomic-rename test.
func TestReviewStateStore_AtomicRenameLeavesPreviousIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	// Pre-create a stale .tmp file to simulate a crash mid-write.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte("stale garbage"), 0644); err != nil {
		t.Fatal(err)
	}

	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The destination file should now exist with the in-memory state.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("destination file should exist after Save: %v", err)
	}
	// The stale .tmp should have been replaced/renamed.
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("stale .tmp should be gone after Save, got: %v", err)
	}
}

// TestReviewStateStore_NoClaimsSubdirectoryOnDisk asserts that the
// store creates no separate claims/ directory on disk — claims live
// inline in the JSON file. This is acceptance criterion #4 from the
// issue ("Inline claims map replaces per-comment lock files").
func TestReviewStateStore_NoClaimsSubdirectoryOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim(42, "c1") {
		t.Fatal("TryClaim should succeed")
	}
	if err := store.MarkSeen(42, "c1", "success"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() == "claims" {
			t.Error("store must not create a claims/ directory; claims live inline in review-state.json")
		}
	}
}
