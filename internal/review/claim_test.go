package review

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaimStore_NewClaimStoreCreatesClaimsDir(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	defer cs.Close()

	claimsDir := filepath.Join(dir, "claims")
	info, err := os.Stat(claimsDir)
	if err != nil {
		t.Fatalf("claims dir should exist: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("claims path should be a directory, got mode %v", info.Mode())
	}
}

func TestClaimStore_TryClaimSucceedsForUnclaimed(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	defer cs.Close()

	claimed, err := cs.TryClaim("abc123")
	if err != nil {
		t.Fatalf("TryClaim: %v", err)
	}
	if !claimed {
		t.Error("TryClaim should succeed for unclaimed ID")
	}

	lockFile := filepath.Join(dir, "claims", "abc123")
	if _, err := os.Stat(lockFile); os.IsNotExist(err) {
		t.Error("lock file should exist after successful claim")
	}
}

func TestClaimStore_TryClaimFailsForClaimed(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	defer cs.Close()

	_, err = cs.TryClaim("abc123")
	if err != nil {
		t.Fatalf("first TryClaim: %v", err)
	}

	claimed, err := cs.TryClaim("abc123")
	if err != nil {
		t.Fatalf("second TryClaim: %v", err)
	}
	if claimed {
		t.Error("TryClaim should return false for already-claimed ID")
	}
}

func TestClaimStore_ReleaseRemovesClaim(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	defer cs.Close()

	_, err = cs.TryClaim("abc123")
	if err != nil {
		t.Fatalf("TryClaim: %v", err)
	}

	if err := cs.Release("abc123"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	lockFile := filepath.Join(dir, "claims", "abc123")
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("lock file should be removed after release")
	}

	claimed, err := cs.TryClaim("abc123")
	if err != nil {
		t.Fatalf("TryClaim after release: %v", err)
	}
	if !claimed {
		t.Error("TryClaim should succeed after Release")
	}
}

func TestClaimStore_CloseReleasesAll(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}

	ids := []string{"a", "b", "c"}
	for _, id := range ids {
		if _, err := cs.TryClaim(id); err != nil {
			t.Fatalf("TryClaim(%s): %v", id, err)
		}
	}

	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, id := range ids {
		lockFile := filepath.Join(dir, "claims", id)
		if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
			t.Errorf("lock file %s should be removed after Close", id)
		}
	}
}

func TestClaimStore_PurgesStaleFiles(t *testing.T) {
	dir := t.TempDir()

	claimsDir := filepath.Join(dir, "claims")
	if err := os.MkdirAll(claimsDir, 0755); err != nil {
		t.Fatal(err)
	}

	staleFile := filepath.Join(claimsDir, "stale")
	freshFile := filepath.Join(claimsDir, "fresh")

	for _, f := range []string{staleFile, freshFile} {
		if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleFile, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	cs, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	defer cs.Close()

	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Error("stale claim file should be purged")
	}
	if _, err := os.Stat(freshFile); os.IsNotExist(err) {
		t.Error("fresh claim file should not be purged")
	}
}

func TestClaimStore_TryClaimIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	defer cs.Close()

	var wg sync.WaitGroup
	winners := make(chan int, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := cs.TryClaim("same-id")
			if err == nil && claimed {
				winners <- 1
			}
		}()
	}
	wg.Wait()
	close(winners)

	count := 0
	for range winners {
		count++
	}
	if count != 1 {
		t.Errorf("exactly one goroutine should claim the ID, got %d", count)
	}
}

func TestClaimStore_ReleaseIdempotent(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	defer cs.Close()

	if err := cs.Release("nonexistent"); err != nil {
		t.Errorf("Release of unclaimed ID should not error: %v", err)
	}

	_, err = cs.TryClaim("foo")
	if err != nil {
		t.Fatalf("TryClaim: %v", err)
	}
	if err := cs.Release("foo"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := cs.Release("foo"); err != nil {
		t.Errorf("Release of already-released ID should not error: %v", err)
	}
}

func TestClaimStore_CommentIDWithSpecialChars(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	defer cs.Close()

	ids := []string{"IC_kwDOBm1q", "123_456", "comment-with-dashes"}
	for _, id := range ids {
		claimed, err := cs.TryClaim(id)
		if err != nil {
			t.Fatalf("TryClaim(%s): %v", id, err)
		}
		if !claimed {
			t.Errorf("TryClaim(%s) should succeed", id)
		}

		if err := cs.Release(id); err != nil {
			t.Fatalf("Release(%s): %v", id, err)
		}
	}
}

// Test that a claim file created by one ClaimStore prevents a second
// ClaimStore from claiming the same ID (cross-instance simulation).
func TestClaimStore_CrossInstanceSafety(t *testing.T) {
	dir := t.TempDir()

	cs1, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore 1: %v", err)
	}
	claimed, err := cs1.TryClaim("cross")
	if err != nil {
		t.Fatalf("cs1.TryClaim: %v", err)
	}
	if !claimed {
		t.Fatal("cs1 should claim successfully")
	}

	cs2, err := NewClaimStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore 2: %v", err)
	}
	defer cs2.Close()

	claimed, err = cs2.TryClaim("cross")
	if err != nil {
		t.Fatalf("cs2.TryClaim: %v", err)
	}
	if claimed {
		t.Error("cs2 should not claim an ID already claimed by cs1")
	}

	cs1.Release("cross")
	cs1.Close()

	claimed, err = cs2.TryClaim("cross")
	if err != nil {
		t.Fatalf("cs2.TryClaim after release: %v", err)
	}
	if !claimed {
		t.Error("cs2 should claim after cs1 released")
	}
}

func TestClaimStore_NewClaimStoreErrorsOnInvalidPrDir(t *testing.T) {
	_, err := NewClaimStore(filepath.Join(t.TempDir(), "nonexistent"), time.Hour)
	if err != nil {
		t.Fatalf("NewClaimStore should succeed even when prDir doesn't exist (MkdirAll creates it): %v", err)
	}

	// Create a file where a directory should be.
	dir := t.TempDir()
	blockingFile := filepath.Join(dir, "claims")
	if err := os.WriteFile(blockingFile, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = NewClaimStore(dir, time.Hour)
	if err == nil {
		t.Fatal("expected error when claims path is a file, not a directory")
	}
	if !strings.Contains(err.Error(), "claims") {
		t.Errorf("error should mention claims, got: %v", err)
	}
}
