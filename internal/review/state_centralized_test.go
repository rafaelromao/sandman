package review

import (
	"path/filepath"
	"testing"
	"time"
)

// recordingSeenCacheInvalidator captures SeenCacheInvalidator callbacks
// so centralized transition tests can assert the terminal-seen and
// forget contracts.
type recordingSeenCacheInvalidator struct {
	terminalSeenCalls []struct {
		prNumber  int
		commentID string
	}
	forgetCalls []struct {
		prNumber  int
		commentID string
	}
}

func (r *recordingSeenCacheInvalidator) MarkTerminalSeen(prNumber int, commentID string) {
	r.terminalSeenCalls = append(r.terminalSeenCalls, struct {
		prNumber  int
		commentID string
	}{prNumber, commentID})
}

func (r *recordingSeenCacheInvalidator) Forget(prNumber int, commentID string) {
	r.forgetCalls = append(r.forgetCalls, struct {
		prNumber  int
		commentID string
	}{prNumber, commentID})
}

// TestReviewStateStore_TryClaimTransitionSucceedsOnFirstAttempt pins
// the contract that a comment which has never been claimed or seen can
// be claimed successfully. This is the (unseen) → claimed transition.
func TestReviewStateStore_TryClaimTransitionSucceedsOnFirstAttempt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("new-comment") {
		t.Fatal("TryClaim should succeed on first attempt for unseen comment")
	}
	if !store.IsClaimed("new-comment") {
		t.Error("comment should be claimed after successful TryClaim")
	}
}

// TestReviewStateStore_TryClaimTransitionFailsOnReClaim pins the
// contract that a comment already claimed by another holder cannot be
// re-claimed. This is the claimed → claimed rejection.
func TestReviewStateStore_TryClaimTransitionFailsOnReClaim(t *testing.T) {
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

// TestReviewStateStore_TryClaimTransitionFailsOnTerminalSeen pins the
// contract that a comment with a terminal status (success, superseded)
// cannot be claimed. This is the terminal-seen → claimed rejection.
func TestReviewStateStore_TryClaimTransitionFailsOnTerminalSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("terminal-comment", "success"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if store.TryClaim("terminal-comment") {
		t.Error("TryClaim should return false for terminal-seen comment")
	}
}

// TestReviewStateStore_MarkSeenSuccessClearsRetryBudget pins the
// contract that MarkSeen("success") resets the attempt count to zero
// and clears the next-attempt gate. This is the failure → success
// transition that the retry path relies on.
func TestReviewStateStore_MarkSeenSuccessClearsRetryBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	stamp := time.Now().Add(time.Hour)
	if err := store.MarkSeenWithBudget("comment-1", "failure", 3, stamp); err != nil {
		t.Fatalf("MarkSeenWithBudget: %v", err)
	}
	if got := ReadFailureAttempts(store, "comment-1"); got != 3 {
		t.Fatalf("setup: ReadFailureAttempts = %d, want 3", got)
	}
	if got := ReadNextAttemptAt(store, "comment-1"); got.IsZero() {
		t.Fatal("setup: NextAttemptAt should not be zero")
	}

	if err := store.MarkSeen("comment-1", "success"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if got := ReadFailureAttempts(store, "comment-1"); got != 0 {
		t.Errorf("ReadFailureAttempts after success = %d, want 0", got)
	}
	if got := ReadNextAttemptAt(store, "comment-1"); !got.IsZero() {
		t.Errorf("NextAttemptAt after success = %v, want zero", got)
	}
}

// TestReviewStateStore_MarkSeenSuccessFiresMarkTerminalSeen pins the
// contract that MarkSeen("success") fires the SeenCacheInvalidator
// hook so the daemon's in-memory seen cache short-circuits subsequent
// ticks.
func TestReviewStateStore_MarkSeenSuccessFiresMarkTerminalSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	fake := &recordingSeenCacheInvalidator{}
	store, err := NewReviewStateStore(path, 42, fake)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("comment-1", "success"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if len(fake.terminalSeenCalls) != 1 {
		t.Fatalf("MarkTerminalSeen calls = %d, want 1", len(fake.terminalSeenCalls))
	}
	if fake.terminalSeenCalls[0].commentID != "comment-1" {
		t.Errorf("MarkTerminalSeen commentID = %q, want comment-1", fake.terminalSeenCalls[0].commentID)
	}
}

// TestReviewStateStore_MarkSeenFailureDoesNotFireMarkTerminalSeen pins
// the contract that MarkSeen("failure") does NOT fire the
// SeenCacheInvalidator hook — failure is retryable, not terminal.
func TestReviewStateStore_MarkSeenFailureDoesNotFireMarkTerminalSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	fake := &recordingSeenCacheInvalidator{}
	store, err := NewReviewStateStore(path, 42, fake)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("comment-1", "failure"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if len(fake.terminalSeenCalls) != 0 {
		t.Errorf("MarkTerminalSeen calls = %d, want 0 (failure is retryable)", len(fake.terminalSeenCalls))
	}
}

// TestReviewStateStore_MarkSeenPendingDoesNotFireMarkTerminalSeen pins
// the contract that MarkSeen("pending") does NOT fire the
// SeenCacheInvalidator hook — pending is retryable via rehydration.
func TestReviewStateStore_MarkSeenPendingDoesNotFireMarkTerminalSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	fake := &recordingSeenCacheInvalidator{}
	store, err := NewReviewStateStore(path, 42, fake)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("comment-1", "pending"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if len(fake.terminalSeenCalls) != 0 {
		t.Errorf("MarkTerminalSeen calls = %d, want 0 (pending is retryable)", len(fake.terminalSeenCalls))
	}
}

// TestReviewStateStore_MarkSeenSupersededFiresMarkTerminalSeen pins
// the contract that MarkSeen("superseded") fires the
// SeenCacheInvalidator hook — superseded is a terminal-seen status.
func TestReviewStateStore_MarkSeenSupersededFiresMarkTerminalSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	fake := &recordingSeenCacheInvalidator{}
	store, err := NewReviewStateStore(path, 42, fake)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("comment-1", "superseded"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if len(fake.terminalSeenCalls) != 1 {
		t.Fatalf("MarkTerminalSeen calls = %d, want 1", len(fake.terminalSeenCalls))
	}
}

// TestReviewStateStore_ReleaseDropsClaimAndFiresForget pins the
// contract that Release clears the claim and fires
// SeenCacheInvalidator.Forget so the daemon's per-process seen cache
// drops the comment, making it re-processable on the next tick.
func TestReviewStateStore_ReleaseDropsClaimAndFiresForget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	fake := &recordingSeenCacheInvalidator{}
	store, err := NewReviewStateStore(path, 42, fake)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("retry-me") {
		t.Fatal("TryClaim should succeed")
	}
	store.Release("retry-me")
	if store.IsClaimed("retry-me") {
		t.Fatal("Release should clear claimed state")
	}
	if len(fake.forgetCalls) != 1 {
		t.Fatalf("Forget calls = %d, want 1", len(fake.forgetCalls))
	}
	if fake.forgetCalls[0].commentID != "retry-me" {
		t.Errorf("Forget commentID = %q, want retry-me", fake.forgetCalls[0].commentID)
	}
	if !store.TryClaim("retry-me") {
		t.Fatal("TryClaim should succeed after Release")
	}
}

