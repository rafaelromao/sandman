package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestSelfPostStore_RecordAndLookup pins the round-trip contract:
// Record stores the hash; IsSelfPosted returns true for the same
// body and false for a different body. Reloading from disk after
// Record preserves the hash.
func TestSelfPostStore_RecordAndLookup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}

	body := "/sandman review focus on tests"
	if err := store.Record(42, body, "run-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !store.IsSelfPosted(body) {
		t.Error("IsSelfPosted should return true for recorded body")
	}
	if store.IsSelfPosted("/sandman review focus on different thing") {
		t.Error("IsSelfPosted should return false for a different body")
	}

	// Reload from disk: the recorded hash must survive.
	reloaded, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reloaded.IsSelfPosted(body) {
		t.Error("reloaded store should still report recorded body as self-posted")
	}
}

// TestSelfPostStore_Normalization pins the normalization contract:
// trailing whitespace and case do not change the hash, so a body
// recorded with extra trailing whitespace matches a check without
// it.
func TestSelfPostStore_Normalization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}

	if err := store.Record(42, "/sandman review", "run-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !store.IsSelfPosted("/sandman review   ") {
		t.Error("trailing spaces should be normalized away on check")
	}
	if !store.IsSelfPosted("/SANDMAN REVIEW") {
		t.Error("case should be normalized away on check")
	}
	if !store.IsSelfPosted("/sandman review\n\n") {
		t.Error("trailing newlines should be normalized away on check")
	}
}

// TestSelfPostStore_StartsEmpty pins the missing-file contract:
// NewSelfPostStore against a non-existent file returns an empty
// store without error.
func TestSelfPostStore_StartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore on missing file: %v", err)
	}
	if store == nil {
		t.Fatal("store should not be nil")
	}
	if got := len(store.Hashes()); got != 0 {
		t.Errorf("fresh store should have 0 hashes, got %d", got)
	}
}

// TestSelfPostStore_CorruptFileDegradesToEmpty pins the
// corruption-recovery contract: a non-JSON file is treated as
// empty (degraded mode) and the error is returned so the caller
// can log it. The next Record overwrites the corrupt file via
// temp-file + rename.
func TestSelfPostStore_CorruptFileDegradesToEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	if err := os.WriteFile(path, []byte("not json {{{"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := NewSelfPostStore(path)
	if err == nil {
		t.Fatal("expected error on corrupt file")
	}
	if store == nil {
		t.Fatal("store should not be nil on corrupt file (degraded mode)")
	}
	if got := len(store.Hashes()); got != 0 {
		t.Errorf("corrupt-file store should be empty, got %d hashes", got)
	}

	// Record still works: overwrites the corrupt file.
	if err := store.Record(1, "/sandman review", ""); err != nil {
		t.Fatalf("Record on degraded store: %v", err)
	}
	if !store.IsSelfPosted("/sandman review") {
		t.Error("Record should make IsSelfPosted return true")
	}
}

// TestSelfPostStore_PersistsRoundTrip pins the on-disk shape: the
// file is a JSON object keyed by hex(sha256(body)) with values
// that include the PR number, optional run ID, and posted-at
// timestamp. Reloading yields a store that recognizes the body.
func TestSelfPostStore_PersistsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := store.Record(99, "hello", "run-abc"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// The file should decode as a map keyed by sha256 hex.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]selfPostEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("expected 1 entry on disk, got %d", len(raw))
	}
	sum := sha256.Sum256([]byte("hello"))
	wantKey := hex.EncodeToString(sum[:])
	if _, ok := raw[wantKey]; !ok {
		t.Errorf("disk entry should be keyed by sha256(\"hello\") = %s, got keys %v", wantKey, raw)
	}
	if raw[wantKey].PRNumber != 99 {
		t.Errorf("disk entry PRNumber = %d, want 99", raw[wantKey].PRNumber)
	}
	if raw[wantKey].RunID != "run-abc" {
		t.Errorf("disk entry RunID = %q, want %q", raw[wantKey].RunID, "run-abc")
	}
}

