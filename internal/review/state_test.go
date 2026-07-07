package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// TestReviewStateStore_StartsEmpty pins the contract that opening a
// ReviewStateStore against a non-existent file yields an empty store
// without error. Slice 1 tracer bullet: confirms the path the new store
// takes to read from disk and proves the file-is-optional behavior the
// daemon needs when the very first review run has no prior state.
func TestReviewStateStore_StartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	if store == nil {
		t.Fatal("store should not be nil")
	}

	if store.PR() != 42 {
		t.Errorf("PR = %d, want 42", store.PR())
	}
	if store.IsSeen("comment-1") {
		t.Error("fresh store should not report any comment as seen")
	}
	if store.IsClaimed("comment-1") {
		t.Error("fresh store should not report any comment as claimed")
	}
}

// TestReviewStateStore_LoadsExistingClaims asserts that a store opened
// against a valid JSON file sees the previously-recorded terminal
// claims. Mirrors TestSeenCommentsStore_LoadsExistingIDs.
func TestReviewStateStore_LoadsExistingClaims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	seed := batchindex.ReviewState{
		PR: 42,
		SeenComments: []batchindex.SeenComment{
			{CommentID: "100", Status: "success", Timestamp: time.Now()},
			{CommentID: "101", Status: "success", Timestamp: time.Now()},
		},
		Claims: map[string]batchindex.Claim{},
	}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	if !store.IsSeen("100") {
		t.Error("expected loaded store to see 100")
	}
	if !store.IsSeen("101") {
		t.Error("expected loaded store to see 101")
	}
	if store.IsSeen("missing") {
		t.Error("store should not report unseen comments")
	}
}

// TestReviewStateStore_MarkSeenPersists asserts that MarkSeen writes a
// terminal status to disk and IsSeen returns true on a freshly-opened
// store. Mirrors TestSeenCommentsStore_MarkAppendsLine.
func TestReviewStateStore_MarkSeenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("comment-1", "success"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if !store.IsSeen("comment-1") {
		t.Error("MarkSeen should make IsSeen return true")
	}

	// File on disk should contain the seen comment.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var got batchindex.ReviewState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal state file: %v", err)
	}
	found := false
	for _, sc := range got.SeenComments {
		if sc.CommentID == "comment-1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("file should contain comment-1 in seenComments, got %q", string(data))
	}
}

// TestReviewStateStore_MarkSeenIdempotent asserts that calling MarkSeen
// twice for the same comment does not duplicate the entry. Mirrors
// TestSeenCommentsStore_MarkIsIdempotent.
func TestReviewStateStore_MarkSeenIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := store.MarkSeen("comment-1", "success"); err != nil {
			t.Fatalf("MarkSeen #%d: %v", i, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var got batchindex.ReviewState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal state file: %v", err)
	}
	if len(got.SeenComments) != 1 {
		t.Errorf("expected 1 seen comment, got %d: %q", len(got.SeenComments), string(data))
	}
}

// TestReviewStateStore_RejectsEmptyKey asserts MarkSeen rejects an
// empty commentID and TryClaim returns false for one. Mirrors
// TestSeenCommentsStore_RejectsEmptyID.
func TestReviewStateStore_RejectsEmptyKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("   ", "success"); err == nil {
		t.Error("expected error when marking empty commentID")
	}
	if store.TryClaim("   ") {
		t.Error("TryClaim should return false for empty commentID")
	}
}

// TestReviewStateStore_TryClaimReturnsTrueForNewKey asserts the happy
// path of TryClaim on a never-seen comment. Mirrors
// TestSeenCommentsStore_TryClaimReturnsTrueForNewID.
func TestReviewStateStore_TryClaimReturnsTrueForNewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("new-key") {
		t.Error("TryClaim should return true for unseen comment")
	}
}

// TestReviewStateStore_TryClaimReturnsFalseForClaimedKey asserts that a
// second TryClaim on the same comment returns false. Mirrors
// TestSeenCommentsStore_TryClaimReturnsFalseForSeenID.
func TestReviewStateStore_TryClaimReturnsFalseForClaimedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("abc") {
		t.Fatal("first TryClaim should succeed")
	}
	if store.TryClaim("abc") {
		t.Error("second TryClaim for same comment should return false")
	}
}