// TestReviewStateStore_MarkSeenWithBudgetRecordsRetryableFailure pins
// the contract that MarkSeenWithBudget("failure", attempts, stamp)
// records the attempt count and next-attempt gate without firing
// MarkTerminalSeen — failure is retryable and the gate prevents
// re-launch until the stamp elapses.
func TestReviewStateStore_MarkSeenWithBudgetRecordsRetryableFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	fake := &recordingSeenCacheInvalidator{}
	store, err := NewReviewStateStore(path, 42, fake)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	stamp := time.Now().Add(30 * time.Second)
	if err := store.MarkSeenWithBudget("comment-1", "failure", 2, stamp); err != nil {
		t.Fatalf("MarkSeenWithBudget: %v", err)
	}
	if got := ReadFailureAttempts(store, "comment-1"); got != 2 {
		t.Errorf("ReadFailureAttempts = %d, want 2", got)
	}
	if got := ReadNextAttemptAt(store, "comment-1"); got.IsZero() {
		t.Error("NextAttemptAt should not be zero after MarkSeenWithBudget")
	}
	if len(fake.terminalSeenCalls) != 0 {
		t.Errorf("MarkTerminalSeen calls = %d, want 0 (failure is retryable)", len(fake.terminalSeenCalls))
	}
}

// TestReviewStateStore_InvalidTransitionSuccessToFailure pins the
// contract that a terminal-seen comment (success) cannot be
// transitioned to a retryable status (failure). This is the
// centralized transition enforcement: once a review is published
// successfully, a later failure recording must not overwrite it.
func TestReviewStateStore_InvalidTransitionSuccessToFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("comment-1", "success"); err != nil {
		t.Fatalf("MarkSeen success: %v", err)
	}
	if store.IsSeen("comment-1") != true {
		t.Fatal("comment should be seen after success")
	}

	err = store.MarkSeen("comment-1", "failure")
	if err == nil {
		t.Fatal("expected error when transitioning terminal success to failure")
	}
}

// TestReviewStateStore_InvalidTransitionSupersededToSuccess pins the
// contract that a terminal-seen comment (superseded) cannot be
// transitioned to success. Superseded is terminal — a newer trigger
// replaced this one and it should not be resurrected.
func TestReviewStateStore_InvalidTransitionSupersededToSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("comment-1", "superseded"); err != nil {
		t.Fatalf("MarkSeen superseded: %v", err)
	}

	err = store.MarkSeen("comment-1", "success")
	if err == nil {
		t.Fatal("expected error when transitioning terminal superseded to success")
	}
}

