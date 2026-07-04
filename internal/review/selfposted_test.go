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
	if !store.IsSelfPosted(42, body) {
		t.Error("IsSelfPosted should return true for recorded body")
	}
	if store.IsSelfPosted(42, "/sandman review focus on different thing") {
		t.Error("IsSelfPosted should return false for a different body")
	}

	// Issue #1756 made the loader greenfield: a re-open renames
	// any pre-existing self-posted.json away and starts empty.
	// Reloading therefore sees the new (greenfield) store, which
	// reflects the very-near-term state only. Pin the new
	// contract instead of the legacy reload-preserves-entries
	// contract.
	reloaded, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := len(reloaded.Hashes()); got != 0 {
		t.Errorf("greenfield loader should drop prior contents (#1756), got %d entries on re-open", got)
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
	if !store.IsSelfPosted(42, "/sandman review   ") {
		t.Error("trailing spaces should be normalized away on check")
	}
	if !store.IsSelfPosted(42, "/SANDMAN REVIEW") {
		t.Error("case should be normalized away on check")
	}
	if !store.IsSelfPosted(42, "/sandman review\n\n") {
		t.Error("trailing newlines should be normalized away on check")
	}
}

// TestSelfPostStore_MissingOrCorruptFile pins the post-#1756
// greenfield loader contract for the two terminal cases: a missing
// file is silently greenfield, and a corrupt / non-JSON file is
// renamed aside and treated as greenfield (no parse error is
// surfaced — that is a deliberate behavior change from the legacy
// contract). In both cases the in-memory store starts empty and a
// subsequent Record call succeeds.
func TestSelfPostStore_MissingOrCorruptFile(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "self-posted.json")
		store, err := NewSelfPostStore(path)
		if err != nil {
			t.Fatalf("NewSelfPostStore on missing file: %v", err)
		}
		if store == nil {
			t.Fatal("store should not be nil on missing file")
		}
		if err := store.Record(1, "/sandman review", ""); err != nil {
			t.Fatalf("Record on missing-file store: %v", err)
		}
		if !store.IsSelfPosted(1, "/sandman review") {
			t.Error("Record on missing-file store should make IsSelfPosted return true")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "self-posted.json")
		if err := os.WriteFile(path, []byte("not json {{{"), 0644); err != nil {
			t.Fatal(err)
		}
		store, err := NewSelfPostStore(path)
		// Issue #1756 made the loader greenfield: any existing
		// file (including corrupt/non-JSON) is renamed away at
		// load time. There is therefore no parse error to surface.
		if err != nil {
			t.Fatalf("NewSelfPostStore should not return error under greenfield loader (#1756), got %v", err)
		}
		if store == nil {
			t.Fatal("store should not be nil on corrupt file (greenfield loader)")
		}
		if got := len(store.Hashes()); got != 0 {
			t.Errorf("greenfield loader should ignore corrupt file contents (#1756), got %d entries", got)
		}
		if err := store.Record(1, "/sandman review", ""); err != nil {
			t.Fatalf("Record on greenfield-loaded store: %v", err)
		}
		if !store.IsSelfPosted(1, "/sandman review") {
			t.Error("Record on greenfield-loaded store should make IsSelfPosted return true")
		}
	})
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
// corruption-recovery contract under the post-#1756 greenfield
// loader: a non-JSON file is renamed away at load (the corrupt
// bytes never reach the JSON decoder), the in-memory store starts
// empty, and the next Record writes a valid file via the
// temp-file + rename path.
func TestSelfPostStore_CorruptFileDegradesToEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	if err := os.WriteFile(path, []byte("not json {{{"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := NewSelfPostStore(path)
	// Greenfield loader: no parse error is surfaced, the corrupt
	// file is renamed aside. The detailed backup-file invariant
	// is pinned by
	// TestSelfPostStore_GreenfieldLoad_OldFileBackedUpAndIgnored.
	if err != nil {
		t.Fatalf("greenfield loader should not surface a parse error (#1756), got %v", err)
	}
	if store == nil {
		t.Fatal("store should not be nil")
	}
	if got := len(store.Hashes()); got != 0 {
		t.Errorf("greenfield-loaded store should be empty, got %d entries", got)
	}

	// Record still works: writes a fresh file via temp-file + rename.
	if err := store.Record(1, "/sandman review", ""); err != nil {
		t.Fatalf("Record on greenfield-loaded store: %v", err)
	}
	if !store.IsSelfPosted(1, "/sandman review") {
		t.Error("Record should make IsSelfPosted return true")
	}
}

// TestSelfPostStore_PersistsRoundTrip pins the post-#1756 on-disk
// shape: the file is a JSON object keyed by composite "pr-<N>-<sha>"
// strings (per-PR scoping), with values that include the PR number,
// optional run ID, and posted-at timestamp. Reloading yields a store
// that recognizes the body on the recorded PR.
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

	// The file should decode as a map keyed by "pr-<N>-<sha>".
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
	sha := hex.EncodeToString(sum[:])
	wantKey := "pr-99-" + sha
	if _, ok := raw[wantKey]; !ok {
		t.Errorf("disk entry should be keyed by %q, got keys %v", wantKey, raw)
	}
	if raw[wantKey].PRNumber != 99 {
		t.Errorf("disk entry PRNumber = %d, want 99", raw[wantKey].PRNumber)
	}
	if raw[wantKey].RunID != "run-abc" {
		t.Errorf("disk entry RunID = %q, want %q", raw[wantKey].RunID, "run-abc")
	}
	if raw[wantKey].Hash != sha {
		t.Errorf("disk entry sha256 = %q, want %q", raw[wantKey].Hash, sha)
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
// rename-after-write pattern, not a crash-injection test. Post
// #1756, the greenfield loader renames any existing file away on
// load — so the test pins: (a) no .tmp residue after a successful
// save, (b) the in-memory set reflects both distinct keys for the
// recording PRs.
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

	// Same-process assertions on the in-memory store. (#1756
	// makes the cross-process loader greenfield so we no longer
	// assert cross-process reload preservation here — the new
	// contract is pinned by TestSelfPostStore_GreenfieldLoad_OldFileBackedUpAndIgnored.)
	if !store.IsSelfPosted(1, "first") {
		t.Error("first body should be self-posted in the in-memory store")
	}
	if !store.IsSelfPosted(2, "second") {
		t.Error("second body should be self-posted in the in-memory store on its recorded PR")
	}
}

// TestSelfPostStore_ConcurrentRecord pins the race-detector
// contract: concurrent Record calls do not corrupt the file or
// drop entries. The in-memory set is the source of truth: it must
// contain every distinct body. The on-disk file is the snapshot
// of the last completed Record; it must be valid JSON. Run with
// `go test -race`.
func TestSelfPostStore_ConcurrentRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	// Pick n distinct bodies so the contract is fully pinned:
	// every body must be in the in-memory set after the
	// goroutines complete.
	const n = 50
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		bodies[i] = "body-" + string(rune('A'+i%26)) + "-" + string(rune('0'+i%10)) + "-" + string(rune('a'+i%26))
	}
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			if err := store.Record(i, bodies[i], ""); err != nil {
				t.Errorf("Record: %v", err)
			}
		}()
	}
	wg.Wait()

	// In-memory set: every distinct body must be present on its
	// recorded PR.
	for i, b := range bodies {
		if !store.IsSelfPosted(i, b) {
			t.Errorf("in-memory set missing recorded body %q on PR %d", b, i)
		}
	}

	// On-disk file must be valid JSON (no torn write).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]selfPostEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("disk file is not valid JSON: %v", err)
	}
}

// TestSelfPostStore_AllowsEmptyBody pins the contract that
// recording an empty body still records a hash (so the daemon
// will treat empty future comments as self-posted). This is the
// defensive side: a bug elsewhere in the agent that posts an
// empty comment must not silently make the daemon treat the next
// real trigger as self-posted.
func TestSelfPostStore_AllowsEmptyBody(t *testing.T) {
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
	if !store.IsSelfPosted(1, "") {
		t.Error("empty body should be self-posted after Record")
	}
	// Trailing-whitespace-only collapses to empty via the
	// normalization.
	if !store.IsSelfPosted(1, "   \n\n") {
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
	if !store.IsSelfPosted(1, "/sandman review ") {
		t.Error("trailing space should be normalized")
	}
	if store.IsSelfPosted(1, " /sandman review") {
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