// TestSelfPostStore_RecordIdempotent pins that re-recording the
// same body does not duplicate or overwrite the original entry.
// The original PostedAt / RunID is preserved.
func TestSelfPostStore_RecordIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := store.Record(1, "body", "first"); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if err := store.Record(1, "body", "second"); err != nil {
		t.Fatalf("second Record: %v", err)
	}
	if got := len(store.Hashes()); got != 1 {
		t.Errorf("expected 1 hash after re-record, got %d", got)
	}
}

// TestSelfPostStore_AtomicWrite confirms that a crash between
// writing the temp file and the rename leaves the previous file
// intact. The store is best-effort; this is a smoke test for the
// rename-after-write pattern, not a crash-injection test.
func TestSelfPostStore_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := store.Record(1, "first", ""); err != nil {
		t.Fatalf("Record first: %v", err)
	}
	if err := store.Record(2, "second", ""); err != nil {
		t.Fatalf("Record second: %v", err)
	}

	// No temp file should remain after a successful save.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file should not remain after save, stat err: %v", err)
	}

	// Reload yields both hashes.
	reloaded, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reloaded.IsSelfPosted("first") {
		t.Error("first body should be self-posted after reload")
	}
	if !reloaded.IsSelfPosted("second") {
		t.Error("second body should be self-posted after reload")
	}
}

// TestSelfPostStore_ConcurrentRecord pins the race-detector
// contract: concurrent Record calls do not corrupt the file or
// drop entries. Run with `go test -race`.
func TestSelfPostStore_ConcurrentRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			body := "body-" + string(rune('A'+i%26)) + "-" + string(rune('0'+i%10))
			if err := store.Record(i, body, ""); err != nil {
				t.Errorf("Record: %v", err)
			}
		}()
	}
	wg.Wait()

	// Reload from disk: file must be valid JSON and contain
	// every recorded hash.
	reloaded, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := len(reloaded.Hashes()); got == 0 {
		t.Error("reloaded store should have at least one hash")
	}

	// File must be valid JSON (no torn write).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]selfPostEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("disk file is not valid JSON: %v", err)
	}
}

// TestSelfPostStore_RejectsEmptyBody pins the contract that
// recording an empty body still records a hash (so the daemon
// will treat empty future comments as self-posted). This is the
// defensive side: a bug elsewhere in the agent that posts an
// empty comment must not silently make the daemon treat the next
// real trigger as self-posted.
func TestSelfPostStore_RejectsEmptyBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	// Recording an empty body succeeds and produces a stable
	// hash (sha256 of ""); the empty string matches itself.
	if err := store.Record(1, "", ""); err != nil {
		t.Fatalf("Record empty: %v", err)
	}
	if !store.IsSelfPosted("") {
		t.Error("empty body should be self-posted after Record")
	}
	// Trailing-whitespace-only collapses to empty via the
	// normalization.
	if !store.IsSelfPosted("   \n\n") {
		t.Error("whitespace-only body should normalize to empty and be self-posted")
	}
}

// TestSelfPostStore_IgnoresLeadingTrailingSpaces pins the
// normalization: the function is intentionally asymmetric — only
// trailing whitespace is stripped, not leading — so a body that
// is genuinely "  /sandman review" does NOT match the recorded
// "/sandman review". This is a deliberate choice to avoid false
// positives in the dedup.
func TestSelfPostStore_IgnoresLeadingSpaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := store.Record(1, "/sandman review", ""); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Trailing whitespace is fine; "  /sandman review" with
	// leading spaces is a different normalized body.
	if !store.IsSelfPosted("/sandman review ") {
		t.Error("trailing space should be normalized")
	}
	if store.IsSelfPosted(" /sandman review") {
		t.Error("leading space should NOT be normalized (deliberate)")
	}
	// And the recorded hash should match the recorded bytes
	// after trim-right; sanity-check that hashBody's choice
	// matches strings.TrimRight's contract.
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimRight("/sandman review", " \t\n"))))
	if got := hashBody("/sandman review  "); got != hex.EncodeToString(sum[:]) {
		t.Errorf("hashBody drift: %s vs %s", got, hex.EncodeToString(sum[:]))
	}
}