// TestReviewStateStore_ValidTransitionFailureToSuccess pins the
// contract that a retryable comment (failure) CAN be transitioned to
// success. This is the retry-then-succeed path: the agent relaunches,
// produces a decision, and the daemon posts it successfully.
func TestReviewStateStore_ValidTransitionFailureToSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	stamp := time.Now().Add(10 * time.Second)
	if err := store.MarkSeenWithBudget("comment-1", "failure", 2, stamp); err != nil {
		t.Fatalf("MarkSeenWithBudget: %v", err)
	}

	if err := store.MarkSeen("comment-1", "success"); err != nil {
		t.Fatalf("expected failure→success transition to succeed: %v", err)
	}
	if got := ReadFailureAttempts(store, "comment-1"); got != 0 {
		t.Errorf("ReadFailureAttempts after success = %d, want 0 (budget cleared)", got)
	}
}

// TestReviewStateStore_ValidTransitionPendingToSuccess pins the
// contract that a retryable comment (pending) CAN be transitioned to
// success. This is the rehydrate path: the daemon restarts, finds
// decision.md on disk, re-posts it, and marks success.
func TestReviewStateStore_ValidTransitionPendingToSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("comment-1", "pending"); err != nil {
		t.Fatalf("MarkSeen pending: %v", err)
	}

	if err := store.MarkSeen("comment-1", "success"); err != nil {
		t.Fatalf("expected pending→success transition to succeed: %v", err)
	}
}

// TestReviewStateStore_RetryableFailurePersistsAcrossReload pins the
// contract that a retryable failure state (with attempts and
// next-attempt gate) survives a daemon restart simulated by closing
// the store and re-opening it from the same on-disk file.
func TestReviewStateStore_RetryableFailurePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")

	cs1, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore 1: %v", err)
	}
	stamp := time.Now().Add(30 * time.Second)
	if err := cs1.MarkSeenWithBudget("persistent", "failure", 3, stamp); err != nil {
		t.Fatalf("MarkSeenWithBudget: %v", err)
	}

	cs2, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore 2: %v", err)
	}
	if got := ReadFailureAttempts(cs2, "persistent"); got != 3 {
		t.Errorf("ReadFailureAttempts after reload = %d, want 3", got)
	}
	if got := ReadNextAttemptAt(cs2, "persistent"); got.IsZero() {
		t.Error("NextAttemptAt after reload should not be zero")
	}
}

// TestReviewStateStore_PendingEntryAllowsRehydration pins the contract
// that a "pending" status on disk (decision.md exists but post step
// did not complete) is retryable — the daemon can re-post the
// existing decision.md without re-launching the agent.
func TestReviewStateStore_PendingEntryAllowsRehydration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	store, err := NewReviewStateStore(path, 42, nil)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if err := store.MarkSeen("comment-1", "pending"); err != nil {
		t.Fatalf("MarkSeen pending: %v", err)
	}
	if !store.IsSeen("comment-1") {
		t.Fatal("pending comment should be seen")
	}

	// Rehydrate path: re-post and mark success
	if err := store.MarkSeen("comment-1", "success"); err != nil {
		t.Fatalf("rehydrate MarkSeen success: %v", err)
	}
	if got := ReadFailureAttempts(store, "comment-1"); got != 0 {
		t.Errorf("ReadFailureAttempts after rehydrate = %d, want 0", got)
	}
}

// TestReviewStateStore_CancellationReleasesClaimForRetry pins the
// contract that when a reviewer dies or is interrupted while holding a
// claim with a pending entry, the claim is released via Release() and
// the comment becomes re-claimable on the next tick. This is the
// cancellation-via-release path that the daemon's ctx-cancel handler
// uses.
func TestReviewStateStore_CancellationReleasesClaimForRetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-state.json")
	fake := &recordingSeenCacheInvalidator{}
	store, err := NewReviewStateStore(path, 42, fake)
	if err != nil {
		t.Fatalf("NewReviewStateStore: %v", err)
	}

	if !store.TryClaim("cancel-me") {
		t.Fatal("TryClaim should succeed")
	}
	if !store.IsClaimed("cancel-me") {
		t.Fatal("comment should be claimed")
	}

	// Simulate cancellation: release the claim without marking terminal
	store.Release("cancel-me")
	if store.IsClaimed("cancel-me") {
		t.Fatal("Release should clear claimed state")
	}
	if len(fake.forgetCalls) != 1 {
		t.Fatalf("Forget calls = %d, want 1", len(fake.forgetCalls))
	}

	// Verify re-claimable: the next tick's processPR can re-launch
	if !store.TryClaim("cancel-me") {
		t.Fatal("TryClaim should succeed after cancellation release")
	}
}