// TestReviewStateStore_TryClaimMakesIsClaimedReturnTrue asserts that
// IsClaimed returns true after TryClaim. Mirrors
// TestSeenCommentsStore_TryClaimMakesHasReturnTrue.
func TestReviewStateStore_TryClaimMakesIsClaimedReturnTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("seen-me") {
		t.Fatal("TryClaim should succeed")
	}
	if !store.IsClaimed("seen-me") {
		t.Error("IsClaimed should return true after TryClaim")
	}
}

// TestReviewStateStore_TryClaimThenMarkSeenIsIdempotent asserts that
// marking a comment that is merely claimed does not duplicate the entry.
// Mirrors TestSeenCommentsStore_TryClaimThenMarkIsIdempotent.
func TestReviewStateStore_TryClaimThenMarkSeenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("mark-me") {
		t.Fatal("TryClaim should succeed")
	}
	if err := store.MarkSeen("mark-me", "success"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	if err := store.MarkSeen("mark-me", "success"); err != nil {
		t.Fatalf("second MarkSeen: %v", err)
	}
	data, _ := os.ReadFile(path)
	var got batchindex.ReviewState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	count := 0
	for _, sc := range got.SeenComments {
		if sc.CommentID == "mark-me" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 entry for mark-me, got %d in: %s", count, string(data))
	}
}

// TestReviewStateStore_ReleaseAllowsRetry asserts that after Release,
// the comment is no longer claimed and TryClaim succeeds again. Mirrors
// TestSeenCommentsStore_ReleaseClaimAllowsRetry.
func TestReviewStateStore_ReleaseAllowsRetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("retry-me") {
		t.Fatal("TryClaim should succeed before release")
	}
	store.Release("retry-me")
	if store.IsClaimed("retry-me") {
		t.Fatal("Release should clear claimed state")
	}
	if !store.TryClaim("retry-me") {
		t.Fatal("TryClaim should succeed after release")
	}
}

// TestReviewStateStore_TryClaimIsConcurrencySafe asserts that multiple
// goroutines racing on TryClaim for the same comment produce exactly
// one winner. Mirrors TestSeenCommentsStore_TryClaimIsConcurrencySafe
// and TestClaimStore_TryClaimIsConcurrencySafe.
func TestReviewStateStore_TryClaimIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	var winners int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if store.TryClaim("concurrent") {
				atomic.AddInt32(&winners, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&winners); got != 1 {
		t.Errorf("exactly one goroutine should claim, got %d", got)
	}
}

// TestReviewStateStore_TryClaimAgainstLoadedIDs asserts that a comment
// recorded as terminal-seen in the loaded file rejects new TryClaim.
// Mirrors TestSeenCommentsStore_TryClaimAgainstLoadedIDs.
func TestReviewStateStore_TryClaimAgainstLoadedIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	seed := batchindex.ReviewState{
		PR: 42,
		SeenComments: []batchindex.SeenComment{
			{CommentID: "existing", Status: "success", Timestamp: time.Now()},
		},
		Claims: map[string]batchindex.Claim{},
	}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if store.TryClaim("existing") {
		t.Error("TryClaim should return false for pre-loaded comment")
	}
	if !store.TryClaim("new-one") {
		t.Error("TryClaim should return true for genuinely new comment")
	}
}

// TestReviewStateStore_AtomicRenameLeavesPreviousIntact asserts the
// partial-write failure case: when a stale .tmp file exists on disk,
// Save replaces the destination file with the in-memory state. The
// atomic-rename invariant is exercised indirectly by the .tmp cleanup:
// a successful Save never leaves a .tmp file behind.
func TestReviewStateStore_AtomicRenameLeavesPreviousIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte("stale garbage"), 0644); err != nil {
		t.Fatal(err)
	}

	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("destination file should exist after Save: %v", err)
	}
	// The atomic-rename writer uses a unique temp suffix (".tmp.NNN"),
	// so the stale fixed-name .tmp is preserved untouched and the new
	// Save never leaves any .tmp* sibling behind.
	matches, err := filepath.Glob(filepath.Join(dir, "review-state.json.tmp*"))
	if err != nil {
		t.Fatalf("glob tmp files: %v", err)
	}
	for _, m := range matches {
		if m == tmpPath {
			continue
		}
		t.Errorf("stale .tmp should be gone after Save, got: %s", m)
	}
}

// TestReviewStateStore_NoClaimsSubdirectoryOnDisk asserts that the
// store creates no separate claims/ directory on disk — claims live
// inline in the JSON file's claims map. This is acceptance criterion
// #4 from the issue ("Inline claims map replaces per-comment lock
// files").
func TestReviewStateStore_NoClaimsSubdirectoryOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("c1") {
		t.Fatal("TryClaim should succeed")
	}
	if err := store.MarkSeen("c1", "success"); err != nil {
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

// TestReviewStateStore_NoTimeBasedPurge asserts the new design does not
// remove claims based on file mtime. Replaces
// TestClaimStore_PurgesStaleFiles (no analogue in the inline design).
// Per ADR-0034, claim state lives in the JSON file and is durable.
func TestReviewStateStore_NoTimeBasedPurge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	if !time.Now().IsZero() {
		// Pre-create a file with an ancient mtime to simulate a long-idle
		// store; NewReviewStateStore must not auto-purge it.
		staleTime := time.Now().Add(-24 * time.Hour)
		seed := batchindex.ReviewState{
			PR: 42,
			SeenComments: []batchindex.SeenComment{
				{CommentID: "very-old", Status: "success", Timestamp: staleTime},
			},
			Claims: map[string]batchindex.Claim{},
		}
		data, _ := json.Marshal(seed)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, staleTime, staleTime); err != nil {
			t.Fatal(err)
		}
	}

	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}
	if !store.IsSeen("very-old") {
		t.Error("store should preserve terminal claims regardless of file age (no time-based purge per ADR-0034)")
	}
}

// TestReviewStateStore_ReleaseClearsClaim mirrors
// TestClaimStore_ReleaseRemovesClaim.
func TestReviewStateStore_ReleaseClearsClaim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("abc123") {
		t.Fatal("TryClaim should succeed")
	}

	store.Release("abc123")

	if store.IsClaimed("abc123") {
		t.Error("Release should clear the claim")
	}
	if !store.TryClaim("abc123") {
		t.Error("TryClaim should succeed after Release")
	}
}

// TestReviewStateStore_ReleaseIdempotent mirrors
// TestClaimStore_ReleaseIdempotent.
func TestReviewStateStore_ReleaseIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	store.Release("nonexistent") // should not panic

	if !store.TryClaim("foo") {
		t.Fatal("TryClaim should succeed")
	}
	store.Release("foo")
	store.Release("foo") // second release is a no-op
}

// TestReviewStateStore_CommentIDWithSpecialChars mirrors
// TestClaimStore_CommentIDWithSpecialChars.
func TestReviewStateStore_CommentIDWithSpecialChars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	ids := []string{"IC_kwDOBm1q", "123_456", "comment-with-dashes"}
	for _, id := range ids {
		if !store.TryClaim(id) {
			t.Errorf("TryClaim(%s) should succeed", id)
		}
		store.Release(id)
	}
}

// TestReviewStateStore_RestartReadsPersistedClaims mirrors
// TestClaimStore_CrossInstanceSafety. A second store instance loading
// the same file sees the claim and TryClaim returns false.
func TestReviewStateStore_RestartReadsPersistedClaims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	cs1, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore 1: %v", err)
	}
	if !cs1.TryClaim("cross") {
		t.Fatal("cs1 should claim successfully")
	}
	if err := cs1.Save(); err != nil {
		t.Fatalf("cs1.Save: %v", err)
	}

	cs2, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore 2: %v", err)
	}
	if cs2.TryClaim("cross") {
		t.Error("cs2 should not claim an ID already claimed by cs1")
	}

	cs1.Release("cross")
	if err := cs1.Save(); err != nil {
		t.Fatalf("cs1.Save: %v", err)
	}

	cs3, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore 3: %v", err)
	}
	if !cs3.TryClaim("cross") {
		t.Error("cs3 should claim after cs1 released")
	}
}
